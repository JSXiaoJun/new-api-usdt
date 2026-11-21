# new-api USDT 收款：独立服务器部署 + U 支付上线指南（BSC + Polygon）

> 目标：这台独立服务器只跑「扫链 + 自动归集」；new-api 仍在你原来的主服务器上，后台已经能看到 USDT 设置页。
> 第一批只开 **BSC (BEP20)** 和 **Polygon (PoS)**，手续费最低、确认最快，跑通后再加 Arbitrum / TRON。
>
> 配套文件：`deploy-kit/`（已编译好 linux/amd64 的部署包，服务器不用连 GitHub、不用装 Go）。

---

## 0. 先看懂全景（30 秒）

```
用户 ──USDT──▶ 他自己的专属地址（合约地址，每人每条链一个，永不回收）
                    │  到账后，这台服务器调用 sweep 归集
                    ▼
              你的收款地址 destination（冷钱包 / 交易所充值地址）

这台服务器（独立机）                      主服务器
usdt-scanner                              new-api
 · 扫描链上 USDT 转账        ──提示──▶     · 用两家不同服务商的节点重新核实
 · 余额到门槛自动归集                      · 两家结果一致才入账、加额度
 · 只出站，不开放任何端口                  · 给用户出地址、充值记录、按哈希补查
```

三个不变的事实，决定了下面的每一步为什么这么做：

1. **按地址认人**，不按金额、不按时间，所以不会串单。
2. **这台服务器上没有能转走用户 USDT 的私钥**：用户的钱只能进 destination，改 destination 只有 owner 能改。
3. **new-api 不信任通知**，所有入账都自己上链核实两遍。

你要做的其实只有 6 件事：准备钱包 → 放程序 → 部署合约 → 后台填地址 → 启动扫链 → 转 2 U 试一次。

---

## 1. 开工前准备（一次性，约 40 分钟）

### 1.1 四个钱包（务必互不相同）

| 角色 | 用途 | 怎么准备 | 放在哪 |
|---|---|---|---|
| **destination** 收款地址 | 用户的钱最终都到这里 | 新建一个钱包地址（MetaMask「新建账户」+ 新助记词），或交易所充值地址 | 冷钱包/离线保存 |
| **owner** 所有者 | 唯一能修改 destination 的地址 | 再新建一个地址（建议硬件钱包或另一套助记词） | 离线，平时不用 |
| **deployer** 部署钱包 | 只付部署合约的手续费 | 临时新建一个 | 部署完把余额转走即可 |
| **gas** 归集钱包 | 只付自动归集的手续费 | 下一步用 `usdt-deploy new-key` 生成 | 私钥只放服务器 `.env` |

硬规则：

- **四个地址不要复用**，尤其不要把 owner 设成你日常在用的热钱包。
- **BSC 和 Polygon 用同一个 owner、同一个 destination**。这样两条链的工厂合约地址相同，同一用户在两条链上拿到的是同一个 `0x` 地址，用户不容易转错链。
- destination 写进合约后，**只有 owner 能改**；owner 丢了钱不会丢（还是会进 destination），但以后永远改不了收款地址。所以 owner 的助记词要离线抄两份。
- 用交易所充值地址做 destination 时，**先转 2 U 试试交易所认不认这笔「合约转入」**，认了再放大额。

### 1.2 RPC 节点（每条链两家不同服务商）

new-api 后台每条链要填**两个**核实用 RPC，且必须来自**不同服务商**（它按域名判断，同一家的两个 Key 会被拒绝）。扫链服务再单独用一个（可以和 new-api 的其中一家相同，但最好错开）。

先用公开节点跑通（零成本，流量小完全够）：

| 链 | 核实用 RPC #1 | 核实用 RPC #2 | 扫链服务用 |
|---|---|---|---|
| BSC | `https://bsc-dataseed.binance.org` | `https://bsc-rpc.publicnode.com` | `https://bsc-dataseed.binance.org` |
| Polygon | `https://polygon-rpc.com` | `https://polygon-bor-rpc.publicnode.com` | `https://polygon-rpc.com` |

