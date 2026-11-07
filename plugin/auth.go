package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// loomyAuth is the typed view of an auth file. Unknown fields (note, priority,
// disabled, anything CPA adds) are preserved through rawAuth on every rewrite.
type loomyAuth struct {
	Type      string `json:"type"`
	Phone     string `json:"phone,omitempty"`
	Password  string `json:"password,omitempty"`
	Session   string `json:"session"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	IssuedAt  string `json:"issued_at,omitempty"`
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

func parseAuthJSON(raw []byte) (*loomyAuth, error) {
	var auth loomyAuth
	if err := json.Unmarshal(raw, &auth); err == nil && auth.Session != "" {
		if auth.Type != "" && !strings.EqualFold(auth.Type, providerKey) {
			return nil, fmt.Errorf("not a loomy credential")
		}
		rememberSecrets(auth.Session, auth.Password)
		auth.Type = providerKey
		return &auth, nil
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if len(s) >= 16 && len(s) <= 256 && !strings.ContainsAny(s, "{}\n :") {
		rememberSecrets(s)
		return &loomyAuth{Type: providerKey, Session: s}, nil
	}
	return nil, fmt.Errorf("no session")
}

// mergeAuthJSON writes the typed fields over the original document, keeping
// every field this plugin does not own.
func mergeAuthJSON(original []byte, auth *loomyAuth) []byte {
	doc := map[string]any{}
	_ = json.Unmarshal(original, &doc)
	if doc == nil {
		doc = map[string]any{}
	}
	typed, _ := json.Marshal(auth)
	var fields map[string]any
	_ = json.Unmarshal(typed, &fields)
	for k, v := range fields {
		doc[k] = v
	}
	for _, k := range []string{"expires_at", "issued_at", "user_id", "password", "phone", "note"} {
		if _, ok := fields[k]; !ok {
			if s, isStr := doc[k].(string); isStr && s == "" {
				delete(doc, k)
			}
		}
	}
	doc["type"] = providerKey
	out, _ := json.Marshal(doc)
	return out
}

func authLabel(auth *loomyAuth) string {
	if auth.Phone != "" {
		return fmt.Sprintf("Loomy (%s)", maskPhone(auth.Phone))
	}
	return "Loomy"
}

func toAuthData(auth *loomyAuth, original []byte, fileName, authID string) authData {
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
		Label:       authLabel(auth),
		StorageJSON: mergeAuthJSON(original, auth),
		Metadata:    map[string]any{"type": providerKey},
		Attributes:  map[string]string{"type": providerKey},
	}
}

func handleAuthParse(body []byte) []byte {
	var req struct {
		FileName string
		RawJSON  []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400)
	}
	auth, err := parseAuthJSON(req.RawJSON)
	if err != nil {
		return okEnvelope(map[string]any{"Handled": false})
	}
	return okEnvelope(map[string]any{"Handled": true, "Auth": toAuthData(auth, req.RawJSON, req.FileName, "")})
}

// sessionDaysLeft returns remaining session lifetime and whether it is exact.
func sessionDaysLeft(auth *loomyAuth, fallback time.Time) (float64, bool) {
	if auth.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, auth.ExpiresAt); err == nil {
			return time.Until(t).Hours() / 24, true
		}
	}
	if !fallback.IsZero() {
		return time.Until(fallback.Add(sessionTTL*time.Second)).Hours() / 24, false
	}
	return -1, false
}

// renewWindow: renew by password once fewer than this many days remain.
const renewWindowDays = 4.0

// handleAuthRefresh is CPA's scheduled refresh.
//   - paused: no upstream traffic, check again in 6h
//   - password present: probe; re-login only when near expiry or probe fails;
//     failures back off 30m → 2h → 6h → 12h to avoid tripping risk control
//   - no password: probe only
func handleAuthRefresh(body []byte) []byte {
	var req struct {
		AuthID      string
		StorageJSON []byte
		Metadata    map[string]any
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400)
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", "credential has no session", 401)
	}
	acct := maskPhone(auth.Phone)

	if settingsSnapshot().RenewPaused {
		ad := toAuthData(auth, req.StorageJSON, "", req.AuthID)
		ad.NextRefreshAfter = time.Now().Add(6 * time.Hour)
		return okEnvelope(map[string]any{"Auth": ad, "NextRefreshAfter": ad.NextRefreshAfter})
	}

	failures := int(toFloat(rawField(req.StorageJSON, "renew_failures")))
	status, probeErr := probeSession(auth.Session)
	alive := probeErr == nil && status == http.StatusOK
	days, _ := sessionDaysLeft(auth, time.Time{})
	next := time.Now().Add(24 * time.Hour)
	renewed := false

	if auth.Password != "" && auth.Phone != "" && (!alive || (days >= 0 && days < renewWindowDays) || auth.ExpiresAt == "") {
		if session, errLogin := loginByPassword(auth.Phone, auth.Password); errLogin == nil {
			stampNewSession(auth, session)
			failures = 0
			renewed = true
			logEvent("info", "renew", acct+" 自动续期成功", "")
		} else {
			failures++
			backoff := []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour}
			next = time.Now().Add(backoff[min(failures-1, len(backoff)-1)])
			logEvent("warn", "renew", fmt.Sprintf("%s 自动续期失败（第 %d 次）", acct, failures), errLogin.Error())
		}
	} else if !alive {
		next = time.Now().Add(1 * time.Hour)
		msg := fmt.Sprintf("%s 会话探活失败 status=%d", acct, status)
		if probeErr != nil {
			msg += " " + probeErr.Error()
			next = time.Now().Add(15 * time.Minute) // network blip, retry sooner
		}
		logEvent("warn", "probe", msg, "")
	}
	if alive || renewed {
		if d, ok := sessionDaysLeft(auth, time.Time{}); ok && d > renewWindowDays {
			at := time.Now().Add(time.Duration((d - renewWindowDays) * 24 * float64(time.Hour)))
			if at.Before(next) || failures == 0 {
				next = minTime(at, time.Now().Add(24*time.Hour))
			}
		}
	}

	merged := setRawField(mergeAuthJSON(req.StorageJSON, auth), "renew_failures", failures)
	ad := toAuthData(auth, merged, "", req.AuthID)
	ad.NextRefreshAfter = next
	return okEnvelope(map[string]any{"Auth": ad, "NextRefreshAfter": next})
}

func stampNewSession(auth *loomyAuth, session string) {
	now := time.Now()
	rememberSecrets(session, auth.Password)
	auth.Session = session
	auth.IssuedAt = now.Format(time.RFC3339)
	auth.ExpiresAt = now.Add(sessionTTL * time.Second).Format(time.RFC3339)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func rawField(raw []byte, key string) any {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc[key]
}

func setRawField(raw []byte, key string, value any) []byte {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return raw
	}
	if value == nil || value == 0 {
		delete(doc, key)
	} else {
		doc[key] = value
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// ---- models ----

// defaultModelSpecs is the built-in fallback, used only when the live
// catalogue cannot be read. Snapshot of GET /api/v1/models measured by
// magpie-loomy (src/models.mjs, 2026-10-26): ids, display names with the
// points multiple, context, vision and tools as published.
var defaultModelSpecs = []struct {
	id     string
	name   string
	ctx    int64
	output int64
	vision bool
	tools  bool
}{
	{"spark-x2.6", "Spark-X2.6 正式版(限时免费)", 1048576, 131072, false, true},
	{"spark-x", "Spark-X2.5（限时免费）", 1048576, 65536, false, true},
	{"DeepSeek-V4.1-Flash", "DeepSeek V4.1 Flash(x2.0)", 1048576, 384000, true, true},
	{"MiniMax-M3", "MiniMax M3(x4.0)", 1048576, 512000, true, true},
	{"Kimi-k2.6", "Kimi k2.6(x6.5)", 262144, 65536, true, true},
	{"qwen-3.8-max", "Qwen 3.8 Max(x12.0)", 1000000, 65536, false, true},
	{"GLM-5.3-Flash", "GLM 5.3 Flash(x0.8)", 1048576, 131072, true, true},
	{"qwen3.8-flash", "qwen 3.8 flash(x0.8)", 1000000, 131072, true, true},
	{"mimo-v2.5", "MiMo V2.5(x3.3)", 1048576, 131072, true, true},
}

// staticGatewayModels is the fallback catalogue in gatewayModel form.
func staticGatewayModels() []gatewayModel {
	out := make([]gatewayModel, 0, len(defaultModelSpecs))
	for _, s := range defaultModelSpecs {
		m := gatewayModel{ID: s.id, Name: s.name, ContextLength: s.ctx, MaxOutput: s.output, Type: "chat", ReasoningEfforts: defaultEfforts}
		m.Capabilities.Vision, m.Capabilities.FunctionCalling = s.vision, s.tools
		out = append(out, m)
	}
	return out
}

// defaultEfforts are the thinking levels every Loomy chat model publishes
// (reasoning_efforts in GET /models, observed by magpie-loomy-auth).
var defaultEfforts = []string{"none", "low", "medium", "high", "xhigh"}

func staticChatModels() []map[string]any {
	out := make([]map[string]any, 0, len(defaultModelSpecs))
	for _, s := range defaultModelSpecs {
		out = append(out, modelEntry(s.id, s.ctx, s.vision, s.name, defaultEfforts...))
	}
	return out
}

func modelEntry(id string, ctx int64, vision bool, display string, efforts ...string) map[string]any {
	if display == "" {
		display = id
	}
	entry := map[string]any{
		"ID":                         modelPrefix + id,
		"Object":                     "model",
		"OwnedBy":                    providerKey,
		"Type":                       "chat",
		"Name":                       id,
		"DisplayName":                display,
		"ContextLength":              ctx,
		"SupportedGenerationMethods": []string{"chat.completions"},
	}
	if vision {
		entry["SupportedInputModalities"] = []string{"text", "image"}
	}
	// Declaring the levels lets CPA turn "lm/<model>(high)" or a client's
	// reasoning settings into reasoning_effort for the gateway.
	if levels := thinkingLevels(efforts); len(levels) > 0 {
		zero := false
		for _, l := range levels {
			if l == "none" {
				zero = true
			}
		}
		entry["Thinking"] = map[string]any{"Levels": levels, "ZeroAllowed": zero}
	}
	return entry
}

func handleModelsForAuth(body []byte) []byte {
	var req struct {
		AuthID      string
		StorageJSON []byte
		Metadata    map[string]any
	}
	fallback := okEnvelope(map[string]any{"Provider": providerKey, "Models": staticChatModels()})
	if err := json.Unmarshal(body, &req); err != nil {
		noteModelsFailure("model.for_auth: bad request: " + err.Error())
		return fallback
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil && req.Metadata != nil {
		// the host also passes the file's fields as metadata
		if raw, errM := json.Marshal(req.Metadata); errM == nil {
			auth, err = parseAuthJSON(raw)
		}
	}
	if err != nil {
		noteModelsFailure(fmt.Sprintf("model.for_auth %s: no session in StorageJSON (%d bytes) or metadata", req.AuthID, len(req.StorageJSON)))
		return fallback
	}
	models, err := fetchGatewayModels(auth.Session, false)
	if err != nil || len(models) == 0 {
		return fallback // fetchGatewayModels already logged why
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, catalogModelEntry(m))
	}
	return okEnvelope(map[string]any{"Provider": providerKey, "Models": out})
}

func thinkingLevels(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
