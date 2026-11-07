package main

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
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Values reverse-engineered from the Loomy desktop client. They may be rotated
// by the vendor at any time; when every login fails with a signature error,
// update these from a fresh client build.
const (
	loomyVersion       = "0.9.39"
	defaultAccountBase = "https://account.xfinfr.com"

	appID      = "GM3LOOMY"
	sessionTTL = 14 * 24 * 3600
	clientUA   = "Loomy|Desktop|Electron|macOS"

	imagesPath = "/images/generations"
	chatPath   = "/chat/completions"
	modelsPath = "/models"
	pointsPath = "/points/records"
)

var (
	jsonUnmarshal = json.Unmarshal
	jsonMarshal   = json.Marshal
)

var (
	accessKeyID  = os.Getenv("LOOMY_ACCESS_KEY_ID")
	accessSecret = os.Getenv("LOOMY_ACCESS_KEY_SECRET")
)

// ---- gateway (loomyad) ----

func setAuthHeaders(req *http.Request, session string) {
	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("token", session)
	req.Header.Set("traceparent", traceparent())
	req.Header.Set("loomy-version", loomyVersion)
	// The gateway rejects "<product>/<version>" style agents (incl. Go-http-client/1.1)
	// with 403 TM.00000001, so present the official desktop client UA.
	req.Header.Set("User-Agent", clientUA)
}

func gatewayGet(path, session string, timeout time.Duration, limit int64) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, gateway(path), nil)
	if err != nil {
		return nil, 0, err
	}
	setAuthHeaders(req, session)
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return raw, resp.StatusCode, err
}

// appErr detects the gateway's "HTTP 200 + business error code" responses.
func appErr(raw []byte) (string, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", ""
	}
	var probe struct {
		Code json.RawMessage `json:"code"`
		Desc string          `json:"desc"`
		Msg  string          `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil || len(probe.Code) == 0 {
		return "", ""
	}
	code := strings.Trim(string(probe.Code), `"`)
	switch code {
	case "", "0", "000000", "200", "null":
		return "", ""
	}
	desc := probe.Desc
	if desc == "" {
		desc = probe.Msg
	}
	return code, desc
}

// isAuthCode reports gateway codes that mean "session is no longer valid".
func isAuthCode(code string) bool {
	switch code {
	case "100001", "100002", "100003", "401":
		return true
	}
	return false
}

type gatewayModel struct {
	ID            string
	Name          string
	ContextLength int64
	MaxOutput     int64
	Type          string
	// thinking levels as published by the gateway (none|low|medium|high|xhigh)
	ReasoningEfforts []string
	Capabilities     struct {
		Vision          bool
		FunctionCalling bool
	}
}

var (
	modelCacheMu   sync.Mutex
	modelCaches    = map[string]*catalogCache{}
	modelErrLogAt  time.Time
	modelSkipLogAt time.Time
)

// Each session has its own catalogue: accounts do not necessarily have the same models.
// Only hashed session keys are retained as cache identities. Entry locks coalesce requests.
// catalogRequestError preserves auth/rate/risk statuses on the execution preflight.
type catalogRequestError struct {
	status  int
	message string
}

func (e *catalogRequestError) Error() string { return e.message }

type catalogCache struct {
	mu      sync.Mutex
	models  []gatewayModel
	at      time.Time
	retryAt time.Time
	err     error
}

func accountCatalogCache(session string) *catalogCache {
	key := sessionKey(session)
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	e := modelCaches[key]
	if e == nil {
		e = &catalogCache{}
		modelCaches[key] = e
	}
	return e
}

// mergeGatewayModels deduplicates exact upstream IDs, with deterministic ordering.
// The union is for display/statistics only, never an account's routing capabilities.
func mergeGatewayModels(catalogs ...[]gatewayModel) []gatewayModel {
	byID := map[string]gatewayModel{}
	for _, list := range catalogs {
		for _, m := range list {
			if _, exists := byID[m.ID]; !exists {
				byID[m.ID] = m
			}
		}
	}
	out := make([]gatewayModel, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func cachedModelUnion() []gatewayModel {
	modelCacheMu.Lock()
	entries := make([]*catalogCache, 0, len(modelCaches))
	for _, e := range modelCaches {
		entries = append(entries, e)
	}
	modelCacheMu.Unlock()
	var lists [][]gatewayModel
	for _, e := range entries {
		e.mu.Lock()
		if time.Since(e.at) < 10*time.Minute {
			lists = append(lists, e.models)
		}
		e.mu.Unlock()
	}
	return mergeGatewayModels(lists...)
}

// numField reads a positive number that the gateway may send as a JSON number
// or as a string ("1048576"); anything else is 0.
func numField(v any) int64 {
	switch n := v.(type) {
	case float64:
		if n > 0 && n < 1e15 {
			return int64(n)
		}
	case json.Number:
		if f, err := n.Float64(); err == nil && f > 0 && f < 1e15 {
			return int64(f)
		}
	case string:
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(n), &f); err == nil && f > 0 && f < 1e15 {
			return int64(f)
		}
	}
	return 0
}

func boolField(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true") || strings.TrimSpace(b) == "1"
	case float64:
		return b != 0
	}
	return false
}

