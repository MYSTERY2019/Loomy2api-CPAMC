#!/bin/bash
# Post-deploy smoke test (run on the VPS). Reads the first client access key from config.yaml.
# Optional: MGMT_KEY=<management key> bash verify-prod.sh  → also checks the console API.
CFG=/home/ubuntu/cliproxy/config.yaml
B=http://127.0.0.1:8317
echo "=== service ==="; systemctl is-active cliproxy
echo "=== loomy 插件日志 ==="; journalctl -u cliproxy -n 200 --no-pager | grep -i "loomy" | tail -5
echo "=== 最近错误 ==="; journalctl -u cliproxy -n 80 --no-pager | grep -iE "error|warn|panic" | tail -5
echo "=== healthz ==="; curl -s -o /dev/null -w "%{http_code}\n" $B/healthz
KEY=$(awk '/^access:/{f=1;next} f&&/^[[:space:]]+- /{gsub(/^[[:space:]]+- /,"");gsub(/"/,"");print;exit}' "$CFG")
[ -n "$KEY" ] || { echo "!! 读不到 access.api-keys"; exit 1; }
echo "=== 模型里的 lm/ 条目 ==="
curl -s -H "Authorization: Bearer $KEY" $B/v1/models | tr ',' '\n' | grep -o '"lm/[^"]*"' | head -10
PAYLOAD='{"model":"lm/spark-x2.6","messages":[{"role":"user","content":"用一句话介绍你自己"}]}'
echo "=== 非流式（中文） ==="
curl -s -o /tmp/loomy-verify.out -w "http=%{http_code} time=%{time_total}s\n" -X POST $B/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" --data-binary "$PAYLOAD"
head -c 240 /tmp/loomy-verify.out; echo
echo "=== 流式（首包时间应明显小于总时间） ==="
curl -sN -o /tmp/loomy-verify-stream.out -w "http=%{http_code} ttfb=%{time_starttransfer}s total=%{time_total}s\n" -X POST $B/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  --data-binary "${PAYLOAD%\}},\"stream\":true}"
grep -c '^data:' /tmp/loomy-verify-stream.out | xargs echo "SSE events:"
tail -c 160 /tmp/loomy-verify-stream.out; echo
if [ -n "${MGMT_KEY:-}" ]; then
  echo "=== 控制台 API ==="
  curl -s -H "Authorization: Bearer $MGMT_KEY" $B/v0/management/loomy/overview | python3 -c "
import json,sys; d=json.load(sys.stdin); s=d['summary']
print('version', d['version'], '| accounts', s['active'], '/', s['accounts'], '| unhealthy', s['unhealthy'], '| today req', s['requests'])
for a in d['accounts']: print(' ', a['phone'], a['state'], 'daily', int(a['daily']), 'avail', int(a['available']), 'days', round(a['days_left'],1), 'auto' if a['auto_renew'] else 'manual')"
fi
