# 节点后端 ↔ Xboard 面板 契约对照表

基线：`cedar2025/Xboard` master `e68fec5`（2026-10-29）、`xboard-core` 当前工作树。
本表每一行都是**源码核对**结论，不是推测；行号以基线 commit 为准。

---

## 1. 端点

| 方法 | 路径 | 面板实现 | 本后端调用点 |
| --- | --- | --- | --- |
| GET/POST | `/api/v2/server/handshake` | `V2\ServerController::handshake` | `client.Handshake`（仅探 WS 推送，失败即纯轮询） |
| GET | `/api/v2/server/config` | `V1\Server\UniProxyController::config` | `client.Config` |
| GET | `/api/v1/server/UniProxy/config` | 同上（同控制器） | 兼容老面板，逻辑一致 |
| GET | `/api/v2/server/user` | `UniProxyController::user` | `client.Users` |
| POST | `/api/v2/server/report` | `V2\ServerController::report` | `client.Report`（四合一） |
| POST | `/api/v2/server/push` | `UniProxyController::push` | 未使用（report 覆盖） |
| POST | `/api/v2/server/alive` | `UniProxyController::alive` | 未使用（report 覆盖） |
| POST | `/api/v2/server/status` | `UniProxyController::status` | 未使用（report 覆盖） |
| GET | `/api/v2/server/alivelist` | `UniProxyController::alivelist` | 未使用（设备数以本地 gate 为准） |

V2 路由在 `app/Http/Routes/V2/ServerRoute.php`，中间件 `server.v2`。

---

## 2. 鉴权

`app/Http/Middleware/ServerV2.php`：

- 带 `machine_id` → 机器模式（`server/machine/*`），本后端 `machine.enabled` 走这条；
- 否则 `token` 必须**严格等于** `admin_setting('server_token')`；
- `node_id` 除 `*/server/handshake` 外必填；
- 面板**不看** `node_type`（v2node 带的那个参数是历史遗留），所以本后端 `query()` 不发它。

请求参数构造见 `internal/panel/client.go` 的 `query()`：machine 模式发 `machine_id`+`token`，
节点模式发 `node_id`+`token`。

---

## 3. `config` 响应：扁平，无 `common` 包装

`UniProxyController::config()`（`app/Http/Controllers/V1/Server/UniProxyController.php:57-71`）：

```php
$response = ServerService::buildNodeConfig($node);
$response['base_config'] = ['push_interval' => ..., 'pull_interval' => ...];
$eTag = sha1(json_encode($response));
if (str_contains($request->header('If-None-Match',''), $eTag)) return response(null, 304);
return response($response)->header('ETag', "\"{$eTag}\"");
```

要点：

1. **没有 `common` 层**，`protocol / listen_ip / server_port / network / networkSettings` 都在顶层；
2. `base_config` 由控制器补，`push_interval` 决定上报节奏；
3. 304 判定是 `str_contains`，所以客户端必须把整个 `ETag`（带引号）原样回传；
4. `networkSettings` 是 **camelCase**（`ServerService.php:251-264`），不是 `network_settings`。

对应 Go：`internal/panel/types.go` 的 `NodeConfig`（`NetworkSettings json:"networkSettings"`）、
`client.Config` 返回 `(node, notModified, err)`。

---

## 4. 协议字段

`ServerService::buildNodeConfig()`（`app/Services/ServerService.php:266-392`）：

| 协议 | 关键字段 | 备注 |
| --- | --- | --- |
| vless | `tls`(int) `flow` `decryption` `tls_settings` `multiplex` | `decryption` = `encryption.enabled ? encryption.decryption : null`；`tls==2` 时 `tls_settings` 的值其实是 `reality_settings` |
| vmess | `tls` `tls_settings` | 同上 tls==2 规则 |
| trojan | `host` `server_name` `multiplex` `tls` `tls_settings` | 同上 tls==2 规则 |
| shadowsocks | `cipher` `plugin` `plugin_opts` `server_key` | 2022 口令在 `server_key` |
| hysteria / tuic / anytls | `tls_settings` 是**内层 tls 对象**（形状与 vmess 的不同） | `up_mbps`/`down_mbps`、`congestion_control`、`zero_rtt_handshake`、`padding_scheme` |
| 全部 | `routes` `custom_outbounds` `custom_routes` `cert_config` | `cert_config` 仅当 `cert_mode !== 'none'` 才下发，且已做 `mode` → `cert_mode` 归一 |

本后端只声明 `vless / vmess / trojan / shadowsocks`（`internal/kernel/xray/node.go` 的 `Supported()`），
其余协议交给 sing-box 内核（M2），够不到的会明确报 `not handled by the xray kernel`。

---

## 5. `user` 响应：有包装

```php
$response['users'] = ServerService::getAvailableUsers($node);   // {"users":[...]}
```

字段：`id / uuid / speed_limit / device_limit`（`select(['id','uuid','speed_limit','device_limit'])`），
过滤 `u+d < transfer_enable`、`expired_at >= now() or null`、`banned = 0`，
并经过 `HookManager::filter('server.users.get', …)`。