> 两个 `*.publicnode.com` 会被当成同一家，所以别这么配。
> 想长期稳定（免费额度也够小店）：去 **Ankr / NodeReal / QuickNode / Alchemy / Infura** 各注册一个 Key，把上面的公开地址换掉。公开节点高峰期会限流，表现为日志里 `rate limit`、区块跟不上。

### 1.3 手续费资金（总共十几美元）

| 钱包 | BSC | Polygon | 说明 |
|---|---|---|---|
| deployer | 0.005 BNB（约 $4） | 0.3 POL（约 $0.15） | 部署工厂 + 探针，各 2 笔交易 |
| gas | 0.01 BNB（约 $8） | 2 POL（约 $1） | BSC 每笔归集约 $0.0001，Polygon 约 $0.005 |

> 宁可多放一点，Gas 不够时归集会卡在「待完成归集」，日志里提示手续费不足。
> 现在不用 TRON，先不准备 TRX。

---

## 2. 第一步：服务器自检（把输出贴给我）

宝塔终端里整段粘贴执行：

```bash
echo "--- 系统 ---"; uname -m; sed -n '1,2p' /etc/os-release; df -h / | tail -1
echo "--- Docker ---"; docker --version; docker compose version 2>/dev/null || docker-compose --version
echo "--- 外网 ---"
curl -s -o /dev/null -w 'github     %{http_code}\n' -m 8 https://github.com
curl -s -o /dev/null -w 'dockerhub  %{http_code}\n' -m 8 https://registry-1.docker.io/v2/
curl -s -o /dev/null -w 'goproxy    %{http_code}\n' -m 8 https://proxy.golang.org
echo "--- 本机出口 IP（后面填白名单用）---"; curl -4 -s -m 8 https://api.ipify.org; echo
echo "--- new-api 可达性（把域名换成你的）---"
DOMAIN="https://CHANGE-ME.example.com"   # ← 改成你的 new-api 域名
curl -s -o /dev/null -w 'site       %{http_code}\n' -m 10 "$DOMAIN/"
curl -s -o /dev/null -w 'usdt-api   %{http_code}\n' -m 10 "$DOMAIN/api/usdt/scanner/state"
```

怎么判断：

| 结果 | 含义 | 走哪条路 |
|---|---|---|
| `usdt-api` 返回 **401 / 403** | 接口存在，只是没带签名（正常） | 继续 |
| `usdt-api` 返回 **404** | 这个 new-api 没有 U 支付接口 | 先别部署，告诉我，得先补 new-api |
| github / dockerhub / goproxy 都通 | 服务器能自己构建 | **方案 A**（见下一节） |
| 只通 github、连不上 dockerhub | 拉不了 Go 镜像 | **方案 C**（Release 直下）或方案 B |
| 有超时/000 | 网络受限（国内机常见） | **方案 B**：本地上传部署包 |

顺便确认时间同步（签名有效期只有 5 分钟，时钟偏了会全部 401）：

```bash
timedatectl 2>/dev/null | grep -Ei 'synchronized|NTP' || date
```

---

## 3. 第二步：把程序放到服务器

统一放在 **`/opt/usdt-scanner`**（不要放 `/www/wwwroot`，那里是网站根目录，`.env` 里有私钥）。

### 方案 A：服务器自己构建（网络通时）

```bash
mkdir -p /opt && cd /opt
git clone https://github.com/JSXiaoJun/new-api-usdt.git usdt-scanner
cd usdt-scanner
cp .env.example .env
docker compose build          # 拉 golang 镜像 + 下依赖，约 2~5 分钟
```

### 方案 B：本地上传部署包（服务器连不上 GitHub 时用）

1. 把 `new-api-usdt-kit-linux-amd64.tar.gz`（≈9.4 MB）下载到本地；
2. 宝塔 → 文件 → 上传到 `/opt/`；
3. 终端执行：

```bash
mkdir -p /opt/usdt-scanner
tar -xzf /opt/new-api-usdt-kit-linux-amd64.tar.gz -C /opt/usdt-scanner
ls -l /opt/usdt-scanner
```

解压出来应该是：`bin/usdt-scanner`、`bin/usdt-deploy`、`.env.example`、`docker-compose.yml`、`Dockerfile`、`deploy.sh`，以及 `DEPLOY-GUIDE.md`（就是本指南）。

