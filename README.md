# new-api-usdt

new-api 的 USDT 收款组件：每个用户每条网络一个**永久专属地址**（合约地址），到账后自动转发到你的收款钱包。

- 支持网络：BSC (BEP20)、Arbitrum One (USDT0)、Polygon (PoS)、TRON (TRC20)，在 new-api 后台逐个开关。
- **不串单**：按地址认人，地址永不回收。
- **不被盗刷**：服务器上没有能转走 USDT 的私钥；new-api 收到扫链服务的通知后，同时向**两家不同服务商的节点**上链核实，两家结果完全一致才入账。
- **回笼快**：钱到用户地址后几分钟内自动转到你的收款地址（destination）。
- **主服务器零负载**：扫链和转发都在这台服务（例如洛杉矶服务器）上。

设计说明见 [PLAN.md](PLAN.md)；后台每个设置项的含义、上线测试流程、日常运维和给用户的付款说明见 [USDT收款使用与配置.md](USDT收款使用与配置.md)。

## 组成

| 目录 | 作用 |
|---|---|
| `contracts/` | `DepositFactory` / `DepositForwarder` 合约及编译产物（`npm run build` 重新编译） |
| `cmd/usdt-deploy` | 部署工厂合约、生成 Gas 钱包、校验地址公式 |
| `cmd/usdt-scanner` | 扫链 + 通知 new-api + 自动转发 |
| `docker-compose.yml` / `.env.example` | 部署文件 |

## 一、准备钱包（3 类，都不要用现有钱包）

| 钱包 | 用途 | 放在哪里 |
|---|---|---|
| **收款地址 destination** | 用户的 USDT 最终转到这里 | 你的冷钱包，或交易所充值地址（先小额测试交易所能否识别合约转入） |
| **owner** | 唯一能修改 destination 的账户 | 硬件钱包 / 冷钱包，平时不用 |
| **部署钱包 deployer** | 只付部署手续费 | 临时钱包，部署完把剩余手续费转走即可 |
| **Gas 钱包** | 只付自动转发的手续费 | 私钥放在扫链服务 `.env`，每条链放几美元手续费币 |

> EVM 三条链（BSC / Arbitrum / Polygon）请使用**同一个 owner 和同一个 destination**，这样三条链的工厂合约地址相同，同一用户在三条链上也是同一个充值地址。TRON 用 T 开头的地址，单独部署。

## 二、准备 RPC 节点

每条链至少准备**两家不同服务商**的节点：new-api 的两个核实节点必须来自不同服务商（后台会拒绝同一家的两个地址），扫链服务可以复用其中一家。任何一家被攻破或出错，都只会让充值暂缓，不会凭空入账。

- BSC / Arbitrum / Polygon：Alchemy、QuickNode、Ankr、NodeReal、Infura 等任选两家。
- TRON：TronGrid（`https://api.trongrid.io/jsonrpc`，申请免费 API Key）+ 另一家支持 TRON JSON-RPC 的服务商。

## 三、把程序放到服务器

三种方式，任选一种。

**方式一：直接拉 CI 构建好的镜像**（最省事，不用源码、不用 Go）：

```bash
mkdir -p /opt/usdt-scanner && cd /opt/usdt-scanner
curl -fsSL -o docker-compose.yml https://raw.githubusercontent.com/JSXiaoJun/new-api-usdt/main/docker-compose.image.yml
curl -fsSL -o .env.example     https://raw.githubusercontent.com/JSXiaoJun/new-api-usdt/main/.env.example
cp .env.example .env
docker compose pull
```

镜像：`ghcr.io/jsxiaojun/new-api-usdt:latest`（amd64 + arm64，每次提交自动构建）。
更新：`docker compose pull && docker compose up -d`。

> 镜像名必须全小写 `jsxiaojun`（用户名 `JSXiaoJun` 的小写形式）。写成 `JSXiaoJun` 或拼错都会 `denied`。
> 首次拉取如果报 `unauthorized`：到 GitHub → Packages → `new-api-usdt` → Package settings 把它设为 Public。

