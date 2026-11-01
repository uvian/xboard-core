# 现状清单：已实现 / 未实现 / 怎么验

一句话：**契约层与 `cedar2025/Xboard` master（`e68fec5`）源码级对齐；Go 侧已用 go1.27.3 全量编译并通过
测试、vet、冒烟与 vendored 单测**（见 §3 的实测命令与输出），PHP 侧补丁仍未 lint（本机无 PHP CLI）。

---

## 1. 已实现（读代码可核对）

### 契约

| 项 | 位置 |
| --- | --- |
| config 扁平解析 + `networkSettings` camelCase + ETag/304 复用 | `internal/panel/types.go`、`internal/panel/client.go:Config` |
| user 列表 `{"users":[…]}` + msgpack + 304 复用 | `client.Users` |
| report 四合一（traffic / alive / online / status / metrics） | `client.Report`、`cmd/xboard-core/main.go:report` |
| status 七项必填（cpu/mem/swap/disk） | `internal/sysinfo/*` |
| V2 鉴权（`node_id` 或 `machine_id` + `token`，不发 `node_type`） | `client.query()` |
| WS 推送能力探测（`handshake`） | `client.Handshake`、`main.go:handshake` |

### 内核

| 项 | 位置 |
| --- | --- |
| Xray v26 已删传输（h2/http/h3/quic）在构建期报错而不是启动期崩 | `internal/kernel/xray/builder.go:removedNetworks` |
| MPTCP 强制关闭（`SocketSettings.TcpMptcp=false`） | `internal/kernel/xray/stream.go:applyStream` |
| REALITY 装配（shortIds/dest/privateKey/serverNames 非空、xver≤2、mldsa65Seed 32B 且 ≠privateKey） | `stream.go:buildReality` |
| `minClientVer` 默认不下发；≥26 直接拒绝构建并说明 mihomo 自报 1.8.2 | `stream.go:buildReality` |
| `maxTimeDiff` 秒 → 纳秒换算（不换算等于静默关闭时间差校验） | `stream.go:maxTimeDiff` |
| VLESS `decryption`（`none` / `mlkem768x25519plus`） | `builder.go:buildVLESS` |
| 用户热更（`AddUser/RemoveUser` 批量 + Reload 保留 users） | `internal/kernel/xray/node.go` |
| 设备数 + 限速闸门（vendored 补丁注入，非 linkname、非复制 dispatcher） | `third_party/xray/patches/0001-usergate.patch`、`internal/gate/gate.go` |

### 面板补丁

| 项 | 位置 |
| --- | --- |
| REALITY schema 加 8 键 + 校验 + snake→camel 归一 + 可选传输守卫 | `panel-patch/0001-reality-schema-and-transport-guard.patch`，说明见 `docs/PANEL-PATCH.md` |

---

## 2. 未实现（明确列为待办，不是"忘了"）

| 项 | 影响 | 计划 |
| --- | --- | --- |
| **PHP 侧未 lint** | 面板补丁可能有语法错误 | 有 PHP 环境后 `php -l` 三个文件（见 §3） |
| **未与真实面板联调** | 契约按源码对齐，但未跑过真机 | 一台节点 + 一个面板跑 §3.3 |
| sing-box 内核 | anytls / tuic / hysteria2 节点会被拒绝（`no kernel supports protocol`） | M2 |
| WS `sync.*` 事件消费 | 配置/用户变动要等下一个轮询周期（≤60s） | M2，且必须保留轮询兜底 |
| ACME 证书签发 | `cert_config` 目前只认 `cert_file/key_file` 与内联 `tls_cert/tls_key`；`provider/acme` 方式不生效 | M3（先做 http-01，DNS-01 复用面板 DNSEnv） |
| `routes` / `custom_outbounds` / `custom_routes` | 面板配的分流不生效（默认 direct/block） | M3 |
| `multiplex` 入站侧 | 目前按关闭处理（上游 inbound 侧 multiplex 语义有限） | 评估后再定 |
| 每节点并发连接上限 | gate 只管设备数与带宽 | M4 |
| metrics 鉴权 | 只监听 `127.0.0.1`，无鉴权（靠网络隔离） | 保持现状，文档写明 |
| `-smoke` 只覆盖 vless/tcp/none | 其它协议/传输没有冒烟用例 | M1 扩表驱动 |

