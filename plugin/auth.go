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
		auth.Type = providerKey
		return &auth, nil
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if len(s) >= 16 && len(s) <= 256 && !strings.ContainsAny(s, "{}\n :") {
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

var defaultModelSpecs = []struct {
	id     string
	ctx    int64
	vision bool
}{
	{"spark-x2.6", 1048576, false},
	{"DeepSeek-V4.1-Flash", 1048576, true},
	{"deepseek-v4-flash-0731", 1048576, false},
	{"GLM-5.3-Flash", 1048576, true},
	{"qwen3.8-flash", 1000000, true},
	{"mimo-v2.5", 1048576, true},
}

func staticChatModels() []map[string]any {
	out := make([]map[string]any, 0, len(defaultModelSpecs))
	for _, s := range defaultModelSpecs {
		out = append(out, modelEntry(s.id, s.ctx, s.vision, ""))
	}
	return out
}

func modelEntry(id string, ctx int64, vision bool, display string) map[string]any {
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
	return entry
}

func handleModelsForAuth(body []byte) []byte {
	var req struct {
		AuthID      string
		StorageJSON []byte
	}
	fallback := okEnvelope(map[string]any{"Provider": providerKey, "Models": staticChatModels()})
	if err := json.Unmarshal(body, &req); err != nil {
		return fallback
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil {
		return fallback
	}
	models, err := fetchGatewayModels(auth.Session, false)
	if err != nil || len(models) == 0 {
		return fallback
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, modelEntry(m.ID, m.ContextLength, m.Capabilities.Vision, m.Name))
	}
	return okEnvelope(map[string]any{"Provider": providerKey, "Models": out})
}
