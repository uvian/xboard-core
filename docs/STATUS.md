# 现状清单：已实现 / 未实现 / 怎么验

一句话：**契约层已经与 `cedar2025/Xboard` master（`e68fec5`）对齐到源码级；代码尚未编译过**
（本机没有 Go 工具链，也没有 PHP CLI），所以下面每一行都属于"读出来的确定"与"跑出来的未确定"两类之一。

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
| **编译未验证** | 可能存在类型/签名错误 | 一有 Go 环境立刻 `make verify`（见 §3） |
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

```bash
# 0) 只看契约，不需要环境：docs/CONTRACT.md §9 的四条 curl
# 1) 有 Go 时（第一次构建）
make vendor          # 拉 Xray-core v26.10.31 到 third_party/xray
make patches         # git am --3way 应用 0001-usergate.patch
make verify          # gofmt + go vet + go test + 补丁 dry-run
make smoke           # go run ./cmd/xboard-core -smoke（合成 vless/tcp 配置，打印 inbound/app 字节数）
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
