#!/usr/bin/env bash
# acct.sh — 账号运维工具包装（禁用 / 启用）
#
# 用法:
#   ./acct.sh list                     # 列出账号与状态（含禁用来源 manual/system）
#   ./acct.sh disable <uid> [原因]     # 禁用：不接下游流量，签到/上报/旅行/保活全部停止
#   ./acct.sh enable  <uid>            # 启用（手动/系统禁用一并解除）
#
# 只想「摘掉下游流量、继续养号」请用休养（面板「休养」/ wpctl rest），不要禁用。
#
# 需 config 里 admin.enabled = true；管理动作要 WP_ADMIN_PASSWORD（管理密码，
# 不是下游接入 key），list 用 WP_API_KEY 或 data/access-keys.json 的启用 key。
# 如 -config / -server 也支持：./acct.sh -server http://127.0.0.1:7863 list
#
# 二进制升级: go build -o acct ./cmd/acct
set -euo pipefail
cd "$(dirname "$0")"
exec go run ./cmd/acct "$@"
