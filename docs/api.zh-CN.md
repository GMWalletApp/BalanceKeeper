# BalanceKeeper HTTP API v1

本文为服务端与调用方的实现契约。[README](../README.md) · [ChainScanner 对接与各链 RPC](chains.zh-CN.md)

## 接口总览

| 操作 | 接口 | 每项除 `network`、`address` 外的参数 |
| --- | --- | --- |
| 初始化地址 | `POST /v1/watches` | 必填 `assets`，可传空数组 |
| 查询登记状态 | `POST /v1/watches/status` | 无 |
| 查询当前余额 | `POST /v1/balances/query` | 可选 `tokens` |
| 主动刷新余额 | `POST /v1/refreshes` | 可选 `tokens` |
| 查询快照历史 | `POST /v1/snapshots/query` | 可选 `tokens`、`limit`、`before` |
| 删除地址及其数据 | `POST /v1/watches/delete` | 无，按网络和地址整组删除 |

每个接口都接收非空 JSON 数组。每项必填一个具体的 `network` 和一个 `address`，包括只操作一项的请求。监听同一个 EVM 地址的多条链，要逐链填写数组项。路径和查询字符串都不携带业务参数。请求头使用 `Authorization: Bearer <BK_TOKEN>` 和 `Content-Type: application/json`。

余额、刷新和历史接口共用同一种选择方式：

| 数组项中的 `tokens` | 范围 |
| --- | --- |
| 省略 | 当前余额取正余额持仓；刷新取受理时的当前持仓和待确认资产；历史取该地址所有已保存快照，包括已归零资产。 |
| 含一个标识 | 仅选中该 token。原生币标识为 `native`。 |
| 含多个标识 | 选中该网络的这些资产，每个标识为合约地址、mint 或该链资产标识。 |

`tokens: []`、`tokens: null` 和重复的规范化 token 标识均为参数错误。登记状态和删除只接收网络与地址，传入 `tokens` 应拒绝，避免单币操作被误解为整地址操作。EVM 地址按链规则规范化，Solana 公钥区分大小写。余额以最小单位十进制整数字符串返回，时间为 UTC RFC 3339。

**批次与错误。** 除登记和删除的事务规则外，各目标独立处理。`results` 与请求数组等长、顺序相同，每项回显 `network` 和 `address`。格式错误、缺必填字段、未知字段、非法地址或不受支持的网络在执行业务前返回请求级错误；单项参数错误包含从零起计的 `error.index`。合法查询或刷新中，未登记地址对应的结果含 `error.code = watch_not_found`，其他目标可继续处理。查询类接口只读 SQLite。

## 初始化地址

`POST /v1/watches`。每项的 `assets` 只用于该地址在该网络的初始化。每项资产必填展示名称 `name` 和 `contract_address`（原生币传 JSON `null`）。空资产列表仍建立地址订阅；随后发现的新资产由 BalanceKeeper 自动维护。

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

`POST /v1/watches/status`。每项仅传网络和地址。成功结果含初始资产数、`subscription`（`pending`、`active` 或 `retrying`）和 `pending_assets`。后者列出正在等待首个成功读数的资产，可带 `last_error`。

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

`POST /v1/snapshots/query`。每项可独立填写 `limit`（默认 50，范围 1–200）及 `before`（字符串游标）。先按该项的网络、地址和 token 范围筛选，再按快照 ID 数值递减分页。每项得到独立的 `items` 和 `next_before`。下一次请求可以只传还有下一页的数组项，沿用原范围，并把该项的 `next_before` 填入 `before`。末页游标为 `null`，无历史的已登记地址返回空页。

```http
POST /v1/snapshots/query HTTP/1.1
Authorization: Bearer <BK_TOKEN>
Content-Type: application/json

[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","limit":1},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","tokens":["native"],"limit":10}
]
```

```json
{"results":[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","items":[{"asset":{"name":"USDC","contract_address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"},"snapshot":{"id":"44","amount_raw":"1250000","decimals":6,"height":"12345690","observed_at":"2026-11-22T08:30:24Z","source":"manual","event":null}}],"next_before":"44"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","items":[],"next_before":null}
]}
```

每条快照记录当时的资产名称、原始金额、精度（未知为 `null`）、观测时间、RPC 读取高度（无法精确取得时为 `null`）和来源。`source` 可为 `initial`、`manual`、`chainscanner`、`reconcile`。事件触发的快照在 `event` 中保存 `delivery_id`、`tx_id` 和事件高度，其余为 `null`。RPC 读取高度可晚于事件高度。

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