---

## 3. 怎么验（按成本从低到高）

已实测通过（go1.27.3 windows/amd64，2026-11-01）：

```
go build ./...                                   # exit 0
go test ./...                                    # gate / kernel/xray / panel / traffic 全 ok
gofmt -l cmd internal                            # 无输出
go vet ./cmd/... ./internal/...                  # 无输出
go run ./cmd/xboard-core -smoke                  # smoke ok: vless/tcp inbound=1 app=6 bytes=1003
cd third_party/xray && go test ./app/usergate/... ./app/dispatcher/...   # 均 ok
bash scripts/verify.sh                           # 以上全套 + 补丁幂等，末尾 "OK: 全部通过"
```

```bash
# 0) 只看契约，不需要环境：docs/CONTRACT.md §9 的四条 curl
# 1) 从零构建
make vendor          # 拉 Xray-core v26.10.31 到 third_party/xray（Windows 无 make 见 README）
make patches         # GNU patch 幂等应用 0001-usergate.patch
bash scripts/verify.sh   # 等价 make verify
# 2) 有 PHP 时
php -l app/Services/ServerService.php
php -l app/Models/Server.php
php -l app/Http/Requests/Admin/ServerSave.php
# 3) 联调（一台节点 + 一个面板）
./bin/xboard-core -c config.yaml   # 观察：config 命中 304 不再重建、report 200、gate 拒绝日志带 email
```

`make vendor` 之后**先别急着改 `third_party/xray`**：所有改动都必须落成 `patches/` 里的补丁，
否则下次 `make vendor` 会被覆盖 —— 这是 vendoring 最常见的翻车点。

---

## 4. 本轮的三个"发现即修正"

1. `networkSettings` 是 camelCase：原先按 `network_settings` 解析，ws/grpc/xhttp 参数会**永远读不到**，
   症状是节点起来了但客户端连不上（配置静默退化）。
2. `reality_settings.server_port` 面板发的是 **integer**，后端原先是 `string` → 整包 unmarshal 失败，
   症状是"节点启动不了"。已改为 `FlexInt`（同时吃 `1` / `"1"` / `null`），`xver` 同理。
3. `maxTimeDiff` 面板填秒、上游要纳秒 → 不做换算时填 60 等于 60ns，**时间差校验静默失效**。

---

## 5. 两轮独立复核的结论（2026-11-01）

由两个独立复核 Agent 分别按「面板契约符合性」与「安全默认 + 补丁可维护性」逐行核对源码，结论：

**已修（本轮代码里）**

| # | 问题 | 后果 |
| --- | --- | --- |
| 1 | `minClientVer` 只看主版本（`majorVersion>=26`） | `1.26.10` / `2.0.0` 会被放行，而它们逐段大于 mihomo 自报的 (1,8,2) → 所有 mihomo 客户端静默回落 dest。已改 `atLeast(v,1,8,2)` 逐段比较，并加回归用例 |
| 2 | REST 错误直接 `%w` 包 `*url.Error` | `Error()` 打印完整 URL（含 `?token=`），被 slog 原样落盘。已加 `redact()` |
| 3 | 机器模式只发 `machine_id` | 面板 V2 中间件除 handshake 外 `node_id` 必填 → config/user/report 全 422。已改为一节点一 runner，同时带两个参数 |
| 4 | users 的 304 覆盖了 ETag | 面板 304 不带 ETag → 隔轮全量拉取。已改为只在 200 时更新 |
| 5 | `base_config.pull_interval` 解析后不用 | 面板改节奏要重启节点。已接到 runner 的 ticker（下限 15s） |
| 6 | 闸门灌入晚于内核 Start/Reload | 窗口内新连接全吃 `ErrUnknownUser`（用户侧"刚切节点就断流"）。已反转顺序 |
| 7 | `bucketFor` 命中路径提前 return | 老用户长期在线时限速桶永不回收（上界=历史用户数×2）。已把清扫移到命中判断之前 |
| 8 | `GateRejects` 无递增点 | 指标恒为 0。已加 `gate.OnReject` 钩子 |
| 9 | `API.Listen` 不校验 | 空值 → `Addr=""` → bind 0.0.0.0:80 且 `/metrics` 无鉴权。已拒绝空值 |
| 10 | `allow_insecure` 静默丢弃 | 运维以为放宽了校验其实没生效。已改为构建期报错 |
| 11 | Makefile `$(MAKE) patches \| tail -1` | 吞掉补丁失败，`verify` 仍报 ok。已去掉管道 |

