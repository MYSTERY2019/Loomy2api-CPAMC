#!/usr/bin/env python3
"""Local-only c-shared ABI smoke test. Never connects to Loomy or reads auth files."""
import base64
import ctypes
import http.server
import json
from pathlib import Path
import sys
import tempfile
import threading

MODELS = ["Hy-Image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro"]
posts = []
mode = "ok"

class Mock(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        assert self.path == "/models", self.path
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps([{"id": m, "type": "image"} for m in MODELS]).encode())

    def do_POST(self):
        assert self.path == "/images/generations", self.path
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        body = json.loads(raw)
        assert self.headers.get("User-Agent") == "Loomy|Desktop|Electron|macOS"
        assert self.headers.get("token") == "mock-image-session"
        assert self.headers.get("Authorization") == "Bearer x"
        assert self.headers.get("Accept") == "application/json"
        assert body["model"] in MODELS and body["n"] == 1
        assert "山".encode() not in raw and b"\\u5c71" in raw
        posts.append(body["model"])
        self.send_response(502 if mode == "uncertain" else 200)
        self.end_headers()
        self.wfile.write(b'bad gateway' if mode == "uncertain" else json.dumps({
            "created": 123, "data": [{"url": "https://example.invalid/fake.png"}],
            "points_consumed": 110,
        }).encode())

class Buf(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]

lib = ctypes.CDLL(str(Path(sys.argv[1]).resolve()))
lib.cliproxyPluginCall.restype = ctypes.c_int
lib.cliproxyPluginCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buf)]
lib.cliproxyPluginFree.argtypes = [ctypes.c_void_p, ctypes.c_size_t]

def call(method, payload=None):
    data = json.dumps(payload).encode() if payload is not None else b""
    out = Buf()
    assert lib.cliproxyPluginCall(method.encode(), data, len(data), ctypes.byref(out)) == 0
    try:
        return json.loads(ctypes.string_at(out.ptr, out.len))
    finally:
        lib.cliproxyPluginFree(out.ptr, out.len)

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Mock)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    with tempfile.TemporaryDirectory(prefix="loomy-images-mock-") as tmp:
        cfg = f"base-url: http://127.0.0.1:{server.server_port}\ndata-dir: {tmp}\n"
        registration = call("plugin.register", {"config_yaml": base64.b64encode(cfg.encode()).decode()})
        assert registration["ok"], registration
        caps = registration["result"]["capabilities"]
        assert "openai-image" in caps["executor_input_formats"]
        storage = base64.b64encode(b'{"session":"mock-image-session","phone":"test-account-0001"}').decode()
        catalog = call("model.for_auth", {"StorageJSON": storage})
        rows = catalog["result"]["Models"]
        assert len(rows) == 3 and all(m["Type"] == "openai-image" for m in rows), catalog
        for model in MODELS:
            payload = json.dumps({"model": "lm/"+model, "prompt": "山水", "n": 1, "size": "1024x1024", "response_format": "url"}).encode()
            request = {"Model": "lm/"+model, "Format": "openai-image", "SourceFormat": "openai-image",
                       "StorageJSON": storage, "Payload": base64.b64encode(payload).decode(),
                       "Metadata": {"request_path": "/v1/images/generations"}}
            result = call("executor.execute", request)
            assert result["ok"], result
            output = json.loads(base64.b64decode(result["result"]["Payload"]))
            assert output["data"][0]["url"] == "https://example.invalid/fake.png", output
            assert output["usage"]["points_consumed"] == 110, output
        mode = "uncertain"
        result = call("executor.execute", request)
        assert not result["ok"] and result["error"]["http_status"] == 400, result
        assert result["error"]["code"] == "image_outcome_unknown", result
        assert len(posts) == 4, posts
        call("plugin.shutdown")
        print(json.dumps({"abi": "passed", "models": len(rows), "mock_generation_posts": len(posts),
                          "uncertain_outcome": "400/no internal generation retry", "live_requests": 0}))
finally:
    server.shutdown()
    server.server_close()
    thread.join()
