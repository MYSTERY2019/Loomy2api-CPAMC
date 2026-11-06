// Loomy provider plugin for CLIProxyAPI.
//
// One Loomy account == one auth file. The host scheduler rotates/cooldowns between
// auth files, so this plugin only implements:
//   auth.parse    - accept {"session","phone","password"} as a credential
//   auth.refresh  - password re-login (unattended) or session probe
//   executor      - forward chat completions with Bearer session
//   management    - send SMS code / login / add-by-password / accounts / points
//   quota         - daily + permanent points for the management UI
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
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

func md5Sum(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}

func hmacSHA1Base64(secret, payload string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// rfc3986Escape matches encodeURIComponent plus the !'()* set.
func rfc3986Escape(seg string) string {
	escaped := url.QueryEscape(seg)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	replacer := strings.NewReplacer("!", "%21", "'", "%27", "(", "%28", ")", "%29", "*", "%2A")
	return replacer.Replace(escaped)
}

const (
	abiVersion   = 1
	providerKey  = "loomy"
	pluginName   = "loomy-accounts"
	pluginVer    = "1.0.0"
	modelPrefix  = "lm/"
	loomyVersion = "0.9.39"

	defaultBase = "https://loomyad.xunfei.cn/api/v1"
	accountBase = "https://account.xfinfr.com"
	accessKeyID = "2thryby66wxi53sk"
	accessSecret = "zsak6eadrbawz683wf5r3m2snrwj868r"
	appID        = "GM3LOOMY"
	sessionTTL   = 14 * 24 * 3600

	chatPath   = "/chat/completions"
	modelsPath = "/models"
	pointsPath = "/points/records"
	imagePath  = "/images/generations"
)

var (
	baseURL  = defaultBase
	proxyURL = ""
)

func gateway(path string) string { return baseURL + path }

func parseConfig(raw []byte) {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kv := strings.SplitN(trimmed, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		switch key {
		case "base-url", "base_url":
			if val != "" {
				baseURL = strings.TrimRight(val, "/")
			}
		case "panel-token", "panel_token":
			panelToken = val
		case "proxy-url", "proxy_url":
			proxyURL = val
		}
	}
}

func httpClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{}
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// ---- RPC envelope ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type loomyAuth struct {
	Type      string `json:"type"`
	Phone     string `json:"phone,omitempty"`
	Password  string `json:"password,omitempty"`
	Session   string `json:"session"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Note      string `json:"note,omitempty"`
}

type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName,omitempty"`
	Label            string            `json:"Label,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON,omitempty"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

// ---- request log (in-memory ring, resets on restart) ----

type reqLog struct {
	Time    string `json:"time"`
	Account string `json:"account"`
	Model   string `json:"model"`
	Status  int    `json:"status"`
	MS      int64  `json:"ms"`
	Points  int64  `json:"points"`
}

var (
	reqMu      sync.Mutex
	reqLogs    []reqLog
	panelToken string
)

func recordRequest(account, model string, status int, ms, points int64) {
	reqMu.Lock()
	defer reqMu.Unlock()
	reqLogs = append([]reqLog{{
		Time:    time.Now().Format("15:04:05"),
		Account: account,
		Model:   model,
		Status:  status,
		MS:      ms,
		Points:  points,
	}}, reqLogs...)
	if len(reqLogs) > 30 {
		reqLogs = reqLogs[:30]
	}
}

func snapshotRequests() []reqLog {
	reqMu.Lock()
	defer reqMu.Unlock()
	out := make([]reqLog, len(reqLogs))
	copy(out, reqLogs)
	return out
}

var _ = strings.TrimSpace
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
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 400))
		return 1
	}
	body := []byte{}
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, _ := handleMethod(C.GoString(method), body)
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

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

// ---- Dispatch ----

func handleMethod(method string, body []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		parseConfig(body)
		return okEnvelope(map[string]any{
			"schema_version": 1,
			"metadata": map[string]any{
				"Name":             pluginName,
				"Version":          pluginVer,
				"Author":           "local",
				"GitHubRepository": "https://github.com/Patrick130306/loomy2api",
				"Logo":             "",
				"ConfigFields": []any{
					map[string]any{"Name": "base-url", "Type": "string", "Description": "Loomy gateway base URL"},
					map[string]any{"Name": "proxy-url", "Type": "string", "Description": "Optional HTTP proxy for the gateway"},
				},
			},
			"capabilities": map[string]any{
				"auth_provider":           true,
				"model_registrar":         true,
				"executor":                true,
				"executor_model_scope":    "both",
				"executor_input_formats":  []string{"chat-completions", "images"},
				"executor_output_formats": []string{"chat-completions", "images"},
				"management_api":          true,
				"quota_provider":          true,
			},
		})
	case "plugin.quiesce", "plugin.shutdown":
		return okEnvelope(map[string]any{})

	case "auth.identifier":
		return okEnvelope(map[string]any{"identifier": providerKey})
	case "auth.parse":
		return handleAuthParse(body)
	case "auth.refresh":
		return handleAuthRefresh(body)
	case "auth.login.start":
		return errorEnvelope("unsupported", "use POST /v0/management/loomy/login instead", 400), nil
	case "auth.login.poll":
		return errorEnvelope("unsupported", "no interactive login; use the management API", 400), nil

	case "model.register", "model.static":
		models := staticChatModels()
		return okEnvelope(map[string]any{"Provider": providerKey, "Models": models})
	case "model.for_auth":
		return handleModelsForAuth(body)

	case "executor.identifier":
		return okEnvelope(map[string]any{"identifier": providerKey})
	case "executor.execute":
		return handleExecute(body, false)
	case "executor.execute_stream":
		return handleExecute(body, true)
	case "executor.count_tokens":
		return okEnvelope(map[string]any{"Payload": b64(`{"total_tokens":0}`)})

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
		return errorEnvelope("unknown_method", "unknown method: "+method, 400), nil
	}
}

// staticImageModels lists the gateway's image generators (/v1/images/generations).
func staticImageModels() []map[string]any {
	ids := []string{"Hy-Image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro"}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"ID":                         modelPrefix + id,
			"Object":                     "model",
			"OwnedBy":                    providerKey,
			"Type":                       "image",
			"Name":                       id,
			"DisplayName":                id,
			"SupportedGenerationMethods": []string{"images.generations"},
			"SupportedInputModalities":   []string{"text"},
			"SupportedOutputModalities":  []string{"image"},
		})
	}
	return out
}

// fallbackModels is used before any credential is available. Only free-tier safe ids.
func fallbackModels() []map[string]any {
	return []map[string]any{modelEntry("spark-x2.6", 1048576, false)}
}

// staticChatModels mirrors the chat-capable ids the Loomy gateway advertises
// (2026-11-06 snapshot). Per-auth discovery via model.for_auth overrides this.
func staticChatModels() []map[string]any {
	type spec struct {
		id     string
		ctx    int64
		vision bool
	}
	specs := []spec{
		{"spark-x2.6", 1048576, false},
		{"DeepSeek-V4.1-Flash", 1048576, true},
		{"deepseek-v4-flash-0731", 1048576, false},
		{"GLM-5.3-Flash", 1048576, true},
		{"qwen3.8-flash", 1000000, true},
		{"mimo-v2.5", 1048576, true},
	}
	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		out = append(out, modelEntry(s.id, s.ctx, s.vision))
	}
	return out
}

func modelEntry(id string, ctx int64, vision bool) map[string]any {
	entry := map[string]any{
		"ID":                         modelPrefix + id,
		"Object":                     "model",
		"OwnedBy":                    providerKey,
		"Type":                       "chat",
		"Name":                       id,
		"DisplayName":                id,
		"ContextLength":              ctx,
		"SupportedGenerationMethods": []string{"chat.completions"},
	}
	if vision {
		entry["SupportedInputModalities"] = []string{"text", "image"}
	}
	return entry
}

// handleModelsForAuth asks the gateway what this account can actually use.
func handleModelsForAuth(body []byte) ([]byte, error) {
	var req struct {
		AuthID      string
		StorageJSON []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return okEnvelope(map[string]any{"Provider": providerKey, "Models": fallbackModels()})
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil || auth.Session == "" {
		return okEnvelope(map[string]any{"Provider": providerKey, "Models": fallbackModels()})
	}
	models, err := fetchModels(auth.Session)
	if err != nil || len(models) == 0 {
		return okEnvelope(map[string]any{"Provider": providerKey, "Models": fallbackModels()})
	}
	return okEnvelope(map[string]any{"Provider": providerKey, "Models": models})
}

func fetchModels(session string) ([]map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, gateway(modelsPath), nil)
	setAuthHeaders(req, session)
	resp, err := httpClient(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 262144))
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
			Capabilities  struct {
				Vision         bool `json:"vision"`
				FunctionCalling bool `json:"function_calling"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" || m.ID == "auto" {
			continue
		}
		entry := modelEntry(m.ID, m.ContextLength, m.Capabilities.Vision)
		if m.Name != "" {
			entry["DisplayName"] = m.Name
		}
		out = append(out, entry)
	}
	return out, nil
}

