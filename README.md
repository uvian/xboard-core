# xboard-core

面向 [cedar2025/Xboard](https://github.com/cedar2025/Xboard) 面板的下一代节点后端。
取 `wyx2685/v2node` 的**现代协议覆盖 + 内核集成方式**，取 `cedar2025/Xboard-Node` 的**运维层（推送/机器模式/证书/双内核）**，内核基线锁 **Xray-core v26.11.9**。

> 文档：
> [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)（架构与决策记录）·
> [docs/CONTRACT.md](docs/CONTRACT.md)（与面板的逐字段契约，含证据行号）·
> [docs/PANEL-PATCH.md](docs/PANEL-PATCH.md)（面板补丁：为什么动、怎么装、怎么回滚）·
> [docs/STATUS.md](docs/STATUS.md)（已实现 / 未实现 / 怎么验）·
> [SECURITY.md](SECURITY.md)（威胁模型 / 安全默认值 / 攻击面与防护 / 部署清单）
> [docs/PANEL-IMPROVEMENTS.md](docs/PANEL-IMPROVEMENTS.md)（面板更长期的改造清单）


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

依赖：Go 1.27+（`go.mod` 要求），git、GNU patch。

```bash
make vendor          # clone Xray-core v26.11.9 到 third_party/xray
make patches         # 打 patches/*.patch（幂等）
make build           # -> bin/xboard-core
bash scripts/verify.sh   # gofmt + vet + test + build + smoke + vendored 单测 + 补丁幂等
```

没有 `make` 的机器（例如 Windows 上的 git-bash）直接跑 `bash scripts/verify.sh`，它与 `make verify` 等价；
`make vendor` 可用一条 `tar` 管道替代，见 Makefile 里的实现。

> Windows 注意：若 `%TEMP%` 不可写，go 会报 `creating work dir: ... Access is denied`，
> 构建前 `export TMP=D:/tmp TEMP=D:/tmp` 即可（`scripts/verify.sh` 支持 `GO_TMP` 变量）。

当前状态见 [`docs/STATUS.md`](docs/STATUS.md)：`go build ./...`、`go test ./...`、`-smoke`、
vendored `app/usergate` 与 `app/dispatcher` 单测均已通过。

## 安全默认（不可被面板覆盖的硬约束）

1. 所有监听 socket `SetMultipathTCP(false)`（CVE-2026-98432），除非配置显式 `sockopt.tcpMptcp: true`。
2. 进程启动即 `debug.SetGCPercent` 之外还校验 `GODEBUG` 是否含 `multipathtcp=0`，缺失则 WARN 并继续；`install.sh` 默认写入。
3. `allowInsecure` 在构建层被拒绝并翻译为 `pinnedPeerCertSha256` / `verifyPeerCertByName`。
4. `h2`/`http`/`h3`/`quic` 传输在构建层直接报错（上游已删除），不会静默降级成 tcp。
5. REALITY：`shortIds` 非空校验、`dest` 必填、`maxTimeDiff` 缺省 90s、可选 `mldsa65Seed`/`xver`/`limitFallback*`。
6. 不下发 `minClientVer`；若面板下发则记 WARN 并在 `/status` 暴露，避免 mihomo 系客户端被静默回落。

## 客户端要求（REALITY 节点）

Xray **v26.9.9** 起，REALITY 服务端只接受携带 **X25519MLKEM768** 密钥共享的 ClientHello
（`xtls/reality` `tls.go:233-235` 硬 `break`），而 mihomo 默认把 MLKEM768 摘掉
（`component/tls/reality.go:58`）。所以：

- **mihomo 系客户端**（FlClash / Clash Verge Rev / Nikki）：订阅里必须带
  `reality-opts.support-x25519mlkem768: true`。本项目 `panel-patch` 已让 Xboard 的
  `ClashMeta.php` 下发该键，直接打补丁即可。客户端内置的 mihomo 内核需支持该键：
  **mihomo v1.19.34 实测可用**（E2E 6d/6e 双向断言：带键通、不带键必失败）；
  更老的内核会忽略未知键，表现为仍然连不上。
- **xray 系客户端**：无影响，本来就发 MLKEM768。
- 缺这个键时的现象：客户端 `REALITY authentication failed`，节点侧只留
  `REALITY: processed invalid connection from <addr>: authentication failed or validation criteria not met`
  （`tls.go:490` + `:484`），两边都没有更细的原因。

