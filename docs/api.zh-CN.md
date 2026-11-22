# BalanceKeeper HTTP API v1

本文为服务端与调用方的实现契约。[README](../README.md) · [ChainScanner 对接与各链 RPC](chains.zh-CN.md)

## 接口总览

| 操作 | 接口 | 每项除 `network`、`address` 外的参数 |
| --- | --- | --- |
| 初始化地址 | `POST /v1/watches` | 必填 `assets`，可传空数组 |
| 查询登记状态 | `POST /v1/watches/status` | 无 |
| 查询当前余额 | `POST /v1/balances/query` | 可选 `tokens` |
| 主动刷新余额 | `POST /v1/refreshes` | 可选 `tokens` |
| 查询快照历史 | `POST /v1/snapshots/query` | 可选 `tokens`、`from`、`to`、`limit`、`before` |
| 删除地址及其数据 | `POST /v1/watches/delete` | 无，按网络和地址整组删除 |

上表六个业务接口都接收非空 JSON 数组。每项必填一个具体的 `network` 和一个 `address`，包括只操作一项的请求。监听同一个 EVM 地址的多条链，要逐链填写数组项。路径和查询字符串都不携带业务参数。请求头使用 `Authorization: Bearer <BK_TOKEN>` 和 `Content-Type: application/json`。

余额、刷新和历史接口共用同一种选择方式：

| 数组项中的 `tokens` | 范围 |
| --- | --- |
| 省略 | 当前余额取正余额持仓；刷新取受理时的当前持仓和待确认资产；历史取该地址所有已保存快照，包括已归零资产。 |
| 含一个标识 | 仅选中该 token。原生币标识为 `native`。 |
| 含多个标识 | 选中该网络的这些资产，每个标识为合约地址、mint 或该链资产标识。 |

`tokens: []`、`tokens: null` 和重复的规范化 token 标识均为参数错误。登记状态和删除只接收网络与地址，传入 `tokens` 应拒绝，避免单币操作被误解为整地址操作。EVM 地址按链规则规范化，Solana 公钥区分大小写。余额以最小单位十进制整数字符串返回，时间为 UTC RFC 3339。

**批次与错误。** 除登记和删除的事务规则外，各目标独立处理。`results` 与请求数组等长、顺序相同，每项回显 `network` 和 `address`。格式错误、缺必填字段、未知字段、非法地址或不受支持的网络在执行业务前返回请求级错误；单项参数错误包含从零起计的 `error.index`。合法查询或刷新中，未登记地址对应的结果含 `error.code = watch_not_found`，其他目标可继续处理。查询类接口只读 SQLite。

## 初始化地址

`POST /v1/watches`。每项的 `assets` 只用于该地址在该网络的初始化。每项资产必填展示名称 `name` 和 `contract_address`（原生币传 JSON `null`）。空资产列表仍建立地址订阅；随后发现的新资产由 BalanceKeeper 自动维护。收到订阅成功回执后，按初始列表逐项查询链上余额。每项首次读取成功时写入一条 `initial` 快照，零余额也写入。读取失败的项保留待重试。空列表和重复提交同一登记请求都不会额外生成初始快照。

```http
POST /v1/watches HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","assets":[{"name":"ETH","contract_address":null},{"name":"USDC","contract_address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"}]},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","assets":[]},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","assets":[]}
]
```

整批在一个 SQLite 事务中提交，至少一项新增时返回 HTTP 202，全部为等价的已有登记时返回 200。批内重复的规范化网络地址组合或重复资产返回 400。已登记地址的原始初始化集合与本次不同则返回 409，整批不写入。等价性忽略资产排列顺序。重发保留自动发现的持仓和快照。

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","initial_asset_count":2,"subscription":"pending"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","initial_asset_count":0,"subscription":"pending"},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","initial_asset_count":0,"subscription":"pending"}
]}
```

## 查询登记状态

`POST /v1/watches/status`。每项仅传网络和地址。成功结果含初始资产数、`subscription`（`pending`、`active` 或 `retrying`）和 `pending_assets`。后者列出正在等待首个成功读数的资产，可带 `last_error`。上游订阅边界前移后，`discovery_gap` 会显示尚在补查的高度区间（`from_height`、`to_height`，含两端）。

```http
POST /v1/watches/status HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111"}
]
```

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","initial_asset_count":2,"subscription":"active","pending_assets":[]},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","initial_asset_count":0,"subscription":"active","pending_assets":[]}
]}
```

## 查询当前余额