func strField(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return ""
}

func hasModality(v any, want string) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok && strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}

// parseGatewayModels is tolerant like magpie-loomy's parseModels: the list may
// be {"data":[...]} or a bare array, numbers may be strings, and a record that
// cannot be read is skipped instead of failing the whole catalogue.
func parseGatewayModels(raw []byte) ([]gatewayModel, int, error) {
	var rows []json.RawMessage
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, 0, fmt.Errorf("models: bad json array: %v", err)
		}
	} else {
		var payload struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, 0, fmt.Errorf("models: bad json: %v", err)
		}
		d := bytes.TrimSpace(payload.Data)
		if len(d) > 0 && d[0] == '{' { // {"data":{"list":[...]}} / {"data":{"data":[...]}}
			var inner map[string]json.RawMessage
			if json.Unmarshal(d, &inner) == nil {
				for _, k := range []string{"data", "list", "models", "items"} {
					if v := bytes.TrimSpace(inner[k]); len(v) > 0 && v[0] == '[' {
						d = v
						break
					}
				}
			}
		}
		if len(d) == 0 || d[0] != '[' {
			return nil, 0, fmt.Errorf("models: no data array")
		}
		if err := json.Unmarshal(d, &rows); err != nil {
			return nil, 0, fmt.Errorf("models: bad data array: %v", err)
		}
	}
	out := make([]gatewayModel, 0, len(rows))
	skipped := 0
	for _, r := range rows {
		var row map[string]any
		if err := json.Unmarshal(r, &row); err != nil || row == nil {
			skipped++
			continue
		}
		m := gatewayModel{ID: strField(row["id"]), Type: strField(row["type"])}
		if m.ID == "" {
			skipped++
			continue
		}
		if m.Type != "" && !strings.EqualFold(m.Type, "chat") && !(strings.EqualFold(m.Type, "image") && isLoomyImageModel(m.ID)) {
			continue // do not advertise unimplemented families or unknown image models
		}
		if isLoomyImageModel(m.ID) && !strings.EqualFold(m.Type, "image") {
			continue // never register a known image generator as chat
		}
		if m.ID == "auto" {
			continue
		}
		m.Name = strField(row["name"])
		if m.Name == "" {
			m.Name = m.ID
		}
		for _, k := range []string{"context_length", "context_window", "max_input_tokens"} {
			if m.ContextLength = numField(row[k]); m.ContextLength > 0 {
				break
			}
		}
		for _, k := range []string{"max_output_tokens", "max_completion_tokens"} {
			if m.MaxOutput = numField(row[k]); m.MaxOutput > 0 {
				break
			}
		}
		if list, ok := row["reasoning_efforts"].([]any); ok {
			for _, e := range list {
				if s, isStr := e.(string); isStr && strings.TrimSpace(s) != "" {
					m.ReasoningEfforts = append(m.ReasoningEfforts, s)
				}
			}
		}
		caps, _ := row["capabilities"].(map[string]any)
		m.Capabilities.Vision = boolField(caps["vision"]) || hasModality(caps["input_modalities"], "image")
		// magpie: tools unless explicitly false
		m.Capabilities.FunctionCalling = true
		if v, ok := caps["function_calling"]; ok && !boolField(v) {
			m.Capabilities.FunctionCalling = false
		}
		if strings.EqualFold(m.Type, "image") {
			m.ReasoningEfforts = nil
			m.Capabilities.FunctionCalling = false
		}
		out = append(out, m)
	}
	return out, skipped, nil
}

// noteModelsFailure logs why the catalogue could not be read (at most every
// 10 minutes) so a silent fallback to the built-in list is visible.
func noteModelsFailure(msg string) {
	modelCacheMu.Lock()
	logIt := time.Since(modelErrLogAt) > 10*time.Minute
	if logIt {
		modelErrLogAt = time.Now()
	}
	modelCacheMu.Unlock()
	if logIt {
		logEvent("warn", "models", "拉取上游模型目录失败，使用内置备用列表", truncate(msg, 500))
	}
}

