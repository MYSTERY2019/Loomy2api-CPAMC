#!/bin/bash
# Deploy the loomy plugin into the production CLIProxyAPI instance (systemd: cliproxy).
set -uo pipefail

CFG=/home/ubuntu/cliproxy/config.yaml
PLUGDIR=/home/ubuntu/cliproxy/plugins/linux/arm64
AUTHDIR=/home/ubuntu/.cli-proxy-api
BAK="$CFG.bak-$(date +%Y%m%d%H%M%S)"

echo "=== 1. 备份配置 ==="
cp "$CFG" "$BAK" && echo "backup: $BAK"

echo "=== 2. 安装插件 ==="
install -m 0755 "$HOME/loomy-plugin/loomy.so" "$PLUGDIR/loomy-v0.2.0.so" && ls -lh "$PLUGDIR/loomy-v0.2.0.so"

echo "=== 3. 写入 configs.loomy ==="
python3 - "$CFG" <<'PY'
import sys
path = sys.argv[1]
lines = open(path, encoding="utf-8").read().split("\n")
if any(l.strip() == "loomy:" for l in lines):
    print("already configured")
    raise SystemExit(0)
out, done = [], False
for line in lines:
    out.append(line)
    if not done and line.rstrip() == "    configs:":
        out.append("        loomy:")
        out.append("            enabled: true")
        done = True
if not done:
    raise SystemExit("configs block not found")
open(path, "w", encoding="utf-8").write("\n".join(out))
print("config patched")
PY

echo "=== 4. 放置账号 ==="
if [ -f "$AUTHDIR/loomy-8109.json" ]; then
  echo "auth already present"
else
  cp /tmp/cpa-test/auth/loomy-8109.json "$AUTHDIR/loomy-8109.json" && chmod 600 "$AUTHDIR/loomy-8109.json" && echo "auth installed"
fi

echo "=== 5. 重启服务 ==="
sudo systemctl restart cliproxy
sleep 8
systemctl is-active cliproxy

echo "=== 6. 插件加载日志 ==="
if journalctl -u cliproxy -n 60 --no-pager | grep -q "plugin registered plugin_id=loomy"; then
  journalctl -u cliproxy -n 60 --no-pager | grep "plugin registered plugin_id=loomy" | tail -1
else
  echo "!! 插件未注册，开始回滚"
  journalctl -u cliproxy -n 40 --no-pager | grep -iE "plugin|error" | tail -10
  cp "$BAK" "$CFG"
  rm -f "$PLUGDIR/loomy-v0.2.0.so"
  sudo systemctl restart cliproxy
  sleep 6
  echo "rolled back; service: $(systemctl is-active cliproxy)"
  exit 1
fi

echo "=== 7. 接口验证 ==="
curl -s -o /dev/null -w "healthz=%{http_code}\n" http://127.0.0.1:8317/healthz
KEY=$(python3 - "$CFG" <<'PY'
import sys
lines = open(sys.argv[1], encoding="utf-8").read().split("\n")
in_access = False
for line in lines:
    if line.startswith("access:"):
        in_access = True
        continue
    if in_access:
        s = line.strip()
        if s.startswith("- "):
            print(s[2:].strip().strip('"'))
            break
        if line and not line.startswith(" "):
            break
PY
)
echo "models containing loomy: $(curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8317/v1/models | tr ',' '\n' | grep -c loomy)"

echo "=== 8. 真实对话 ==="
curl -s -o /tmp/prod-chat.out -w "http=%{http_code}\n" -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  --data-binary @/tmp/cn-payload.json
head -c 220 /tmp/prod-chat.out
echo
