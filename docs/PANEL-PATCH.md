# 面板侧补丁：REALITY 字段补齐 + 传输守卫

补丁文件：[`panel-patch/0001-reality-schema-and-transport-guard.patch`](../panel-patch/0001-reality-schema-and-transport-guard.patch)
目标仓库：`cedar2025/Xboard`（基线 master `e68fec5`，2026-10-29）
改动规模：3 文件，+64 / −2 行，**0 个迁移，0 次数据改写**

---

## 1. 为什么必须动面板

后端（`xboard-core`）要把 `minClientVer`、`mldsa65Seed`、`dest`、`short_ids`、`xver` 交给
Xray-core v26 的 REALITY 装配器，但面板会把它们**在读和写两个方向上都吃掉**。

`app/Models/Server.php:326-372`：

```php
private function castSettingsWithConfig(array $settings, array $configs): array
{
    $result = [];
    foreach ($configs as $key => $config) {          // ← 只遍历 schema 里声明的键
        $value = $settings[$key] ?? null;
        $result[$key] = $this->castValueWithConfig($value, $config);
    }
    return $result;
}
```

`getProtocolSettingsAttribute()` 与 `setProtocolSettingsAttribute()` 都走它，所以：
**常量 `REALITY_CONFIGURATION` 里没有的键，读出来是 null，写回去直接消失。**
这不是"后端能不能兼容"的问题，是字段根本到不了后端。

同时 `ServerService::buildNodeConfig()` 之前把 `reality_settings` **整对象透传**，
DB 里存的是 snake_case（`short_id` / `mldsa65_seed`），而 Xray-core 的 REALITY 配置是
camelCase（`shortIds` / `mldsa65Seed` / `minClientVer`），后端只能靠改名去猜。

---

## 2. 改了什么

### 2.1 `app/Models/Server.php` — 白名单加 8 个键（+10 行）

```php
'dest'          => ['type' => 'string',  'default' => null],
'server_names'  => ['type' => 'array',   'default' => null],
'short_ids'     => ['type' => 'array',   'default' => null],
'mldsa65_seed'  => ['type' => 'string',  'default' => null],
'xver'          => ['type' => 'integer', 'default' => 0],
'max_time_diff' => ['type' => 'integer', 'default' => null],
'min_client_ver'=> ['type' => 'string',  'default' => null],
'max_client_ver'=> ['type' => 'string',  'default' => null],
```

老节点读出来的结果：这 8 个键全是 `null`（`xver` 是 `0`），**其它字段一字不变**。
`castValueWithConfig()` 只支持 `integer / boolean / string / array / object` 五种类型，
这里刻意只用其中四种，不引入新语义。

### 2.2 `app/Http/Requests/Admin/ServerSave.php` — 校验（+16 行）

```php
'reality_settings.mldsa65_seed'    => 'nullable|string|size:43',      // 32B base64url
'reality_settings.xver'            => 'nullable|integer|in:0,1,2',
'reality_settings.min_client_ver'  => ['nullable','string','regex:/^\d{1,3}(\.\d{1,3}){2}$/'],
```

版本号必须写全 `x.y.z`（Xray 只接受 3 段，写 `26.3` 会在内核启动时才炸）。

### 2.3 `app/Services/ServerService.php` — 归一化（+40 / −2 行）

新增 `realityConfig()`，把 snake_case 转成后端与 Xray-core 都认的 camelCase，
并强制 `minClientVer / maxClientVer` 缺省为 `null`：

```php
'minClientVer' => $reality['min_client_ver'] ?? null,
'maxClientVer' => $reality['max_client_ver'] ?? null,
```

`trojan` 与 `vless` 两个分支的 `tls_settings` 从"整对象透传"改为 `self::realityConfig(...)`。

> 为什么默认必须是 null：上游 v26.7.11 / v26.7.28 曾把默认值硬编码成 `26.3.27`，
> 结果 3x-ui 与一大批 mihomo 系客户端（Clash Verge Rev / FlClash / OpenWrt-nikki，
> 都自报 `1.8.2`）连不上，现象是**静默回落到 dest**（看起来像"连上了但没网"）。
> 见 `XTLS/Xray-core#6482`、`MHSanaei/3x-ui#5922`。

### 2.4 传输守卫（可选，默认关闭）

```php
if (config('xboard.strict_transport', false) && in_array($type, ['vmess','trojan','vless'], true)) {
    $rules['protocol_settings.network'] = 'required|string|in:tcp,ws,grpc,httpupgrade,xhttp,splithttp';
}
```