> 部署包里 `bin/usdt-scanner`、`bin/usdt-deploy` 已经编译好，只需要能拉到 `alpine:3.21` 基础镜像（约 8 MB）。
> 如果连 alpine 都拉不下来：`docker pull alpine:3.21` 换宝塔 Docker 里的镜像加速，或者改用第 12 节「不用 Docker 直接跑二进制」。

### 方案 C：从 GitHub Release 直接下载（推荐，不用本地上传）

服务器能连 github.com 时，这一步代替方案 B 的“下载到本地再上传”：

```bash
mkdir -p /opt/usdt-scanner && cd /opt/usdt-scanner
curl -fL -o /tmp/kit.tar.gz https://github.com/JSXiaoJun/new-api-usdt/releases/download/v0.1.0/new-api-usdt-kit-linux-amd64.tar.gz
tar -xzf /tmp/kit.tar.gz -C /opt/usdt-scanner
cp .env.example .env
curl -fsSL -o update.sh https://raw.githubusercontent.com/JSXiaoJun/new-api-usdt/main/update.sh
chmod +x update.sh
```

以后更新/回滚一条命令：

```bash
bash update.sh              # 更新到最新 Release
bash update.sh v0.1.1       # 指定版本 / 回滚
```

`.env` 和 `data/` 不会被覆盖。

> 关于 GitHub 镜像（GHCR）：CI 每次提交都会构建 `ghcr.io/JSXiaoJun/new-api-usdt`，但这个账号的用户名含大写，
> GHCR 只认 `JSXiaoJun/...`、Docker 的镜像名只允许小写，`docker pull` 无法引用，所以**不要走镜像这条路**，用 Release 部署包。
> 方案 B 和 C 拿到的是同一个包，区别只是 C 由服务器自己去下载。

---

## 4. 第三步：生成 Gas 钱包 + 写 `.env`

```bash
cd /opt/usdt-scanner
cp .env.example .env          # 方案 A 已经复制过就跳过

docker compose run --rm --entrypoint usdt-deploy usdt-scanner new-key
```

输出三行，抄下来：

```
private key  : 3f2a....（64 位十六进制）
EVM address  : 0xAbC...      ← 这就是 Gas 钱包，BSC 和 Polygon 通用同一个地址
TRON address : T...          ← 现在不用
```

1. 给这个 **EVM 地址**转：BSC 上 0.01 BNB、Polygon 上 2 POL。
2. 编辑 `.env`（`nano .env` 或 `vim .env`），只改这几行：

```ini
NEWAPI_URL=https://你的new-api域名
SCANNER_SECRET=                  # 第五步后台生成后再填
GAS_PRIVATE_KEY=3f2a....         # 上面那串，不要带 0x 也行
RPC_BSC=https://bsc-dataseed.binance.org
RPC_POLYGON=https://polygon-rpc.com
```

注意：

- 值不要加引号、不要留行尾空格；`NEWAPI_URL` 末尾不要 `/`。
- 此时 `SCANNER_SECRET` 还是空的，服务**启动会报错退出是正常的**，等第五步（new-api 后台配置）生成密钥后再填、再启动。
- 用 `chmod 600 .env` 收一下权限。

---

## 5. 第四步：部署工厂合约（BSC、Polygon 各一次）

> 这一步只做一次。factory 地址一旦在后台「向用户显示过」，就**永久锁定不能改**，所以先确认 owner / destination 没填错。

```bash
cd /opt/usdt-scanner
read -rs DEPLOYER_PRIVATE_KEY; export DEPLOYER_PRIVATE_KEY   # 粘贴部署钱包私钥，不回显

docker compose run --rm -e DEPLOYER_PRIVATE_KEY --entrypoint usdt-deploy usdt-scanner \
  evm -chain bsc -rpc https://bsc-dataseed.binance.org \
  -owner 0x你的owner地址 -destination 0x你的收款地址

docker compose run --rm -e DEPLOYER_PRIVATE_KEY --entrypoint usdt-deploy usdt-scanner \
  evm -chain polygon -rpc https://polygon-rpc.com \
  -owner 0x你的owner地址 -destination 0x你的收款地址

unset DEPLOYER_PRIVATE_KEY
```

正常输出（两条链各一份）：