对应 Go：`panel.UsersResponse{Users []UserInfo}`；`client.Users` 用 `X-Response-Format: msgpack`，
返回 `nil, nil` 表示 304（沿用上次快照）。

> 这意味着**可用用户集合由面板裁好**，后端不需要自己判断过期/流量超限；
> 后端 gate 只做设备数与限速，不重复做封禁逻辑（否则两边阈值不一致时会出现"面板说能用、节点说不能"）。

---

## 6. 上报：`report` 是四合一

`V2\ServerController::report` 一次性接收 `traffic` / `alive` / `online` / `status` / `metrics`，
各自 `is_array && !empty` 才处理。

| 键 | 形状 | 面板处理 | 本后端 |
| --- | --- | --- | --- |
| `traffic` | `{uid: [up, down]}` | `count($item)===2 && is_numeric(...)`，否则跳过 | `traffic.Collector.Drain() map[int][2]int64` |
| `alive` | `{uid: ["ip", ...]}` | 遍历顶层 key，**没有 `alive` 包装层** | `gate.Online(tag)` |
| `online` | `{uid: 连接数}` | 存 device state | `onlineCount()`（按 IP 去重后的活跃连接） |
| `status` | 见下 | 校验失败直接 422 | `sysinfo.Stat()` |
| `metrics` | 见下 | 可选 | `metrics` 快照 |

`status` 的必填项（`V2\ServerController::report` 的 `validate`）——**少一个就整包 422**：

```
cpu, mem.total, mem.used, swap.total, swap.used, disk.total, disk.used
```

所以 `internal/sysinfo` 的三份实现（linux / other / 接口）都保证这七项恒有值，
非 Linux 平台给满足校验的最小值，绝不省略键。

`metrics` 接受的长键：`uptime, goroutines, active_connections, total_connections, total_users,
active_users, inbound_speed, outbound_speed, cpu_per_core, load, speed_limiter, gc, api, ws, limits, kernel_status`。

---

## 7. WS 推送（存在，但本后端暂不依赖）

`app/Services/NodeSyncService.php`：

- `isNodeOnline()` 读缓存 `node_ws_alive:{id}`；
- `notifyConfigUpdated` → `sync.config {config}`、`notifyUsersUpdatedByGroup` → `sync.users {users}`、
  `notifyUserChanged` → `sync.user.delta {action, users}`；`app/WebSocket/NodeEventHandlers.php` 另有 `sync.devices`；
- `handshake` 返回 `{websocket:{enabled, ws_url}}`，要求 `server_ws_enable` 且 worker 心跳存在。

本后端 `client.Handshake` 只做能力探测：拿到 `enabled` 就记一笔日志，`TODO(M2)` 再接 `sync.*`。
在接之前**必须保留轮询**，否则 WS 断线时节点会停在旧配置上——这是比"省一次请求"严重得多的问题。

---

## 8. 已知缺口（面板侧尚未提供）

| 缺口 | 影响 | 现状 |
| --- | --- | --- |
| `reality_settings` 只有 6 个键 | `dest / short_ids / mldsa65Seed / xver` 等下发不了 | 已由 `panel-patch/0001-…` 补上（见 `docs/PANEL-PATCH.md`） |
| 无 `kernel_type` 列 | 一个节点用哪个内核只能后端 `Supported()` 猜 | 建议 P1 加列 |
| 无 `contract_version` | 契约演进靠字段名兼容 | 建议 P1 加列 |
| 无 `server.config` / `server.save` hook | 插件系统无法注入节点字段，面板改动只能是核心补丁 | 已在 `docs/PANEL-PATCH.md` §6 说明 |

---

## 9. 最小化验证（无 Go / 无 PHP 环境也能跑）

```bash
# A. 面板侧：老节点配置在补丁后仍可下发（不 500），且新增键存在
curl -s "https://PANEL/api/v2/server/config?node_id=1&token=$TOKEN" | jq '{protocol, network, networkSettings, tls, tls_settings}'

# B. 304 语义：拿上一步的 ETag 再请求一次，应为 304 + 空 body
ET=$(curl -s -D- -o /dev/null "https://PANEL/api/v2/server/config?node_id=1&token=$TOKEN" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
curl -s -o /dev/null -w '%{http_code}\n' -H "If-None-Match: $ET" "https://PANEL/api/v2/server/config?node_id=1&token=$TOKEN"

# C. 用户列表形状
curl -s -H 'X-Response-Format: msgpack' "https://PANEL/api/v1/server/UniProxy/user?node_id=1&token=$TOKEN" | jq '.users | length'

# D. status 必填项（缺一项就 422，用这个反推面板版本行为）
curl -s -X POST "https://PANEL/api/v2/server/report?node_id=1&token=$TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"status":{"cpu":1.5,"mem":{"total":1,"used":1},"swap":{"total":0,"used":0},"disk":{"total":1,"used":1}}}' -w '\n%{http_code}\n'
```

D 返回 200 说明七项齐全；返回 422 且指出缺失键，说明面板版本比本表更新，需要同步 `sysinfo`。
