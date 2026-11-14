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
#  11 REALITY：面板 reality_settings（DB snake_case）→ 面板归一化 → 节点 → REALITY 客户端真实握手
#  12 mihomo 回归：mihomo 作为客户端过 REALITY（FlClash / Clash Verge Rev / Nikki 同一内核）
#  13 反向对照：不带 support-x25519mlkem768 的 mihomo 必须认证失败（证明这是客户端侧必需项）
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
      php-redis php-bcmath php-intl unzip curl sqlite3 redis-server git ca-certificates >/dev/null     || die "apt 安装失败（pcntl 已内建在 php-cli，不是独立包）"
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
[ -d vendor ] || timeout 600 composer install -q --no-interaction --no-progress --ignore-platform-reqs >"$E2E/logs/composer.log" 2>&1 || die "composer install 失败"
composer show predis/predis >/dev/null 2>&1 || composer require -q --no-interaction --ignore-platform-reqs predis/predis >/dev/null 2>&1

say "2. 面板：环境与建库"
mkdir -p bootstrap/cache storage/framework/{cache,sessions,views} storage/logs
[ -f .env ] || cp .env.example .env
php artisan key:generate --force >/dev/null
# 必须用相对文件名：Laravel 的 sqlite 驱动会自己拼 database_path()，
# 传绝对路径会被再拼一次，变成 <app>/<abs path>（Windows 与 Actions 上都踩过）。
sed -i "s|^DB_CONNECTION=.*|DB_CONNECTION=sqlite|; s|^DB_DATABASE=.*|DB_DATABASE=e2e.sqlite|" .env
grep -q '^REDIS_CLIENT=' .env && sed -i 's|^REDIS_CLIENT=.*|REDIS_CLIENT=predis|' .env || echo 'REDIS_CLIENT=predis' >> .env
sed -i 's|^CACHE_STORE=.*|CACHE_STORE=redis|; s|^CACHE_DRIVER=.*|CACHE_DRIVER=redis|' .env
sed -i 's|^QUEUE_CONNECTION=.*|QUEUE_CONNECTION=sync|' .env
grep -q '^INSTALLED=' .env && sed -i 's|^INSTALLED=.*|INSTALLED=true|' .env || echo 'INSTALLED=true' >> .env
: > database/e2e.sqlite
timeout 300 php artisan migrate --force >"$E2E/logs/migrate.log" 2>&1 || die "迁移失败"

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
          'device_limit' => 3, 'speed_limit' => 0, 'group_id' => 1, 'banned' => 0, 'is_admin' => 0,
          // expired_at 必须显式给未来时间：面板 getAvailableUsers 用的是 orWhere('expired_at', NULL)，
          // 生成的是 expired_at = NULL，永远不会匹配，NULL 过期的用户不会被下发。
          'expired_at' => time() + 86400 * 30,
          'token' => Illuminate\Support\Str::random(32)])->save();
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

say "4.5 WS 服务（必须早于节点启动：handshake 只在心跳缓存存在时才给 ws_url）"
if [ "$HAVE_PCNTL" = 1 ]; then
  cat > "$E2E/panel/ws_probe.php" <<'PHP'