```
factory address: 0x1234...abcd
deploy tx: 0x....
implementation : 0x....
destination    : 0x你的收款地址
probe wallet   : 0x.... (address formula verified)

Next: in new-api, USDT settings → BNB Smart Chain → factory address 0x1234...abcd → Verify.
```

必须核对三件事：

1. **两个 `factory address` 完全一样**——不一样说明两次的 owner/destination 填得不同，去后台之前先改对。
2. 看到 `(address formula verified)`——这是工具真的在链上部署了一个探针地址，证明地址公式正确。**没看到就不要开这条链**。
3. `destination` 打印出来是你填的收款地址。

常见报错对照：

| 报错 | 原因 | 处理 |
|---|---|---|
| `RPC chain id 56 ... does not match polygon (137)` | RPC 地址串链了 | 检查 `-rpc` 是不是对应链的地址 |
| `deterministic deployment proxy not found` | 该 RPC 不支持或不是真节点 | 换一个 RPC |
| `insufficient funds for gas` | deployer 没币 | 转 BNB / POL 后重跑（已部署的合约会自动跳过） |
| `replacement transaction underpriced` / `nonce too low` | 上一次交易还没落 | 等 1 分钟重跑 |

把两个 factory 地址记下来，第五步后台要填。

---

## 6. 第五步：new-api 后台配置（关键一步）

位置：**系统设置 → 计费与支付 → USDT 充值（USDT Deposits）**。

先确认总开关 **「启用 USDT 充值」是关的**，填好再开。

### 6.1 每条链逐项填写

| 设置项 | BSC 填什么 | Polygon 填什么 |
|---|---|---|
| 工厂合约地址 | 第四步部署输出的 `factory address`（两条链同一个） | 同左 |
| 核实用 RPC 地址 | `https://bsc-dataseed.binance.org` | `https://polygon-rpc.com` |
| 第二个核实用 RPC 地址 | `https://bsc-rpc.publicnode.com` | `https://polygon-bor-rpc.publicnode.com` |
| 最终确认方式 | `finalized`（默认） | `finalized`（默认） |
| 最小充值金额 | `1` | `1` |
| 归集门槛 | `1` | `1` |
| 向用户显示这个网络 | **先关着** | **先关着** |

> 两个核实用 RPC 必须是**不同域名**，否则保存会报错「同一服务商」。
> Arbitrum / TRON 这一轮不用管，保持关闭。

### 6.2 操作顺序（顺序错了会白折腾）

1. 两条链都填完 → 点 **保存 USDT 设置**。
2. 点每条链的 **校验工厂合约 / Verify factory** → 看到「已校验」。
3. 再打开 **「向用户显示这个网络」** → 再保存一次。
4. 入账汇率：想要结果可预期就选 **固定汇率 = 1**（1 USDT 入账 $1）；想跟行情就选「市场行情」。想收 2% 手续费就填 `0.98`，想送 2% 就填 `1.02`。
5. 测试期间把 **「仅管理员可见（测试用）」打开**——只有管理员能看到 U 支付入口，服务器端也会拒绝普通用户。
6. 点 **「生成扫链服务密钥」**，**只显示一次**，立刻复制到 `.env` 的 `SCANNER_SECRET`。
7. **扫链服务 IP 白名单**：先留空（等跑通再填第一步自检时查到的出口 IP）。填错会直接 403、扫链服务显示离线。
8. 最后才打开 **「启用 USDT 充值」**，保存。

> 校验失败最常见的原因：factory 地址填错一位、两个 RPC 有一个不通、owner/destination 两条链不一致。
> 另外「充值赠送」档位和「邀请返利」对 USDT 充值同样生效，按入账美元金额算。

---

## 7. 第六步：启动扫链服务

```bash
cd /opt/usdt-scanner
mkdir -p data && chown -R 10001:10001 data     # 容器以 uid 10001 运行，这行别省
docker compose up -d --build
docker compose logs -f --tail=80 usdt-scanner
```

正常日志长这样：

```
level=INFO msg="gas wallet (fund it with the chain's native coin)" chain=bsc address=0x你的Gas钱包
level=INFO msg="gas wallet (fund it with the chain's native coin)" chain=polygon address=0x你的Gas钱包
level=INFO msg="usdt-scanner started" version=0.x.x chains=2 sweep=true
```

