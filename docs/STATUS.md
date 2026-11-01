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
2. WS `sync.*` 客户端未实现（`main.go:handshake` 只探测），配置/用户变动仍靠 ≤60s 轮询。
3. 机器模式的 `sync.nodes`、`sync.devices` 事件未消费。
4. `metrics.ActiveConns` 无递增点（当前恒 0，`report` 里改用在线数）。
5. ECH 只解析不使用。
6. PHP 侧补丁未 `php -l`，也未与真实面板联调。