**方式二：用 Release 里的部署包**（服务器连不上 `ghcr.io` 时用，只要能连 github.com）：

```bash
mkdir -p /opt/usdt-scanner && cd /opt/usdt-scanner
VERSION=v0.1.0
curl -fL -o /tmp/kit.tar.gz "https://github.com/JSXiaoJun/new-api-usdt/releases/download/$VERSION/new-api-usdt-kit-linux-amd64.tar.gz"
tar -xzf /tmp/kit.tar.gz -C /opt/usdt-scanner
cp .env.example .env
```

以后更新/回滚只要一条命令（包里自带 `update.sh`）：

```bash
bash update.sh              # 最新 Release
bash update.sh v0.1.1       # 指定版本
```

**方式三：服务器本地构建**：

```bash
git clone <本仓库> new-api-usdt && cd new-api-usdt
cp .env.example .env
docker compose build
```

生成 Gas 钱包（输出私钥和 EVM / TRON 地址）：

```bash
docker compose run --rm --entrypoint usdt-deploy usdt-scanner new-key
```

把私钥写进 `.env` 的 `GAS_PRIVATE_KEY`，然后往这个地址转少量 BNB（BSC）、ETH（Arbitrum）、POL（Polygon）、TRX（TRON）。

## 四、部署工厂合约（每条链一次）

部署钱包里需要有对应链的手续费币。私钥只通过环境变量临时传入：

```bash
docker compose run --rm -e DEPLOYER_PRIVATE_KEY=<部署钱包私钥> --entrypoint usdt-deploy usdt-scanner \
  evm -chain bsc -rpc <BSC RPC> -owner <owner 0x地址> -destination <收款 0x地址>
```

把 `-chain` 换成 `arbitrum`、`polygon` 各执行一次（owner / destination 保持一致）。TRON：

```bash
docker compose run --rm -e DEPLOYER_PRIVATE_KEY=<部署钱包私钥> -e TRON_API_KEY=<TronGrid Key> --entrypoint usdt-deploy usdt-scanner \
  tron -owner <owner T地址> -destination <收款 T地址>
```

每次部署会输出：

- `factory address`：填到 new-api 后台；
- `probe wallet ... (address formula verified)`：工具已经真实部署了一个探针钱包，证明这条链上地址公式正确。**如果这一步报错，不要在后台开启这条链。**

## 四之二、维护：改收款地址 / 转移所有权

用当前 **owner** 私钥签名，指令直接打到工厂合约（镜像 v0.1.1 起提供）：

```bash
# 改收款地址：以后归集的钱进新地址，工厂地址和用户地址都不变
docker run --rm -e OWNER_PRIVATE_KEY --entrypoint usdt-deploy ghcr.io/jsxiaojun/new-api-usdt:v0.1.1 \
  set-destination -chain polygon -rpc https://polygon-bor-rpc.publicnode.com \
  -factory 0x工厂地址 -destination 0x新收款地址

# 两步转移所有权：先由当前 owner 发起
docker run --rm -e OWNER_PRIVATE_KEY --entrypoint usdt-deploy ghcr.io/jsxiaojun/new-api-usdt:v0.1.1 \
  transfer-owner -chain polygon -rpc https://polygon-bor-rpc.publicnode.com \
  -factory 0x工厂地址 -new-owner 0x新owner

# 再由新 owner 接受（私钥换成新 owner 的）
docker run --rm -e OWNER_PRIVATE_KEY --entrypoint usdt-deploy ghcr.io/jsxiaojun/new-api-usdt:v0.1.1 \
  accept-ownership -chain polygon -rpc https://polygon-bor-rpc.publicnode.com -factory 0x工厂地址
```