启动时会先校验每个 RPC 的 chain id（接错网络直接退出，防止扫错链），然后每 30 秒向 new-api 同步一次观察列表。

复查：

```bash
docker compose ps                                             # Up (healthy)；刚启动 60 秒内是 health: starting
docker compose exec usdt-scanner wget -qO- http://127.0.0.1:9180/   # 返回 ok <version>
docker compose logs --tail=30 usdt-scanner | grep -i 'error\|warn' || echo "没有错误"
```

回到 new-api 后台 USDT 页面，1 分钟内应看到：

- **扫链服务：Online**，最近上报时间在更新；
- 每条链的 **最新区块在涨**、**已扫描区块**在追；
- **Gas 余额**显示 BNB / POL 数量；
- **待完成归集 = 0**。

> 这里没 Online，先看 `docker compose logs`：`HTTP 401` = 密钥不对；`HTTP 403` = IP 白名单或 Cloudflare 拦了；`dial tcp` = 到 new-api 网络不通。

---

## 8. 第七步：真钱小额测试（每条链都要做）

> 这一步不能跳。合约和扫链在本地模拟链上验证过，但**没在主网实测**，2 U 就是最便宜的保险。

1. 用**管理员账号**登录站点 → **钱包 → 添加资金 → 付款方式 → USDT 支付**。
2. 选 **BSC** → 点 **显示我的充值地址**（这个地址是这个用户在这条链上的永久地址）。
3. 从交易所或钱包**转 2 USDT**，**网络必须选 BSC (BEP20)**。
4. 等 1~3 分钟，页面充值记录应显示 **已入账**，余额增加（= 2 × 汇率 × 站点额度换算）。
5. 等 30 秒~2 分钟，后台「待完成归集」归零；到 BscScan 用你的 destination 地址查，应看到 2 USDT 到账。
6. 在「已转账？用交易哈希查询到账」里**再提交一次同一笔哈希** → 应提示「这笔充值已经入账过了」，余额不变（证明幂等，不会重复入账）。
7. 换 **Polygon** 重复 1~6（Polygon 上用 POL 付手续费，转 2 USDT 即可）。

转账时容易踩的坑：

- 交易所提币**网络选错**（比如选了 ERC20 的 USDT）：钱会进那条链上的同名地址（地址相同），但当前没开那条链，不会自动入账。先用区块浏览器确认，再按第 10 节最后一行处理。
- 金额低于最小充值（BSC/Polygon 是 1 U）：会记录为「低于最小金额」，管理员可在后台点「手动入账」。
- 有的交易所会拦「合约地址」提币：先小额试，不行就让用户用钱包直接转。

---

## 9. 第八步：对外开放 + 日常运维

测试通过后：

1. 后台关掉 **「仅管理员可见（测试用）」**，保存。
2. 可选：**扫链服务 IP 白名单**填第一步自检时查到的出口 IP（多个用逗号分隔），多一层防护。
3. 给用户的话（可直接复制）：

> 充值请用 **USDT**，选 **BSC (BEP20)** 或 **Polygon**，网络必须和页面选的一致，只转 USDT，金额任意（不低于页面最小金额）。地址是你的专属地址，长期有效，可以重复使用。链上确认后 1~3 分钟自动到账；超过 10 分钟没到，在充值页用交易哈希点「查询到账」。

日常只需要盯三件事：

| 看什么 | 频率 | 处理 |
|---|---|---|
| 扫链服务是否 Online | 每天 | 离线就看 `docker compose logs`，期间用户仍可用哈希自助入账 |
| 待完成归集是否堆积 | 每天 | 堆积 = Gas 不够或 Gas 价上限太低，补 BNB/POL |
| Gas 钱包余额 | 每周 | 低于 1 美元就补 |

安全与备份：

- owner 助记词、destination 私钥离线保存；**Gas 钱包私钥只放服务器 `.env`**，被盗最多损失里面几美元手续费。
- **改收款地址**：用 owner 钱包在区块浏览器（BscScan / PolygonScan）对工厂合约调 `setDestination(新地址)`，new-api 和扫链服务都不用改。
- **备份 new-api 数据库**（地址、充值记录、salt secret 都在里面）；扫链服务的 `./data/scanner.db` 可以不备份，丢了会从回看区块重扫，new-api 不会重复入账。
- 换扫链密钥：后台重新生成 → 改 `.env` → `docker compose up -d --force-recreate`。

