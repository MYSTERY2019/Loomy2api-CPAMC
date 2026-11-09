// Loomy provider plugin for CLIProxyAPI (CPA).
//
// One Loomy account == one auth file. CPA's scheduler rotates / cools down
// between auth files; this plugin implements:
//
//	auth.parse / auth.refresh  - session credential, password re-login
//	executor                   - chat completions (streaming) and JSON image generation
//	model registrar            - per-auth model discovery
//	quota                      - daily + permanent points
//	management API             - console data + account onboarding (management-key protected)
//	resource /accounts         - static console shell (no data; data needs the management key)
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"unsafe"
)

const (
	abiVersion    = 1
	schemaVersion = 6 // raw JSON management responses (no HTML escaping)
	providerKey   = "loomy"
	pluginName    = "loomy-accounts"
	pluginVer     = "1.0.2"
	modelPrefix   = "lm/"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	// A panic escaping a c-shared export aborts the whole CPA process.
	// Convert every panic into an error envelope instead.
	defer func() {
		if r := recover(); r != nil {
			logEvent("error", "panic", fmt.Sprintf("%v", r), string(debug.Stack()))
			writeResponse(response, errorEnvelope("plugin_panic", fmt.Sprintf("loomy plugin panic: %v", r), 500))
			rc = 0
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 400))
		return 1
	}
	body := []byte{}
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, handleMethod(C.GoString(method), body))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = size
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	stopScheduler()
	flushStore()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr != nil {
		response.ptr = ptr
		response.len = C.size_t(len(raw))
	}
}

// callHost invokes a host callback. Safe to call from any goroutine.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	reqPtr := (*C.uint8_t)(C.CBytes(rawPayload))
	defer C.free(unsafe.Pointer(reqPtr))
	if C.call_host_api(cMethod, reqPtr, C.size_t(len(rawPayload)), &response) != 0 || response.ptr == nil {
		return nil, fmt.Errorf("host call %s failed", method)
	}
	defer C.free_host_buffer(response.ptr, response.len)
	buf := C.GoBytes(response.ptr, C.int(response.len))
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *envelopeError  `json:"error"`
	}
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host call %s rejected", method)
	}
	return env.Result, nil
}

// goSafe runs fn in a goroutine that can never crash the host.
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logEvent("error", "panic", fmt.Sprintf("%s: %v", name, r), string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// ---- dispatch ----

func handleMethod(method string, body []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		parseConfig(body)
		initStore()
		startScheduler()
		return okEnvelope(registration())
	case "plugin.quiesce", "plugin.shutdown":
		if method == "plugin.shutdown" {
			stopScheduler()
		}
		flushStore()
		return okEnvelope(map[string]any{})

	case "auth.identifier":
		return okEnvelope(map[string]any{"identifier": providerKey})
	case "auth.parse":
		return handleAuthParse(body)
	case "auth.refresh":
		return handleAuthRefresh(body)
	case "auth.login.start", "auth.login.poll":
		return errorEnvelope("unsupported", "use the Loomy console (加号) or POST /v0/management/loomy/add", 400)

	case "model.register", "model.static":
		return okEnvelope(map[string]any{"Provider": providerKey, "Models": staticChatModels()})
	case "model.for_auth":
		return handleModelsForAuth(body)

	case "executor.identifier":
		return okEnvelope(map[string]any{"identifier": providerKey})
	case "executor.execute":
		return handleExecute(body, false)
	case "executor.execute_stream":
		return handleExecute(body, true)
	case "executor.count_tokens":
		return okEnvelope(map[string]any{"Payload": []byte(`{"total_tokens":0}`)})

	case "quota.identifier":
		return okEnvelope(map[string]any{"identifier": providerKey})
	case "quota.describe":
		return okEnvelope(map[string]any{
			"supported_providers": []string{providerKey},
			"display_name":        "Loomy",
			"supports_reset":      false,
		})
	case "quota.fetch":
		return handleQuotaFetch(body)

	case "management.register":
		return handleManagementRegister()
	case "management.handle":
		return handleManagement(body)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 400)
	}
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": schemaVersion,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVer,
			"Author":           "Loomy2api contributors",
			"GitHubRepository": "",
			"Logo":             "",
			"ConfigFields": []any{
				map[string]any{"Name": "base-url", "Type": "string", "Description": "Loomy gateway base URL"},
				map[string]any{"Name": "proxy-url", "Type": "string", "Description": "Optional HTTP(S)/SOCKS5 proxy for upstream calls"},
				map[string]any{"Name": "data-dir", "Type": "string", "Description": "Where request logs / events are persisted (default ~/.loomy-plugin)"},
				map[string]any{"Name": "stream-mode", "Type": "string", "Description": "live (default, true streaming) or buffered (legacy)"},
				map[string]any{"Name": "log-retention-days", "Type": "string", "Description": "Days of request log to keep (default 30)"},
				map[string]any{"Name": "default-reasoning-effort", "Type": "string", "Description": "none|low|medium|high|xhigh — applied when the client sends no reasoning_effort (empty = client decides)"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":   true,
			"model_registrar": true,
			// model_provider makes CPA call model.for_auth per credential; with only
			// model_registrar it registers the static list from model.register and
			// never reads the live catalogue (the v2.1 "only built-in models" bug).
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "both",
			"executor_input_formats":  []string{"chat-completions", "openai-image"},
			"executor_output_formats": []string{"chat-completions", "openai-image"},
			"management_api":          true,
			"quota_provider":          true,
		},
	}
}

// ---- RPC envelope ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("marshal_error", err.Error(), 500)
	}
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}
