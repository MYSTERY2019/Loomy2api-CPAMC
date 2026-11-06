#!/bin/bash
# Deploy the loomy plugin into the production CLIProxyAPI instance (systemd: cliproxy).
# Usage (on the VPS):  bash deploy-prod.sh [path/to/loomy.so]
# Rolls back the previous .so and config automatically if the plugin fails to register.
set -uo pipefail

VERSION=2.0.0
SRC=${1:-$HOME/loomy-plugin/loomy-linux-arm64.so}
CFG=/home/ubuntu/cliproxy/config.yaml
PLUGDIR=/home/ubuntu/cliproxy/plugins/linux/arm64
AUTHDIR=/home/ubuntu/.cli-proxy-api
STAMP=$(date +%Y%m%d%H%M%S)
BAKDIR=/home/ubuntu/cliproxy/plugin-backups/$STAMP

[ -f "$SRC" ] || { echo "!! 找不到插件文件 $SRC"; exit 1; }
file "$SRC" | grep -q "ARM aarch64" || { echo "!! $SRC 不是 linux/arm64 产物"; exit 1; }

echo "=== 1. 备份配置与旧插件 → $BAKDIR ==="
mkdir -p "$BAKDIR"
cp "$CFG" "$BAKDIR/config.yaml"
shopt -s nullglob
OLD=("$PLUGDIR"/loomy*.so)
for f in "${OLD[@]}"; do mv "$f" "$BAKDIR/" && echo "moved old: $(basename "$f")"; done
shopt -u nullglob

echo "=== 2. 安装插件 v$VERSION ==="
install -m 0755 "$SRC" "$PLUGDIR/loomy-v$VERSION.so" && ls -lh "$PLUGDIR/loomy-v$VERSION.so"

echo "=== 3. 确认 configs.loomy ==="
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
        out += ["        loomy:", "            enabled: true"]
        done = True
if not done:
    raise SystemExit("configs block not found")
open(path, "w", encoding="utf-8").write("\n".join(out))
print("config patched")
PY

echo "=== 4. 凭证文件权限 ==="
chmod 700 "$AUTHDIR" 2>/dev/null
for f in "$AUTHDIR"/loomy-*.json; do [ -f "$f" ] && chmod 600 "$f"; done
ls -l "$AUTHDIR"/loomy-*.json 2>/dev/null | awk '{print $1, $NF}'

echo "=== 5. 重启服务 ==="
sudo systemctl restart cliproxy
sleep 8
systemctl is-active cliproxy

rollback() {
  echo "!! $1，开始回滚"
  journalctl -u cliproxy -n 40 --no-pager | grep -iE "plugin|error|panic" | tail -10
  rm -f "$PLUGDIR/loomy-v$VERSION.so"
  cp "$BAKDIR"/loomy*.so "$PLUGDIR/" 2>/dev/null
  cp "$BAKDIR/config.yaml" "$CFG"
  sudo systemctl restart cliproxy
  sleep 6
  echo "rolled back; service: $(systemctl is-active cliproxy)"
  exit 1
}

echo "=== 6. 插件加载日志 ==="
LINE=$(journalctl -u cliproxy -n 80 --no-pager | grep "plugin registered plugin_id=loomy" | tail -1)
[ -n "$LINE" ] || rollback "插件未注册"
echo "$LINE"
echo "$LINE" | grep -q "version=$VERSION" || rollback "加载的不是 v$VERSION"

echo "=== 7. 接口验证 ==="
curl -s -o /dev/null -w "healthz=%{http_code}\n" http://127.0.0.1:8317/healthz
curl -s -o /dev/null -w "panel(resource)=%{http_code}\n" http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts
curl -s -o /dev/null -w "old data route (expect 404)=%{http_code}\n" http://127.0.0.1:8317/v0/resource/plugins/loomy/accounts/data
echo
echo "完成。面板：管理中心 → 插件页 → Loomy 账号池（首次打开输入 management key）。"
echo "跑一次 verify-prod.sh 做真实对话验证。旧插件与配置备份在 $BAKDIR"