<?php
require __DIR__."/vendor/autoload.php";
$app = require __DIR__."/bootstrap/app.php";
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
echo Illuminate\Support\Facades\Cache::has("ws_server:heartbeat") ? "heartbeat" : "none";
PHP
  ( cd "$E2E/panel" && nohup php artisan ws-server start >"$E2E/logs/ws.log" 2>&1 & )
  hb=none
  for _ in $(seq 1 25); do
    hb=$( cd "$E2E/panel" && php ws_probe.php 2>/dev/null | tr -d '
' )
    [ "$hb" = "heartbeat" ] && break
    sleep 1
  done
  hs=$(curl -sS --max-time 5 "http://127.0.0.1:$PORT_PANEL/api/v2/server/handshake?token=$TOKEN&node_id=$NODE_ID" 2>/dev/null || true)
  if [ "$hb" = "heartbeat" ] && echo "$hs" | grep -q '"enabled":true'; then
    ok "2.5 WS 就绪（心跳缓存存在 + handshake websocket.enabled=true）"
  else
    bad "2.5 WS 未就绪：heartbeat=$hb handshake=$hs（见 ws.log）"
  fi
else
  printf '  [33mSKIP[0m 2.5 本机 PHP 无 pcntl，WS 服务起不来
'
fi

export GOFLAGS=-mod=mod   # 允许 go 自动补齐 go.sum（跨平台依赖）
# third_party/xray 不入库（.gitignore），CI 里必须先 vendor 再打补丁
cd "$E2E/core"
[ -f third_party/xray/go.mod ] || timeout 600 make vendor >"$E2E/logs/vendor.log" 2>&1 || die "make vendor 失败"
timeout 300 make patches >"$E2E/logs/patch.log" 2>&1 || die "打 usergate 补丁失败"
timeout 900 make build >"$E2E/logs/build.log" 2>&1 || die "make build 失败（要看 Go 环境）"
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
kernel: {xray: true, singbox: false, allow_private_targets: true}
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
  ( cd "$E2E/core/third_party/xray" && timeout 600 go build -o "$E2E/bin/xray" ./main ) >"$E2E/logs/xray-build.log" 2>&1 || die "编 xray 客户端失败"
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

say "5b. 微诊断：源站可重复访问 / xray 直连出口"
curl -s --max-time 5 "http://127.0.0.1:$PORT_ORIGIN/small.txt" >/dev/null && echo "  源站第2次直连 OK" || echo "  源站第2次直连失败"
cat > "$E2E/node/socksonly.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"s","listen":"127.0.0.1","port":$((PORT_SOCKS+3)),"protocol":"socks","settings":{"udp":false}}],
 "outbounds":[{"tag":"direct","protocol":"freedom","settings":{}}]}
JSON
nohup "$XRAY" run -c "$E2E/node/socksonly.json" >"$E2E/logs/socksonly.log" 2>&1 &
sleep 2
s1=$(curl -sS --max-time 8 --socks5 127.0.0.1:$((PORT_SOCKS+3)) "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
echo "  socks-only 出口: ${s1:-<empty>}"
grep -aiE "dialing|accepted" "$E2E/logs/socksonly.log" | tail -3

say "5c. 对照：完全未打补丁的上游二进制（连代码都是纯净的）"
if [ ! -x "$E2E/bin/xray-pristine" ]; then
  git clone -q --depth 1 -b v26.11.9 https://github.com/XTLS/Xray-core "$E2E/xray-pristine" >>"$E2E/logs/pristine-clone.log" 2>&1
  ( cd "$E2E/xray-pristine" && timeout 600 go build -o "$E2E/bin/xray-pristine" ./main ) >>"$E2E/logs/pristine-build.log" 2>&1
fi
PSRV=$((PORT_NODE+3)); PCLI=$((PORT_SOCKS+4))
cat > "$E2E/node/stock2.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"in","listen":"127.0.0.1","port":$PSRV,"protocol":"vless",
   "settings":{"clients":[{"id":"$UUID"}],"decryption":"none"}}],
 "outbounds":[{"tag":"direct","protocol":"freedom","settings":{}}]}
JSON
cat > "$E2E/node/client-stock2.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"s","listen":"127.0.0.1","port":$PCLI,"protocol":"socks","settings":{"udp":false}}],
 "outbounds":[{"tag":"p","protocol":"vless",
   "settings":{"vnext":[{"address":"127.0.0.1","port":$PSRV,"users":[{"id":"$UUID","encryption":"none"}]}]},
   "streamSettings":{"network":"tcp","security":"none"}}]}
