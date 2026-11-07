#!/usr/bin/env python3
"""把 Loomy 插件的模型能力同步进 DSH 的 profile 配置。

背景：CPA 的 /v1/models 只返回 {id, object, owned_by}，不带模态信息；
DSH 这类客户端因此把 `lm/*` 当成纯文本模型，读图时会直接拒绝。
插件自己的 /v0/management/loomy/models 是带 `vision` 的，本脚本用它来补
profile 里缺失的 `input:` 声明。

用法：
    export LOOMY_MGMT_KEY='<明文管理密钥>'
    python3 sync-dsh-models.py --profile ~/.dsh/profiles/desktop/cordis.patch.yml
    python3 sync-dsh-models.py --profile ... --dry-run

只改以 `lm/` 开头的模型块，其余 provider 原样保留；写回前自动备份。
"""
import argparse
import json
import os
import re
import shutil
import sys
import time
import urllib.request


def fetch_models(base, key):
    url = base.rstrip("/") + "/v0/management/loomy/models"
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + key})
    with urllib.request.urlopen(req, timeout=20) as resp:
        payload = json.load(resp)
    rows = payload.get("items") or payload.get("models") or []
    return {r["id"]: bool(r.get("vision")) for r in rows if r.get("id")}


def split_blocks(lines):
    """Return [(start_index, end_index, id, indent)] for '- id: X' blocks."""
    starts = []
    for i, line in enumerate(lines):
        m = re.match(r"^(\s*)-\s+id:\s*(\S+)\s*$", line)
        if m:
            starts.append((i, m.group(2), len(m.group(1))))
    out = []
    for idx, (start, ident, indent) in enumerate(starts):
        end = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
        out.append((start, end, ident, indent))
    return out


def patch_block(lines, start, end, indent, want_image):
    """Ensure the block declares input: [text] (+ image). Returns (new_lines, changed)."""
    block = lines[start:end]
    body = block[1:]
    inputs = []
    input_at = None
    i = 0
    while i < len(body):
        if re.match(r"^\s+input:\s*$", body[i]):
            input_at = i
            j = i + 1
            while j < len(body) and re.match(r"^\s+-\s+\w+\s*$", body[j]):
                inputs.append(body[j].strip()[2:])
                j += 1
            break
        i += 1

    target = ["text"] + (["image"] if want_image else [])
    if input_at is not None and inputs == target:
        return lines, False

    pad = " " * (indent + 2)
    if input_at is not None:
        j = input_at + 1
        while j < len(body) and re.match(r"^\s+-\s+\w+\s*$", body[j]):
            j += 1
        new_body = body[:input_at] + [pad + "input:"] + [pad + "  - " + k for k in target] + body[j:]
    else:
        new_body = body + [pad + "input:"] + [pad + "  - " + k for k in target]

    return lines[:start + 1] + new_body + lines[end:], True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", required=True)
    ap.add_argument("--url", default="http://127.0.0.1:8317")
    ap.add_argument("--key", default=os.environ.get("LOOMY_MGMT_KEY", ""))
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    if not args.key:
        print("需要管理密钥：export LOOMY_MGMT_KEY='<明文 key>'", file=sys.stderr)
        return 2
    profile = os.path.expanduser(args.profile)
    if not os.path.isfile(profile):
        print("找不到 profile: " + profile, file=sys.stderr)
        return 2

    try:
        vision = fetch_models(args.url, args.key)
    except Exception as exc:  # noqa: BLE001
        print("读取插件模型清单失败: %s" % exc, file=sys.stderr)
        return 1
    if not vision:
        print("插件没有返回任何模型", file=sys.stderr)
        return 1

    lines = open(profile, encoding="utf-8").read().split("\n")
    changed, report = False, []
    # 从后往前改，避免行号漂移
    for start, end, ident, indent in reversed(split_blocks(lines)):
        if not ident.startswith("lm/"):
            continue
        bare = ident[3:]  # 管理接口返回的 id 不带 lm/ 前缀
        if bare not in vision:
            continue
        want = vision[bare]
        before = lines[start:end]
        lines, did = patch_block(lines, start, end, indent, want)
        if did:
            changed = True
            report.append("%s -> %s" % (ident, "text+image" if want else "text"))

    if not changed:
        print("已是最新，无需修改")
        return 0

    print("将修改:")
    for r in sorted(report):
        print("  " + r)
    if args.dry_run:
        print("(dry-run，不写文件)")
        return 0

    backup = profile + ".bak-sync-" + time.strftime("%Y%m%d%H%M%S")
    shutil.copy2(profile, backup)
    open(profile, "w", encoding="utf-8").write("\n".join(lines))
    print("已写入 %s（备份 %s）" % (profile, os.path.basename(backup)))
    print("重启 DSH 后生效。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