// fetchGatewayModels caches each session separately for 10 minutes. A forced refresh
// bypasses only that session; it never replaces another account's capabilities.
func fetchGatewayModels(session string, force bool) (models []gatewayModel, resultErr error) {
	e := accountCatalogCache(session)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !force && e.err != nil && time.Now().Before(e.retryAt) {
		return nil, e.err
	}
	defer func() {
		if resultErr != nil {
			e.err = resultErr
			e.retryAt = time.Now().Add(5 * time.Second)
		} else {
			e.err = nil
			e.retryAt = time.Time{}
		}
	}()
	if !force && len(e.models) > 0 && time.Since(e.at) < 10*time.Minute {
		return e.models, nil
	}

	raw, status, err := gatewayGet(modelsPath, session, 20*time.Second, 1<<20)
	if err != nil {
		err = fmt.Errorf("models request: %v", err)
		noteModelsFailure(err.Error())
		return nil, err
	}
	if code, desc := appErr(raw); code != "" {
		mapped := businessStatus(code, desc)
		if !isAuthCode(code) && (status == 401 || status == 403 || status == 429) {
			mapped = status
		}
		err = &catalogRequestError{status: mapped, message: fmt.Sprintf("models http %d, code %s: %s", status, code, desc)}
		noteModelsFailure(err.Error())
		return nil, err
	}
	if status >= 400 {
		err = &catalogRequestError{status: status, message: fmt.Sprintf("models http %d: %s", status, truncate(strings.TrimSpace(string(raw)), 200))}
		noteModelsFailure(err.Error())
		return nil, err
	}
	out, skipped, err := parseGatewayModels(raw)
	if err != nil {
		err = fmt.Errorf("models http %d: %v", status, err)
		noteModelsFailure(err.Error())
		return nil, err
	}
	if skipped > 0 {
		modelCacheMu.Lock()
		logIt := time.Since(modelSkipLogAt) > 10*time.Minute
		if logIt {
			modelSkipLogAt = time.Now()
		}
		modelCacheMu.Unlock()
		if logIt {
			logEvent("warn", "models", fmt.Sprintf("模型目录有 %d 条记录无法解析，已跳过", skipped), "")
		}
	}
	if len(out) == 0 {
		err = fmt.Errorf("models http %d: catalogue has no supported model (skipped %d)", status, skipped)
		noteModelsFailure(err.Error())
		return nil, err
	}
	e.models, e.at = out, time.Now()
	return out, nil
}

func probeSession(session string) (int, error) {
	raw, status, err := gatewayGet(modelsPath, session, 20*time.Second, 4096)
	if err != nil {
		return 0, err
	}
	if code, _ := appErr(raw); code != "" {
		return http.StatusUnauthorized, nil
	}
	return status, nil
}

type pointsInfo struct {
	Permanent float64
	Daily     float64
	Available float64
}

func fetchPoints(session string) (pointsInfo, error) {
	raw, status, err := gatewayGet(pointsPath, session, 20*time.Second, 65536)
	if err != nil {
		return pointsInfo{}, err
	}
	if code, desc := appErr(raw); code != "" {
		if isAuthCode(code) {
			return pointsInfo{}, &authError{code: code, msg: desc}
		}
		return pointsInfo{}, fmt.Errorf("%s: %s", code, desc)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return pointsInfo{}, &authError{code: fmt.Sprint(status), msg: "unauthorized"}
	}
	if status < 200 || status >= 300 {
		return pointsInfo{}, fmt.Errorf("points response http %d", status)
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Data == nil {
		return pointsInfo{}, fmt.Errorf("bad points response (http %d)", status)
	}
	return pointsInfo{
		Permanent: toFloat(out.Data["balance"]),
		Daily:     toFloat(out.Data["dailyBalance"]),
		Available: toFloat(out.Data["availableBalance"]),
	}, nil
}

type authError struct{ code, msg string }

func (e *authError) Error() string { return "会话失效 " + e.code + ": " + e.msg }

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(n, &f)
		return f
	}
	return 0
}

// ---- account service (account.xfinfr.com) ----

func accountRequest(path string, param map[string]any) (map[string]any, error) {
	if accessKeyID == "" || accessSecret == "" {
		return nil, fmt.Errorf("Loomy account signing credentials are not configured")
	}
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

	req, err := http.NewRequest(http.MethodPost, currentConfig().AccountURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
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
	if out.Data == nil {
		out.Data = map[string]any{}
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
	sum := md5.Sum(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func signAccount(method, path, body, date, nonce string) string {
	parts := []string{method, escapePath(path), "", contentMD5([]byte(body)), "application/json", date, nonce, "", ""}
	mac := hmac.New(sha1.New, []byte(accessSecret))
	mac.Write([]byte(strings.Join(parts, "\n")))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func rfc3986Escape(seg string) string {
	escaped := strings.ReplaceAll(url.QueryEscape(seg), "+", "%20")
	return strings.NewReplacer("!", "%21", "'", "%27", "(", "%28", ")", "%29", "*", "%2A").Replace(escaped)
}

func escapePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg != "" {
			segs[i] = rfc3986Escape(seg)
		}
	}
	return strings.Join(segs, "/")
}

// ---- small helpers ----

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

func maskPhone(p string) string {
	if len(p) == 11 {
		return p[:3] + "****" + p[7:]
	}
	if p == "" {
		return "(无手机号)"
	}
	return p
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
