# 架构与决策记录

## 0. 目标与非目标

**目标**：给 Xboard 面板一个"内核跟得上上游、不用背大 fork"的节点后端，把内核升级从"半年不敢动"变成"每次上游发版 1 小时内重放补丁"。

**非目标**：不重写协议实现；不做面板的替代品；不追求协议数量超过 sing-box。

## 1. 三个项目的真实分歧点

两个既有实现的差别不在功能清单，而在**怎么把自己插进 xray 的数据面**：

| 项目 | 挂载方式 | 补丁面 | 代价 |
|---|---|---|---|
| cedar2025/Xboard-Node | `//go:linkname typeCreatorRegistry` 替换 dispatcher 工厂为 `limitDispatcherFactory` | fork 里 3 处结构私改 | 依赖 `common.typeCreatorRegistry`（`common/type.go:13`，**未导出**）；上游改地图/键即刻失效，且失败是运行期 panic 而非编译错误 |
| wyx2685/v2node | 把 `app/dispatcher` 整体复制进自身模块，自定义 `v2node.dispatcher.Config` 靠 `common.RegisterConfig` 注册 | 复制 ~700 行 | 上游 dispatcher 每次结构改动（26.x 重写过）都要人工并入副本 |
| **本项目** | vendored xray + `app/usergate` 接口 + dispatcher 两处 `Admit`/`Limit` 调用 | **1 新文件 + 4 hunk** | 需要 vendoring（仓库体积 +~40MB），升级需 `git am` 一次 |

结论：挂载点选"由节点注入的接口"，xray 侧不积累面板知识，这是三者中唯一可静态审查、失败可见（编译期）的做法。

## 2. 决策记录

### ADR-1 内核基线 = Xray-core v26.11.9

理由：v26.10.31 是首个默认 `lc.SetMultipathTCP(false)` 的版本（`transport/internet/system_listener.go:92-94`），对应 CVE-2026-98432（Linux IPv4 Record Route/Timestamp 选项 UAF，CVSS 9.8，被动路径即可触发）。继续留在 v26.3.27（Xboard-Node 现状）等于把每个节点暴露在该漏洞下。

**边界必须说清**：关掉 MPTCP 只是**收缩暴露面**，不是修复。真正的修复在 Linux 内核（5.10.272 / 5.15.223 / 6.1.190 / 6.6.159 / 6.12.113 / 6.18.56 / 7.2.10 及以上）。节点跑在未打补丁的内核上时，安全保证仍要由「内核升级」兑现，见 docs/SECURITY.md 的运维清单。

顺带拿到的：印度 CCA 三张根证书移除（v26.10.21）、`allowInsecure` 的真正替代（`pinnedPeerCertSha256`/`verifyPeerCertByName`）、REALITY 的 `mldsa65Seed`/`xver`/`limitFallback*`。

**v26.10.31 → v26.11.9（2026-11-09）**：`app/dispatcher` 一行未动（补丁上下文零冲突），`infra/conf/xray.go`、`transport_security.go`、`policy.go`、`proxy/*` 均未变。与本项目相关的三处：

1. `infra/conf/log.go` 的 JSON tag `loglevel` → `logLevel`（我们直接构造 `conf.LogConfig` 结构体，不受影响；但运维手写 JSON 配置时旧键会被忽略）；
2. `proxy/freedom` 的 `finalRules` 新增 `logLevel`、`blockDelay` 更名 `delay`（旧的 `blockDelay` 仍接受并打弃用告警）；**`action != block` 的规则带 `delay`/`logLevel` 会直接报错** —— 我们的 allow 规则只写 `action/network/ip`，安全，但以后别加；
3. `infra/conf/geodata.go` 会把 geodata 官方 `.dat` 地址自动改写成 `.gz`（我们不用 geodata，无感）。

一条纠错（避免以后误判）：**XDRIVE 传输不是 v26.11.9 新增**，`transport/internet/xdrive/` 在 v26.10.31 就存在，本次只是优化；`xtls/reality` 换 utls fork 后 `HelloChrome_Auto` 的指向变了（`tls.go:278` 加了别名），也不是 xray 侧新增默认指纹。我们的传输白名单仍然只放行 tcp/ws/grpc/httpupgrade/xhttp，遇到 `xdrive` 会明确报错而不是静默降级。

### ADR-2 零协议补丁：xray 只跑 vless/vmess/trojan/ss2022

`wyx2685/xray-core` 相对上游 **+6978/−191 行**，其中 ~2200 行 AnyTLS、~1900 行 TUIC、~1500 行 SS 抗主动探测。这些是"上游没有"，不是"我们需要改数据面"。

