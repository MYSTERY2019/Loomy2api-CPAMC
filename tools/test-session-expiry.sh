#!/bin/bash
# 线上实验：把一个账号的 session 改成无效值，看插件和 CPA 怎么处理。
#
# 通过条件（v2.2.0）：客户端请求没有失败，并且事件日志出现该账号的「自动续期成功」。
# 只给警告、不算失败的情况：
#   - 没看到该账号的 401：后台探活可能先发现 session 失效并完成续期，请求根本轮不到它
#   - 出现 kind=risk 事件（session 失效本不该触发风控，看到了请人工确认）
# 背景：网关把失效 session 报成 HTTP 200 + code 1000xx，插件映射为 401（auth_failed），不算风控；
#       有密码的账号由刷新流程用密码重登；客户端请求由 CPA 换号重试，返回 200。
# 注意：这个实验测的是「session 失效 → 重登」这条路径。403/TM.* 风控那条路径不会因为 session 无效而触发，
#       线上要测它需要故障注入（目前只在 mock 上验证过）。
#
# 用法（在 Linux host 上，以运行 CPA 的用户执行）：
#   bash tools/test-session-expiry.sh <path/to/auth.json>
# 可选环境变量：
#   LOOMY_DATA=~/.loomy-plugin   插件数据目录（config 里设了 data-dir 时改成那个）
#   WAIT=90                       最多等多少秒（看到续期成功会立即结束）
#   MAXREQ=6                      最多发几个请求
#   MODEL=lm/spark-x2.6           发请求用的模型（默认免费模型，不耗积分）
# 任何时候 Ctrl-C 或超时，只要 session 还是那个无效值，都会把原文件恢复回去。
set -uo pipefail

