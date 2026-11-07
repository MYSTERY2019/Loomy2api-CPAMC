#!/bin/bash
# Deploy the loomy plugin into the CLIProxyAPI instance (systemd: cliproxy).
# Usage (on the Linux host):  bash deploy-prod.sh [path/to/loomy.so]
# Rolls back the previous .so and config automatically if the plugin fails to register.
set -uo pipefail

SRC=${1:-$HOME/loomy-plugin/loomy-linux-arm64.so}
CFG=${CFG:-${CPA_CONFIG:-$HOME/cliproxy/config.yaml}}
PLUGDIR=${PLUGDIR:-${CPA_PLUGIN_DIR:-$HOME/cliproxy/plugins/linux/arm64}}
AUTHDIR=${AUTHDIR:-${CPA_AUTH_DIR:-$HOME/.cli-proxy-api}}
STAMP=$(date +%Y%m%d%H%M%S)
BASE_URL=${BASE_URL:-http://127.0.0.1:8317}
BAKDIR=${BACKUP_DIR:-$HOME/cliproxy/plugin-backups/$STAMP}

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
[ -r "$HERE/prod-lib.sh" ] || { echo "!! 缺少 $HERE/prod-lib.sh（和本脚本一起拷到 Linux host）"; exit 1; }
. "$HERE/prod-lib.sh"

[ -f "$SRC" ] || { echo "!! 找不到插件文件 $SRC"; exit 1; }
file "$SRC" | grep -q "ARM aarch64" || { echo "!! $SRC 不是 linux/arm64 产物"; exit 1; }

# 版本号从 .so 自己注册的 metadata 里读，不再硬编码；读不到时可用 VERSION=x.y.z 覆盖
SOVER=$(so_version "$SRC")
VERSION=${VERSION:-$SOVER}
[ -n "$VERSION" ] || { echo "!! 读不到 $SRC 的版本号（可用 VERSION=x.y.z bash deploy-prod.sh 指定）"; exit 1; }
[ -z "$SOVER" ] || [ "$SOVER" = "$VERSION" ] || { echo "!! VERSION=$VERSION 与 .so 自报的 $SOVER 不一致"; exit 1; }
echo "插件版本：v$VERSION"

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
  plog 60 | grep -iE "plugin|error|panic" | tail -10
  rm -f "$PLUGDIR/loomy-v$VERSION.so"
  cp "$BAKDIR"/loomy*.so "$PLUGDIR/" 2>/dev/null
  cp "$BAKDIR/config.yaml" "$CFG"
  sudo systemctl restart cliproxy
  sleep 6
  echo "rolled back; service: $(systemctl is-active cliproxy)"
  exit 1
}

echo "=== 6. 插件加载日志 ==="
LINE=$(plog 300 | grep "plugin registered plugin_id=loomy" | tail -1)
[ -n "$LINE" ] || rollback "插件未注册"
echo "$LINE"
echo "$LINE" | grep -q "version=$VERSION" || rollback "加载的不是 v$VERSION"

echo "=== 7. 接口验证 ==="
curl -s -o /dev/null -w "healthz=%{http_code}\n" "${BASE_URL}/healthz"
curl -s -o /dev/null -w "panel(resource)=%{http_code}\n" "${BASE_URL}/v0/resource/plugins/loomy/accounts"
curl -s -o /dev/null -w "old data route (expect 404)=%{http_code}\n" "${BASE_URL}/v0/resource/plugins/loomy/accounts/data"
echo
echo "完成。面板：管理中心 → 插件页 → Loomy 账号池（首次打开输入 management key 明文；config.yaml 里的是 bcrypt 哈希，不能用）。"
echo "跑一次 verify-prod.sh 做真实对话验证。旧插件与配置备份在 $BAKDIR"