因此：anytls/tuic/hysteria2 交给 **sing-box 内核**（Xboard-Node 已经证明双内核可行），xray 侧保持零协议补丁。代价是 sing-box 与 xray 的用户模型要对齐（见 ADR-5），收益是升级面收敛到 1 个补丁。

### ADR-3 配置构建层集中收敛安全默认

`internal/kernel/xray/stream.go` 是唯一的传输层装配点，三件事在这里强制：

1. `SocketConfig.TcpMptcp = false`（不信任面板，也不给"忘开关"的机会）；
2. 已删除传输（h2/http/h3/quic）**直接报错**，不静默降级——静默降级会把"面板还发 h2"变成线上 502；
3. REALITY 前置校验 `shortIds` 非空、`dest` 必填、`privateKey` 非空；**不写** `minClientVer`（上游默认值已注释掉，显式下发会把自报 1.8.2 的 mihomo 系客户端全部打回 dest）。

### ADR-4 闸门接口而非闸门实现

`app/usergate.Gate` 只有两个方法：`Admit(ctx, tag, email, ip) error`、`Rate(tag, email) int64`。

- `internal/gate` 负责设备数（按 IP 去重 + 2 分钟活跃窗口）与限速值计算（节点级与用户级取非零最小值，含带过期时间的动态限速）；
- xray 侧只管调用与令牌桶包装，**不认识 uid/uuid/面板字段**。

这样闸门逻辑可以单测（`internal/gate` 无 xray 依赖），也不需要复制 dispatcher。

### ADR-5 面板契约用 V2，字段向前兼容

`/api/v2/server/{config,user,push,alive,alivelist,status,handshake,report}` 是 Xboard 现行节点协议（`app/Http/Routes/V2/ServerRoute.php`，middleware `server.v2`：`token` == `admin_setting('server_token')` + `node_id`）。

v2node 用的正是这套（它多带的 `node_type=v2node` 被忽略），所以协议兼容性已被第三方实现验证过。

面板尚未下发的字段（`mldsa65Seed`、`xver`、`encryption_settings`）在 `panel.TLSSettings` 里先占位，零值 = 未配置，不会因为面板旧而失败。

### ADR-6 观测面只监听 loopback

Xboard-Node 的 health server（`cmd/xboard-node/main.go:76`）无鉴权且默认对外。本项目 `/healthz` 与 `/metrics` 绑定 `127.0.0.1:8090`，需要外部抓取时由反代做鉴权。

### ADR-7 重载语义：结构变则重建，用户变则热更

- 节点结构指纹（protocol/tls/port/network）变化 → 重建 `core.Instance`，旧实例在替换后关闭（在线连接会断，这是 xray 的既定约束）；
- 只有用户增删改 → `inbound.Manager.GetHandler` + `AddUser/RemoveUser`，不动内核（不断线）；
- 304 复用：面板用 ETag + msgpack，空轮询成本接近 0（沿用 v2node 的做法）。

### ADR-8 私网兜底放在 router 层，不依赖上游 per-inbound 规则

**背景**：`allow_private_targets=false`（默认）时，我们希望节点绝不把用户流量送到
`127.0.0.1` / RFC1918 / `169.254.169.254`。最开始的做法是给 freedom 出站加一条让私网通过的
`finalRules`（给联调用的开关），并把"默认拦截"寄托在上游
`proxy/freedom/freedom.go:161-180 getDefaultFinalRule`。

**问题**：那条默认规则按 `session.Inbound.Name` 判定，而该字段在整个上游只有两处赋值——
`proxy/vless/inbound/inbound.go:534`（`"vless"`）与 `proxy/vmess/inbound/inbound.go:272`（`"vmess"`）。
`app/proxyman/inbound/worker.go` 构造 `session.Inbound` 时**不填 Name**，于是
**trojan / shadowsocks 入站的默认规则恒为 nil**：付费用户用 trojan 节点就能访问节点本机的
`127.0.0.1:8090`（`/metrics`、健康检查都没有鉴权）、整个内网段、以及云厂商的实例元数据接口。

**决策**：在 router 层兜底，而不是修补 freedom 或给上游打协议级补丁。

- `allow_private_targets=false` 时**无条件注册 router**（哪怕面板一条路由都没配），
  第一条规则恒为 `{type: field, ip: [9 个私网 CIDR], outboundTag: block}`；