---

## 10. 排错速查表

| 现象 | 最可能原因 | 处理 |
|---|---|---|
| 扫链服务一直 Offline | `SCANNER_SECRET` 与后台不一致 | 后台重新生成 → 改 `.env` → `docker compose up -d --force-recreate` |
| 日志 `state: HTTP 401` | 密钥错 / 服务器时钟偏差 > 5 分钟 | 核对密钥；`timedatectl set-ntp true` 后重启容器 |
| 日志 `state: HTTP 403` | IP 白名单填错，或被 Cloudflare 拦 | 清空白名单先跑通；CF 对该域名关代理（灰云）或用 IP 直连 |
| 日志 `HTTP 404` | `NEWAPI_URL` 带了 `/api` 或子路径不对 | 改成根地址，如 `https://api.example.com` |
| 日志 `x509: certificate signed by unknown authority` | new-api 用了自签证书 / 证书链不全 | 换正规证书，或临时用 `http://服务器IP:3000` |
| 日志 `context deadline exceeded`（扫链时） | RPC 被墙 / 限流 | 换 RPC，或把 `POLL_SECONDS` 调大 |
| 后台红色「Verification nodes disagree」 | 两家 RPC 结果不一致（其中一家出错/被攻破） | 区块浏览器核对这笔交易，换掉出问题的 RPC；被暂缓的会自动重试 |
| `待完成归集` 只增不减 | Gas 不足 / Gas 价超上限 / nonce 卡住 | 补 BNB、POL；调 `MAX_GAS_PRICE_WEI_*`；看日志 |
| 容器起不来、日志 `permission denied ... scanner.db` | data 目录属主不对 | `chown -R 10001:10001 data` |
| `docker compose build` 卡在拉镜像 | 网络 | 用部署包方案 B；或给 Docker 配镜像加速 |
| new-api 校验工厂合约失败 | 地址填错 / 某条 RPC 不通 / 两链 owner 不一致 | 逐项核对，先在区块浏览器读合约 `destination()` |
| 用户说付了没到 | 交易未最终确认 / 网络选错 / 低于最小额 | 让用户用哈希查询；管理员后台「核实并入账」 |
| 用户转错链、转了别的币 | 不会自动入账，但钱还在用户自己的合约地址里 | 原生币（BNB/POL）用工厂 `sweepNative(salt)`、其他代币用 `sweep(salt, 代币合约)` 转到 destination；`salt` 在 new-api 数据库 `usdt_deposit_addresses` 表该地址对应行 |

需要我远程判断时，把这些贴给我：`docker compose logs --tail=80 usdt-scanner`、后台 USDT 页面截图、部署合约时的完整输出。

---

## 11. 参数速查

| 网络 | USDT 合约 | 小数 | 最终确认 | 最小充值 | 归集门槛 |
|---|---|---|---|---|---|
| BSC (BEP20) | `0x55d398326f99059fF775485246999027B3197955` | 18 | finalized | 1 | 1 |
| Polygon (PoS) | `0xc2132D05D31c914a87C6611C10748AEb04B58e8F` | 6 | finalized | 1 | 1 |
| Arbitrum（以后） | `0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9` | 6 | safe | 1 | 1 |
| TRON（以后） | `TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t` | 6 | 20 确认 | 10 | 50 |

路径与命令：

```bash
cd /opt/usdt-scanner                 # 程序目录
nano .env                            # 配置
docker compose logs -f usdt-scanner  # 看日志
docker compose up -d --force-recreate  # 改完 .env 重建容器
docker compose up -d --build         # 更新程序
ls -l data/scanner.db                # 扫链进度库
```

---

## 12. 不用 Docker 直接跑二进制（备用）

如果服务器完全拉不到任何镜像，用部署包里的二进制 + systemd：

```bash
cd /opt/usdt-scanner
chmod +x bin/*
mkdir -p data
cp usdt-scanner.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now usdt-scanner
systemctl status usdt-scanner --no-pager
journalctl -u usdt-scanner -f
```

`usdt-deploy` 同理，直接跑：`./bin/usdt-deploy new-key`。