// ---- auth ----

func handleAuthParse(body []byte) ([]byte, error) {
	var req struct {
		FileName string
		RawJSON  []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400), nil
	}
	auth, err := parseAuthJSON(req.RawJSON)
	if err != nil {
		return okEnvelope(map[string]any{"Handled": false})
	}
	return okEnvelope(map[string]any{"Handled": true, "Auth": toAuthData(auth, req.FileName, "")})
}

func toAuthData(auth *loomyAuth, fileName, authID string) authData {
	storage, _ := json.Marshal(auth)
	label := "Loomy"
	if auth.Phone != "" {
		label = fmt.Sprintf("Loomy (%s****%s)", prefix(auth.Phone, 3), suffix(auth.Phone, 4))
	}
	id := authID
	if id == "" {
		id = auth.Phone
	}
	if id == "" {
		id = prefix(auth.Session, 8)
	}
	return authData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		StorageJSON: storage,
		Metadata:    map[string]any{"type": providerKey, "has_password": auth.Password != ""},
		Attributes:  map[string]string{"type": providerKey},
	}
}

func parseAuthJSON(raw []byte) (*loomyAuth, error) {
	var auth loomyAuth
	if err := json.Unmarshal(raw, &auth); err == nil && auth.Session != "" {
		if auth.Type == "" {
			auth.Type = providerKey
		}
		return &auth, nil
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if len(s) >= 16 && !strings.ContainsAny(s, "{}\n") {
		return &loomyAuth{Type: providerKey, Session: s}, nil
	}
	return nil, fmt.Errorf("no session")
}

// handleAuthRefresh renews with password when available, otherwise probes.
func handleAuthRefresh(body []byte) ([]byte, error) {
	var req struct {
		AuthID      string
		StorageJSON []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400), nil
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", "credential has no session", 401), nil
	}

	next := time.Now().Add(48 * time.Hour)
	note := "session ok"

	if auth.Password != "" && auth.Phone != "" {
		if session, err := loginByPassword(auth.Phone, auth.Password); err == nil {
			auth.Session = session
			auth.ExpiresAt = time.Now().Add(sessionTTL * time.Second).Format(time.RFC3339)
			next = time.Now().Add(10 * 24 * time.Hour)
			note = "renewed by password"
		} else {
			note = "password login failed: " + err.Error()
			next = time.Now().Add(30 * time.Minute)
		}
	} else {
		status, _ := probeSession(auth.Session)
		if status != http.StatusOK {
			note = fmt.Sprintf("session invalid (status=%d)", status)
			next = time.Now().Add(10 * time.Minute)
		}
	}

	ad := toAuthData(auth, "", req.AuthID)
	ad.Metadata["refresh"] = note
	ad.NextRefreshAfter = next
	return okEnvelope(map[string]any{"Auth": ad, "NextRefreshAfter": next})
}

