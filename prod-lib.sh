#!/bin/bash
# Shared helpers for deploy-prod.sh / verify-prod.sh. Source it; do not run it.
# Copy it to the Linux host next to the two scripts.

# CPA's app log is not in journald: cliproxy.service redirects stdout with
# StandardOutput=append:<file>. Read the path from the unit, fall back to the usual file,
# and use journalctl only when neither is readable.
LOGFILE=$(grep -o "append:[^ ]*" /etc/systemd/system/cliproxy.service 2>/dev/null | head -1 | cut -d: -f2)
[ -r "${LOGFILE:-}" ] || LOGFILE="${CPA_HOME:-$HOME/cliproxy}/cliproxy.log"
[ -r "$LOGFILE" ] || LOGFILE=""

plog() {  # plog N -> last N lines of the CPA app log
  if [ -n "$LOGFILE" ]; then tail -n "$1" "$LOGFILE"
  else journalctl -u cliproxy -n "$1" --no-pager 2>/dev/null; fi
}

so_version() {  # so_version path/to/loomy.so -> the Version the plugin registers with
  # Loads the .so in a throwaway python process with data-dir pointed at a temp dir, so it
  # never touches the live request log; the check-in scheduler waits 30 s and dies with the process.
  local tmp; tmp=$(mktemp -d)
  python3 - "$1" "$tmp" <<'PY' 2>/dev/null
import base64, ctypes, json, sys
class Buf(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]
lib = ctypes.CDLL(sys.argv[1])
lib.cliproxyPluginCall.restype = ctypes.c_int
lib.cliproxyPluginCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buf)]
# CPA passes the plugin config as base64 YAML in config_yaml
cfg = base64.b64encode(("data-dir: " + sys.argv[2] + "\n").encode()).decode()
p = json.dumps({"config_yaml": cfg}).encode()
b = Buf()
lib.cliproxyPluginCall(b"plugin.register", p, len(p), ctypes.byref(b))
out = json.loads(ctypes.string_at(b.ptr, b.len).decode()) if b.ptr else {}
print(out.get("result", {}).get("metadata", {}).get("Version", ""))
s = Buf()
lib.cliproxyPluginCall(b"plugin.shutdown", None, 0, ctypes.byref(s))
PY
  rm -rf "$tmp"
}

is_bcrypt() {  # is_bcrypt "$KEY" -> true when the value is a bcrypt hash, not a usable key
  case "$1" in '$2a$'*|'$2b$'*|'$2y$'*) return 0 ;; *) return 1 ;; esac
}
