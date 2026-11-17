# new-api USDT 收款方案（第一期）

## 目标

- 支持 BSC (BEP20)、Arbitrum One (USDT0)、Polygon (PoS)、TRON (TRC20) 四条网络的 USDT 充值，每条网络可在 new-api 后台单独开关。
- **不串单**：每个用户每条网络一个永久专属地址，按地址认人，与金额、时间无关。
- **不被盗刷**：服务器上没有任何能转走 USDT 的私钥；new-api 不信任扫链服务的回调，自己上链核实后才入账。
- **回笼快**：用户地址是合约地址，到账后几分钟内自动转发到你的收款地址。
- **主服务器零负载**：扫链和转发在洛杉矶服务器，主服务器每笔充值只多 1~3 次 RPC 查询。

## 架构

```
用户钱包 ──USDT──▶ 用户专属地址（合约，CREATE2 计算得出）
                        │  sweep(salt, USDT)：任何人都能触发，钱只能进 destination
                        ▼
                  你的收款地址 destination（冷钱包 / 交易所充值地址，只有 owner 能改）

洛杉矶服务器 new-api-usdt（本项目）            主服务器 new-api
  ├ 拉取观察列表（HMAC 签名）  ◀────────────  ├ 后台：开关网络、合约地址、RPC、确认方式、最小金额、汇率
  ├ 扫 USDT Transfer 日志                      ├ 给用户生成专属地址（本地计算 + 上链校验）
  ├ 达到最终确认后推送"提示"  ──────────────▶  ├ 收到提示 → 用自己的 RPC 重新核实 → 幂等入账
  └ 余额达到门槛时调用 sweep（Gas 钱包付手续费）  └ 用户充值页 / 后台充值记录 / 按交易哈希补查
```

## 合约（contracts/）

- `DepositFactory`：owner、destination、create2Prefix（EVM 0xff，TRON 0x41）、implementation。
  - `walletAddress(salt)`：预测用户地址。
  - `deployWallet(salt)`：CREATE2 部署 EIP-1167 克隆，**部署结果必须等于预测地址，否则回滚**。
  - `sweep(salt, token)`：部署（如需）并把该地址的全部代币转到 destination，按 destination 余额差确认到账（兼容 ETH USDT 无返回值、TRON USDT 返回 false）。
  - `sweepNative(salt)`：误转的 BNB/POL/ETH/TRX 同样只能转到 destination。
  - `setDestination` / 两步转移 owner：只有 owner（你的冷钱包）能调用。
- `DepositForwarder`：克隆的实现合约，只接受 factory 调用。
- 编译：solc 0.8.x，evmVersion `istanbul`（不使用 PUSH0，TRON 兼容）。
- 三条 EVM 链通过确定性部署代理（0x4e59b448…）部署，**同一个用户在三条 EVM 链上是同一个 0x 地址**；在未开通的 EVM 链上误转，之后部署同样合约即可找回。

## 地址与 salt

- `salt = keccak256(saltSecret(32 字节) ‖ uint256(userId))`。saltSecret 由 new-api 首次启用时随机生成；一旦发放过地址就禁止修改。
- 每条地址记录都保存自己的 salt，扫链服务转发时直接使用。
- 网络启用前必须通过"合约校验"：链上 `deployWallet` 模拟调用的结果 = 链上 `walletAddress` = new-api 本地计算。三者一致才允许展示地址，防止 TRON 等链的 CREATE2 公式差异导致地址无法转出。

## 入账规则（new-api）

1. 扫链服务推送 `{chain, tx_hash, log_index}`（HMAC 签名、时间戳 5 分钟有效，可加 IP 白名单）。
2. new-api 同时用两家不同服务商的 RPC 核实，两家结果（回执状态、区块哈希、每条转账的收款人和金额）完全一致才继续，否则暂缓并在后台告警：
   - 回执状态成功；该 log 来自写死的官方 USDT 合约；Transfer 事件；收款方是本站发放的地址；
   - 已达到最终确认（`finalized` / `safe` / N 个确认，后台按网络配置）；区块仍在主链上。
3. 唯一键 `(chain, tx_hash, log_index)`，入账、写充值订单、加额度、邀请返利在同一个数据库事务内完成。
4. 额度 = USDT 数量 × 汇率 × QuotaPerUnit；汇率模式：
   - `market`：Coinbase 行情 USDT/USD（备用 Kraken），60 秒缓存；拿不到行情时不入账，等待重试；
   - `fixed`：后台固定比例。
5. 低于该网络最小金额：记录为"低于最小金额"，不入账，后台可人工入账。
6. 不设单笔上限（按你的选择）。

## 网络默认值

| 网络 | USDT 合约 | 小数 | 最终确认 | 最小充值 | 转发门槛 |
|---|---|---|---|---|---|
| BSC | 0x55d398326f99059fF775485246999027B3197955 | 18 | finalized | 1 | 1 |
| Arbitrum One | 0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9 | 6 | safe | 1 | 1 |
| Polygon | 0xc2132D05D31c914a87C6611C10748AEb04B58e8F | 6 | finalized | 1 | 1 |
| TRON | TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t | 6 | 20 个确认 | 10 | 50 |

## 交付物

- `D:\AiFiles\new-api-usdt`：合约、部署工具 `usdt-deploy`、扫链转发服务 `usdt-scanner`、Docker 部署文件、部署说明。
- new-api：后台设置、两张新表（`usdt_deposit_addresses`、`usdt_deposits`）、扫链接口、核实入账、用户充值页、后台充值记录与补查。

## 你需要准备

1. 收款地址 destination（建议冷钱包；EVM 一个 0x、TRON 一个 T 开头）和 owner 地址（能修改 destination 的钱包，建议硬件钱包）。
2. 部署用的钱包：每条链放少量手续费币（BNB / ETH(Arbitrum) / POL / TRX），部署完可清空。
3. 洛杉矶服务器上的 Gas 钱包：新建一个，每条链放几美元手续费币，用于触发转发。
4. RPC：每条链至少两家不同服务商（new-api 两个核实节点各用一家，扫链服务可复用其中一家），TRON 用 TronGrid API Key 加另一家。
5. 每条链 2~5 USDT 用于真钱测试。