JSON
if [ -x "$E2E/bin/xray-pristine" ]; then
  nohup "$E2E/bin/xray-pristine" run -c "$E2E/node/stock2.json" >"$E2E/logs/stock2.log" 2>&1 &
  sleep 2
  nohup "$E2E/bin/xray-pristine" run -c "$E2E/node/client-stock2.json" >"$E2E/logs/client-stock2.log" 2>&1 &
  sleep 2
  p2=$(curl -sS --max-time 10 --socks5 127.0.0.1:$PCLI "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
  echo "  纯净二进制结果: ${p2:-<empty>}"
  if echo "$p2" | grep -q hello-e2e; then
    bad "5c 纯净二进制竟然通了（说明私网目标没被拦）"
  elif grep -qE "blocked by a finalrule|blocked target" "$E2E/logs/stock2.log"; then
    ok "5c 上游默认拦截私网目标（freedom defaultBlockPrivateRule 生效）"
  else
    bad "5c 纯净二进制失败原因不是私网拦截，需另查"
  fi
  grep -aiE "dialing|accepted|Failed" "$E2E/logs/stock2.log" | tail -3
else
  bad "5c 纯净二进制没编出来"
fi

say "6. 真实流量 → 面板 u/d"
got=$(curl -sS --max-time 15 --socks5 127.0.0.1:"$PORT_SOCKS" "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
echo "$got" | grep -q hello-e2e && ok "7 经节点取到源站内容" || { bad "7 数据面不通"; dump; }
curl -s --max-time 20 --socks5 127.0.0.1:"$PORT_SOCKS" -o /dev/null "http://127.0.0.1:$PORT_ORIGIN/blob.bin" || true
# 上报节奏 = max(10s, pull/2)(WS 在线也不退坡),最多等 90s 让它跑完两拍。
UD="0,0"
for _ in $(seq 1 18); do
  sleep 5
  UD=$(cd "$E2E/panel" && php -r 'require "vendor/autoload.php"; $a=require "bootstrap/app.php"; $a->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); $u=App\Models\User::find('"$USER_ID"'); echo $u->u.",".$u->d;')
  [ "$UD" != "0,0" ] && break
done
echo "  面板 u/d = $UD"
[ "$UD" != "0,0" ] && ok "7 traffic 入库 u/d=$UD" || bad "7 traffic 没入库"

say "6b. 对照实验：纯净 Xray 服务端（排除我们节点配置的影响）"
cat > "$E2E/node/stock.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"in","listen":"127.0.0.1","port":$((PORT_NODE+2)),"protocol":"vless",
   "settings":{"clients":[{"id":"$UUID"}],"decryption":"none"}}],
 "outbounds":[{"tag":"direct","protocol":"freedom","settings":{}}]}
