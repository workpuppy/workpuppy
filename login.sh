#!/usr/bin/env bash
# login.sh — WorkBuddy OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh                 # 交互式选域（默认 1/国内版 cn）
#   ./login.sh --realm=global  # 传参优先，直达国际版
#   ./login.sh --realm=cn      # 传参优先，直达国内版
#   非交互（管道/cron）→ 回落默认 cn
#
# 流程:
#   1. 选域：--realm 传参优先；否则 stdin 是 tty → 交互式选域（go 侧 login realm
#      子命令提示 1)国内版 2)国际版，默认 1/cn）；非交互 → 回落 cn
#   2. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   3. 你在浏览器打开 URL 完成登录
#   4. 回到这里按 y → poll 拿 token+uid+nickname
#   5. `login save` 完成剩余全部工作（首次签到 / 原子落盘 / 国际版注册地区+trial）
#   6. 重启 workbuddy2api 容器加载新账号
#
# 为什么脚本里不再有 Python：镜像已不装 python3（目标形态是单镜像 + Go 二进制），
# 原来用 python 做的 JSON 解析、签到、落盘、地区完善都搬进了 `login` 二进制的
# save / accounts 子命令 —— 那些逻辑现在和主程序共用 internal/auth、internal/region。
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR" 2>/dev/null || true
# ─── auths/ 可写性预检（issue #160）：chown -R 10001 后 host 侧 uid 无写权限，
#      此时走完整 OAuth 再在落盘处失败 = 白跑一次浏览器授权。OAuth 启动前 fail-fast。───
if ! [[ -w "$AUTH_DIR" ]]; then
    echo "❌ 无法写入 $AUTH_DIR（auths/ 已归属容器 uid 10001？）" >&2
    echo "    请在容器内登录（无需 chown 回滚，属主自动正确）：" >&2
    echo "    docker compose exec -it wb2api bash -c './login.sh' && docker compose restart wb2api" >&2
    exit 1
fi

# login 工具：缺失或构建输入（*.go 与 go.mod/go.sum）比它新时重编——realm 路由
# 逻辑在 login 二进制里，过期的二进制会把 global 凭证打向 CN 端点，症状与
# 「token 过期」无法区分。
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$LOGIN_BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 login（或镜像内置 /app/login）" >&2
        exit 1
    fi
    go build -o "$LOGIN_BIN" ./cmd/login
fi

# ─── realm 选域：--realm=cn|global 传参优先（跳过询问）──────────────
# 无传参：stdin 是 tty → 交互式选域（login realm 子命令负责提示+读数，go 侧逻辑可测）；
#         非交互（管道/cron，[ -t 0 ] 为假）→ 回落默认 cn 并提示。
REALM=""
if [[ $# -gt 0 && "$1" == --realm=* ]]; then
    REALM="${1#--realm=}"
fi
if [[ -z "$REALM" ]]; then
    if [[ -t 0 ]]; then
        REALM=$("$LOGIN_BIN" realm)
    else
        REALM="cn"
        echo "（非交互 stdin，默认国内版 cn；可用 --realm=global 指定国际版）" >&2
    fi
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" "--realm=$REALM" url)

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" "--realm=$REALM" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

# ─── 落盘 + 首次签到 + 国际版注册地区/trial（全部在 Go 侧）─────────────
# 凭据从 stdin 传入（不落 argv：token 出现在进程列表里等于泄露）。
# 失败即中止：auth 文件没写成，重启容器也没有意义。
if ! printf '%s' "$RESULT" | WB2A_AUTH_DIR="$AUTH_DIR" "$LOGIN_BIN" "--realm=$REALM" save; then
    echo "" >&2
    echo "落盘失败，见上方提示（auth 文件未写入）" >&2
    exit 1
fi

echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    COUNT=$("$LOGIN_BIN" accounts 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: ${COUNT:-?}"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi
