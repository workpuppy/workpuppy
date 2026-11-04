#!/usr/bin/env bash
# school_open_day_cron.sh — 开学季/夜猫子任务的**手动/cron 触发入口**。
#
# 两个入口（一个脚本两种模式，crontab 分两条调）：
#   school  12:00  school_bin ALL --run --yes
#                  任务点亮 + claim + 自动抽空抽奖余额。
#                  活动下线（in_period=false）时 task 与抽奖段各自全量跳过、正常退出。
#   cat     01:00  task_runner ALL --yes --only black_cat
#                  夜猫窗口 23:00–08:00 CST，窗口内最多补 1 次（任务内部判定，
#                  非窗口期打印 skip 正常退出）。
#
# 推荐 crontab（系统时区即 CST 时直接照抄）：
#   0 12 * * *  root /root/workbuddy2api/scripts/school_open_day_cron.sh school
#   0  1 * * *  root /root/workbuddy2api/scripts/school_open_day_cron.sh cat
#
# 说明：**日常不需要 crontab** —— 网关调度器已按 schedule.school_hours /
# schedule.cat_hours 内置执行（到点起子进程）。这个脚本留给「临时补跑」与
# 仍想用系统 cron 的部署。
#
# 为什么不再有 python：任务实现已 Go 化（internal/growth → school_bin / task_runner），
# 二进制与 wb2api 同目录（镜像里都平铺在 /app），源码模式则在仓库根。
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/.." && pwd)"
cd "$REPO_ROOT"

# ---- 模式 -> 二进制与参数 ----
MODE="${1:-}"
case "$MODE" in
    school) BIN_NAME="school_bin"; ARGS=( "ALL" "--run" "--yes" ) ;;
    cat)    BIN_NAME="task_runner"; ARGS=( "ALL" "--yes" "--only" "black_cat" ) ;;
    *)
        echo "用法: $0 {school|cat}" >&2
        echo "  school  12:00 开学任务：任务 + claim + 自动抽空抽奖" >&2
        echo "  cat     01:00 夜猫窗口(23-08 CST) 补 1 次" >&2
        exit 2
        ;;
esac

# ---- 二进制解析：WB2A_TASK_BIN_DIR > 仓库根 > PATH ----
if [[ -n "${WB2A_TASK_BIN_DIR:-}" && -x "$WB2A_TASK_BIN_DIR/$BIN_NAME" ]]; then
    BIN="$WB2A_TASK_BIN_DIR/$BIN_NAME"
elif [[ -x "$REPO_ROOT/$BIN_NAME" ]]; then
    BIN="$REPO_ROOT/$BIN_NAME"
elif command -v "$BIN_NAME" >/dev/null 2>&1; then
    BIN="$(command -v "$BIN_NAME")"
else
    echo "[$(date '+%F %T')] ERROR: 找不到 $BIN_NAME（应与 wb2api 同目录，或设 WB2A_TASK_BIN_DIR）" >&2
    echo "  构建：CGO_ENABLED=0 go build -o $BIN_NAME ./cmd/$([[ $BIN_NAME == school_bin ]] && echo school || echo taskrunner)" >&2
    exit 1
fi
CMD=( "$BIN" "${ARGS[@]}" )

# ---- 日志与锁 ----
mkdir -p data
LOG="$REPO_ROOT/data/school-cron.log"
LOCK="$REPO_ROOT/data/school-cron.lock"

exec 9>"$LOCK"
if ! flock -n 9; then
    echo "[$(date '+%F %T')] skip[$MODE]: 上一次运行仍持有锁，跳过本次" >>"$LOG"
    exit 0
fi

{
    echo "[$(date '+%F %T')] === cron[$MODE] start: ${CMD[*]} ==="
    if "${CMD[@]}"; then
        echo "[$(date '+%F %T')] === cron[$MODE] end: 完成 ==="
    else
        ec=$?
        echo "[$(date '+%F %T')] ERROR[$MODE]: 退出码 $ec（详见上方输出）"
        echo "[$(date '+%F %T')] === cron[$MODE] end: 失败 ec=$ec ==="
    fi
    # 日志轮转：超 2MB 截断保留尾部 2000 行
    if [[ -f "$LOG" ]] && [[ $(stat -c%s "$LOG") -gt 2000000 ]]; then
        tail -n 2000 "$LOG" >"$LOG.tmp" && mv "$LOG.tmp" "$LOG"
    fi
} >>"$LOG" 2>&1
