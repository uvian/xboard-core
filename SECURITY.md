# 安全模型

这份文档回答三个问题：**谁能打到我**、**默认值挡住了什么**、**哪些坑必须由运维自己填**。
代码里的每一条断言都能在仓库中找到对应实现与测试，凡是"没做到"的部分单列在 §6。

---

## 1. 威胁模型

| 角色 | 能力假设 | 我们关心什么 |
| --- | --- | --- |
| 付费用户（持有有效 UUID） | 能跑任意客户端、能改本地配置、能构造任意目标地址 | 不能借节点访问私网 / 云元数据 / 管理口；不能超设备数；不能超速；不能在别的用户之间蹭流量 |
| 未认证扫描器 | 能全端口扫、能发任意 ClientHello | 不能从 TLS/REALITY 的差异里区分"这是代理"与"这是真网站"；不能借 REALITY 回落把节点当内网探测器 |
| 面板管理员 | 能改任何节点字段（含 dest、证书路径、路由规则） | 危险配置必须**显式报错**而不是静默降级（配错了宁可不启动） |
| 本机其它进程 / 低权用户 | 能读 `/metrics`、能连 loopback | 管理口默认只听 `127.0.0.1`；日志里不出现节点 token |
| 上游供应链 | Xray / sing-box / 面板代码 | 只 vendor 一个内核、补丁面 ≤ 5 个 hunk、每次升级重新验证补丁等价 |

不在威胁模型内：宿主机已被 root 拿下、面板数据库被拖库、DNS 被投毒、客户端被植入木马。
这些情况下任何节点软件都救不了你，别指望它。

---

## 2. 安全默认值一览

| 配置项 | 默认 | 为什么是这个默认 | 想改的话 |
| --- | --- | --- | --- |
| `kernel.allow_private_targets` | `false` | 节点不该是内网跳板。开启后用户可访问 `127.0.0.1`、`10/8`、`169.254.169.254`（云实例凭证接口） | 只在"节点必须回源内网服务"时开，且配合主机防火墙 |
| `kernel.strict_client_ver` | `false` | 面板下发的 REALITY `minClientVer` 默认照发（这是正当的防降级旋钮），只打 WARN 提示"mihomo 系客户端自报 1.8.2，会被打回 dest" | 只服务 Xray 官方客户端时开 `true`，让错误配置直接起不来 |
| 私网兜底路由 | 无条件注册 | 见 §3.1：上游按"入站协议名"拦私网，而只有 vless/vmess 会填这个字段 | 不需要改，除非 `allow_private_targets=true` |
| `API.Listen` | `127.0.0.1:8090` | `/metrics` 与健康检查没有鉴权，绝不能听 `0.0.0.0` | 真要远程抓取就套 SSH 隧道 / 反代加 basic auth |
| `mptcp` | socket 层显式 `SetMultipathTCP(false)` | CVE-2026-98432（Linux IPv4 UAF，CVSS 9.8）只需监听 socket 收 SYN 即可触发 | 不要开；这是**缓解**，根治靠内核补丁 |
| `accept_proxy_protocol` | `false` | 打开就等于信任客户端自报源 IP，设备数闸门会被一行 header 绕过 | 只有在可信 LB 后面才开，且 LB 必须剥离客户端自带的 PROXY 头 |
| `allow_insecure` | 拒绝启动 | 上游 v26 已移除该字段，静默丢弃会让运维误以为还生效 | 改用 `pinned_peer_cert_sha256` / `verify_peer_cert_by_name` |
| sing-box 节点的设备数/限速 | 拒绝启动 | 闸门挂在 xray dispatcher 上，sing-box 侧没有等价钩子 | 把节点内核改成 xray，或在面板去掉这些限制 |
| REALITY `dest` | 校验 | 校验失败时服务端把 TLS 连接**原样转给 dest**，指向内网即等于开放内网探测器 | 只能填公网 TLS 站点（有合法证书，否则 uTLS 握手失败） |

---

## 3. 攻击面与对应防护

### 3.1 数据面：用户能把流量送到哪里

这是最容易被忽略、后果最严重的一类。

- **私网兜底在 router 层，不在 freedom**。上游 `proxy/freedom/freedom.go:161-180` 的
  `getDefaultFinalRule` 按 `session.Inbound.Name` 决定是否拦私网，而该字段全仓只有
  `proxy/vless/inbound/inbound.go:534` 与 `proxy/vmess/inbound/inbound.go:272` 会赋值 →
  **trojan / shadowsocks 入站的默认规则恒为 nil，私网目标直接放行**。
  我们在 `internal/kernel/xray/routing.go` 里无条件插一条 `私网 CIDR → block`，
  与协议无关，并启用 `DomainStrategy=IpIfNonMatch` 让"域名解析到内网"同样被拦。
- **REALITY dest 校验**：拒绝空值、`@`/`//` 开头的 unix socket、`localhost`、
  私网/回环/链路本地/未指定/组播地址。理由同上：回落目标是攻击者能借用的出站通道。
- **sing-box 侧同规则**：`route.rules[].ip_cidr → reject`，`final: direct`。

**REALITY 的客户端前提**：Xray v26.9.9 起的 REALITY 服务端只接受带 X25519MLKEM768 密钥共享的 ClientHello
（`xtls/reality` `tls.go:233-235` 是硬 `break`）。mihomo 默认把它摘掉，所以 FlClash / Clash Verge Rev / Nikki
必须拿到带 `reality-opts.support-x25519mlkem768: true` 的订阅配置才能连上，否则表现为 `REALITY authentication failed`
（节点侧日志 `REALITY: processed invalid connection from <addr>`，失败原因是 `authentication failed or validation criteria not met`，`xtls/reality` `tls.go:490` + `:484`）。我们的面板补丁已在
`app/Protocols/ClashMeta.php` 两处 `reality-opts` 里下发该键（`docs/PANEL-IMPROVEMENTS.md` P0-4），E2E 6d/6e 双向验证。

### 3.2 数据面：设备数与限速

- 闸门只有一处实现：`third_party/xray/app/usergate`（vendored 补丁），在
  `app/dispatcher/default.go` 的 `Dispatch` / `DispatchLink` 两条路径上各插一次 `Admit` + `Limit`。
- `Admit` 用 IP 去重判设备数（窗口 2 分钟），`Limit` 按 `tag|email|方向` 维护令牌桶。
- **可观测性即安全**：`usergate.ActiveConns()` 会同步到 `/metrics` 的 `xboard_core_active_conns`
  与面板上报的 `active_connections`。补丁一旦在重 vendor 后失效，连接照常建立、日志照常安静，
  只有这个数字会掉回 0——这是唯一的信号，请给它建告警。
- 已知 fail-open：`email == ""`（匿名 socks/http 入站、或 vless `fallbacks` 转进来的连接）没有身份可比，
  直接放行。当前我们不生成 `fallbacks` 配置，所以不可利用；启用它之前请重新评估。

### 3.3 控制面：面板 → 节点

- 鉴权：`token` 必须严格等于面板的 `server_token`（面板侧 `ServerV2` 中间件），机器模式走 `machine_id`。
- 传输：默认 `http://`，凭 token 在 query 上。**这意味着 token 会出现在**
  面板访问日志、中间代理日志、以及任何拼了 URL 的错误串里。
  - 我们对错误串做了 `scrub()`（`token|api_key|machine_id` → `<redacted>`），
    `*url.Error` 一律只保留 `op` 与底层原因；
  - 但**根因在协议设计上**：只要 token 还在 query 上，链路上任何能看日志的人都拿得到它。
    生产建议：把面板与节点放在同一内网 / WireGuard 里，或用 `ws_insecure: false` + 反代 TLS。
- 面板返回的配置被当作**不可信输入**处理：所有字段先过结构体校验，未知/已移除字段（`h2`、`quic`、
  `allow_insecure`、`proxySettings`）**报错**而不是忽略。
- 路由规则里引用的 `custom_outbounds` tag 必须真实存在，否则报错。否则规则会静默落到默认出口，
  运维以为在走专线、实际在裸奔。

### 3.4 进程面

- 管理口（health + metrics）只监听 loopback，且没有鉴权——**不要改 Listen**，要远程抓取请套隧道。
- 内核实例崩溃/重启由 supervisor 兜住，但**不会**自动降级到"无闸门模式"：
  xray 起不来就是起不来，日志里能看到原因。
- 日志默认不打用户 UUID 与 token（`scrub` + `redact`）；调试级日志会打 email（= `uid.uuid`），
  排障结束后请调回 `info`。