```bash
# 看当前余额（只读，不发交易；USDT 保留两位小数）
docker compose run --rm --entrypoint usdt-deploy usdt-scanner balance \
  -chain polygon -rpc https://polygon-bor-rpc.publicnode.com \
  -factory 0x工厂地址
```

输出：

```
factory     : 0x2c22...
owner       : 0xdd89...
destination : 0xd613...
USDT        : 2.00 USDT (Polygon (PoS))
收款地址 gas : 0.0000 POL
Gas 钱包     : 0x2384...  0.9300 POL
```

不带 `OWNER_PRIVATE_KEY` 运行时只打印 `factory / owner / pendingOwner / destination`，不会发交易——先用它核对状态最安全。

## 五、配置 new-api

后台 → 系统设置 → 计费 → **USDT Deposits**：

1. 每条链填写：工厂合约地址、**两个核实用 RPC**（不同服务商）、最终确认方式、最小充值金额、转发门槛，点保存。
2. 点每条链的 **Verify factory**：两个节点都要确认工厂合约与地址公式，结果一致才通过。校验通过后才能打开“向用户显示这个网络”。
3. 选择入账汇率：行情（Coinbase / Kraken 的 USDT/USD）或固定比例。
4. 打开 **Enable USDT deposits**，保存。
5. 点 **Generate scanner secret**，把密钥填到扫链服务 `.env` 的 `SCANNER_SECRET`（只显示一次）。可选：在“Scanner IP allowlist”填洛杉矶服务器的出口 IP。

> 隐藏某条网络只会让用户看不到它；已经发出去的地址仍会继续扫描、入账和转发。

## 六、启动扫链服务

`.env` 填好 `NEWAPI_URL`（new-api 根地址，不带子路径）、`SCANNER_SECRET`、`GAS_PRIVATE_KEY`、各链 `RPC_*`、`TRON_API_KEY` 后：

```bash
docker compose up -d
docker compose logs -f usdt-scanner
```

启动时会校验每个 RPC 的 chain id（防止接错网络），并打印各链 Gas 钱包地址。几分钟后，后台 USDT 页面的 “Scanner service” 会显示 Online、最新区块和 Gas 余额。

## 七、真钱小额测试（每条链都做）

1. 用普通用户账号打开钱包页，选网络，显示充值地址。
2. 从交易所或钱包转 **2 USDT** 到该地址。
3. 等最终确认（BSC / Polygon 约 1 分钟内，Arbitrum 数分钟，TRON 约 1 分钟），确认余额增加、充值记录显示“已入账”。
4. 等转发：后台 “Pending sweeps” 归零后，在区块浏览器确认 USDT 已到 destination。
5. 用“已转账？用交易哈希查询到账”再提交一次同一笔哈希，应提示“已入账过”，余额不变。

## 八、邮件告警（可选，推荐）

在 `.env` 里加上（QQ / 163 用「授权码」当密码，不是登录密码）：

```ini
SMTP_HOST=smtp.qq.com
SMTP_PORT=465
SMTP_USER=you@qq.com
SMTP_PASS=授权码
MAIL_FROM=you@qq.com
MAIL_TO=you@qq.com
TZ=Asia/Shanghai
ALERT_COOLDOWN_SECONDS=21600
ALERT_EXPECTED_DESTINATION_POLYGON=0x你的收款地址
```

会监控：

| 事件 | 说明 |
|---|---|
| Gas 钱包不够用了 | 按「还能归集多少次」判断：**剩余 ≤ 30 次就发信，之后每 30 分钟提醒一次**，补够后发一封“已恢复”（成本默认 Polygon 0.03、BSC 0.0002、Arbitrum 0.0002、TRON 2，可用 `ALERT_SWEEP_COST_*` 调） |
| `setDestination` 被调用 | 秒级邮件，带新旧地址 + 区块浏览器链接；新地址与 `ALERT_EXPECTED_DESTINATION_*` 不一致时标记“⚠️ 请立刻核实” |
| 归集连续失败 3 次 | 常见原因是 Gas 不足 / RPC 异常 / gas 价超上限 |
| 收款地址余额变化 | **每满 100 USDT 提醒一次**（`ALERT_DESTINATION_STEP_*` 可调）；**余额减少立刻提醒**（减少金额 + 当前余额），方便随时知道钱袋子里还有多少 |
| 每日汇总 | 默认 20:00（按 `TZ`），含各链 Gas 余额、区块进度、待归集数、**收款地址余额**、当天归集笔数与金额 |

