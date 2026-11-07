#!/bin/bash
# 在隔离实例（:8399）上加载 v2.1.0，看 CPA 的真实反应
set -u
PORT=${PORT:-8399}
PROJECT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PLUGIN_SO=${PLUGIN_SO:-$PROJECT_DIR/loomy-linux-arm64.so}
CPA_BIN=${CPA_BIN:-$HOME/cliproxy/cli-proxy-api}
mkdir -p /tmp/cpa-test/plugins
cp "$PLUGIN_SO" /tmp/cpa-test/plugins/loomy-v2.1.0.so
chmod 755 /tmp/cpa-test/plugins/loomy-v2.1.0.so

PID=$(ss -lntp 2>/dev/null | grep ":$PORT " | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
[ -n "${PID:-}" ] && kill "$PID" && sleep 2

cd /tmp/cpa-test || exit 1
nohup "$CPA_BIN" -config /tmp/cpa-test/config.yaml > /tmp/cpa-test/run.log 2>&1 &
sleep 10

echo "=== 插件相关日志 ==="
grep -iE "plugin|loomy" /tmp/cpa-test/run.log | tail -20
echo
echo "=== 全部 warn/error ==="
grep -iE "warn|error|panic" /tmp/cpa-test/run.log | tail -15
echo
echo "=== 服务状态 ==="
curl -s -o /dev/null -w "healthz=%{http_code}\n" "http://127.0.0.1:$PORT/healthz"
