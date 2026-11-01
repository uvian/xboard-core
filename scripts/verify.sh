#!/usr/bin/env bash
# 一键验证，等价于 `make verify`（Windows 上没有 make 时用它）。
# 需要 Go 1.27+；Windows 上请确保 TMP/TEMP 指向可写目录，否则 go 会在新建 work dir 时失败。
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

GO=${GO:-go}
export GOFLAGS=${GOFLAGS:--mod=mod}

if [ -n "${GO_TMP:-}" ]; then
  export TMP="$GO_TMP" TEMP="$GO_TMP"
fi

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

say "gofmt"
unformatted=$(gofmt -l cmd internal || true)
if [ -n "$unformatted" ]; then
  echo "以下文件未格式化："
  echo "$unformatted"
  exit 1
fi

say "go vet"
"$GO" vet ./cmd/... ./internal/...

say "go test"
"$GO" test ./...

say "build"
mkdir -p bin
EXE=""
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;;
esac
"$GO" build -trimpath -ldflags "-s -w" -o "bin/xboard-core$EXE" ./cmd/xboard-core

say "smoke"
"$GO" run ./cmd/xboard-core -smoke

say "vendored xray"
(cd "$ROOT/third_party/xray" && "$GO" test ./app/usergate/... ./app/dispatcher/...)

say "patch idempotency"
bash scripts/apply-patches.sh

say "OK: 全部通过"