AUTH=${1:-}
CFG=${CFG:-${CPA_CONFIG:-$HOME/cliproxy/config.yaml}}
B=${BASE_URL:-${B:-http://127.0.0.1:8317}}
LOOMY_DATA=${LOOMY_DATA:-$HOME/.loomy-plugin}
WAIT=${WAIT:-90}
MAXREQ=${MAXREQ:-6}
MODEL=${MODEL:-lm/spark-x2.6}
EV="$LOOMY_DATA/events.jsonl"
RQ="$LOOMY_DATA/requests.jsonl"

[ -n "$AUTH" ] && [ -f "$AUTH" ] || { echo "用法: bash $0 <auth 文件路径>"; exit 1; }
[ -r "$EV" ] && [ -r "$RQ" ] || { echo "!! 读不到 $EV / $RQ（设置 LOOMY_DATA 为插件的 data-dir）"; exit 1; }

read -r PHONE HASPW < <(python3 - "$AUTH" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
p = d.get("phone", "")
print(p or "-", "yes" if d.get("password") else "no")
PY
)
[ "$PHONE" != "-" ] || { echo "!! 文件里没有 phone 字段"; exit 1; }
[ "$HASPW" = "yes" ] || { echo "!! 这个账号没存密码，session 改坏后无法自动续期，换一个有密码的账号"; exit 1; }
MASK="${PHONE:0:3}****${PHONE:7}"

KEY=$(awk '/^access:/{f=1;next} f&&/^[[:space:]]+- /{gsub(/^[[:space:]]+- /,"");gsub(/"/,"");print;exit}' "$CFG")
[ -n "$KEY" ] || { echo "!! 读不到 access.api-keys"; exit 1; }

STAMP=$(date +%Y%m%d%H%M%S)
BAK="$HOME/loomy-plugin/test-backups/$STAMP"
mkdir -p "$BAK" && chmod 700 "$BAK"
cp -p "$AUTH" "$BAK/" || { echo "!! 备份失败"; exit 1; }
BAD="invalid-test-$STAMP"
EV0=$(wc -l < "$EV"); RQ0=$(wc -l < "$RQ")
T0=$(($(date +%s) * 1000))

restore_if_bad() {
  if grep -q "$BAD" "$AUTH" 2>/dev/null; then
    cp -p "$BAK/$(basename "$AUTH")" "$AUTH" && echo ">> 已恢复原文件（session 仍是测试值）"
  fi
}
trap 'restore_if_bad; exit 130' INT TERM

echo "=== 账号 $MASK，备份在 $BAK ==="
python3 - "$AUTH" "$BAD" <<'PY'
import json, os, sys
path, bad = sys.argv[1], sys.argv[2]
d = json.load(open(path, encoding="utf-8"))
d["session"] = bad
tmp = path + ".tmp"
with open(tmp, "w", encoding="utf-8") as f:
    json.dump(d, f, ensure_ascii=False, indent=2)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY
echo ">> session 已改成无效值 $BAD"

# 统计实验开始后的新事件 / 新请求
new_events()   { tail -n +"$((EV0 + 1))" "$EV"; }
new_requests() { tail -n +"$((RQ0 + 1))" "$RQ"; }
hit_target()   { new_requests | grep -F "\"account\":\"$MASK\"" | grep -q '"status":401'; }
renewed()      { new_events | grep -F "$MASK" | grep -q '续期成功'; }

PAYLOAD="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":8}"
OK=0; FAIL=0; SENT=0
DEADLINE=$(( $(date +%s) + WAIT ))
echo "=== 发请求直到轮到这个账号或看到续期（模型 $MODEL，最多 ${WAIT}s / $MAXREQ 个请求） ==="
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  renewed && break
  if ! hit_target && [ $SENT -lt "$MAXREQ" ]; then
    CODE=$(curl -s -o /dev/null -w "%{http_code}" -m 30 -X POST "$B/v1/chat/completions" \
      -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" --data-binary "$PAYLOAD")
    SENT=$((SENT + 1))
    [ "$CODE" = "200" ] && OK=$((OK + 1)) || FAIL=$((FAIL + 1))
    echo "  请求 #$SENT → $CODE"
    continue
  fi
  sleep 2
done

echo
echo "=== 结果 ==="
PASS=1; WARN=0
if [ "$FAIL" -eq 0 ]; then
  echo "✔ 客户端请求：$OK 个 200，没有失败"
else
  echo "✘ 客户端请求：$OK 个 200，$FAIL 个失败（应全部由 CPA 换号重试成功）"; PASS=0
fi
if renewed; then
  echo "✔ 已自动续期：$(new_events | grep -F "$MASK" | grep '续期成功' | tail -1 | python3 -c 'import json,sys;print(json.loads(sys.stdin.read())["msg"])')"
else
  echo "✘ ${WAIT}s 内没有看到「续期成功」"; PASS=0
  new_events | grep -F "$MASK" | tail -5 | python3 -c '
import json, sys
for l in sys.stdin:
    try: e = json.loads(l); print("   ", e["level"], e["kind"], e["msg"], e.get("detail", "")[:120])
    except Exception: pass'
fi
if hit_target; then
  echo "✔ 该账号收到 401（session 失效被识别为鉴权失败）"
else
  echo "⚠ 没看到该账号的 401：后台探活可能已抢先续期（不算失败）"; WARN=$((WARN + 1))
fi
RISK=$(new_events | grep -c '"kind":"risk"')
if [ "$RISK" = "0" ]; then
  echo "✔ 没有风控事件，熔断未触发"
else
  echo "⚠ 出现 $RISK 条风控事件（请人工确认）："; new_events | grep '"kind":"risk"' | tail -3; WARN=$((WARN + 1))
fi

if grep -q "$BAD" "$AUTH"; then
  restore_if_bad
else
  echo "✔ 文件里已是新的 session（插件续期后写回），不需要恢复；原文件留在 $BAK"
fi
echo
if [ "$PASS" = 1 ]; then
  echo "=== 通过（警告 $WARN 条），用时 $(( ($(date +%s) * 1000 - T0) / 1000 ))s ==="; exit 0
fi
echo "=== 未通过 ==="; exit 1