- `DomainStrategy = IpIfNonMatch`：纯 `AsIs` 只能拦 IP 字面量，"域名解析到内网"会漏；
- `allow_private_targets=true` 时不注册这条规则，也不启用 `IpIfNonMatch`（少一次解析、少一次 DNS 泄漏）；
- sing-box 侧注入等价的 `route.rules[].ip_cidr → reject`。

**代价**：有兜底规则时每个请求多一次域名解析（仅当域名未被其它规则命中）；router 变成一个
常驻组件，"没配路由"不再是"没有 router"。

**为什么不是别的方案**：给 freedom 打补丁要改上游数据面（增加 vendored 补丁面）；
靠面板表单校验挡不住"管理员直接改数据库/调 API"；靠主机防火墙只能防本机、防不住内网段。
router 层的规则是内核自己执行的第一跳判定，与协议、与面板、与运维习惯解耦。

### ADR-9 闸门的可观测性视为安全需求

设备数 / 限速挂在 vendored 的 `app/usergate` 上。这条链路有一个很坏的失效模式：
补丁在某次重 vendor 后没打上，**节点照样启动、连接照样建立、日志照样安静**，
只是限速和设备数不再生效——除非有人手工去连一堆设备验证。

**决策**：`usergate.Limit` 在给链路套令牌桶之前先把链路登记进 `activeConns`，
读端收到 EOF/错误或写端出错时减一（`connFlag.once` 保证只减一次）。
`ActiveConns()` 同时暴露到 `/metrics`（`xboard_core_active_conns`）与面板上报
（`metrics.active_connections`）。补丁失效时这个数字恒为 0，可以被监控直接抓到。

**不做的事**：不把它做成"精确的在线用户数"。UDP/UoT 会复用链路，语义是"当前托管的链路数"，
这一点在 `SECURITY.md` §6 里写明了。

## 3. 代码地图

```
cmd/xboard-core/main.go        runner：pull 循环、内核生命周期、上报
internal/config                YAML → Conf，含 machine/node 两种模式校验
internal/panel                 Xboard V2 契约：ETag + msgpack，types.go 是唯一字段真相
internal/kernel                Kernel 接口 + 按协议路由的 Registry
internal/kernel/xray           Build（安全默认）/ Node（启动、热更新、stats）
internal/gate                  设备数 + 限速值（无 xray 依赖，可单测）
internal/traffic               per-user 增量计数，Drain 上报
internal/metrics               Prometheus 文本协议，零依赖
third_party/xray/patches       0001-usergate.patch
```

## 4. 请求生命周期

```
面板 /api/v2/server/config ──┐
                            ├─► runner.pull ─► Registry.For(protocol) ─► kernel.Start
面板 /api/v2/server/user  ───┘                                   │
                                                                 ├─► gate.LoadUsers（设备数/限速）
                                                                 └─► xray.Build（安全默认）→ core.New → Start

客户端连接 → xray inbound → dispatcher.Dispatch
                              ├─ usergate.Admit  → gate.Admit（设备数超限 → 断连）
                              └─ usergate.Limit  → gate.Rate（令牌桶包 writer/reader）

runner.report → alive（在线设备）/ push（流量增量）/ status（存活+版本）
```

## 5. 升级与同步政策

- 上游发版 → `third_party/xray` 切新 tag → `git am --3way patches/*.patch`；
- 冲突只可能落在 `app/dispatcher/default.go` 的 `Dispatch`/`DispatchLink` 两处（hunk 头已写明上下文）；
- 每次升级必须跑：`go test ./... ` + `go test ./app/dispatcher/... ./app/usergate/...`（上游用例） + `make smoke`；
- 若上游某天提供官方限速/设备数扩展点（`features/` 里出现等价能力），立刻废弃 0001 补丁回到纯上游。

## 6. 路线图

| 阶段 | 内容 | 验收 |
|---|---|---|
| M0（当前） | 脚手架 + 补丁 + 安全默认 | `go build ./...` 通过、`make smoke` 用假面板跑通一次 config→start |
| M1 | sing-box 内核接入（anytls/tuic/hysteria2） | 三种协议各一条真机连通 + 流量上报对齐 |
| M2 | WS 推送（`/api/v2/server/handshake` + 事件） | 用户变更端到端 < 2s，轮询降级为兜底 |
| M3 | 证书自动化（certmagic + libdns） | DNS-01 签发 + 热重载，不停机 |
| M4 | 面板侧改造（见 PANEL-IMPROVEMENTS.md） | 面板能下发 mldsa65Seed/xver/encryption_settings |