Xray-core v26 已删除 `h2 / http / h3 / quic` 传输（`infra/conf/transport_internet.go:35-38`
直接 `PrintRemovedFeatureError`）。**默认不开启**，因为开启后旧节点在保存时会被拦下要求改传输。
想在新节点上就挡住误配，再加一行 `.env`：

```dotenv
XBOARD_STRICT_TRANSPORT=true
```

---

## 3. 为什么这是"非破坏性"的

| 关注点 | 结论 | 依据 |
| --- | --- | --- |
| 数据库迁移 | **不需要** | `protocol_settings` 是 JSON 列（`2025_01_05_131425_create_v2_server_table.php:28`），字段进 schema 常量即可落盘 |
| 既有节点数据 | 不改写 | 补丁只增加"读时补默认值"，`setProtocolSettingsAttribute` 只在保存该节点时触发 |
| 旧版面板读新数据 | 不炸，但会丢 | 旧代码的 `castSettingsWithConfig` 会丢掉新键 —— 这就是**回滚必须趁早**的原因（见 §5） |
| 升级流程 | 不需要额外步骤 | `XboardUpdate::handle()` 只跑 `migrate --force`，本补丁没有迁移文件，等于零动作 |
| 客户端订阅 | 完全不变 | `app/Protocols/*.php` 一行没改，订阅产物与改动前逐字节一致 |
| 节点后端兼容 | 双向 | 老后端（Xboard-Node / v2node）读新字段会当作不认识的键忽略；新后端读老数据得到全 null 也能跑 |

改完之后 `buildNodeConfig` 的输出变化，只对 **REALITY 节点**且 **只在补丁后保存过一次** 的节点生效：

```diff
 "tls_settings": {
   "show": false,
   "dest": null,
   "server_name": "www.microsoft.com",
-  "short_id": "ab12"
+  "short_id": "ab12",
+  "short_ids": null,
+  "mldsa65Seed": null,
+  "xver": "0",
+  "maxTimeDiff": null,
+  "minClientVer": null,
+  "maxClientVer": null
 }
```

新增键全为 null 时，后端与 Xray-core 的行为与之前**完全一致**（不设门禁、不用 ML-DSA、xver=0）。

---

## 4. 应用

```bash
cd /path/to/Xboard            # 面板仓库根目录
git checkout -b xboard-core-panel
git apply --3way /path/to/xboard-core/panel-patch/0001-reality-schema-and-transport-guard.patch
php artisan optimize:clear     # 配置/视图缓存（有 horizon 的机器再 php artisan horizon:terminate）
```

Docker 部署则重新构建镜像（`docker compose up -d --build xboard`），因为这是 PHP 源码改动，
不是数据库改动 —— 不要指望 `php artisan xboard:update` 帮你带上它。

装完验证：

```bash
# 1) 语法（容器内）
docker compose exec xboard php -l app/Services/ServerService.php

# 2) 老 REALITY 节点仍能下发（字段应为 null，不是 500）
curl -s "https://PANEL/api/v2/server/config?node_id=NODE_ID&token=TOKEN" | jq '.tls_settings'

# 3) 面板后台 Save 一次该节点，DB 里应出现新键
docker compose exec mysql mysql -e \
  "select json_extract(protocol_settings,'$.reality_settings') from v2_server where id=NODE_ID"
```

---

## 5. 回滚

```bash
cd /path/to/Xboard
git apply -R /path/to/xboard-core/panel-patch/0001-reality-schema-and-transport-guard.patch
php artisan optimize:clear
```

回滚后 DB 里已有的 `dest / mldsa65_seed / min_client_ver …` 仍留在 JSON 列里，
但**下一次在后台保存该节点时会被旧代码丢弃**（白名单投射）。想保住这些值：

```bash
# 回滚前先导出，回滚后再按需手工写回
docker compose exec mysql mysqldump -t xboard v2_server \
  --where="json_extract(protocol_settings,'$.reality_settings.dest') is not null" > v2_server_reality_backup.sql
```

这是唯一真正需要提醒的破坏性风险，且只在"先打补丁、填了新字段、又回滚"这条路径上发生。

---

## 6. 还没做、但建议做的（P1）

| 项 | 原因 |
| --- | --- |
| `v2_server.kernel_type` 列（`xray` / `singbox` / `auto`） | 现在节点协议 → 内核是后端按 `Supported()` 猜的；面板显式声明才不会给 anytls 节点派错内核 |
| `contract_version` 字段 | 后端据此决定要不要兼容老字段名，避免以后每次改契约都要同步两端 |
| 上报短键 + 幂等键 | `report` 一次带四类数据，重放风险靠幂等键消掉 |
| `server_token` ≥32 字节 + 双 token 轮换 | 现在单 token 明文比对（`ServerV2.php`），轮换必须停所有节点 |
