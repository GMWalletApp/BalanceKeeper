# BalanceKeeper

BalanceKeeper 为钱包和支付服务维护指定地址的资产余额。调用方用**网络、钱包地址、初始资产列表**登记一次地址。之后由 BalanceKeeper 维护该地址的当前持仓和余额历史。首次收到新代币时自动识别并查询余额，部分转出时更新余额，全部转出时记录零余额并从当前持仓移除。

**当前阶段为 v1 接口设计。** 本仓库本次交付 README 和接口文档，以下请求用于开发和联调对照，服务端与链适配器待实现。[完整 HTTP 接口](docs/api.zh-CN.md) · [ChainScanner 对接与各链 RPC](docs/chains.zh-CN.md)

BalanceKeeper 作为消费者调用 ChainScanner 现有的订阅和事件接口。部署时给 BalanceKeeper 配置 ChainScanner 地址和消费者令牌即可复用监听服务。各链的资产识别、余额查询和快照写入均由 BalanceKeeper 实现。

Broadcaster 的全局通知提供另一条刷新入口：节点接收交易后推送网络与哈希，BalanceKeeper 按哈希核验入块和受影响资产，只刷新本地跟踪的钱包。钱包地址只需登记在 BalanceKeeper。[广播通知与余额刷新设计](docs/api.zh-CN.md#broadcaster)

## 一次登记多个网络和地址

调用 `POST /v1/watches`，JSON 数组中每项必填 `network`、`address` 和该链的初始 `assets`。只登记一个地址时传一项。下面的一次请求在 Base 和 Ethereum 上登记同一地址，同时在 Base 上登记另一个地址。

```sh
export BK_URL=http://127.0.0.1:9082
export BK_TOKEN=your-balancekeeper-token

curl -sS -X POST "$BK_URL/v1/watches" \
  -H "Authorization: Bearer $BK_TOKEN" -H 'Content-Type: application/json' \
  -d '[
  {"network":"base","address":"0x1111111111111111111111111111111111111111","assets":[{"name":"ETH","contract_address":null},{"name":"USDC","contract_address":"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"}]},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","assets":[]},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","assets":[]}
]'
```

每项只监听明确指定的网络。监听所有目标 EVM 链时，在同一数组中逐链列出地址即可。空的 `assets` 也会建立订阅，以便发现后续入账。首次登记返回 `202`，原样重发已成功的批次返回 `200`，保留此后自动发现的持仓和历史。[具体网络 ID](docs/chains.zh-CN.md)

## 批量接口

所有业务接口都使用固定路径，接收非空 JSON 数组。查询一个地址时只传一项。每项独立填写具体网络和钱包地址。

| 操作 | 接口 | 每项除 `network`、`address` 外的参数 |
| --- | --- | --- |
| 初始化地址 | `POST /v1/watches` | 必填 `assets`，可传空数组 |
| 查询登记状态 | `POST /v1/watches/status` | 无 |
| 查询当前余额 | `POST /v1/balances/query` | 可选 `tokens` |
| 主动刷新余额 | `POST /v1/refreshes` | 可选 `tokens` |
| 查询快照历史 | `POST /v1/snapshots/query` | 可选 `tokens`、`limit`、`before` |
| 删除地址及其数据 | `POST /v1/watches/delete` | 无，按网络和地址整组删除 |

余额、刷新和历史查询共用 `tokens` 选择规则：省略表示整地址，传一个或多个标识表示只操作这些资产。原生币使用 `native`，其他资产使用规范化的合约地址或链上标识。

例如，下面用一次请求刷新 Base 上第一个地址的整组持仓、Ethereum 上同一地址的 ETH，以及 Base 上第二个地址的 ETH 和 USDC。

```sh
curl -sS -X POST "$BK_URL/v1/refreshes" \
  -H "Authorization: Bearer $BK_TOKEN" -H 'Content-Type: application/json' \
  -d '[
  {"network":"base","address":"0x1111111111111111111111111111111111111111"},
  {"network":"ethereum","address":"0x1111111111111111111111111111111111111111","tokens":["native"]},
  {"network":"base","address":"0x2222222222222222222222222222222222222222","tokens":["native","0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"]}
]'
```

把同一请求体发给 `/v1/balances/query` 可以读取当前余额，每个地址的结果里用 `balances` 返回资产与最小单位余额的键值对。发给 `/v1/snapshots/query` 可以查历史，每项有独立的分页游标。批量删除会清除所列网络和地址的 token 列表、余额、历史和待处理任务，并触发退订。[完整请求与响应示例](docs/api.zh-CN.md)

## 一种新代币从收到到转出的完整过程

假设上述地址只用 ETH 和 USDC 初始化，随后收到一种此前从未出现的代币 T。ChainScanner 投递这笔地址活动，BalanceKeeper 从收款方和转账日志中取出 T 的合约，直接调用该网络的 RPC 查询余额。调用方无需再次登记。

| T 的链上余额（示例） | 当前资产列表 | 本次写入的余额快照 |
| --- | --- | --- |
| 首次收到 10 T | 加入 T | 10 T |
| 转出 3 T，剩余 7 T | 保留 T | 7 T |
| 再转出 7 T，余额为零 | 移出 T，钱包地址继续订阅 | 0 T，之前的快照仍可查 |
| 再次收到 2 T | 恢复同一合约的 T | 2 T，接续原有历史 |

以上每一步均以 RPC 实际读到的余额为准。快照、当前余额和资产列表在同一事务内更新。刷新结果相同也留一条观测记录。RPC 超时则保留上次成功值，已发现的新合约留待重试。

## SQLite 选型

首版按单实例、单机持久盘设计，使用 SQLite WAL。网络请求在事务外完成，写入时将快照、当前余额指针和持仓状态放在同一短事务内。当前余额按网络、地址、资产定位，历史按地址与快照序号分页。写入量取决于**每秒成功刷新多少项资产**，也包括余额相同的刷新；历史空间随快照数增长。

本机 arm64、SQLite 3.51.0、WAL、`synchronous=FULL` 的单写者小测中，1,000 个地址各跟踪 10 项资产，每轮写 10,000 条快照，同时维护历史索引和当前指针。每事务写 1 项约 8,200 项/秒，每事务写 10 项约 69,000 项/秒。测试仅测本地短事务，未计入 RPC、订阅消费、并发读或长期历史。这个结果支持首版先用 SQLite；生产容量仍需用目标磁盘、实际资产数和峰值刷新流量验证。

## License

[AGPL-3.0](LICENSE)
