# xboard-core

面向 [cedar2025/Xboard](https://github.com/cedar2025/Xboard) 面板的下一代节点后端。
取 `wyx2685/v2node` 的**现代协议覆盖 + 内核集成方式**，取 `cedar2025/Xboard-Node` 的**运维层（推送/机器模式/证书/双内核）**，内核基线锁 **Xray-core v26.10.31**。

> 文档：
> [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)（架构与决策记录）·
> [docs/CONTRACT.md](docs/CONTRACT.md)（与面板的逐字段契约，含证据行号）·
> [docs/PANEL-PATCH.md](docs/PANEL-PATCH.md)（面板补丁：为什么动、怎么装、怎么回滚）·
> [docs/STATUS.md](docs/STATUS.md)（已实现 / 未实现 / 怎么验）·
> [docs/PANEL-IMPROVEMENTS.md](docs/PANEL-IMPROVEMENTS.md)（面板更长期的改造清单）

## 为什么不是继续用现成的两个

| 维度 | Xboard-Node（dev 2026-04） | v2node（2026-10） | 本项目 |
|---|---|---|---|
| 内核版本 | fork 冻结在 v26.3.27 | v26.9.30 | **v26.10.31**（含 MPTCP 默认关闭） |
| 设备数/限速 | `//go:linkname` 劫持未导出注册表 | 复制整个 dispatcher 包到自身模块 | **单补丁 `app/usergate`（3 处 hunk）** |
| 协议面 | vless/vmess/trojan/ss/hysteria2/tuic/anytls（xray+sing-box） | vless/vmess/trojan/ss/hysteria2/tuic/**anytls**，含 **VLESS encryption / mldsa65Seed / xver** | xray 侧 vless/vmess/trojan/ss2022 + **完整 REALITY 硬化**；anytls/tuic/hysteria2 交 sing-box（**零协议补丁**） |
| 传输 | ws/grpc/httpupgrade/**h2(已删)**/xhttp | tcp/ws/grpc/httpupgrade/xhttp | **xhttp 优先，h2/quic 编译期拒绝** |
| 推送 | WS handshake 推送 | 仅轮询（ETag + msgpack） | WS 推送 + 轮询回退 |
| 证书 | certmagic + 21 个 libdns provider | lego | certmagic + libdns（沿用 xbn 方案） |
| 机器模式 | 支持（多节点/单进程） | 单进程多 Nodes | 支持 |

关键结论：两个项目的**真正分歧不在功能，而在"怎么把自己插进 xray 的数据面"**。
- xbn 选 linkname（补丁最少，但依赖未导出符号）；
- v2node 选整体复制 dispatcher（自包含，但要跟同步 700 行上游改动）；
- 本项目选第三条：**在 vendor 的 xray 里加一个 `app/usergate` 包 + 3 处 hunk**，数据面语义由 xray 自己的 dispatcher 承担，闸门只做 `Admit`/`Limit` 两个纯函数。

## 目录

```
cmd/xboard-core/          # 入口（node 模式 / machine 模式 / standalone）
internal/config/          # YAML 配置 + 校验 + 热重载
internal/panel/           # 面板契约：/api/v2/server/*（含 V1 兼容）
internal/kernel/          # 内核抽象（xray / singbox 双实现）
internal/kernel/xray/     # 配置构建（安全默认在此集中收敛）+ 运行时热更新
internal/gate/            # 设备数 + 限速闸门（被 usergate 补丁调用）
internal/sysinfo/         # 面板 status 必填项（cpu/mem/swap/disk，零依赖）
internal/traffic/         # 每用户流量计数与上报
internal/metrics/         # Prometheus 指标 + 健康检查
internal/cert/            # ACME 证书管理（certmagic + libdns）
third_party/xray/         # vendored Xray-core + 补丁序列
panel-patch/              # 面板侧补丁（对 cedar2025/Xboard 的零迁移改动）
docs/                     # 架构决策记录、面板改造清单
```

## 构建

```bash
./scripts/apply-patches.sh          # 把 patches/*.patch 打到 third_party/xray
go build ./...
go test ./...
make smoke                          # 起一个假面板 + 单节点，验证配置构建与闸门
```

> 当前仓库处于脚手架阶段：`internal/*` 的接口与安全默认已按上述决策写定，`third_party/xray` 尚未 vendored（`go.mod` 里是本地 replace 占位）。

## 安全默认（不可被面板覆盖的硬约束）

1. 所有监听 socket `SetMultipathTCP(false)`（CVE-2026-98432），除非配置显式 `sockopt.tcpMptcp: true`。
2. 进程启动即 `debug.SetGCPercent` 之外还校验 `GODEBUG` 是否含 `multipathtcp=0`，缺失则 WARN 并继续；`install.sh` 默认写入。
3. `allowInsecure` 在构建层被拒绝并翻译为 `pinnedPeerCertSha256` / `verifyPeerCertByName`。
4. `h2`/`http`/`h3`/`quic` 传输在构建层直接报错（上游已删除），不会静默降级成 tcp。
5. REALITY：`shortIds` 非空校验、`dest` 必填、`maxTimeDiff` 缺省 90s、可选 `mldsa65Seed`/`xver`/`limitFallback*`。
6. 不下发 `minClientVer`；若面板下发则记 WARN 并在 `/status` 暴露，避免 mihomo 系客户端被静默回落。
