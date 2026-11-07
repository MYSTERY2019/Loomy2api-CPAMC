#!/bin/bash
# 交互式输入明文 management key 后跑 verify-prod.sh（不回显、不落盘）。
# 注意：config.yaml 里的 management.secret-key 在 CPA 启动后会被改写成 bcrypt 哈希，不能拿来用。
# 已设置 MANAGEMENT_PASSWORD / MGMT_KEY 环境变量时直接使用。
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$SCRIPT_DIR" || exit 1
MKEY=${MGMT_KEY:-${MANAGEMENT_PASSWORD:-}}
if [ -z "$MKEY" ]; then
  read -r -s -p "management key（明文）: " MKEY; echo
fi
case "$MKEY" in '$2a$'*|'$2b$'*|'$2y$'*) echo "!! 这是 bcrypt 哈希，不是明文密钥"; exit 1 ;; esac
MGMT_KEY="$MKEY" bash verify-prod.sh 2>&1