`POST /v1/balances/query`。按每项范围返回 `balances` 键值对。整地址读取只列出最新成功读数大于零的资产，无持仓时为 `{}`。指定 token 时，已确认归零返回 `"0"`，正在初查返回 `null`，从未观测且未进入初查的 token 列入 `errors`。一次 RPC 失败不会覆盖最近成功读数。

```http
POST /v1/balances/query HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","tokens":["native"]},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","tokens":["native","0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"]}
]
```

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","balances":{"native":"1000000000000000000","0x833589fcd6edb6e08f4c7c32d4f71b54bda02913":"1250000"}},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","balances":{"native":"0"}},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","balances":{"native":null},"errors":{"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913":{"code":"token_not_found","message":"尚无该资产的观测"}}}
]}
```

## 主动刷新

`POST /v1/refreshes`。同一批可混合整地址、单 token 和多 token。显式指定的受支持资产即使从未持有或已经归零，也可触发 RPC 核验。批内重叠的目标共用一次资产读取和一条快照，各项分别返回所选范围内的结果。

```http
POST /v1/refreshes HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","tokens":["native"]},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","tokens":["native","0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"]}
]
```

同步等待各项执行。每项的 `balances` 是本次已写入快照的余额，`errors` 按 token 列出读取失败原因。状态为 `succeeded`、`partial` 或 `failed`。整地址没有待刷新资产时返回成功及空映射。每次成功的资产读取均写入一条快照，余额相同也写入；正余额加入或保留持仓，零余额移出持仓并保留历史。失败资产保留旧数据。

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","status":"succeeded","balances":{"native":"1000000000000000000","0x833589fcd6edb6e08f4c7c32d4f71b54bda02913":"1250000"}},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","status":"succeeded","balances":{"native":"0"}},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","status":"partial","balances":{"native":"0"},"errors":{"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913":{"code":"rpc_timeout","message":"余额查询超时"}}}
]}
```

批次执行结束返回 HTTP 200，调用方逐项检查结果。即使部分 RPC 失败，已提交的快照也保留。重试时可只发送失败的网络、地址与 token。再次提交成功项会产生新的观测。

## 查询快照历史

`POST /v1/snapshots/query`。每项可独立填写 `limit`（默认 50，范围 1–200）及 `before`（字符串游标）。可选 `from`（含）和 `to`（不含）按刷新时间 `observed_at` 筛选，使用 UTC RFC 3339 时间；省略一端表示该端不限。时间格式无效或同时给出两端且 `from >= to` 时返回 HTTP 400 并指出该项索引。先按网络、地址、token 和时间范围筛选，再按快照 ID 数值递减分页。每项得到独立的 `items` 和 `next_before`。下一次请求可以只传还有下一页的数组项，沿用原范围，并把该项的 `next_before` 填入 `before`。末页游标为 `null`，无历史的已登记地址返回空页。

```http
POST /v1/snapshots/query HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","from":"2026-11-22T00:00:00Z","to":"2026-11-23T00:00:00Z","limit":1},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","tokens":["native"],"from":"2026-11-22T00:00:00Z","to":"2026-11-23T00:00:00Z","limit":10}
]
```

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","items":[{"asset":{"name":"USDC","contract_address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"},"snapshot":{"id":"44","amount_raw":"1250000","decimals":6,"height":"12345690","block_time":"2026-11-22T08:30:20Z","observed_at":"2026-11-22T08:30:24Z","source":"manual","event":null}}],"next_before":"44"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","items":[],"next_before":null}
]}
```

每条快照记录当时的资产名称、原始金额、精度（未知为 `null`）、观测时间、RPC 读取高度（无法精确取得时为 `null`）和来源。`source` 可为 `initial`、`manual`、`chainscanner`、`broadcaster`、`reconcile`。事件触发的快照在 `event` 中保存 `tx_id` 和事件高度。ChainScanner 来源另存 `delivery_id`，Broadcaster 来源另存 `event_id` 和 `broadcast_hash`。非事件触发时 `event` 为 `null`。RPC 读取高度可晚于事件高度。

`observed_at` 是成功读取余额的刷新时间，每条快照都有。`block_time` 是该余额对应区块的链上时间，使用 UTC RFC 3339；无法核实对应区块或其时间时返回 `null`。触发通知的交易可能早于余额读取，两者各用各自的链上位置。

接口返回各资产实际采集的快照，前端可据此绘图。整地址结果中各资产可能在不同时间刷新。两个数据点之间没有快照，只表示该资产在这段时间没有新的余额读数。

## 删除地址及其全部记录

`POST /v1/watches/delete`。每项只接受具体网络和钱包地址。整批校验后在同一事务内清除所列目标的登记、初始化列表、当前及已归零 token 记录、余额、全部快照、派生地址关联和待处理任务，并标记需要同步上游订阅。校验失败则整批不删除。成功返回 HTTP 200，逐项标明 `deleted` 或 `already_absent`。其他网络或地址的数据独立保留。

```http
POST /v1/watches/delete HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111"},
  {"network":"base","address":"0x2222222222222222222222222222222222222222"}
]
```

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","status":"deleted"},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","status":"already_absent"}
]}
```

