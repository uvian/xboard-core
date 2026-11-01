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

### ADR-1 内核基线 = Xray-core v26.10.31

理由：v26.10.31 是首个默认 `lc.SetMultipathTCP(false)` 的版本（`transport/internet/system_listener.go:92-94`），对应 CVE-2026-98432（Linux IPv4 MPTCP 监听 socket UAF，CVSS 9.8，被动路径即可触发）。继续留在 v26.3.27（Xboard-Node 现状）等于把每个节点暴露在该漏洞下。

顺带拿到的：印度 CCA 三张根证书移除（v26.10.21）、`allowInsecure` 的真正替代（`pinnedPeerCertSha256`/`verifyPeerCertByName`）、REALITY 的 `mldsa65Seed`/`xver`/`limitFallback*`。

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
