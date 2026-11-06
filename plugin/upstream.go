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
	accessKeyID        = "2thryby66wxi53sk"
	accessSecret       = "zsak6eadrbawz683wf5r3m2snrwj868r"
	appID              = "GM3LOOMY"
	sessionTTL         = 14 * 24 * 3600

	chatPath   = "/chat/completions"
	modelsPath = "/models"
	pointsPath = "/points/records"
)

var jsonUnmarshal = json.Unmarshal

// ---- gateway (loomyad) ----

func setAuthHeaders(req *http.Request, session string) {
	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("token", session)
	req.Header.Set("traceparent", traceparent())
	req.Header.Set("loomy-version", loomyVersion)
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
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int64  `json:"context_length"`
	Capabilities  struct {
		Vision          bool `json:"vision"`
		FunctionCalling bool `json:"function_calling"`
	} `json:"capabilities"`
}

var (
	modelCacheMu sync.Mutex
	modelCache   []gatewayModel
	modelCacheAt time.Time
)

// fetchGatewayModels returns the catalogue, cached for 10 minutes.
func fetchGatewayModels(session string, force bool) ([]gatewayModel, error) {
	modelCacheMu.Lock()
	if !force && len(modelCache) > 0 && time.Since(modelCacheAt) < 10*time.Minute {
		out := modelCache
		modelCacheMu.Unlock()
		return out, nil
	}
	modelCacheMu.Unlock()

	raw, status, err := gatewayGet(modelsPath, session, 20*time.Second, 512*1024)
	if err != nil {
		return nil, err
	}
	if code, desc := appErr(raw); code != "" {
		return nil, fmt.Errorf("%s: %s", code, desc)
	}
	if status >= 400 {
		return nil, fmt.Errorf("models http %d", status)
	}
	var payload struct {
		Data []gatewayModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]gatewayModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" && m.ID != "auto" {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		modelCacheMu.Lock()
		modelCache, modelCacheAt = out, time.Now()
		modelCacheMu.Unlock()
	}
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
