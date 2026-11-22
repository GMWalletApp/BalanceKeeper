# BalanceKeeper

BalanceKeeper 为钱包和支付服务维护指定地址的资产余额。调用方用**网络、钱包地址、初始资产列表**登记一次地址。之后由 BalanceKeeper 维护该地址的当前持仓和余额历史。首次收到新代币时自动识别并查询余额，部分转出时更新余额，全部转出时记录零余额并从当前持仓移除。

服务端使用 Go 和 SQLite。余额查询直接读本地记录，主动刷新和事件消费向对应网络的节点读取余额。[完整 HTTP 接口](docs/api.zh-CN.md) · [ChainScanner 对接与各链 RPC](docs/chains.zh-CN.md)

BalanceKeeper 作为消费者调用 ChainScanner 现有的订阅和事件接口。部署时给 BalanceKeeper 配置 ChainScanner 地址和消费者令牌即可复用监听服务。各链的资产识别、余额查询和快照写入均由 BalanceKeeper 实现。

Broadcaster 的全局通知提供另一条刷新入口：节点接收交易后推送网络与哈希，BalanceKeeper 按哈希核验入块和受影响资产，只刷新本地跟踪的钱包。钱包地址只需登记在 BalanceKeeper。[广播通知与余额刷新](docs/api.zh-CN.md#broadcaster)

## 启动服务

使用 Go 1.26.2 或更高版本。从仓库根目录构建并复制配置。

```sh
go build -o balancekeeper .
cp config.example.json config.json
```

示例启用 Base 和 Ethereum。将这两个网络的 RPC 地址分别设为 `BASE_RPC_URL`、`ETHEREUM_RPC_URL`。将 ChainScanner 的服务地址和专属消费者令牌设为 `CHAINSCANNER_URL`、`CHAINSCANNER_CONSUMER_TOKEN`。设置供业务方调用的 `BK_TOKEN`，以及两端共享的独立签名密钥 `BROADCASTER_NOTIFICATION_SECRET`。三个凭证分别配置，每个至少 24 字节。只启用某个网络时，从配置里移除其余网络。

```sh
./balancekeeper -config config.json
```

服务启动时核对每个已配置 RPC 的网络身份。默认监听 `127.0.0.1:9082`，把 SQLite 文件和 WAL 放在同一持久盘。每个数据库运行一个实例，每个实例独占一个 ChainScanner 消费者令牌。Broadcaster 的 `broadcastNotifications.url` 填能访问此服务的 HTTPS 地址，路径为 `/v1/notifications/broadcasts`。仅用 ChainScanner 时可省略 `broadcastSecret`。[配置示例](config.example.json) · [Broadcaster 配置](https://github.com/GMWalletApp/Broadcaster/blob/main/docs/api.zh-CN.md#balancekeeper)

各网络可设 `bearer` 作为 RPC 的 Bearer 令牌。`confirmations` 是 EVM 余额读取距最新块的深度，默认为 0。Solana 使用 finalized，`cluster` 可填 Broadcaster 自定义的主网配置键。Sui 的 `rpc` 填 gRPC 服务根地址，Aptos、TRON 和 TON 填 HTTP 服务根地址。TON 需能访问 v2 余额接口和 v3 索引；索引位于独立地址时配置 `index` 和可选 `indexBearer`。运行 `go test -race ./...` 可使用临时数据库和本地节点模拟服务验证。

## 一次登记多个网络和地址

调用 `POST /v1/watches`，JSON 数组中每项必填 `network`、`address` 和该链的初始 `assets`。只登记一个地址时传一项。下面的一次请求在 Base 和 Ethereum 上登记同一地址，同时在 Base 上登记另一个地址。

```sh
export BK_URL=http://127.0.0.1:9082
# BK_TOKEN 与启动服务时配置的业务令牌相同

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
| 查询快照历史 | `POST /v1/snapshots/query` | 可选 `tokens`、`from`、`to`、`limit`、`before` |
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

## 余额何时刷新

首次导入地址并建立订阅后，逐项读取初始资产的链上余额。每项资产首次读成功就写入自己的第一条快照，包括零余额。空资产列表也会建立订阅。在支持钱包持仓枚举的网络上，后台对账还可发现已有持仓。重发同一登记请求会保留已有历史。

日常刷新由相关链上活动触发。ChainScanner 命中地址后，BalanceKeeper 重查受影响资产。Broadcaster 通知先启动交易核验，待交易入块、达到确认条件且余额节点跟上后再刷新。同一笔交易在同一区块经两个来源报告，同一登记地址的同一资产只写一次成功观测。手动刷新和订阅中断后的恢复对账也可以产生快照。

Solana、Sui、TON 以及配置了持仓索引的 EVM 网络，还会定期按钱包枚举资产以补查漏报。查询当前余额或历史只读本地记录。每个数据点的金额来自一次成功的链上读取，即使余额与上次相同也保留。两次读取之间可能发生未被通知的变动，例如合约内部转入 ETH。BalanceKeeper 提供按地址、资产和时间范围查询的快照数据，由前端绘制走势图。[快照查询参数与示例](docs/api.zh-CN.md#查询快照历史)

## 合约交易和新币发现

同一笔 swap 或批量交易可以使多个钱包同时收到新币、花掉旧币。即使 ChainScanner 通知只列出了第一笔转账，BalanceKeeper 也会按哈希补取整笔交易与完整回执，处理所有 ERC-20 `Transfer`，并重查命中钱包的已知资产，包括曾经归零的 token。4337 的每个 UserOperation 和 7702 的授权钱包分别匹配。节点提供调用树时，还会检查内部调用、委托执行和新部署的合约。

EVM 部署可以在单个网络的配置中加入 `"tokenInventoryRPC": "${BASE_TOKEN_INVENTORY_RPC}"`。该地址需同时支持对应网络的 `eth_chainId` 和分页 `alchemy_getTokenBalances`，Bearer 凭证可用 `tokenInventoryBearer` 配置。持仓索引用于发现完全没有转账日志的钱包持仓，快照金额始终由余额 RPC 核验。使用普通 EVM RPC 时，未知 token 若未出现在交易证据中，其静默入账可能无法发现。[发现范围与节点要求](docs/chains.zh-CN.md#asset-discovery)

## SQLite 选型

首版按单实例、单机持久盘设计，使用 SQLite WAL。网络请求在事务外完成，写入时将快照、当前余额指针和持仓状态放在同一短事务内。当前余额按网络、地址、资产定位，历史按地址与快照序号分页。写入量取决于**每秒成功刷新多少项资产**，也包括余额相同的刷新；历史空间随快照数增长。

本机 arm64、SQLite 3.51.0、WAL、`synchronous=FULL` 的单写者小测中，1,000 个地址各跟踪 10 项资产，每轮写 10,000 条快照，同时维护历史索引和当前指针。每事务写 1 项约 8,200 项/秒，每事务写 10 项约 69,000 项/秒。测试仅测本地短事务，未计入 RPC、订阅消费、并发读或长期历史。这个结果支持首版先用 SQLite；生产容量仍需用目标磁盘、实际资产数和峰值刷新流量验证。

## License

[AGPL-3.0](LICENSE)
