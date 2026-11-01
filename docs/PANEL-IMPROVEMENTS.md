# Xboard 面板适用性优化清单

针对 `cedar2025/Xboard`（master `e68fec50`，2026-10-29，无 release）。每条给出落点、现状证据、建议改动、验收标准。

## P0-1 传输收敛：停发 h2/http/h3

**现状**：`app/Protocols/ClashMeta.php:37` 把 `'h2' => '0.0.0'` 当"全版本可用"，h2 在 `:381/:476/:565` 三处被写进订阅；`app/Protocols/General.php:123/212/290` 同样。而 Xray-core v26.x 已删除 h2/http/h3 传输（`infra/conf/` 的 `PrintRemovedFeatureError`），节点侧构建会直接失败。

**改动**：
- `ClashMeta.php` / `General.php`：把 `h2`、`http` 映射改成不可选（从 `$this->protocols` 与版本 map 里移除），`xhttp` 升为默认 stream；
- 保留存量节点的读取兼容（`if ($network === 'h2') { $network = 'xhttp'; }` 在服务端做一次迁移映射），但**订阅输出不再出现 h2**；
- `SingBox.php:45/53/61` 已经是 `'xhttp' => '9999.0.0'`（视为不支持），确认 SingBox 侧确实要挡；若 sing-box 1.14+ 已支持 xhttp 则应改真值，否则维持并加注释。

**验收**：面板里新建节点时 UI 不再提供 h2；老节点发布订阅时输出 xhttp；节点后端 `xray.Build` 不再收到 h2。

## P0-2 REALITY 字段补齐

**现状**：`app/Services/ServerService.php:295-300` 的 vless 分支只下发 `tls_settings`（`reality_settings` 直出），面板 php 全库无 `mldsa65`/`xver`。后端能拿到的只有 `{server_name, dest, short_ids, private_key}`。

**改动**（`app/Http/Requests/Admin/ServerSave.php` + `app/Models/Server.php:291` 的 protocol_settings 校验 + 服务端渲染）：
```php
// ServerSave.php 增加
'mldsa65Seed'     => ['nullable', 'string', 'size:64'],   // 32 字节 hex
'xver'            => ['nullable', 'integer', 'in:0,1,2'],
'maxTimeDiff'     => ['nullable', 'integer', 'min:0', 'max:600'],
'limitFallbackUp' => ['nullable', 'integer', 'min:0'],
'limitFallbackDown' => ['nullable', 'integer', 'min:0'],
```
并在 `ServerService::getServer()` 的 reality 分支把这些字段透传（键名与后端 `panel.TLSSettings` 一致：`mldsa65Seed`/`xver` 用字符串形式，`xver` 走 `,string`）。

**同时加的校验**（后端已前置校验，面板做一层 UX）：`short_ids` 非空、`dest` 必填、`private_key` 长度合法。

**验收**：面板保存 REALITY 节点后可看到 mldsa65/xver 输入框；节点 config 响应里出现这两个键；后端构建出的 REALITY 配置含 `mldsa65Seed`。

## P0-3 minClientVer 不要默认下发（若要下发则必须显式开关）

**现状**：面板不生成 `minClientVer`（全库零命中），这是**对的**。上游默认值在 v26.7.11/v26.7.28 短暂生效后被注释掉（`infra/conf/transport_security.go:103-119`），因为 mihomo 系客户端在 ClientHello 里自报 `1.8.2`（`component/tls/reality.go:74-76`），设 ≥26.x 会把 FlClash / Clash Verge Rev / Nikki 全部打回 `dest`——表现为"能握手、立刻断"，极难排查。

**改动**：在节点 REALITY 面板加一个默认关闭的开关 `minClientVer`（空 = 不下发），旁边写明后果；一旦下发，`/status` 上报里带上该值，便于事后定位"某批用户连不上"。

**验收**：默认关闭；开启后有明显警告文案；后端 `buildReality` 保持不写该字段（由显式配置驱动）。

## P1-1 VLESS encryption 从"单字段"升级为"设置对象"

**现状**：面板只有 `encryption.enabled` + `encryption.decryption`（`ServerService.php:300-303`），一次只能表达一个字符串。上游 v26.x 与 v2node 的模型是 `{mode, ticket, server_padding, private_key}`（`mlkem768x25519plus` 需要这些参数）。

**改动**：`protocol_settings.encryption` 增加子键 `mode/ticket/server_padding/private_key`，`ServerService` 输出 `encryption_settings` 对象（保持 `decryption` 字段兼容旧节点）。

## P1-2 协议→内核路由变成数据

**现状**：后端靠协议名硬编码选择内核（anytls/tuic/hysteria2 → sing-box）。Xboard-Node 也是这么做的，结果是"想换内核要改代码"。

**改动**：`v2_server` 表加 `kernel_type`（`xray` / `singbox` / `auto`，默认 auto），`getServer()` 输出 `kernel_type`；后端 `Registry.For()` 优先按它选择，缺失时回落到协议映射。

## P1-3 契约版本号

**现状**：config 响应没有版本字段，面板与节点只能靠"字段存在与否"互相猜。

**改动**：`getServer()` 顶层加 `"contract_version": 2`；后端按版本解释字段（新增字段一律在版本内向后兼容追加）。

## P2-1 上报接口批量化与幂等

**现状**：`alive` 接受 `{alive: {uid: [ips]}}`；`push` 接受 `{data: [{uid,u,d}]}`。字段名是全称，payload 偏大。

**改动**：接受 `u/d` 短键（兼容全称）；`alive` 支持数组；两接口加 `X-Idempotency-Key`（节点侧用 (node_id, 轮次) 生成），弱网重试不去重计费。

## P2-2 节点健康与漂移可见

**现状**：面板有节点状态列，但不显示"内核版本"和"距上游最新版差多少"。

**改动**：`status` 上报里已带 `kernel`/`version`（本项目已实现），面板节点列表加两列：内核版本、构建时间；超过 N 天未更新则标黄。

## P2-3 `server_token` 加固

**现状**：`app/Http/Middleware/ServerV2.php` 用 `$value !== admin_setting('server_token')` 直接比较，单 token 无轮换、无最小长度约束。

**改动**：`admin_setting('server_token')` 生成时强制 ≥32 字节随机；支持 `server_token_secondary`（轮换期双 token 并存，`hash_equals` 比较）；`handshake` 加每 IP 限流。

## 优先级与成本

| 项 | 影响 | 成本 | 顺序 |
|---|---|---|---|
| P0-1 停发 h2 | 决定节点能否升到 v26.x 后正常起 | 小（2 文件 + 迁移映射） | 1 |
| P0-2 REALITY 字段补齐 | 解锁 mldsa65/xver/limitFallback | 中（表单+校验+渲染） | 2 |
| P0-3 minClientVer 开关 | 防止误下发打死 mihomo 全系 | 小 | 3 |
| P1-1 encryption 对象化 | 解锁 VLESS 抗量子加密 | 中 | 4 |
| P1-2 kernel_type | 免改代码换内核 | 小（迁移+1 字段） | 5 |
| P1-3 contract_version | 面板/节点解耦升级 | 小 | 6 |
| P2-1/2/3 | 运维可见性与安全 | 小 | 7 |