**已确认无绕过**

- MPTCP：唯一 inbound 路径无条件 `SocketConfig{TcpMptcp:false}`，ws/grpc/httpupgrade/xhttp 共用同一份 `sc`；全仓无 raw JSON 直通（`buildInbound` 只放协议 settings，outbound 只放 freedom/blackhole）→ CVE-2026-98432 的触发面在内核侧已关闭。
- 补丁资源：`Dispatch`/`DispatchLink` 两条失败路径都 `usergate.Close()`；`rateReader` 实现了 `TimeoutReader` 且 `Limit` 在断言之前。
- 补丁面积极小：只改上游 `app/dispatcher/default.go` +14/−0，冲突点 `default.go:287-294` 与 `:349-355`。

**仍未做（已知、明确）**

1. `Admit` 对空 `email` 放行 —— 匿名入站（socks/http）没有身份可比，这是设计约束；**副作用**：vless 的 `fallbacks` 转发进来的连接不带 email，会绕过设备数/限速。
2. `metrics.ActiveConns` 无递增点（当前恒 0，`report` 里改用在线数）。
3. ECH 只解析不使用。
4. PHP 侧补丁未 `php -l`，也未与真实面板联调。
5. `sync.devices` 只记 debug：设备状态由本节点 gate 按连接自管，回灌会和自见冲突（面板侧那份用于它自己的展示）。

## 6. WS 推送（M2 完成）

面板 `ws-server`（Workerman，默认 8076）存在，`handshake` 会回 `{websocket:{enabled,ws_url}}`。节点侧已实现完整客户端：

| 方向 | 事件 | 处理 |
| --- | --- | --- |
| 面板 → 节点 | `auth.success` | 记 debug |
| | `sync.config` | `AdoptConfig` 解成 `NodeConfig` → `apply(..., changed=true)`，按 `finger()` 决定重建还是热更 |
| | `sync.users` | 全量替换，走 `apply()` 的 diff 分支 |
| | `sync.user.delta`（`action=add/update/remove`） | `applyDelta()`：只动受影响用户，uuid/限额缺失时用本地快照补齐 |
| | `sync.devices` | 忽略（见未做 5） |
| | `sync.nodes`（机器模式） | supervisor 增删 runner |
| | `ping` | 回 `{"event":"pong"}` |
| | `error` | 返回错误 → 重连 |
| 节点 → 面板 | `pong` | 心跳 |
| | 帧级 `ping` | 客户端每 30s 主动探活（面板 55s 才 ping 一次，读超时 90s） |

- 自研最小 RFC6455 客户端（`internal/wsclient`，250 行）：校验 `Sec-WebSocket-Accept`、帧掩码、控制帧不分片、分片重组、16MB 上限；不引第三方依赖。
- 推送在线时轮询退化为 **300s 兜底**（防"事件丢了"），掉线立刻回到面板配置的 `pull_interval`。重连指数退避 2s→30s，连满 30s 后退避归零。
- 连接建立后立刻补一次 `pullOnce`，避免"连上了但推送刚好丢了"要等 300s。
- 自签证书：`Node.WsInsecure` / `Machine.WsInsecure` 单点开关，不做全局 `InsecureSkipVerify`——配置下发链路里带 token。

