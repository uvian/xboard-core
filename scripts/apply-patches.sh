#!/usr/bin/env bash
# 把 third_party/xray/patches/*.patch 依次应用到 vendored Xray-core。
#
# 用 GNU patch 而不是 git apply：vendored 目录没有 .git（它是外层仓库的普通子目录），
# 而 `patch --forward` 天然幂等——已应用的 hunk 会被跳过，不必靠 reverse 探测。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
XDIR="${ROOT}/third_party/xray"

[[ -d "${XDIR}/core" ]] || { echo "error: ${XDIR} 未 vendored，先运行 make vendor" >&2; exit 1; }
command -v patch >/dev/null || { echo "error: 需要 GNU patch（git for Windows 自带）" >&2; exit 1; }

shopt -s nullglob
patches=("${XDIR}"/patches/*.patch)
[[ ${#patches[@]} -gt 0 ]] || { echo "no patches found"; exit 0; }

for p in "${patches[@]}"; do
  name="$(basename "$p")"
  if patch -p1 -d "${XDIR}" --forward --dry-run <"$p" >/dev/null 2>&1; then
    patch -p1 -d "${XDIR}" --forward --no-backup-if-mismatch <"$p" >/dev/null
    echo "applied  ${name}"
  elif patch -p1 -d "${XDIR}" --forward --dry-run --reverse <"$p" >/dev/null 2>&1; then
    echo "skipped  ${name} (already applied)"
  else
    echo "error: ${name} 既不能应用也不能反应用 —— 上游结构已变，需 rebase 补丁" >&2
    exit 2
  fi
done
echo "done: ${#patches[@]} patch(es) at ${XDIR}"
