import base64, ctypes, json, sys, tempfile
from pathlib import Path

lib = ctypes.CDLL(sys.argv[1] if len(sys.argv) > 1 else str(Path(__file__).resolve().parents[1] / "loomy-linux-arm64.so"))

class Buf(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]

lib.cliproxyPluginCall.restype = ctypes.c_int
lib.cliproxyPluginCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buf)]

def call(method, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    b = Buf()
    rc = lib.cliproxyPluginCall(method.encode(), data, len(data) if data else 0, ctypes.byref(b))
    body = ctypes.string_at(b.ptr, b.len).decode() if b.ptr else "{}"
    try:
        return rc, json.loads(body)
    except Exception:
        return rc, {"raw": body[:400]}

print("== dlopen: ok ==")
# data-dir points at a temp dir so this never touches the live request log
tmp = tempfile.mkdtemp(prefix="loomy-inspect-")
rc, out = call("plugin.register", {"config_yaml": base64.b64encode(("data-dir: " + tmp + "\n").encode()).decode()})
res = out.get("result", {})
print("rc:", rc, "ok:", out.get("ok"))
print("metadata:", json.dumps(res.get("metadata", {}), ensure_ascii=False)[:300])
print("capabilities:", json.dumps(res.get("capabilities", {}), ensure_ascii=False))
print("schema_version:", res.get("schema_version"))
if out.get("error"):
    print("error:", json.dumps(out["error"], ensure_ascii=False))

call("plugin.shutdown")