## 7. sing-box 内核（anytls / tuic / hysteria2）

xray 上游没有 anytls/tuic/hysteria2（只有 wyx2685 fork 有）。我们按 ADR-2 用 **子进程** 接 sing-box，
而不是把 sing-box 作为库引进来：协议转换这件事不值得一棵几十个包的依赖树。

| 项 | 实现 |
| --- | --- |
| 协议 | `anytls` / `tuic` / `hysteria2`（`hysteria` 别名按 v2 参数集处理） |
| 配置生成 | `internal/kernel/singbox/build.go`：inbound + 直连 outbound，不写 route（分流在面板侧） |
| 用户字段 | `name = id.uuid`（和 xray 的 email 同格式，排障能直接对上人） |
| TLS | 必须：`cert_config.cert_file/key_file` 或 `tls_settings`；缺了直接报错，不起空监听 |
| tuic 默认 | `congestion_control=bbr`、`auth_timeout=3s`、`zero_rtt_handshake=false`（面板没下发时补默认，否则 sing-box 拒整份 config） |
| hysteria2 | `up_mbps/down_mbps`；`obfs` 只认 `salamander`，认不出的名字整段丢掉（写进去会拒 config） |
| 用户变更 | **重启进程**（sing-box 无热更用户的 API）；相同批次的重复推送不触发重启 |
| 就绪判定 | 启动后按 TCP 拨号确认端口真的在听，超时则收掉进程并回带日志尾巴 |
| 可执行文件 | `Kernel.SingboxBinary`（默认 `sing-box`）、`Kernel.SingboxDir`（默认 `run/singbox`） |

**已知缺口（必须记住）**：设备数限制与限速挂在 xray 的 dispatcher 上（`app/usergate`）。sing-box 是子进程，
没有等价注入点 → **这三类协议上闸门不生效**，进程启动时会对每个节点打一条 WARN。
需要设备数/限速的节点请用 vless/vmess/trojan/ss2022（xray 侧）。


## 8. 端到端联调（Windows 本机，2026-11-01）

本机没有 WSL（`wsl.exe -l -v` 报未安装，装它要管理员+重启），因此用原生 Windows 栈搭了一套**真面板**：

- 便携 PHP 8.4.27 NTS（`D:\DSH\_tools\php`）+ composer 2.11 + `predis` + SQLite + 本机 Redis；
- 面板：`cedar2025/Xboard` master `e68fec5` 全新克隆，`git apply` 我们的补丁 **CLEAN**，`find app -name '*.php' | xargs php -l` 零语法错误；
- 面板服务：`php artisan serve --host=127.0.0.1 --port=8099`；节点：`xboard-core.exe -config config.yaml` 直连。

**已实测通过**

| 项 | 证据 |
| --- | --- |
| V2 鉴权 | 无 token 请求 `/api/v2/server/config` → 422（`ServerV2` 中间件生效） |
| config 扁平契约 | 节点按 `protocol/network/networkSettings/tls_settings/server_port` 建出 inbound；面板侧 `protocol_settings` 白名单投射可见 |
| users `{users:[...]}` | 节点日志 `users=1`，metrics 回读 `total_users=1` |
| ETag/304 | 第二轮拉取走 304，未重复全量 |
| 内核启动 | `kernel started kernel=xray protocol=vless tag=node-1 network=tcp port=4443 users=1`，`netstat` 见 `0.0.0.0:4443 LISTENING` |
| **report 五合一** | 面板 cache 回读 `SERVER_VLESS_LAST_CHECK_AT_1`、`SERVER_VLESS_LOAD_STATUS_1`（含 `kernel_status:true`）、`SERVER_VLESS_METRICS_1`（`uptime/goroutines/total_users`）全部落库 |
| 节点负载 | `LOAD_STATUS` 里出现真实数值（CPU%、内存、磁盘），不再是占位 |