同一个告警按 key 在 `ALERT_COOLDOWN_SECONDS` 内只发一次，重启不会重复轰炸。没配 `SMTP_HOST` 时整个功能自动关闭，服务行为不变。

测试发信：

```bash
docker compose run --rm --entrypoint usdt-notify usdt-scanner "测试邮件：部署完成"
```

## 九、手动归集（忽略后台门槛）

后台的「归集门槛」只决定**新充值**什么时候被归集；已经躺在地址里、低于门槛的余额不会自己动。要一次性收回来：

```bash
docker compose run --rm --entrypoint usdt-scanner usdt-scanner sweep-once -min 0.01
```

- `-min`：只归集余额 ≥ 该值的地址（默认 0.01 USDT）
- 每 25 个地址一笔交易，逐笔等待确认
- 需要 `GAS_PRIVATE_KEY` 和 new-api 可达（工厂/合约信息从 new-api 读）
- 跑完会打印 `force sweep: sent chain=... tx=... wallets=N amount=X`

> 从 v0.1.12 起，**在后台修改归集门槛会自动把该链所有地址重新排队**，存量余额 30 秒内开始分批补扫，不用再手动跑命令。\n> 手动 `sweep-once` 仍然保留，用于“立刻全收”或后台不可用时。
## 日常运维

- **修改收款地址**：用 owner 钱包在区块浏览器（BscScan / Arbiscan / PolygonScan / TronScan）对工厂合约调用 `setDestination(新地址)`。扫链服务和 new-api 不需要改。
- **用户转错币或转错链**：钱仍在用户的合约地址里。原生币（BNB/ETH/POL/TRX）任何人都可以调用工厂的 `sweepNative(salt)` 转到 destination；其他代币用 `sweep(salt, 代币合约)`。EVM 链上转到未开通的链：在那条链用同样的 owner/destination 部署工厂，地址相同，就能取回。
- **核实节点结果不一致**：后台 USDT 页面顶部会出现红色提示“Verification nodes disagree”，列出被暂缓的交易。说明其中一家服务商出错或被攻破，先在区块浏览器核对，再更换出问题的节点；换好后这些充值会被自动重试入账。
- **漏单兜底**：用户可以在充值页提交交易哈希；管理员可以在后台“Check and credit”按哈希补查。都会重新上链核实，不会重复入账。
- **低于最小金额的充值**：记录为“低于最小金额”，管理员可点“手动入账”（会再次上链核实）。
- **扫链服务数据**：`./data/scanner.db` 只保存扫描进度和发送记录。丢失后从 `LOOKBACK_BLOCKS_*` / `START_BLOCK_*` 重新扫描即可，new-api 的入账记录不会重复。
- **安全**：Gas 钱包被盗只会损失里面的手续费；工厂合约只会把钱转给 destination。owner 私钥请离线保管。

## 已验证与未验证

- 已在本地模拟链（geth 模拟后端，BSC/Polygon 链 ID）上验证：三种 USDT 转账返回值约定、错误前缀被拒、仅 owner 能改收款地址、扫链→通知→自动转发全流程、确定性部署代理得到的跨链同一工厂地址、new-api 的工厂校验与链上核实。
- **未在主网实测**，TRON 的部署与转发流程依赖 TronGrid HTTP 接口，只做了逻辑实现和代码审查。上线前请严格按第七步在每条链做小额测试。