### 3.5 上游依赖

- 只 vendor 一个内核（Xray-core），补丁面 = 新增 `app/usergate`（1 文件）+ `app/dispatcher/default.go`（3 hunk）。
  每次升级都必须重新验证补丁等价（`scripts/verify.sh` 会做 `patch -p1 --forward` 的幂等检查）。
- 不引 `//go:linkname`（Xboard-Node 的做法，依赖未导出符号），也不复制 700 行 dispatcher
  （v2node 的做法，上游改动要人工同步）。
- 版本锚定在 `go.mod` 与 `Makefile` 的 `XRAY_VERSION`；升级流程见 `third_party/xray/README.md`。

---

## 4. 部署清单（节点）

```bash
# 1) 先修内核，MPTCP 只是缓解：CVE-2026-98432 需要 5.10.272+/5.15.223+/6.1.190+/6.6.159+/6.12.113+/6.18.56+/7.2.10+
uname -r

# 2) 系统层关 MPTCP（双保险；Go 侧已 SetMultipathTCP(false)）
sysctl -w net.mptcp.enabled=0
echo 'net.mptcp.enabled=0' > /etc/sysctl.d/99-no-mptcp.conf

# 3) 管理口只在本机
ss -lntp | grep 8090     # 必须是 127.0.0.1:8090

# 4) 确认私网兜底生效（在节点上跑，应看到 routing enabled rules>=1 allow_private=false）
journalctl -u xboard-core -n 100 | grep 'routing enabled'

# 5) 确认闸门真的在跑：连一条后看 active_conns 是否从 0 涨起来
curl -s 127.0.0.1:8090/metrics | grep active_conns
```

---

## 5. 怎么验证这些说法

```bash
# 单元测试（含私网兜底、dest 校验、令牌脱敏、闸门语义）
go test ./...

# 面板契约 + 内核装配 + REALITY + mihomo 回归（需要 Linux）
sudo bash scripts/e2e/linux-e2e.sh

# 补丁等价（干净上游 + patch -p1 后的文件必须与 vendor 树一致）
bash scripts/verify.sh
```

E2E 里与本文件直接对应的断言：

| 断言 | 验证的安全属性 |
| --- | --- |
| `5c` 上游默认拦截私网目标 | 说明"默认拦截"是上游行为，但只对 vless/vmess 生效 |
| `7` 经节点取到源站内容 | 数据面通（在显式允许私网的前提下） |
| `8` 未知 UUID 被拒 | 内核鉴权在闸门之前 |
| `11` REALITY 节点起得来 | dest 校验没有误杀合法配置 |
| `12` mihomo 能握手 REALITY | 没有无意中启用了 `minClientVer` 门禁 |

---

## 6. 已知限制（诚实清单）

1. **token 在 URL 上**：协议层面无法消除，只能靠网络隔离与日志脱敏降低影响（见 §3.3）。
2. **`active_conns` 是"链路数"**：一条 TCP 连接一对链路，UDP/UoT 因 dispatcher 复用会略高于实际连接数。
3. **sing-box 节点无闸门**：这是设计选择（fail-closed 拒绝启动），不是遗漏。
4. **ECH 只解析不下发**：面板字段会被读入，但内核侧未接上。
5. **面板补丁未做多节点灰度**：只在 E2E 单实例上验证过。
6. **`fallbacks` 会绕过闸门**：见 §3.2 的 fail-open 说明。
7. **DNS 重绑定没有完全封死**：私网兜底走 router 的 `IpIfNonMatch`，判定用的是"规则匹配时解析到的地址"，
   freedom 实际拨号时会**再解析一次**。理论上攻击者可以用 TTL=0 的域名，让两次解析拿到不同结果
   （第一次公网、第二次内网）绕过拦截。要彻底封死得让出站固定到已校验的 IP（或在内核侧做
   dialer 级校验），当前实现没做——虽然构造难度高，但知道比不知道好。
8. **不防面板数据库被脱**：UUID 泄漏后攻击者就是一个"合法用户"，只能靠设备数/限速/流量限额止损。

---

## 7. 报告问题

发现安全问题请直接开 issue 并在标题里写 `[security]`，不要贴可直接利用的 PoC；
维护者会先复现、再决定是否需要私下沟通修复窗口。