**E2E 才发现并已修的真 bug**

1. `xray: build instance: common: *proxyman.InboundConfig is not registered` —— 只 import 数据类型不会注册处理器，补 `internal/kernel/xray/distro.go` 空导入 `main/distro/all`；
2. xray 日志级别被 `logConfig("")` 写死，补 `xray.SetLogLevel(cfg.Log.Level)`；
3. 面板挂 PHP-FPM/nginx 时 keep-alive 复用会吃 RST（`wsarecv: An existing connection was forcibly closed`），resty 自带重试不覆盖，`internal/panel/client.go` 新增连接层重试 + 命中后转短连接；
4. Windows 上 `sysinfo` 原来返回占位值（内存 1 字节），新增 `internal/sysinfo/sysinfo_windows.go`，直接调 kernel32/psapi 取 CPU/内存/提交量/磁盘。

**未通过 / 受限（如实记录）**

- **数据面**：经本节点（vless:4443）与上游纯净二进制（v26.10.31 `a4a3dff`、v26.9.30）分别测 vless/vmess/ss2022，客户端 socks 入口均失败：vless 报 `failed to decode response header > unexpected response version. Expecting 0 but actually 72`（72='H'），vmess/ss2022 超时；同一客户端二进制的**纯 socks 入站**正常（`socks:200`）。服务端从未出现 `accepted tcp:...` 的 dispatch 日志。→ 判定为**本机 Windows 环境**（回环 + 本地安全软件/代理拦截）导致，与 usergate 补丁无关，需在 Linux 上复验。
- **WS 推送**：面板侧 Workerman 需要 `ext-pcntl`，Windows 版 PHP 没有，`ws-server` 起不来；`sync.*` 事件只能到 Linux 上联调。
- 设备数闸门、限速、traffic（u/d）入库：都依赖真实流量，随之顺延到 Linux。

### 8.1 数据面失败的现场判据（2026-11-01，Windows）

为排除本项目自身代码，做了三组对照：

1. **纯净上游二进制**：v26.10.31（`a4a3dff`）与 v26.9.30 各跑一遍 vless/vmess/ss2022，症状与我们的构建一致（'H' / 超时）。
2. **同二进制纯 socks 入站**：正常（`socks:200`），说明 transport/socket 层没问题。
3. **手写 VLESS 客户端**（不经 xray 客户端）：直接向本节点 4443 发
   `00 | uuid(16) | 00 | 01 | port(2) | atyp=01 | 127.0.0.1` + HTTP 请求，
   **收到的不是 VLESS 响应头，而是 `HTTP/1.1 400 Bad request\r\nConnection: close\r\nContent-Type: text/html…`**；
   同一脚本重跑一次却变成超时无响应。xray 的 vless 入站不可能回 HTTP 400。

→ 结论：这台机器上**回环连接被本机的安全软件/网络过滤层介入**（进程列表只有 Defender，WinHTTP 直连，但这正是典型的企业/安全软件 loopback 拦截表现：非 HTTP 流量被塞 HTTP 错误页或直接丢弃）。因此数据面（真实流量、traffic 入库、设备数闸门、限速）**不能在本机验证**，必须搬到 Linux。

已备好一把梭脚本：`scripts/e2e/linux-e2e.sh`（204 行，`bash -n` 通过），一条命令覆盖 10 项断言：

```bash
sudo bash scripts/e2e/linux-e2e.sh        # 加 --keep 保留上一轮现场
```

它做的事：装 PHP 扩展/Redis → 克隆面板并打我们的补丁 → 建 sqlite 库并跑迁移 → 造夹具（token/用户/节点）→ 起面板 + 起节点 → 起源站与 xray 客户端 → 跑真实流量 → 断言 u/d 增长 → 回读面板 cache 的 `LAST_CHECK_AT/LOAD_STATUS/METRICS` → 未知 UUID 被拒 → WS 推送（有 pcntl 才测）。失败自动 `tail` 全部日志。
