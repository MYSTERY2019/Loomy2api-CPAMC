#!/bin/bash
CFG=/home/ubuntu/cliproxy/config.yaml
echo "=== service ==="
systemctl is-active cliproxy
echo "=== loomy 插件日志 ==="
journalctl -u cliproxy -n 120 --no-pager | grep -i "loomy" | tail -5
echo "=== 最近的错误 ==="
journalctl -u cliproxy -n 40 --no-pager | grep -iE "error|warn" | tail -5
echo "=== healthz ==="
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8317/healthz
KEY=$(awk '/^access:/{f=1;next} f&&/^[[:space:]]+- /{gsub(/^[[:space:]]+- /,"");gsub(/"/,"");print;exit}' "$CFG")
echo "=== 模型里的 loomy 条目 ==="
curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models | tr ',' '\n' | grep -i loomy | head -10
echo "=== 真实请求 ==="
curl -s -o /tmp/prod-chat.out -w "http=%{http_code}\n" -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  --data-binary @/tmp/cn-payload.json
head -c 240 /tmp/prod-chat.out
echo