func probeSession(session string) (int, error) {
	req, _ := http.NewRequest(http.MethodGet, gateway(modelsPath), nil)
	setAuthHeaders(req, session)
	resp, err := httpClient(20 * time.Second).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if code, _ := appErr(raw); code != "" {
		return http.StatusUnauthorized, nil
	}
	return resp.StatusCode, nil
}

func appErr(raw []byte) (string, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", ""
	}
	var probe struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return "", ""
	}
	switch probe.Code {
	case "", "0", "000000", "200":
		return "", ""
	}
	return probe.Code, probe.Desc
}

// ---- account service (account.xfinfr.com) ----

func accountRequest(path string, param map[string]any) (map[string]any, error) {
	body := map[string]any{
		"base": map[string]any{
			"appid":   appID,
			"modelid": "Web",
			"version": "1.0.0",
			"devid":   "web",
			"ua":      "Loomy|Desktop|Electron|Windows",
			"traceid": randomHex(16),
		},
		"param": param,
	}
	raw, _ := json.Marshal(body)
	date := time.Now().UTC().Format(http.TimeFormat)
	nonce := uuidish()
	sig := signAccount(http.MethodPost, path, string(raw), date, nonce)

	req, _ := http.NewRequest(http.MethodPost, accountBase+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "account "+accessKeyID+":"+sig)
	req.Header.Set("Date", date)
	req.Header.Set("Nonce", nonce)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-MD5", contentMD5(raw))
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient(25 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	var out struct {
		Code string         `json:"code"`
		Desc string         `json:"desc"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("account service: bad json (http %d)", resp.StatusCode)
	}
	if out.Code != "" && out.Code != "000000" {
		return nil, fmt.Errorf("account service %s: %s", out.Code, out.Desc)
	}
	return out.Data, nil
}

func sendSMS(phone string) (string, error) {
	data, err := accountRequest("/login/phone/sendMsgCode", map[string]any{"ccode": "86", "phone": phone, "expire": 300})
	if err != nil {
		return "", err
	}
	msgid, _ := data["msgid"].(string)
	if msgid == "" {
		return "", fmt.Errorf("no msgid returned")
	}
	return msgid, nil
}

func loginBySMS(phone, code, msgid string) (string, string, error) {
	data, err := accountRequest("/login/phone/checkCode", map[string]any{
		"ccode": "86", "phone": phone, "mcode": code, "msgid": msgid, "expire": sessionTTL,
	})
	if err != nil {
		return "", "", err
	}
	session, _ := data["session"].(string)
	userid, _ := data["userid"].(string)
	if session == "" {
		return "", "", fmt.Errorf("no session returned")
	}
	return session, userid, nil
}

func loginByPassword(phone, password string) (string, error) {
	keyData, err := accountRequest("/login/account/getPuKey", nil)
	if err != nil {
		return "", err
	}
	pukey, _ := keyData["pukey"].(string)
	rcode, _ := keyData["rcode"].(string)
	if pukey == "" {
		return "", fmt.Errorf("no public key")
	}
	encrypted, err := rsaEncryptPassword(pukey, password)
	if err != nil {
		return "", err
	}
	data, err := accountRequest("/login/account/byPwd", map[string]any{
		"loginid": phone, "password": encrypted, "rcode": rcode, "type": 1, "expire": sessionTTL,
	})
	if err != nil {
		return "", err
	}
	session, _ := data["session"].(string)
	if session == "" {
		return "", fmt.Errorf("no session returned")
	}
	return session, nil
}

// rsaEncryptPassword uses the 1024-bit DER public key handed out by getPuKey.
func rsaEncryptPassword(pukeyB64, password string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(pukeyB64)
	if err != nil {
		return "", err
	}
	var pub *rsa.PublicKey
	if parsed, errPKIX := x509.ParsePKIXPublicKey(der); errPKIX == nil {
		if rp, ok := parsed.(*rsa.PublicKey); ok {
			pub = rp
		}
	}
	if pub == nil {
		if rp, errPKCS1 := x509.ParsePKCS1PublicKey(der); errPKCS1 == nil {
			pub = rp
		}
	}
	if pub == nil {
		return "", fmt.Errorf("unsupported public key")
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

func contentMD5(body []byte) string {
	sum := md5Sum(body)
	return base64.StdEncoding.EncodeToString(sum)
}

func signAccount(method, path, body, date, nonce string) string {
	parts := []string{method, escapePath(path), "", contentMD5([]byte(body)), "application/json", date, nonce, "", ""}
	return hmacSHA1Base64(accessSecret, strings.Join(parts, "\n"))
}

func escapePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		segs[i] = rfc3986Escape(seg)
	}
	return strings.Join(segs, "/")
}

// ---- executor ----

func handleExecute(body []byte, stream bool) ([]byte, error) {
	var req struct {
		AuthID      string
		Model       string
		Stream      bool
		Payload     []byte
		StorageJSON []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400), nil
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil || auth.Session == "" {
		return errorEnvelope("invalid_auth", "selected auth has no session", 401), nil
	}
	if isImageRequest(req.Payload) {
		return handleImageExecute(req.Payload, auth.Session)
	}

	started := time.Now()
	st := http.StatusOK
	defer func() {
		recordRequest(maskPhone(auth.Phone), req.Model, st, time.Since(started).Milliseconds(), 0)
	}()
	if _, statErr := os.Stat("/tmp/loomy-debug"); statErr == nil {
		f, _ := os.OpenFile("/tmp/loomy-payload.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintf(f, "--- stream=%v model=%s\n%s\n", stream, req.Model, string(req.Payload))
			f.Close()
		}
	}

	upReq, _ := http.NewRequest(http.MethodPost, gateway(chatPath), bytes.NewReader(escapeNonASCII(stripModelPrefix(req.Payload))))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	setAuthHeaders(upReq, auth.Session)

	resp, err := httpClient(300 * time.Second).Do(upReq)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), 502), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		st = resp.StatusCode
		return errorEnvelope("auth_failed", string(msg), resp.StatusCode), nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		st = http.StatusTooManyRequests
		return errorEnvelope("rate_limited", string(msg), 429), nil
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		st = resp.StatusCode
		return errorEnvelope("upstream_error", string(msg), resp.StatusCode), nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorEnvelope("read_error", err.Error(), 502), nil
	}
	if code, desc := appErr(raw); code != "" {
		status := 502
		switch code {
		case "100001", "100002", "100003":
			status = http.StatusUnauthorized
		}
		st = status
		return errorEnvelope("upstream_"+code, desc, status), nil
	}

	if !stream {
		return okEnvelope(map[string]any{
			"Payload": raw,
			"Headers": map[string][]string{"content-type": {contentType(resp)}},
		})
	}
	chunks := make([]map[string]any, 0, 16)
	for _, part := range splitSSE(raw) {
		chunks = append(chunks, map[string]any{"Payload": part})
	}
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"content-type": {"text/event-stream"}},
		"chunks":  chunks,
	})
}

// stripModelPrefix removes the "lm/" routing prefix from the payload model name.
// Only the "model" key is touched; every other raw field is preserved byte-for-byte.
func stripModelPrefix(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	modelRaw, ok := fields["model"]
	if !ok {
		return raw
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil {
		return raw
	}
	if !strings.HasPrefix(model, modelPrefix) {
		return raw
	}
	bare, err := json.Marshal(strings.TrimPrefix(model, modelPrefix))
	if err != nil {
		return raw
	}
	fields["model"] = bare
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// escapeNonASCII rewrites raw UTF-8 into \uXXXX escapes. The Loomy gateway
// answers 400 "请求参数错误" when the JSON body carries raw multibyte characters,
// while the same text sent as \uXXXX escapes is accepted.
func escapeNonASCII(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	ascii := true
	for _, b := range in {
		if b >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return in
	}
	var out bytes.Buffer
	out.Grow(len(in) * 2)
	for i := 0; i < len(in); {
		if in[i] < 0x80 {
			out.WriteByte(in[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(in[i:])
		if r == utf8.RuneError && size == 1 {
			out.WriteByte(in[i])
			i++
			continue
		}
		i += size
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", hi, lo)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}

// isImageRequest detects an image generation body (prompt, no messages).
func isImageRequest(payload []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false
	}
	_, hasPrompt := fields["prompt"]
	_, hasMessages := fields["messages"]
	return hasPrompt && !hasMessages
}

// handleImageExecute forwards an image request to the gateway and returns its body.
func handleImageExecute(payload []byte, session string) ([]byte, error) {
	body := escapeNonASCII(stripModelPrefix(payload))
	upReq, _ := http.NewRequest(http.MethodPost, gateway(imagePath), bytes.NewReader(body))
	upReq.Header.Set("Content-Type", "application/json")
	setAuthHeaders(upReq, session)

	resp, err := httpClient(180 * time.Second).Do(upReq)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), 502), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if code, desc := appErr(raw); code != "" {
		status := 502
		switch code {
		case "100001", "100002", "100003":
			status = http.StatusUnauthorized
		}
		return errorEnvelope("upstream_"+code, desc, status), nil
	}
	if resp.StatusCode >= 400 {
		return errorEnvelope("upstream_error", string(raw[:min(len(raw), 512)]), resp.StatusCode), nil
	}
	return okEnvelope(map[string]any{
		"Payload": raw,
		"Headers": map[string][]string{"content-type": {contentType(resp)}},
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func setAuthHeaders(req *http.Request, session string) {	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("token", session)
	req.Header.Set("traceparent", traceparent())
	req.Header.Set("loomy-version", loomyVersion)
}

func splitSSE(raw []byte) [][]byte {
	parts := bytes.Split(raw, []byte("\n\n"))
	out := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(bytes.TrimSpace(part)) == 0 {
			continue
		}
		out = append(out, append(append([]byte{}, part...), '\n', '\n'))
	}
	if len(out) == 0 {
		out = append(out, raw)
	}
	return out
}

func contentType(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json"
}

// ---- quota ----

func handleQuotaFetch(body []byte) ([]byte, error) {
	var req struct {
		AuthID      string
		StorageJSON []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400), nil
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil || auth.Session == "" {
		return errorEnvelope("invalid_auth", "no session", 401), nil
	}
	points, err := fetchPoints(auth.Session)
	if err != nil {
		return errorEnvelope("quota_error", err.Error(), 502), nil
	}
	daily := 5000.0
	if v, ok := points["dailyBalance"].(float64); ok && v > 0 {
		daily = v
	}
	permanent, _ := points["balance"].(float64)
	available, _ := points["availableBalance"].(float64)

	return okEnvelope(map[string]any{
		"groups": []map[string]any{{
			"displayName": "Loomy points",
			"buckets": []map[string]any{{
				"window":            "daily",
				"remainingFraction": clamp01(daily / 5000.0),
				"description":       fmt.Sprintf("today %.0f / 5000", daily),
			}},
		}},
		"summary": []map[string]any{
			{"key": "available", "label": "Available", "value": available, "unit": "pts"},
			{"key": "permanent", "label": "Permanent", "value": permanent, "unit": "pts"},
			{"key": "daily", "label": "Today", "value": daily, "unit": "pts"},
		},
	})
}

func fetchPoints(session string) (map[string]any, error) {
	req, _ := http.NewRequest(http.MethodGet, gateway(pointsPath), nil)
	setAuthHeaders(req, session)
	resp, err := httpClient(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	var out struct {
		Code string         `json:"code"`
		Desc string         `json:"desc"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad points response")
	}
	if out.Code != "" && out.Code != "000000" {
		return nil, fmt.Errorf("%s: %s", out.Code, out.Desc)
	}
	return out.Data, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---- management API ----

func handleManagementRegister() ([]byte, error) {
	return okEnvelope(map[string]any{
		"Routes": []map[string]any{
			{"Method": "POST", "Path": "/loomy/send-code", "Description": "Send a Loomy login SMS code"},
			{"Method": "POST", "Path": "/loomy/login", "Description": "Verify SMS code and save the account"},
			{"Method": "POST", "Path": "/loomy/add", "Description": "Add an account with phone + password"},
			{"Method": "GET", "Path": "/loomy/accounts", "Description": "List Loomy auth files"},
			{"Method": "GET", "Path": "/loomy/points", "Description": "Points for one auth (?auth_id=)"},
		},
		"Resources": []map[string]any{
			{"Path": "/accounts", "Menu": "Loomy 账号池", "Description": "账号列表、余额与刷新"},
			{"Path": "/accounts/data", "Menu": "", "Description": "JSON 数据"},
		},
	})
}

func handleManagement(body []byte) ([]byte, error) {
	var req struct {
		Method string
		Path   string
		Query  map[string][]string
		Body   []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400), nil
	}

	if strings.Contains(req.Path, "/resource/") {
		return handlePanel(req.Method, req.Path, req.Query)
	}

	switch {
	case req.Method == "POST" && strings.HasSuffix(req.Path, "/send-code"):
		var in struct {
			Phone string `json:"phone"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil || len(in.Phone) != 11 {
			return mgmtJSON(400, map[string]any{"error": "phone required (11 digits)"}), nil
		}
		msgid, err := sendSMS(in.Phone)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()}), nil
		}
		return mgmtJSON(200, map[string]any{"msgid": msgid, "phone": in.Phone}), nil

	case req.Method == "POST" && strings.HasSuffix(req.Path, "/login"):
		var in struct {
			Phone string `json:"phone"`
			Code  string `json:"code"`
			Msgid string `json:"msgid"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil || in.Phone == "" || in.Code == "" || in.Msgid == "" {
			return mgmtJSON(400, map[string]any{"error": "phone, code, msgid required"}), nil
		}
		session, userid, err := loginBySMS(in.Phone, in.Code, in.Msgid)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()}), nil
		}
		return saveAccount(&loomyAuth{Type: providerKey, Phone: in.Phone, Session: session, UserID: userid})

	case req.Method == "POST" && strings.HasSuffix(req.Path, "/add"):
		var in struct {
			Phone    string `json:"phone"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil || in.Phone == "" || in.Password == "" {
			return mgmtJSON(400, map[string]any{"error": "phone and password required"}), nil
		}
		session, err := loginByPassword(in.Phone, in.Password)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()}), nil
		}
		return saveAccount(&loomyAuth{Type: providerKey, Phone: in.Phone, Password: in.Password, Session: session})

	case req.Method == "GET" && strings.HasSuffix(req.Path, "/accounts"):
		list, err := callHost("host.auth.list", map[string]any{})
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()}), nil
		}
		return mgmtJSON(200, map[string]any{"auths": json.RawMessage(list)}), nil

	case req.Method == "GET" && strings.HasSuffix(req.Path, "/points"):
		authIndex := ""
		if v, ok := req.Query["auth_index"]; ok && len(v) > 0 {
			authIndex = v[0]
		}
		list, errList := callHost("host.auth.list", map[string]any{})
		if errList != nil {
			return mgmtJSON(502, map[string]any{"error": errList.Error()}), nil
		}
		var listing struct {
			Files []struct {
				AuthIndex string `json:"auth_index"`
				Path      string `json:"path"`
			} `json:"files"`
		}
		_ = json.Unmarshal(list, &listing)
		path := ""
		for _, f := range listing.Files {
			if authIndex == "" || f.AuthIndex == authIndex {
				path = f.Path
				break
			}
		}
		if path == "" {
			return mgmtJSON(404, map[string]any{"error": "auth not found"}), nil
		}
		rawFile, errRead := os.ReadFile(path)
		if errRead != nil {
			return mgmtJSON(502, map[string]any{"error": errRead.Error()}), nil
		}
		auth, err := parseAuthJSON(rawFile)
		if err != nil {
			return mgmtJSON(400, map[string]any{"error": "auth has no session"}), nil
		}
		points, err := fetchPoints(auth.Session)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()}), nil
		}
		return mgmtJSON(200, points), nil

	default:
		return handlePanel(req.Method, req.Path, req.Query)
	}
}

func saveAccount(auth *loomyAuth) ([]byte, error) {
	name := fmt.Sprintf("loomy-%s.json", suffix(auth.Phone, 4))
	payload, _ := json.Marshal(auth)
	if _, err := callHost("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(payload)}); err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error(), "session": auth.Session}), nil
	}
	return mgmtJSON(200, map[string]any{"saved": name, "phone": auth.Phone, "session": auth.Session}), nil
}

func mgmtJSON(status int, payload any) []byte {
	raw, _ := json.Marshal(payload)
	resp := map[string]any{
		"StatusCode": status,
		"Headers":    map[string][]string{"content-type": {"application/json; charset=utf-8"}},
		"Body":       raw,
	}
	env, _ := okEnvelope(resp)
	return env
}

// ---- helpers ----

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("marshal_error", err.Error(), 500), nil
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}

func b64(s string) []byte {
	return []byte(base64.StdEncoding.EncodeToString([]byte(s)))
}

func traceparent() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	return "00-" + hex.EncodeToString(buf[:16]) + "-" + hex.EncodeToString(buf[16:]) + "-01"
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func uuidish() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func suffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
