#!/usr/bin/env bash
# playwright 动词分发器 —— bed 内 playwright 系 CLI 的 hostel 客户端。
#
# 镜像装配方把本脚本装成 /usr/local/bin/playwright，并软链同名 playwright-cli
# （两个名字都要经这里，调用方直呼 playwright-cli 才能同样享受懒 attach）。
# COPY 必须放在 npm install 之后，压住 `playwright` / `@playwright/cli` 两个包的
# 同名 bin。
#
# 镜像前置（脚本自身不装东西）：
#   - node + 全局 `playwright`（版本与烘焙的 chromium build 锚定）+ `@playwright/cli`
#   - jq、curl
#   - hostel 给 bed 进程注入 HOSTEL_BED_ID（bed 无法自知身份）
#
# 路由规则（不模拟任何 CLI 参数面，只按动词分发）：
#   install / install-deps → 已预装，no-op 成功返回。bed 内非 root 装不动系统依赖
#                            （--with-deps 走 su 提权必失败），且浏览器 build 已烘焙。
#   页面动词               → 交给真 @playwright/cli。其 daemon 每 bed 一个（跑在
#                            bed 内、HOME 按 bed 分）；session 未打开时懒 attach 到
#                            本 bed 的 CDP 代理切片（curl browser/info 拿 cdp_url，
#                            见 docs/amenity.md §6）。懒是刻意的：amenity 的
#                            Chromium 本身惰性启动，bed init 时 attach 会破坏它。
#   open <url>             → 翻译成 goto。attach 后 `playwright-cli open` 会另起新
#                            浏览器而不是复用 attached 会话（实测），必须拦。
#   其余子命令             → 透传真 playwright CLI（codegen 等），真实语义。
#
# 不在 bed 内（无 HOSTEL_BED_ID，如宿主级调试）时页面动词直接透传真 @playwright/cli：
# 它会自起一次性浏览器，行为正确但无共享会话。
set -euo pipefail

BASE_URL="${HOSTEL_BROWSER_BASE_URL:-http://127.0.0.1:8872}"
BED="${HOSTEL_BED_ID:-}"
CLI_REAL="${PLAYWRIGHT_CLI_REAL:-/usr/local/lib/node_modules/@playwright/cli/playwright-cli.js}"
REAL_CLI="${PLAYWRIGHT_REAL_CLI:-/usr/local/lib/node_modules/playwright/cli.js}"

run_cli() { node "${CLI_REAL}" "$@"; }

# 本 bed 的代理 CDP endpoint。browser/info 会惰性建 tenant + 铸 token，因此只在
# 真正要 attach 时才调用。
bed_cdp_url() {
  curl -fsS "${BASE_URL}/v1/beds/${BED}/browser/info" | jq -re '.data.cdp_url'
}

ensure_attached_run() {
  local out rc
  set +e
  out="$(run_cli "$@" 2>&1)"
  rc=$?
  set -e
  if [[ ${rc} -ne 0 && -n "${BED}" && "${out}" == *"is not open"* ]]; then
    run_cli attach --cdp="$(bed_cdp_url)" >/dev/null
    exec node "${CLI_REAL}" "$@"
  fi
  printf '%s\n' "${out}"
  exit "${rc}"
}

cmd="${1:-}"
case "${cmd}" in
  ""|-h|--help|help)
    exec node "${CLI_REAL}" --help
    ;;
  install|install-deps)
    echo "playwright browsers and system dependencies are preinstalled in this image;" \
      "'${cmd}' skipped (browsers live in ${PLAYWRIGHT_BROWSERS_PATH:-/opt/ms-playwright})."
    exit 0
    ;;
  open|navigate)
    shift
    if [[ $# -ge 1 && "${1}" =~ ^(https?|file):// ]]; then
      ensure_attached_run goto "$@"
    fi
    # 裸 open（不带 url）语义 = 确保浏览器可用：绑到本 bed 切片即可。
    [[ -n "${BED}" ]] && exec node "${CLI_REAL}" attach --cdp="$(bed_cdp_url)"
    exec node "${CLI_REAL}" open
    ;;
  attach)
    shift
    # 不带 --cdp 的 attach 补上本 bed 的代理 endpoint，防裸 attach 摸到别的东西。
    if [[ -n "${BED}" && "$*" != *--cdp* && "$*" != *--endpoint* ]]; then
      exec node "${CLI_REAL}" attach --cdp="$(bed_cdp_url)" "$@"
    fi
    exec node "${CLI_REAL}" attach "$@"
    ;;
  goto|screenshot|snapshot|find|click|dblclick|fill|type|drag|drop|hover|select|upload|check|uncheck|eval|dialog-accept|dialog-dismiss|resize|delete-data|go-back|go-forward|reload|press|keydown|keyup|mousemove|mousedown|mouseup|mousewheel|pdf|tab-list|tab-new|tab-close|tab-select|state-load|state-save)
    ensure_attached_run "$@"
    ;;
  detach|close)
    exec node "${CLI_REAL}" "$@"
    ;;
  *)
    exec node "${REAL_CLI}" "$@"
    ;;
esac
