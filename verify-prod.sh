#!/bin/bash
# Post-deploy smoke test (run on the Linux host). Reads the first client access key from config.yaml.
# Optional: MGMT_KEY=<plaintext management key> bash verify-prod.sh  → also checks the console API.
# The secret-key in config.yaml is a bcrypt hash once CPA has started; it will not work here.
CFG=${CFG:-${CPA_CONFIG:-$HOME/cliproxy/config.yaml}}
B=${BASE_URL:-${B:-${B}}}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
[ -r "$HERE/prod-lib.sh" ] || { echo "!! 缺少 $HERE/prod-lib.sh（和本脚本一起拷到 Linux host）"; exit 1; }
. "$HERE/prod-lib.sh"
echo "=== service ==="; systemctl is-active cliproxy
echo "=== loomy 插件日志 ==="; plog 400 | grep -i "loomy" | tail -5
echo "=== 最近错误 ==="; plog 200 | grep -iE "error|warn|panic" | tail -5
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
MGMT_KEY=${MGMT_KEY:-${MANAGEMENT_PASSWORD:-}}
if [ -n "$MGMT_KEY" ] && is_bcrypt "$MGMT_KEY"; then
  echo "=== 控制台 API ==="
  echo "跳过：MGMT_KEY 是 bcrypt 哈希（来自 config.yaml），不是明文密钥。没发请求，避免累计认证失败被封 IP。"
elif [ -n "$MGMT_KEY" ]; then
  echo "=== 控制台 API ==="
  curl -s -H "Authorization: Bearer $MGMT_KEY" $B/v0/management/loomy/overview > /tmp/loomy-mgmt.json
  python3 - <<'PY'
import json
d = json.load(open('/tmp/loomy-mgmt.json'))
if 'error' in d:
    print('management API error:', d['error'])
    print('提示：CPA 启动时会把 config.yaml 里的明文 management secret 转成 bcrypt 哈希，'
          '因此配置文件里读到的不是可用密钥；必须使用你最初设定的明文密钥（或用 MANAGEMENT_PASSWORD 环境变量）。')
    raise SystemExit(0)
s = d.get('summary', {})
print('version', d.get('version'), '| accounts', s.get('active'), '/', s.get('accounts'),
      '| unhealthy', s.get('unhealthy'), '| today req', s.get('requests'))
for a in d.get('accounts', []):
    print(' ', a.get('phone'), a.get('state'), 'daily', int(a.get('daily', 0)),
          'avail', int(a.get('available', 0)), 'days', round(a.get('days_left', 0), 1),
          'auto' if a.get('auto_renew') else 'manual')
PY
fi
