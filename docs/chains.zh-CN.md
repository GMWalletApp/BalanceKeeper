# ChainScanner 订阅与余额读取

本文定义 BalanceKeeper v1 内部消费者和多链适配器的实现约定。服务只接受配置文件已启用的网络。[BalanceKeeper HTTP API](api.zh-CN.md) · [上游接口](https://github.com/GMWalletApp/ChainScanner/blob/main/docs/api/zh-CN.md) · [上游网络清单](https://github.com/GMWalletApp/ChainScanner/blob/main/docs/guide/zh-CN.md#networks)

## 对接方向

调用方只登记到 BalanceKeeper。每个登记项指定一个具体的 `(network,address)`，一个请求可以提交多个这样的组合。即使地址格式在 EVM 链之间通用，也只向上游提交调用方逐链登记过的组合。BalanceKeeper 主动向 ChainScanner 提交钱包地址、按网络领取活动并确认投递。ChainScanner 沿用现有服务和接口，无需为 BalanceKeeper 修改代码或增加回调。下文的多链适配器都属于 BalanceKeeper，负责解析事件并直接调用链节点查询余额。

## 同步当前地址集合

BalanceKeeper 使用独占的 ChainScanner 消费者令牌。服务需要配置 ChainScanner 的 HTTP 地址、消费者令牌，以及各网络独立的余额 RPC。以下调用发往 **ChainScanner**；业务方只调用 BalanceKeeper。

```http
PUT /v1/subscriptions HTTP/1.1
Authorization: Bearer <CHAINSCANNER_CONSUMER_TOKEN>
Content-Type: application/json

[{"network":"base","address":"0x1111111111111111111111111111111111111111"},{"network":"ethereum","address":"0x1111111111111111111111111111111111111111"}]
```

1. 本地事务保存地址、资产集合、登记版本及待同步标记。批量登记整体提交后，同步器从 SQLite 汇总所有有效的 `(network,address)`，连同适配器所需的派生地址去重后，一次提交**跨网络完整集合**。钱包订阅的生命周期由地址登记决定。即使当前持仓为空，钱包地址也留在订阅清单中，才能收到首次或再次入账。普通代币的增减只更新本地资产列表。上游正文上限 8 MiB，拆成多个 PUT 会相互覆盖；超限应显式报告容量错误。
2. 同一消费者串行替换订阅。请求在途时若本地登记发生变化，收到回执后再送最新版。上游注册项含 `from_height`（含首块）。收到相应登记的成功回执后才启动新增资产的基线读取；在已订阅地址上新增普通代币可以直接初查。读基线时需确认节点已覆盖注册边界前一块。初查期间收到的事件照常持久化。
3. `200` 表示本次回执中的退订已完成，`202` 表示仍有地址排空到固定终止高度。按最新目标清单持续对账并读取退订网络的剩余事件。`409` 表示无法确定高度，原订阅保留，使用退避重试；参数错误应记录并修正。仅在数据库成功读取且确知目标为空时发送 `[]`。
4. 服务重启时重新提交本地完整目标，恢复待处理任务。若上游使用内存存储或曾丢失订阅，重建后对中断期间可能受影响的地址执行全资产刷新，标记为 `reconcile`。对可能漏掉的新资产，优先重放缺口内的地址活动，或通过支持按 owner 枚举的节点或索引补查候选。仅核验已知持仓时需保留发现缺口，直到补查完成。中断期间没有实际观测的历史时点不能补作快照。

## 消费、识别和确认

v1 使用 HTTP 长轮询，每个消费者在每个网络运行一个读取循环。上游还提供 gRPC 与 WebSocket，两者共享订阅和确认状态。[上游事件协议](https://github.com/GMWalletApp/ChainScanner/blob/main/docs/api/zh-CN.md#deliveries)

| 操作 | 请求与结果 |
| --- | --- |
| 领取下一条 | `GET /v1/events/next?network=base&wait=20`。带消费者 Bearer 令牌。200 返回事件，204 表示本轮空闲。客户端读取超时需大于 wait。 |
| 读取事件 | 外层 `id` 为投递 ID。`activity` 内有 `network`、`height`、`tx_id`、`time_ms`、`confirmations`、`addresses[]` 和链专属 `data`。同一投递可命中多个地址。 |
| 持久化与确认 | 在 SQLite 事务中保存完整事件和可处理的登记版本范围，再调用 `POST /v1/events/{id}/ack`，空正文，成功为 204。后台从收件箱解析资产并以事务提交工作项。确认后进程重启仍会继续处理。 |

对每个命中的钱包，按链从收付款方、日志、余额变动或写集中提取受影响的资产标识。无论资产是否在初始化列表中，都按规范化的合约、mint 或 coin type 创建余额核验任务。原生转账和手续费也可首次发现原生币。一条交易可同时更新多个钱包和多种资产，各钱包分别核验。

首次看到合约时先持久化候选项，解析支持的资产标准及元数据，再读取该钱包的余额。名称或精度暂缺时按链上标识入库，稍后补全元数据。收到与钱包无关的日志、交易失败或 RPC 超时，都不能单独作为增减持仓的依据。

若事件信息不足，先用网络与交易 ID 补取完整回执或交易数据。仍有歧义时，对支持按所有者枚举资产的链枚举候选并结合已知资产重查；其余链保留待解析事件并重试。重查当前已知资产只能核验已有持仓，不能据此判定新资产发现已完成。

工作项以消费者、网络、链上交易标识、地址、资产和本次登记周期去重，同时保留投递 ID 供确认。同一交易在链重组后重新纳入不同块时，按新块身份重新观测；缺少块哈希时，检测到分叉或位置冲突应触发重新对账。确认响应丢失后可再次确认，已持久化的同一工作项只执行到成功提交一次。显式删除钱包时按网络和地址清除本地资产、快照及相关工作项，再用剩余目标集合向上游退订。入库前重新核对地址的登记周期，丢弃已删除周期的在途结果。处理仍含其他地址的投递时只为有效登记生成任务。重新登记使用新周期，并根据新订阅的起始高度隔离迟到的旧投递。单项资产归零不结束钱包登记周期，再次收到同一资产时沿用原标识和历史。

## 各链读取与事件解析

| 网络与登记标识 | BalanceKeeper 本地余额 RPC | ChainScanner 事件如何选资产 |
| --- | --- | --- |
| `bitcoin`，原生 BTC | 从 Esplora 的 `chain_stats` 读取已确认的收入与支出，以聪记账。配置 `confirmations` 后，分页回退目标高度以上的地址交易。核对快照高度的块哈希及索引链尖。 | 按 txid 重读交易，以输入前序输出和输出锁定脚本匹配钱包。支持 P2PKH、P2SH、P2WPKH、P2WSH 和 P2TR。[节点清单](https://github.com/GMWalletApp/ChainScanner/blob/main/docs/bitcoin-nodes.zh-CN.md) |
| EVM 原生币或 ERC-20 合约 | 原生币用 `eth_getBalance`，ERC-20 用 `eth_call` 调合约的 `balanceOf(owner)`。同次刷新固定已确认区块并核对区块身份。 | 从钱包参与的 Transfer 日志取得发出合约，核验 ERC-20 转账格式以区分 ERC-721 等日志，并查询该合约余额。原生转账、手续费付款方以及可用 trace 中的内部转账均参与匹配，失败交易也可能消耗 gas。[Ethereum RPC](https://ethereum.org/developers/docs/apis/json-rpc/) |
| `tron`，TRX 或 TRC-20 合约 | 在固化节点用 `/walletsolidity/getaccount` 读 TRX，用 `/walletsolidity/triggerconstantcontract` 调 TRC-20 的 `balanceOf(address)`。 | 解析转账、回执日志、内部转账和手续费，统一 Base58Check 与带网络前缀的十六进制表示。[账户接口](https://developers.tron.network/reference/walletsolidity-getaccount) · [只读合约调用](https://developers.tron.network/reference/triggerconstantcontract-1) |
| `solana`，SOL 或 SPL mint | SOL 用 `getBalance`。SPL 用 `getTokenAccountsByOwner(owner,{mint})` 读取该所有者在该 mint 下的所有账户，累加原始整数金额，采用 finalized commitment，记录返回的 context slot。 | 用 `preTokenBalances`、`postTokenBalances` 的 owner、mint 和账户索引识别变动，覆盖新建与关闭账户。原生币使用交易账户及前后 lamports，包含手续费。[Solana RPC](https://solana.com/docs/rpc/http/gettokenaccountsbyowner) |
| `sui`，原生 SUI 或完整 coin type | 用 gRPC v2 `StateService/GetBalance(owner,coin_type)` 读取总余额，原生币映射为 `0x2::sui::SUI`。总额包含 coin objects 和地址余额。 | 解析 gRPC 的 `balance_changes` 或 GraphQL 的 `balanceChanges`；按所有者、coin type 匹配，并检查 gas 赞助者。[Sui 余额接口](https://www.docs.sui.io/onchain-finance/asset-custody/address-balances/using-address-balances) |
| `aptos`，APT、完整 Coin type 或 FA metadata 地址 | 通过全节点 REST `GET /v1/accounts/{address}/balance/{asset_type}` 读取账户标准余额；原生 APT 映射到 `0x1::aptos_coin::AptosCoin`。 | 从交易事件、写集和费用付款方识别 Coin 或主存储 FA；从 store 解析所有者与 metadata，映射不完整时执行地址全刷。[Aptos 余额接口](https://aptos.dev/build/sdks/python-sdk/fetch-data-via-sdk) |
| `ton`，原生币或 Jetton master | 从账户状态读原生余额。对 master 调 `get_wallet_address(owner)` 派生 Jetton wallet，再调用 wallet 的 `get_wallet_data`，核对 owner 和 master 后读余额。 | 收到未知 Jetton 的通知或相关消息时从候选 wallet 解析 owner 与 master，再向 master 反查派生地址，验证后加入派生 wallet 订阅。保留已知派生 wallet 映射，即使归零也可识别再次入账。服务也会在登记生效后通过 TON v3 的 owner 索引发现未订阅的 Jetton wallet，此后每个 owner 约五分钟检查一次。派生失败须可重试，不能视为零余额。[TON Jetton 处理](https://docs.ton.org/applications/payments/jettons) |

EVM 适配器共用一套读取逻辑，网络 ID 与 ChainScanner 保持一致：`ethereum`、`arbitrum`、`binance`、`avalanche`、`polygon`、`optimism`、`base`、`hyperevm`、`monad`、`arc`、`plasma`、`robinhood`。启动时核对节点 chain ID。各链原生资产均以 `contract_address:null` 登记；已知与原生资产等价的链上别名应归一或在登记时拒绝重复。

<a id="asset-discovery"></a>

## 复杂交易的资产发现范围

| 交易情况 | 如何确定需要重读的资产 |
| --- | --- |
| DEX swap、路由、多跳与批量转账 | 按哈希读取交易和完整回执，核对交易哈希、区块号和规范区块哈希。遍历全部日志，将每笔 ERC-20 转账的合约分别分配给真实收发钱包。同一钱包、区块内交易和 token 共用一次成功观测。 |
| ERC-4337 与 EIP-7702 | 对每个 `UserOperationEvent` 分别匹配 sender 和 paymaster，再处理整笔交易的代币费用、退款和转账。对 7702 授权组验签恢复授权者，并按本链 chain ID 过滤。授权目标指向的代码地址与授权钱包分别处理。 |
| 内部调用、代理执行、部署新币 | 尝试 `debug_traceTransaction` 的 `callTracer`，遍历成功调用、`DELEGATECALL` 和 `CREATE/CREATE2`，跳过回滚的子树。从调用参数、事件主题和事件数据中的 ABI 地址字匹配已登记钱包，可发现只出现在自定义 mint 参数里的收款方。对回执或调用树中出现的未知合约，在事件高度检查钱包的 `balanceOf`、ERC-20 的 `allowance` 和 `totalSupply`，并排除声明 ERC-721 的合约。符合条件后进入正式余额读取队列。历史状态读取失败会重试。 |
| 通知遗漏第二种币、此前已归零的币再次到账 | 对命中的每个登记钱包，同时重查已有的资产记录。完整回执已经识别的资产可先写快照，临时 trace 故障独立退避重试，补到的内部资产复用同一交易身份。 |
| 无可识别日志的变动 | Solana 枚举两个 token program 的持仓，Sui 分页枚举 coin type，TON 使用 Jetton owner 索引。EVM 可配置 `tokenInventoryRPC` 分页枚举钱包持仓。索引金额仅用于识别差异；新币、余额差异及索引中消失的旧持仓均通过本链 RPC 核验。 |

完整回执覆盖整笔交易中符合 ERC-20 规范的转账事件，包括嵌套调用产生的日志。标准 EVM RPC 没有按 owner 列出所有 ERC-20 的方法。对于静默 rebase、只修改内部记账的协议，或从未出现在回执及可用调用树里的新 token，发现能力取决于持仓索引覆盖范围。索引延迟期间保留上次成功读数；后续对账写入实际读到的余额。ERC-721、ERC-1155 和协议内部份额不在本服务的同质化 token 余额模型中。[ERC-20 事件规范](https://eips.ethereum.org/EIPS/eip-20) · [Geth 调用树](https://geth.ethereum.org/docs/developers/evm-tracing/built-in-tracers) · [ERC-4337](https://eips.ethereum.org/EIPS/eip-4337) · [EIP-7702](https://eips.ethereum.org/EIPS/eip-7702) · [持仓索引接口](https://www.alchemy.com/docs/data/token-api/token-api-endpoints/alchemy-get-token-balances)

<a id="broadcast-refresh"></a>

## 按广播哈希触发余额刷新

1. **开始核验。** 收件箱按发送来源和 `eventId` 去重；入块证据按本地网络和实际交易哈希合并 `broadcast.accepted`、`broadcast.included`。入块通知可立即唤醒等待任务。EVM 可通过 ChainScanner 已有的交易查询或本地节点查询交易及回执；Solana 按签名查 finalized 交易。若先收到入块事件，后到的接收事件不能让任务退回 pending。
2. **解析真正的交易。** 原生链按适配器读取交易证据。TON 广播标识是规范化的外部消息哈希，须先通过入站消息索引找到实际交易，或使用入块通知中的 `txHash`，并核对消息与交易关联。其他链保留各自的签名、digest 或 txid 编码。发现替换时核验胜出哈希，不能拿新版本的转账参数为旧版本记账。
3. **筛选和刷新。** 从交易、回执、代币日志和可用的内部调用证据提取地址及候选资产，复用各链的解析规则。匹配范围是本网络的有效登记钱包和派生地址，包含首次收到的新币。证据足以排除全部登记地址时结束；证据不足则继续补查。每次读取余额前确认节点已覆盖实际入块位置，并在落库前核对登记周期。已明确丢弃、过期或被替换且未入块的版本不产生交易快照；长期缺少证据时保留待对账状态，允许后续入块证据恢复处理。
4. **合并两个来源。** ChainScanner 和 Broadcaster 最终用本地网络、实际链上交易标识、规范块身份、地址登记周期及资产标识关联同一刷新工作。唯一键与快照写入在事务中生效。两个来源同时到达只生成一次成功观测，保存两者的事件关联；对方补充识别的新资产仍要刷新。手动刷新是独立观测。链重组并重新纳入时按新的块身份重新核验。

## 快照一致性

- 每次成功观测保存来源、最小单位金额、精度、刷新时间、可确定的读取位置及该位置对应的区块时间。区块时间无法核实时记为 `null`。金额直接取自本次链上余额查询。事件触发时另外保存对应的交易与事件高度。若适配器只能读最新状态，应先确认所用节点已追到事件位置；无法把返回值精确绑定到某个高度时，快照高度写 `null`。
- 同一地址资产的初查、事件和主动刷新共用串行执行顺序。节点回退至已知最新位置之前时换节点或重试；缺少精确高度时需使用满足最低进度的节点并串行读取。RPC 在写事务外执行。成功时在同一短事务中检查钱包登记周期、追加余额快照、更新当前指针，并按金额维护当前持仓。正余额加入或保留，零余额移出并保留历史，再将工作项置为完成，防止重投多写或旧结果覆盖新值。
- RPC 超时、合约调用失败、缺少字段和确定的链上零余额应分别处理。部分失败保留成功项，失败项记录错误，后台任务退避重试。某个资产的节点故障不阻塞其余地址。
- 地址订阅只覆盖 ChainScanner 能匹配的链上活动。例如在 EVM 上，合约内部给钱包转入 ETH，收款地址只出现在调用树中时，关闭调用追踪会漏掉该地址通知。标准 ERC-20 转账则可通过 `Transfer` 的索引收款地址匹配。漏报期间的余额变动可在下次相关刷新、主动刷新或恢复对账时反映到新快照中。[ChainScanner 的 EVM 地址匹配](https://github.com/GMWalletApp/ChainScanner/blob/c8ca3a7407e33662c6ad62486c9e98960f70cc1c/internal/chains/evm.go#L142-L185)
