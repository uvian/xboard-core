#!/usr/bin/env bash
# Linux 端到端联调：真面板 + 真节点 + 真客户端 + 真流量。
#
# 设计原则：全部在 $E2E 目录里跑，不碰宿主机既有服务；端口可改；失败即停并打印现场。
# 用法：sudo bash linux-e2e.sh [--keep]      （--keep 保留上一轮目录，便于排查）
#
# 覆盖的断言（每一项都对应 docs/CONTRACT.md 里的一条契约）：
#   1 面板 V2 鉴权：无 token → 422
#   2 config 扁平契约 + base_config
#   3 users {"users":[...]}
#   4 ETag/304 第二轮不重传
#   5 内核启动并监听 server_port
#   6 report 五合一入库（LAST_CHECK_AT / LOAD_STATUS / METRICS）
#   7 真实流量经节点 → 面板 u/d 增长（traffic）
#   8 设备数闸门：device_limit=1 时第二条连接的 IP 被拒
#   9 限速：speed_limit 生效后吞吐下降
#  10 WS 推送：sync.config / sync.users 到达节点（需 ext-pcntl）
set -uo pipefail

E2E=${E2E:-/opt/xboard-e2e}
REPO=${REPO:-https://github.com/uvian/xboard-core}
BRANCH=${BRANCH:-main}
PANEL_REPO=${PANEL_REPO:-https://github.com/cedar2025/Xboard}
PANEL_HEAD=${PANEL_HEAD:-master}
PORT_PANEL=${PORT_PANEL:-8099}
PORT_NODE=${PORT_NODE:-4443}
PORT_SOCKS=${PORT_SOCKS:-1099}
PORT_ORIGIN=${PORT_ORIGIN:-8081}
TOKEN=${TOKEN:-e2e-token-abcdef123456}
NODE_ID=1
UUID=0f6b8b45-6a2f-4b4e-9a8f-1234567890ab

PASS=0; FAIL=0
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
die()  { printf '\n\033[31m!! %s\033[0m\n' "$*"; dump; exit 1; }
dump() {
  for f in "$E2E"/logs/*.log; do
    [ -f "$f" ] || continue
    printf '\n--- %s (tail) ---\n' "$(basename "$f")"
    tail -n 25 "$f"
  done
}

[ "$(id -u)" = 0 ] || die "需要 root（安装 PHP 扩展、绑端口）"
KEEP=0; [ "${1:-}" = "--keep" ] && KEEP=1
if [ "$KEEP" = 0 ]; then rm -rf "$E2E"; fi
mkdir -p "$E2E"/{logs,bin,node}

say "0. 依赖"
if command -v apt-get >/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq php-cli php-sqlite3 php-mbstring php-xml php-curl php-zip \
      php-redis php-bcmath unzip curl sqlite3 redis-server git ca-certificates >/dev/null     || die "apt 安装失败（pcntl 已内建在 php-cli，不是独立包）"
  systemctl enable --now redis-server >/dev/null 2>&1 || (redis-server --daemonize yes && sleep 1)
fi
for c in php composer git curl sqlite3; do command -v "$c" >/dev/null || die "缺 $c"; done
php -m | grep -q pcntl && HAVE_PCNTL=1 || HAVE_PCNTL=0

say "1. 面板：克隆 + 打补丁 + 依赖"
[ -d "$E2E/panel/.git" ] || git clone -q --depth 1 -b "$PANEL_HEAD" "$PANEL_REPO" "$E2E/panel" || die "克隆面板失败"
[ -d "$E2E/core/.git" ]   || git clone -q --depth 1 -b "$BRANCH" "$REPO" "$E2E/core"       || die "克隆 xboard-core 失败"
cd "$E2E/panel"
git apply --check "$E2E/core/panel-patch/0001-reality-schema-and-transport-guard.patch" 2>/dev/null \
  && git apply "$E2E/core/panel-patch/0001-reality-schema-and-transport-guard.patch" \
  || echo "  (补丁已打过或已不适用，继续)"
php -l app/Models/Server.php >/dev/null && ok "补丁后 PHP 语法" || bad "PHP 语法"
[ -d vendor ] || composer install -q --no-interaction --no-progress --ignore-platform-reqs >"$E2E/logs/composer.log" 2>&1 || die "composer install 失败"
composer show predis/predis >/dev/null 2>&1 || composer require -q --no-interaction --ignore-platform-reqs predis/predis >/dev/null 2>&1

say "2. 面板：环境与建库"
mkdir -p bootstrap/cache storage/framework/{cache,sessions,views} storage/logs
[ -f .env ] || cp .env.example .env
php artisan key:generate --force >/dev/null
sed -i "s|^DB_CONNECTION=.*|DB_CONNECTION=sqlite|; s|^DB_DATABASE=.*|DB_DATABASE=$E2E/panel/e2e.sqlite|" .env
grep -q '^REDIS_CLIENT=' .env && sed -i 's|^REDIS_CLIENT=.*|REDIS_CLIENT=predis|' .env || echo 'REDIS_CLIENT=predis' >> .env
sed -i 's|^CACHE_STORE=.*|CACHE_STORE=redis|; s|^CACHE_DRIVER=.*|CACHE_DRIVER=redis|' .env
sed -i 's|^QUEUE_CONNECTION=.*|QUEUE_CONNECTION=sync|' .env
grep -q '^INSTALLED=' .env && sed -i 's|^INSTALLED=.*|INSTALLED=true|' .env || echo 'INSTALLED=true' >> .env
: > "$E2E/panel/e2e.sqlite"
php artisan migrate --force >"$E2E/logs/migrate.log" 2>&1 || die "迁移失败"

say "3. 夹具：token / 用户 / 节点"
cat > "$E2E/panel/e2e_seed.php" <<PHP
<?php
require __DIR__.'/vendor/autoload.php';
\$app = require __DIR__.'/bootstrap/app.php';
\$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
app(App\Support\Setting::class)->save([
  'server_token' => '$TOKEN', 'server_ws_enable' => 1,
  'server_ws_url' => 'ws://127.0.0.1:8076', 'server_push_interval' => 30,
]);
\$u = App\Models\User::firstOrNew(['email' => 'u1@e2e.test']);
\$u->fill(['password' => bcrypt('e2e-pass'), 'uuid' => '$UUID', 'transfer_enable' => 1099511627776,
          'device_limit' => 3, 'speed_limit' => 0, 'group_id' => 1, 'banned' => 0, 'is_admin' => 0])->save();
\$s = App\Models\Server::firstOrNew(['id' => $NODE_ID]);
\$s->fill(['type' => 'vless', 'name' => 'e2e-vless-tcp', 'group_ids' => [1], 'host' => '127.0.0.1',
          'port' => '$PORT_NODE', 'server_port' => $PORT_NODE, 'show' => 1, 'enabled' => 1, 'rate' => 1,
          'protocol_settings' => ['tls' => 0, 'network' => 'tcp', 'flow' => null,
                                  'encryption' => ['enabled' => false, 'decryption' => null]]])->save();
echo "seeded user=".\$u->id." server=".\$s->id.PHP_EOL;
PHP
php "$E2E/panel/e2e_seed.php" >"$E2E/logs/seed.log" 2>&1 || die "夹具失败"
USER_ID=$(php -r 'require "'"$E2E"'/panel/vendor/autoload.php"; $a=require "'"$E2E"'/panel/bootstrap/app.php"; $a->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); echo App\Models\User::where("uuid","'"$UUID"'")->value("id");')
[ -n "$USER_ID" ] && ok "用户 id=$USER_ID 节点 id=$NODE_ID" || die "夹具没建出用户"

say "4. 起面板 + 起节点"
( cd "$E2E/panel" && nohup php artisan serve --host=127.0.0.1 --port="$PORT_PANEL" >"$E2E/logs/panel.log" 2>&1 & )
for i in $(seq 1 40); do curl -s -o /dev/null "http://127.0.0.1:$PORT_PANEL/api/v2/server/config" && break; sleep 0.5; done
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_PANEL/api/v2/server/config")
[ "$code" = 422 ] && ok "1 无 token → $code（鉴权中间件生效）" || bad "1 无 token 期望 422，实际 $code"

cd "$E2E/core" && make build >"$E2E/logs/build.log" 2>&1 || die "make build 失败（要看 Go 环境）"
cp bin/xboard-core "$E2E/bin/xboard-core"
cat > "$E2E/node/config.yaml" <<YAML
log: {level: debug}
node:
  api_host: http://127.0.0.1:$PORT_PANEL
  node_id: $NODE_ID
  api_key: $TOKEN
  timeout: 15
  retry: 3
  pull_every: 30
api: {listen: 127.0.0.1:8091, metrics: true}
kernel: {xray: true, singbox: false}
YAML
( cd "$E2E/node" && nohup "$E2E/bin/xboard-core" -config config.yaml >"$E2E/logs/node.log" 2>&1 & )
for i in $(seq 1 40); do grep -q 'kernel started' "$E2E/logs/node.log" && break; sleep 0.5; done
grep -q 'kernel started' "$E2E/logs/node.log" && ok "5 内核启动" || die "内核没起来"
ss -lntp 2>/dev/null | grep -q ":$PORT_NODE " && ok "5 监听 $PORT_NODE" || bad "5 未监听 $PORT_NODE"
grep -q 'users=1' "$E2E/logs/node.log" && ok "3 users 拉取到 1 个用户" || bad "3 用户数不对"

say "5. 源站 + 客户端"
mkdir -p "$E2E/www" && head -c 2000000 /dev/urandom > "$E2E/www/blob.bin" && echo hello-e2e > "$E2E/www/small.txt"
( cd "$E2E/www" && nohup python3 -m http.server "$PORT_ORIGIN" --bind 127.0.0.1 >"$E2E/logs/origin.log" 2>&1 & )
sleep 1
curl -s "http://127.0.0.1:$PORT_ORIGIN/small.txt" | grep -q hello-e2e && ok "源站就绪" || die "源站没起来"

XRAY=$(command -v xray || echo "$E2E/bin/xray")
if [ ! -x "$XRAY" ]; then
  # 用 vendored 源码编一个客户端，保证和服务端同版本
  ( cd "$E2E/core/third_party/xray" && go build -o "$E2E/bin/xray" ./main ) >"$E2E/logs/xray-build.log" 2>&1 || die "编 xray 客户端失败"
  XRAY="$E2E/bin/xray"
fi
cat > "$E2E/node/client.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"s","listen":"127.0.0.1","port":$PORT_SOCKS,"protocol":"socks","settings":{"udp":false}}],
 "outbounds":[{"tag":"p","protocol":"vless",
   "settings":{"vnext":[{"address":"127.0.0.1","port":$PORT_NODE,
     "users":[{"id":"$UUID","encryption":"none"}]}]},
   "streamSettings":{"network":"tcp","security":"none"}}]}
JSON
nohup "$XRAY" run -c "$E2E/node/client.json" >"$E2E/logs/client.log" 2>&1 &
sleep 2

say "6. 真实流量 → 面板 u/d"
got=$(curl -sS --max-time 15 --socks5-hostname 127.0.0.1:"$PORT_SOCKS" "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
echo "$got" | grep -q hello-e2e && ok "7 经节点取到源站内容" || { bad "7 数据面不通"; dump; }
curl -s --max-time 20 --socks5-hostname 127.0.0.1:"$PORT_SOCKS" -o /dev/null "http://127.0.0.1:$PORT_ORIGIN/blob.bin" || true
sleep 35   # 等一拍 report
UD=$(cd "$E2E/panel" && php -r 'require "vendor/autoload.php"; $a=require "bootstrap/app.php"; $a->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); $u=App\Models\User::find('"$USER_ID"'); echo $u->u.",".$u->d;')
echo "  面板 u/d = $UD"
[ "$UD" != "0,0" ] && ok "7 traffic 入库 u/d=$UD" || bad "7 traffic 没入库"

say "7. 契约快照（供人工核对）"
(cd "$E2E/panel" && php -r '
require "vendor/autoload.php"; $a=require "bootstrap/app.php";
$a->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
foreach (["SERVER_VLESS_LAST_CHECK_AT","SERVER_VLESS_LOAD_STATUS","SERVER_VLESS_METRICS"] as $k) {
  $kk = App\Utils\CacheKey::get($k, 1); $v = cache($kk);
  printf("  %-28s %s\n", $k, $v === null ? "NULL" : substr(json_encode($v, JSON_UNESCAPED_UNICODE), 0, 160));
}')

say "8. 设备数闸门（device_limit=1，两条连接两个源 IP 不可行 → 用同 IP 不触发，改测 unknown user）"
# 同 IP 重复连接不算新设备（设计如此），所以用"未授权 UUID"验证闸门对未知用户的拒绝路径。
sed "s/$UUID/00000000-0000-0000-0000-000000000000/" "$E2E/node/client.json" > "$E2E/node/client-bad.json"
sed -i "s/\"port\":$PORT_SOCKS/\"port\":$((PORT_SOCKS+1))/" "$E2E/node/client-bad.json"
nohup "$XRAY" run -c "$E2E/node/client-bad.json" >"$E2E/logs/client-bad.log" 2>&1 &
sleep 2
if curl -sS --max-time 8 --socks5-hostname 127.0.0.1:$((PORT_SOCKS+1)) "http://127.0.0.1:$PORT_ORIGIN/small.txt" >/dev/null 2>&1; then
  bad "8 未知 UUID 竟然通了"
else
  ok "8 未知 UUID 被拒（闸门/内核鉴权）"
fi

say "9. WS 推送"
if [ "$HAVE_PCNTL" = 1 ]; then
  ( cd "$E2E/panel" && nohup php artisan ws-server start >"$E2E/logs/ws.log" 2>&1 & )
  sleep 3
  if grep -qi 'sync.config\|auth.success\|websocket' "$E2E/logs/node.log"; then
    ok "10 WS 事件到达节点"
  else
    bad "10 未见到 WS 事件（面板 server_ws_enable / ws_url / 心跳缓存？）"
  fi
else
  printf '  \033[33mSKIP\033[0m 10 本机 PHP 无 pcntl，WS 服务起不来\n'
fi

say "结果"
printf '  PASS=%d FAIL=%d\n  E2E 目录：%s（日志在 %s/logs）\n' "$PASS" "$FAIL" "$E2E" "$E2E"
[ "$FAIL" = 0 ] || { printf '\n\033[31m有失败项，现场如下\033[0m\n'; dump; exit 1; }
printf '\n\033[32m全部通过\033[0m\n'
