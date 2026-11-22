# 主备节点配置

BalanceKeeper 自己查询交易、回执、余额和持仓。每个启用的网络在 `networks` 中配置一个主节点 `rpc`。省略 `backup` 时，服务自动采用该网络的默认备用节点。设置 `backup.rpc` 可以改用自己的第二个节点。若主节点已使用内置备用地址，请显式指定另一个备用地址。[18 个主网的完整配置与环境变量](../config.mainnets.example.json)

下面的配置启用 Base 和 Bitcoin。主节点地址由部署环境提供。Base 使用默认备用，Bitcoin 显式指定备用 Esplora 索引。

```json
{
  "networks": {
    "base": {
      "rpc": "${BASE_RPC_URL}",
      "bearer": "${BASE_RPC_TOKEN}",
      "confirmations": 12
    },
    "bitcoin": {
      "rpc": "${BITCOIN_ESPLORA_URL}",
      "backup": { "rpc": "https://mempool.space/api" }
    }
  }
}
```

每次读取从主节点开始。遇到连接超时、限流、链身份错误、节点落后或必需数据缺失时，服务在备用节点重新核验网络并重做本次读取。余额核验、交易证据和分页枚举各自构成独立读取。分页切换从第一页开始，避免混用两端的游标或结果。下一次读取仍先访问主节点。如果两端都无法提供有效余额，保留上次成功值，失败的刷新任务保存在 SQLite 并退避重试。启动时每个已启用网络至少要有一端通过网络身份检查。

## 各链需要的节点接口

| 网络 | 主备两端需要提供的接口 | 内置备用 |
| --- | --- | --- |
| Ethereum、Arbitrum、BNB Smart Chain、Avalanche C-Chain、Polygon PoS、Optimism、Base、HyperEVM、Arc、Robinhood | EVM JSON-RPC。用链 ID 和区块哈希核验网络与快照，读取原生余额、ERC-20、交易、完整回执和日志。内部调用分析使用支持 `callTracer` 的 `debug_traceTransaction`。 | PublicNode 的对应主网入口 |
| Monad、Plasma | 同上 | MonadInfra、Plasma 官方 RPC |
| Solana | finalized JSON-RPC。支持按 mint 和 program ID 调用 `getTokenAccountsByOwner`，覆盖 SPL Token 与 Token-2022，并支持 `getTransaction`。 | Solana Vibe Station。匿名 PublicNode 对按钱包查询代币账户有限制 |
| Sui | 原生 gRPC v2，使用 LedgerService 和 StateService 查询交易、余额和持仓分页。填主机和端口，可用 `https://` 或 `grpcs://` 指定 TLS。 | PublicNode gRPC |
| Aptos | 全节点 REST v1，读取指定 ledger version 的 Coin/FA 余额、交易和账户资源。地址可含 `/v1`。 | PublicNode REST |
| TRON | HTTP Wallet 和 WalletSolidity，读取固化区块、TRX、TRC-20 只读合约调用与交易回执。 | PublicNode TRON HTTP |
| TON | TON Center 兼容 v2 读取 TON、Jetton 合约，v3 查询交易、消息、Jetton 持仓。独立索引在同一组配置的 `index` 中填写。 | TON Center v2 和 v3 |
| Bitcoin | Esplora REST，需支持区块、已确认地址统计、地址交易分页及带前序输出的交易。 | mempool.space Esplora |

链的精确标识和 HTTPS 地址见[主网配置清单](../config.mainnets.example.json)。资产标准、确认规则和事件解析见[多链适配器](chains.zh-CN.md)。

## 认证和可选索引

- 主节点和备用节点分别配置凭证。各自的 `bearer` 用于 HTTP Authorization 或 Sui gRPC metadata。TON 的 `apiKey` 使用 `X-API-Key`，TRON 的 `apiKey` 使用 `TRON-PRO-API-KEY`。公共备用通常只需 `rpc`。
- TON 的 `rpc` 可填根地址或以 `/api/v2` 结尾的地址。同一服务提供 v3 时复用同组地址与凭证。单独配置 v3 时使用 `index`、`indexBearer` 或 `indexApiKey`，可填以 `/api/v3` 结尾的地址。备用节点遵循相同规则。
- EVM 若要按钱包枚举全部 ERC-20 持仓，需在该网络配置 `tokenInventoryRPC`，并可配置 `tokenInventoryBearer`。它需要支持 `eth_chainId` 和分页的 `alchemy_getTokenBalances`。如有第二个兼容索引，在 `backup` 内配置相同字段。仅配置一个索引时，枚举失败会重试该索引。枚举结果用于发现资产，快照金额始终通过余额节点核验。
- 公共服务的限额、历史数据保留期和 trace 权限依供应商而异。两端均缺少 EVM trace 时，仍处理经核验的回执日志；覆盖无日志资产变化需要持仓索引。[资产发现范围](chains.zh-CN.md#asset-discovery)