本地提交后异步退订。迟到投递或在途刷新不能恢复被删除周期的数据。重新登记会建立新的登记周期，历史从空开始。

<a id="broadcaster"></a>

## 接收 Broadcaster 全局广播事件

`POST /v1/notifications/broadcasts`。这是 Broadcaster 调用的服务间接口。Broadcaster 向同一端点发送所有提交方的广播事件，BalanceKeeper 自行筛选钱包。发送方只需知道接收 URL 和签名密钥。[发送方契约](https://github.com/GMWalletApp/Broadcaster/blob/main/docs/api.zh-CN.md#balancekeeper)

请求体为非空 JSON 数组，可混合网络。每项必填 `schemaVersion`（整数 1）、`eventId`（稳定字符串）、`event`、`family`、`network`、`hash`。`broadcast.accepted` 表示节点首次确认接收该签名版本或已认识它。`broadcast.included` 表示 Broadcaster 已观察到入块，还须携带 `txHash`。后者单独到达时也应启动核验。正文仅传路由字段和哈希，钱包地址由接收方查交易取得。

```json
[
  {
    "schemaVersion": 1,
    "eventId": "8af9bce60622455c8710516cde6d9981",
    "event": "broadcast.accepted",
    "family": "evm",
    "network": "8453",
    "hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  {
    "schemaVersion": 1,
    "eventId": "51a0053ad0c34a84834dbee02ab77d69",
    "event": "broadcast.included",
    "family": "evm",
    "network": "8453",
    "hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "txHash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
]
```

| 接收结果 | 响应及行为 |
| --- | --- |
| 整批验签、校验并持久化完成 | `204` 空正文。同一来源的相同事件 ID 重投只复用已有记录。 |
| 签名无效或缺失 | `401`。用接收端独立密钥验证 `X-Broadcaster-Signature`，对原始正文计算 HMAC-SHA256，按小写十六进制常量时间比较。此端点使用 Webhook 签名鉴权。 |
| 空批、非法字段、未知版本/事件类型或同一 ID 内容冲突 | `400`，整批不提交。有问题的条目用 `error.index` 返回零起始下标。 |
| 暂时限流或无法持久化 | `429` 或 `503`，可附 `Retry-After`。发送方可重投整批。 |

**网络和资产筛选。** 先把发送方的 `(family, network)` 映射成本地具体网络，例如 `(evm, "8453") → base`、`(evm, "1") → ethereum`。Solana 的自定义 cluster 名须显式映射到已验证的同一网络。Bitcoin 主网映射为 `(bitcoin, "bitcoin") → bitcoin`，本部署未启用的网络记录为已忽略并确认，不能阻塞批内其他网络。没有登记钱包的网络可直接跳过。

收到受支持网络的事件后，通过交易哈希核验链上位置，解析发送方、实际收款方、日志、代付手续费账户和受影响资产，再与本地登记地址（含派生地址）匹配。例如 Base 上转入 USDC 时，交易的 `to` 可以是 USDC 合约；应从回执 Transfer 日志取出真正的钱包。命中后，即使该钱包之前没有 USDC，也将它交给本地余额读取和资产发现流程。[余额刷新与去重](chains.zh-CN.md#broadcast-refresh)

**确认时机。** 返回 204 只表示收件箱已提交。后台随即查哈希；pending、回执缺失、RPC 滞后时保留任务并退避。确认入块、满足本链确认策略且余额节点已追上后，才写余额快照。执行回退的交易仍需核验实际支付的手续费。

## 请求级错误

```json
{"error":{"code":"invalid_request","index":1,"message":"该项缺少 network"}}
```

| HTTP | 含义 |
| --- | --- |
| 400 | 请求格式、字段、地址、token 选择或游标无效。 |
| 401 | 访问令牌无效。 |
| 409 | 已登记地址的初始化集合冲突。 |
| 422 | 网络或指定资产类型未启用。 |
| 503 | 存储不可用。分项提交的刷新结果可在服务恢复后查询历史核对。 |