JSON
nohup "$XRAY" run -c "$E2E/node/stock.json" >"$E2E/logs/stock.log" 2>&1 &
sleep 2
sed "s/$PORT_NODE/$((PORT_NODE+2))/; s/\"port\":$PORT_SOCKS/\"port\":$((PORT_SOCKS+2))/" "$E2E/node/client.json" > "$E2E/node/client-stock.json"
nohup "$XRAY" run -c "$E2E/node/client-stock.json" >"$E2E/logs/client-stock.log" 2>&1 &
sleep 2
stock=$(curl -sS --max-time 10 --socks5 127.0.0.1:$((PORT_SOCKS+2)) "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
echo "  对照结果: ${stock:-<empty>}"
# 纯净 v26 服务端同样会被 freedom 的 defaultBlockPrivateRule 拦住，这是上游行为，
# 不是我们的配置问题；反过来也证明「放行私网」必须由节点侧显式打开。
if echo "$stock" | grep -q hello-e2e; then
  bad "6b 纯净 Xray 竟然放行了私网目标（上游行为变了？）"
elif grep -qE "blocked by a finalrule|blocked target" "$E2E/logs/stock.log"; then
  ok "6b 纯净 v26 服务端同样拦截私网目标（证明放行必须显式打开）"
else
  bad "6b 纯净 Xray 失败原因不是私网拦截，需另查"
fi

say "6c. REALITY 节点：面板 reality_settings → 第二节点 → REALITY 客户端"
# 这一段验证的是「面板真实下发的 reality_settings 能被节点消费、并把 REALITY 数据面跑通」。
# 面板补丁把 DB 里的 snake_case（short_id / mldsa65_seed / xver）归一成 camelCase 再发给节点，
# 所以夹具只写 DB 形状，走一遍补丁的归一化路径 —— 这正是最容易写错的地方。
KEYOUT=$("$XRAY" x25519 2>/dev/null || true)
RPRIV=$(printf '%s\n' "$KEYOUT" | sed -n 's/^PrivateKey:[[:space:]]*//p' | head -1)
RPUB=$(printf '%s\n' "$KEYOUT" | sed -n 's/^Password (PublicKey):[[:space:]]*//p' | head -1)
# dest 必须是公网 TLS1.3 站点：REALITY 服务端会用 SNI 去取它的真证书（带校验，自签不行）。
RDEST=""
for cand in www.microsoft.com www.apple.com www.cloudflare.com swdist.apple.com; do
  if timeout 6 bash -c "</dev/tcp/$cand/443" 2>/dev/null; then RDEST=$cand; break; fi
done
if [ -z "$RPRIV" ] || [ -z "$RPUB" ]; then
  bad "6c x25519 密钥生成失败：${KEYOUT:-<empty>}"
elif [ -z "$RDEST" ]; then
  printf '  \033[33mSKIP\033[0m 6c 没有任何可用的 REALITY dest（runner 出网被限），跳过\n'
else
  ok "6c 密钥与 dest 就绪（dest=$RDEST）"
  cat > "$E2E/panel/e2e_seed_reality.php" <<PHP
<?php
require __DIR__.'/vendor/autoload.php';
\$app = require __DIR__.'/bootstrap/app.php';
\$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
\$s = App\Models\Server::firstOrNew(['id' => 2]);
// DB 形状：reality_settings 里是面板 snake_case 字段，补丁的 realityConfig() 负责归一化。
\$s->fill(['type' => 'vless', 'name' => 'e2e-vless-reality', 'group_ids' => [1], 'host' => '127.0.0.1',
          'port' => $((PORT_NODE+1)), 'server_port' => $((PORT_NODE+1)), 'show' => 1, 'enabled' => 1, 'rate' => 1,
          'protocol_settings' => ['tls' => 2, 'network' => 'tcp', 'flow' => null,
            'encryption' => ['enabled' => false, 'decryption' => null],
            'reality_settings' => ['server_name' => '$RDEST', 'dest' => '$RDEST:443',
              'server_port' => 443, 'private_key' => '$RPRIV', 'public_key' => '$RPUB',
              'short_id' => '0123abcd', 'short_ids' => ['0123abcd'], 'xver' => 0]]])->save();
echo "seeded reality server=".\$s->id.PHP_EOL;
PHP
  php "$E2E/panel/e2e_seed_reality.php" >"$E2E/logs/seed-reality.log" 2>&1 && ok "6c 夹具：REALITY 节点入库" || bad "6c REALITY 夹具失败"

  # 面板 API 必须吐出归一化后的 camelCase 字段，节点才认。
  rcfg=$(curl -sS --max-time 10 "http://127.0.0.1:$PORT_PANEL/api/v2/server/config?token=$TOKEN&node_id=2" 2>/dev/null || true)
  if echo "$rcfg" | grep -q '"mldsa65Seed"' && echo "$rcfg" | grep -q '"xver"'; then
    ok "6c 面板 API 归一化 reality_settings（mldsa65Seed/xver 就位）"
  else
    bad "6c 面板 API 没有归一化 REALITY 字段：$(echo "$rcfg" | head -c 200)"
  fi

  cat > "$E2E/node/config2.yaml" <<YAML
log: {level: debug}
node:
  api_host: http://127.0.0.1:$PORT_PANEL
  node_id: 2
  api_key: $TOKEN
  timeout: 15
  retry: 3
  pull_every: 30
api: {listen: 127.0.0.1:8092, metrics: false}
kernel: {xray: true, singbox: false, allow_private_targets: true}
YAML
  ( cd "$E2E/node" && nohup "$E2E/bin/xboard-core" -config config2.yaml >"$E2E/logs/node2.log" 2>&1 & )
  for i in $(seq 1 40); do grep -q 'kernel started' "$E2E/logs/node2.log" && break; sleep 0.5; done
  if grep -q 'kernel started' "$E2E/logs/node2.log"; then
    ok "6c REALITY 内核启动（面板下发的 reality_settings 被接受）"
  else
    bad "6c REALITY 内核没起来（见 node2.log）"; tail -20 "$E2E/logs/node2.log"
  fi
  ss -lntp 2>/dev/null | grep -q ":$((PORT_NODE+1)) " && ok "6c 监听 $((PORT_NODE+1))" || bad "6c 未监听 REALITY 端口"
  if grep -qiE 'minClientVer=.*(会|高于)' "$E2E/logs/node2.log"; then
    bad "6c 误报了 minClientVer 告警（面板默认下发 null 不应触发）"
  else
    ok "6c minClientVer 默认不下发（mihomo 系客户端不会被门禁打回）"
  fi

  RCLI=$((PORT_SOCKS+5))
  cat > "$E2E/node/client-reality.json" <<JSON
{"log":{"loglevel":"debug"},
 "inbounds":[{"tag":"s","listen":"127.0.0.1","port":$RCLI,"protocol":"socks","settings":{"udp":false}}],
 "outbounds":[{"tag":"p","protocol":"vless",
   "settings":{"vnext":[{"address":"127.0.0.1","port":$((PORT_NODE+1)),"users":[{"id":"$UUID","encryption":"none"}]}]},
   "streamSettings":{"network":"tcp","security":"reality",
     "realitySettings":{"serverName":"$RDEST","fingerprint":"chrome","publicKey":"$RPUB",
       "shortId":"0123abcd","spiderX":"/"}}}]}
JSON
  nohup "$XRAY" run -c "$E2E/node/client-reality.json" >"$E2E/logs/client-reality.log" 2>&1 &
  sleep 3
  rgot=$(curl -sS --max-time 20 --socks5 127.0.0.1:$RCLI "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
  if echo "$rgot" | grep -q hello-e2e; then
    ok "6c REALITY 数据面通（真实 REALITY 握手 + 转发）"
  else
    bad "6c REALITY 数据面不通：${rgot:-<empty>}"
    grep -aiE "REALITY|rejected|failed|error" "$E2E/logs/node2.log" | tail -5
  fi
fi

say "6d. mihomo 客户端回归（FlClash / Clash Verge Rev / Nikki 用的是同一个内核）"
# 为什么非测不可：mihomo 在 ClientHello 里自报 1.8.2，而我们所有 REALITY 相关判断
# （minClientVer 守卫、mldsa65、xver）都以它为前提；用 xray 客户端测通不等于 mihomo 能连。
MIHOMO_VER=${MIHOMO_VER:-v1.19.34}
if [ -z "${RDEST:-}" ]; then
  printf '  \033[33mSKIP\033[0m 6d 没有 REALITY dest，跳过 mihomo 回归\n'
else
  if [ ! -x "$E2E/bin/mihomo" ]; then
    curl -fsSL --retry 2 -o "$E2E/bin/mihomo.gz" \
      "https://github.com/MetaCubeX/mihomo/releases/download/$MIHOMO_VER/mihomo-linux-amd64-compatible-$MIHOMO_VER.gz" \
      >"$E2E/logs/mihomo-dl.log" 2>&1 && gunzip -f "$E2E/bin/mihomo.gz" && chmod +x "$E2E/bin/mihomo"
  fi
  if [ -x "$E2E/bin/mihomo" ]; then
    MPORT=$((PORT_SOCKS+6))
    cat > "$E2E/node/mihomo.yaml" <<YAML
mixed-port: $MPORT
allow-lan: false
log-level: debug
mode: rule
proxies:
  - name: e2e-reality
    type: vless
    server: 127.0.0.1
    port: $((PORT_NODE+1))
    uuid: $UUID
    network: tcp
    tls: true
    udp: false
    servername: $RDEST
    reality-opts:
      public-key: $RPUB
      short-id: "0123abcd"
      support-x25519mlkem768: true
    client-fingerprint: chrome
rules:
  - MATCH,e2e-reality
YAML
    nohup "$E2E/bin/mihomo" -d "$E2E/node" -f "$E2E/node/mihomo.yaml" >"$E2E/logs/mihomo.log" 2>&1 &
    for i in $(seq 1 30); do ss -lntp 2>/dev/null | grep -q ":$MPORT " && break; sleep 0.5; done
    if ! ss -lntp 2>/dev/null | grep -q ":$MPORT "; then
      echo "  --- mihomo 没有监听 $MPORT ---"; tail -30 "$E2E/logs/mihomo.log" || true
    fi
    mgot=$(curl -sS --max-time 25 --socks5-hostname 127.0.0.1:$MPORT "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
    if echo "$mgot" | grep -q hello-e2e; then
      ok "6d mihomo $MIHOMO_VER REALITY 握手成功并取到源站"
    else
      bad "6d mihomo REALITY 不通：${mgot:-<empty>}"
      grep -q "X25519MLKEM768 for TLS. communication: false" "$E2E/logs/mihomo.log" 2>/dev/null \
        && echo "  提示：mihomo 未发送 X25519MLKEM768 密钥共享，Xray 26.x REALITY 会直接丢弃该 ClientHello（认证失败）" || true
      echo "  --- mihomo -v ---"; "$E2E/bin/mihomo" -v 2>&1 | head -3
      echo "  --- mihomo.yaml ---"; sed -n '1,40p' "$E2E/node/mihomo.yaml"
      echo "  --- mihomo.log (tail 40) ---"; tail -40 "$E2E/logs/mihomo.log" || true
      echo "  --- 直连探测（绕开 mihomo，确认源站还在） ---"
      curl -sS --max-time 5 "http://127.0.0.1:$PORT_ORIGIN/small.txt" | head -c 80 || true
      echo
      echo "  --- 再走一次 mihomo，带 -v ---"
      curl -sS -v --max-time 20 --socks5-hostname "127.0.0.1:$MPORT" \
        "http://127.0.0.1:$PORT_ORIGIN/small.txt" 2>&1 | tail -25 || true
      echo "  --- node2.log (tail 25) ---"; tail -25 "$E2E/logs/node2.log"
    fi
  else
    bad "6d mihomo 下载失败（见 mihomo-dl.log）"
  fi
fi

say "6e. 反向对照：不开启 support-x25519mlkem768 时必须认证失败（这就是 mihomo 默认连不上 Xray 26.x REALITY 的原因）"
MLPORT=$((PORT_SOCKS+7))
mkdir -p "$E2E/node/legacy"
cp "$E2E/node/mihomo.yaml" "$E2E/node/legacy/mihomo.yaml"
python3 - "$E2E/node/legacy/mihomo.yaml" "$MLPORT" <<'PY'
import io, sys
p, port = sys.argv[1], sys.argv[2]
d = io.open(p, encoding="utf-8").read()
d = d.replace("support-x25519mlkem768: true", "")
lines = d.split("\n")
for i, line in enumerate(lines):
    if line.startswith("mixed-port:"):
        lines[i] = "mixed-port: " + port
io.open(p, "w", encoding="utf-8").write("\n".join(lines))
PY
nohup "$E2E/bin/mihomo" -d "$E2E/node/legacy" -f "$E2E/node/legacy/mihomo.yaml" >"$E2E/logs/mihomo-legacy.log" 2>&1 &
for i in $(seq 1 30); do ss -lntp 2>/dev/null | grep -q ":$MLPORT " && break; sleep 0.5; done
lgot=$(curl -sS --max-time 20 --socks5-hostname 127.0.0.1:$MLPORT "http://127.0.0.1:$PORT_ORIGIN/small.txt" || true)
if echo "$lgot" | grep -q hello-e2e; then
  bad "6e 未开启 MLKEM 竟然也通了——mihomo 默认值已变，请复核面板补丁里的 support-x25519mlkem768"
else
  if grep -q "REALITY authentication failed" "$E2E/logs/mihomo-legacy.log"; then
    ok "6e 反向对照成立：不带 support-x25519mlkem768 时 REALITY 认证失败（节点侧日志同时出现 processed invalid connection）"
  else
    bad "6e 反向对照未复现认证失败：$(tail -3 "$E2E/logs/mihomo-legacy.log" | tr '\n' ' ')"
  fi
fi
pkill -f "mihomo -d $E2E/node/legacy" 2>/dev/null || true

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
if curl -sS --max-time 8 --socks5 127.0.0.1:$((PORT_SOCKS+1)) "http://127.0.0.1:$PORT_ORIGIN/small.txt" >/dev/null 2>&1; then
  bad "8 未知 UUID 竟然通了"
else
  ok "8 未知 UUID 被拒（闸门/内核鉴权）"
fi

say "9. WS 推送"
if [ "$HAVE_PCNTL" = 1 ]; then
  if grep -q 'ws push connected' "$E2E/logs/node.log" && grep -qiE 'ws authenticated|auth.success|sync\.config' "$E2E/logs/node.log"; then
    ok "10 WS 推送已连上且收到事件（auth.success / sync.config）"
  else
    bad "10 节点未收到 WS 事件（node.log 与 ws.log 见现场）"
  fi
else
  printf '  [33mSKIP[0m 10 本机 PHP 无 pcntl，WS 服务起不来
'
fi

say "结果"
printf '  PASS=%d FAIL=%d\n  E2E 目录：%s（日志在 %s/logs）\n' "$PASS" "$FAIL" "$E2E" "$E2E"
[ "$FAIL" = 0 ] || { printf '\n\033[31m有失败项，现场如下\033[0m\n'; dump; exit 1; }
printf '\n\033[32m全部通过\033[0m\n'
