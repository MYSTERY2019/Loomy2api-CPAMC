package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dailyGrant = 5000.0

// ---- quota (CPA native quota view) ----

func handleQuotaFetch(body []byte) []byte {
	var req struct {
		AuthID      string
		StorageJSON []byte
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400)
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", "no session", 401)
	}
	p, err := fetchPoints(auth.Session)
	if err != nil {
		status := 502
		if _, ok := err.(*authError); ok {
			status = 401
		}
		return errorEnvelope("quota_error", err.Error(), status)
	}
	// v1 bug: a fully spent day (dailyBalance == 0) was shown as 5000/5000.
	return okEnvelope(map[string]any{
		"groups": []map[string]any{{
			"displayName": "Loomy 积分",
			"buckets": []map[string]any{{
				"window":            "daily",
				"remainingFraction": clamp01(p.Daily / dailyGrant),
				"description":       fmt.Sprintf("今日剩余 %.0f / %.0f", p.Daily, dailyGrant),
			}},
		}},
		"summary": []map[string]any{
			{"key": "available", "label": "可用合计", "value": p.Available, "unit": "pts"},
			{"key": "permanent", "label": "永久积分", "value": p.Permanent, "unit": "pts"},
			{"key": "daily", "label": "今日剩余", "value": p.Daily, "unit": "pts"},
		},
	})
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

// ---- route table ----

func handleManagementRegister() []byte {
	route := func(m, p, d string) map[string]any { return map[string]any{"Method": m, "Path": p, "Description": d} }
	return okEnvelope(map[string]any{
		"Routes": []map[string]any{
			route("GET", "/loomy/overview", "Console data: summary, accounts, stats"),
			route("GET", "/loomy/requests", "Request log (filter + paging, format=csv)"),
			route("GET", "/loomy/models", "Gateway model catalogue"),
			route("GET", "/loomy/events", "Plugin event log"),
			route("POST", "/loomy/settings", "Update plugin settings"),
			route("POST", "/loomy/renew", "Re-login one account by stored password now"),
			route("POST", "/loomy/reset-cooldown", "Clear CPA cooldown for one account"),
			route("POST", "/loomy/send-code", "Send a Loomy login SMS code"),
			route("POST", "/loomy/login", "Verify SMS code and save the account"),
			route("POST", "/loomy/add", "Add an account with phone + password"),
			route("GET", "/loomy/accounts", "List Loomy auth files (compat)"),
			route("GET", "/loomy/points", "Points for one auth (?auth_index=)"),
		},
		"Resources": []map[string]any{
			{"Path": "/accounts", "Menu": "Loomy 账号池", "Description": "账号、请求、模型与日志"},
		},
	})
}

type mgmtRequest struct {
	Method string
	Path   string
	Query  map[string][]string
	Body   []byte
}

func (r mgmtRequest) q(key string) string {
	if v, ok := r.Query[key]; ok && len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

func handleManagement(body []byte) []byte {
	var req mgmtRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400)
	}
	// Unauthenticated resource route: static shell only, never data.
	if strings.Contains(req.Path, "/v0/resource/") {
		if req.Method != "GET" {
			return mgmtJSON(405, map[string]any{"error": "method not allowed"})
		}
		return mgmtHTML(panelHTML)
	}
	path := req.Path
	if i := strings.Index(path, "/loomy/"); i >= 0 {
		path = path[i+len("/loomy"):]
	}
	switch req.Method + " " + strings.TrimRight(path, "/") {
	case "GET /overview":
		return mgmtJSON(200, buildOverview(req.q("refresh") == "1"))
	case "GET /requests":
		return handleRequestsQuery(req)
	case "GET /models":
		return handleModelsList(req.q("refresh") == "1")
	case "GET /events":
		limit, _ := strconv.Atoi(req.q("limit"))
		if limit <= 0 || limit > maxEventMemory {
			limit = 200
		}
		return mgmtJSON(200, map[string]any{"items": snapshotEvents(req.q("level"), limit)})
	case "POST /settings":
		var in struct {
			RenewPaused *bool `json:"renew_paused"`
		}
		if json.Unmarshal(req.Body, &in) != nil || in.RenewPaused == nil {
			return mgmtJSON(400, map[string]any{"error": "renew_paused required"})
		}
		s := updateSettings(func(s *pluginSettings) { s.RenewPaused = *in.RenewPaused })
		state := "已恢复"
		if s.RenewPaused {
			state = "已暂停"
		}
		logEvent("info", "system", "自动续期"+state, "")
		return mgmtJSON(200, s)
	case "POST /renew":
		return handleRenewNow(req.Body)
	case "POST /reset-cooldown":
		var in struct {
			AuthIndex string `json:"auth_index"`
		}
		if json.Unmarshal(req.Body, &in) != nil || in.AuthIndex == "" {
			return mgmtJSON(400, map[string]any{"error": "auth_index required"})
		}
		if _, err := callHost("host.routing.reset_cooldown", map[string]any{"auth_index": in.AuthIndex}); err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		return mgmtJSON(200, map[string]any{"ok": true})
	case "POST /send-code":
		var in struct {
			Phone string `json:"phone"`
		}
		if json.Unmarshal(req.Body, &in) != nil || !validPhone(in.Phone) {
			return mgmtJSON(400, map[string]any{"error": "请输入 11 位手机号"})
		}
		msgid, err := sendSMS(in.Phone)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		return mgmtJSON(200, map[string]any{"msgid": msgid, "phone": in.Phone})
	case "POST /login":
		var in struct {
			Phone, Code, Msgid string
		}
		if json.Unmarshal(req.Body, &in) != nil || !validPhone(in.Phone) || in.Code == "" || in.Msgid == "" {
			return mgmtJSON(400, map[string]any{"error": "phone, code, msgid required"})
		}
		session, userid, err := loginBySMS(in.Phone, in.Code, in.Msgid)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		a := &loomyAuth{Type: providerKey, Phone: in.Phone, UserID: userid}
		stampNewSession(a, session)
		return saveAccount(a, false)
	case "POST /add":
		var in struct {
			Phone    string `json:"phone"`
			Password string `json:"password"`
		}
		if json.Unmarshal(req.Body, &in) != nil || !validPhone(in.Phone) || in.Password == "" {
			return mgmtJSON(400, map[string]any{"error": "需要 11 位手机号和密码"})
		}
		session, err := loginByPassword(in.Phone, in.Password)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		a := &loomyAuth{Type: providerKey, Phone: in.Phone, Password: in.Password}
		stampNewSession(a, session)
		return saveAccount(a, true)
	case "GET /accounts":
		files, err := loomyFiles()
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		return mgmtJSON(200, map[string]any{"auths": files})
	case "GET /points":
		idx := req.q("auth_index")
		if idx == "" {
			return mgmtJSON(400, map[string]any{"error": "auth_index required"})
		}
		auth, _, _, err := getLoomyAuth(idx)
		if err != nil {
			return mgmtJSON(404, map[string]any{"error": err.Error()})
		}
		p, err := fetchPoints(auth.Session)
		if err != nil {
			return mgmtJSON(502, map[string]any{"error": err.Error()})
		}
		return mgmtJSON(200, map[string]any{"balance": p.Permanent, "dailyBalance": p.Daily, "availableBalance": p.Available})
	}
	return mgmtJSON(404, map[string]any{"error": "not found: " + req.Method + " " + req.Path})
}

var phoneRe = regexp.MustCompile(`^1\d{10}$`)

func validPhone(p string) bool { return phoneRe.MatchString(p) }

// ---- host auth helpers ----

type hostAuthEntry struct {
	ID             string    `json:"id"`
	AuthIndex      string    `json:"auth_index"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	Label          string    `json:"label"`
	Status         string    `json:"status"`
	StatusMessage  string    `json:"status_message"`
	Disabled       bool      `json:"disabled"`
	Unavailable    bool      `json:"unavailable"`
	Path           string    `json:"path"`
	ModTime        time.Time `json:"modtime"`
	CreatedAt      time.Time `json:"created_at"`
	LastRefresh    time.Time `json:"last_refresh"`
	NextRetryAfter time.Time `json:"next_retry_after"`
	Priority       int       `json:"priority"`
	Note           string    `json:"note"`
	Success        int64     `json:"success"`
	Failed         int64     `json:"failed"`
}

func loomyFiles() ([]hostAuthEntry, error) {
	raw, err := callHost("host.auth.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var listing struct {
		Files []hostAuthEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, err
	}
	out := make([]hostAuthEntry, 0, len(listing.Files))
	for _, f := range listing.Files {
		if strings.EqualFold(f.Provider, providerKey) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// getLoomyAuth reads the physical auth JSON through the host (no direct disk
// access, and only loomy credentials are accepted).
func getLoomyAuth(authIndex string) (*loomyAuth, []byte, string, error) {
	raw, err := callHost("host.auth.get", map[string]any{"auth_index": authIndex})
	if err != nil {
		return nil, nil, "", err
	}
	var got struct {
		Name string          `json:"name"`
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, nil, "", err
	}
	auth, err := parseAuthJSON(got.JSON)
	if err != nil {
		return nil, nil, "", fmt.Errorf("not a loomy credential")
	}
	return auth, got.JSON, got.Name, nil
}

func saveAuthFile(name string, doc []byte) error {
	_, err := callHost("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(doc)})
	return err
}

// saveAccount writes a new or re-logged account. An existing file for the same
// phone is updated in place so its note / priority / disabled state survive.
func saveAccount(a *loomyAuth, withPassword bool) []byte {
	name := fmt.Sprintf("loomy-%s.json", suffix(a.Phone, 4))
	var original []byte
	if files, err := loomyFiles(); err == nil {
		for _, f := range files {
			if existing, raw, fname, errGet := getLoomyAuth(f.AuthIndex); errGet == nil && existing.Phone == a.Phone {
				name, original = fname, raw
				if !withPassword && existing.Password != "" {
					a.Password = existing.Password // SMS re-login keeps auto-renew
				}
				if a.Note == "" {
					a.Note = existing.Note
				}
				break
			}
		}
	}
	doc := setRawField(mergeAuthJSON(original, a), "renew_failures", 0)
	if err := saveAuthFile(name, doc); err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	verb := "新增"
	if original != nil {
		verb = "重新登录"
	}
	logEvent("info", "account", fmt.Sprintf("%s账号 %s（%s）", verb, maskPhone(a.Phone), name), "")
	invalidatePoints("")
	return mgmtJSON(200, map[string]any{"saved": name, "phone": maskPhone(a.Phone), "auto_renew": a.Password != ""})
}

func handleRenewNow(body []byte) []byte {
	var in struct {
		AuthIndex string `json:"auth_index"`
	}
	if json.Unmarshal(body, &in) != nil || in.AuthIndex == "" {
		return mgmtJSON(400, map[string]any{"error": "auth_index required"})
	}
	auth, raw, name, err := getLoomyAuth(in.AuthIndex)
	if err != nil {
		return mgmtJSON(404, map[string]any{"error": err.Error()})
	}
	if auth.Password == "" || auth.Phone == "" {
		return mgmtJSON(409, map[string]any{"error": "该账号没有保存密码，请用短信重新登录"})
	}
	session, err := loginByPassword(auth.Phone, auth.Password)
	if err != nil {
		logEvent("warn", "renew", maskPhone(auth.Phone)+" 手动续期失败", err.Error())
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	stampNewSession(auth, session)
	if err := saveAuthFile(name, setRawField(mergeAuthJSON(raw, auth), "renew_failures", 0)); err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	_, _ = callHost("host.routing.reset_cooldown", map[string]any{"auth_index": in.AuthIndex})
	logEvent("info", "renew", maskPhone(auth.Phone)+" 手动续期成功", "")
	invalidatePoints(in.AuthIndex)
	return mgmtJSON(200, map[string]any{"ok": true, "expires_at": auth.ExpiresAt})
}

// ---- overview ----

type accountRow struct {
	AuthIndex   string  `json:"auth_index"`
	File        string  `json:"file"`
	Phone       string  `json:"phone"`
	Note        string  `json:"note"`
	State       string  `json:"state"` // ok | disabled | cooldown | invalid | error
	StateDetail string  `json:"state_detail,omitempty"`
	CooldownTo  int64   `json:"cooldown_to,omitempty"`
	Permanent   float64 `json:"permanent"`
	Daily       float64 `json:"daily"`
	Available   float64 `json:"available"`
	DaysLeft    float64 `json:"days_left"`
	DaysExact   bool    `json:"days_exact"`
	AutoRenew   bool    `json:"auto_renew"`
	Priority    int     `json:"priority"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	TodayReq    int64   `json:"today_req"`
	TodayFail   int64   `json:"today_fail"`
	LastUsed    int64   `json:"last_used,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	LastRefresh int64   `json:"last_refresh,omitempty"`
	PointsAt    int64   `json:"points_at,omitempty"`
}

type pointsCacheEntry struct {
	p   pointsInfo
	err error
	at  time.Time
}

var (
	pointsMu    sync.Mutex
	pointsCache = map[string]pointsCacheEntry{}
)

func invalidatePoints(authIndex string) {
	pointsMu.Lock()
	defer pointsMu.Unlock()
	if authIndex == "" {
		pointsCache = map[string]pointsCacheEntry{}
	} else {
		delete(pointsCache, authIndex)
	}
}

func cachedPoints(authIndex, session string, force bool) pointsCacheEntry {
	pointsMu.Lock()
	e, ok := pointsCache[authIndex]
	pointsMu.Unlock()
	if ok && !force && time.Since(e.at) < 60*time.Second {
		return e
	}
	p, err := fetchPoints(session)
	e = pointsCacheEntry{p: p, err: err, at: time.Now()}
	pointsMu.Lock()
	pointsCache[authIndex] = e
	pointsMu.Unlock()
	return e
}

func buildOverview(force bool) map[string]any {
	files, err := loomyFiles()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	stats := computeStats()
	rows := make([]accountRow, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range files {
		wg.Add(1)
		i, f := i, f
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					rows[i].State, rows[i].StateDetail = "error", fmt.Sprint(r)
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = buildRow(f, force)
		}()
	}
	wg.Wait()

	type summary struct {
		Accounts, Active, Unhealthy int
		Available, Today, TodayUsed float64
		Requests, RequestsOK        int64
		Tokens, AvgMS               int64
		ExpiringSoon                int
	}
	s := summary{Accounts: len(rows)}
	for i := range rows {
		r := &rows[i]
		if a := stats.PerAccount[r.Phone]; a != nil {
			r.TodayReq, r.TodayFail, r.LastUsed, r.LastError = a.Today, a.TodayFail, a.LastTS, a.LastError
		}
		switch r.State {
		case "ok", "cooldown":
			s.Active++
			s.Available += r.Available
			s.Today += r.Daily
			s.TodayUsed += dailyGrant - r.Daily
		case "disabled":
		default:
			s.Unhealthy++
		}
		if r.State != "disabled" && !r.AutoRenew && r.DaysLeft >= 0 && r.DaysLeft < 3 {
			s.ExpiringSoon++
		}
	}
	s.Requests, s.RequestsOK, s.Tokens, s.AvgMS = stats.Today, stats.TodayOK, stats.TodayTokens, stats.AvgMS
	c := currentConfig()
	return map[string]any{
		"updated_at": time.Now().UnixMilli(),
		"version":    pluginVer,
		"summary": map[string]any{
			"accounts": s.Accounts, "active": s.Active, "unhealthy": s.Unhealthy,
			"available": s.Available, "today": s.Today, "today_used": s.TodayUsed,
			"requests": s.Requests, "requests_ok": s.RequestsOK, "tokens": s.Tokens, "avg_ms": s.AvgMS,
			"expiring_soon": s.ExpiringSoon,
		},
		"days":     stats.Days,
		"accounts": rows,
		"settings": settingsSnapshot(),
		"config":   map[string]any{"base_url": c.BaseURL, "stream_mode": c.StreamMode, "retention_days": c.RetentionDays, "proxy": c.ProxyURL != "", "model_prefix": modelPrefix},
	}
}

func buildRow(f hostAuthEntry, force bool) accountRow {
	row := accountRow{AuthIndex: f.AuthIndex, File: f.Name, Note: f.Note, Priority: f.Priority, Success: f.Success, Failed: f.Failed, State: "ok", DaysLeft: -1}
	if !f.LastRefresh.IsZero() {
		row.LastRefresh = f.LastRefresh.UnixMilli()
	}
	auth, _, _, err := getLoomyAuth(f.AuthIndex)
	if err != nil {
		row.State, row.StateDetail = "error", err.Error()
		return row
	}
	row.Phone = maskPhone(auth.Phone)
	if row.Note == "" {
		row.Note = auth.Note
	}
	row.AutoRenew = auth.Password != ""
	row.DaysLeft, row.DaysExact = sessionDaysLeft(auth, f.ModTime)
	pe := cachedPoints(f.AuthIndex, auth.Session, force)
	row.PointsAt = pe.at.UnixMilli()
	if pe.err != nil {
		if _, ok := pe.err.(*authError); ok {
			row.State = "invalid"
		} else {
			row.State = "error"
		}
		row.StateDetail = pe.err.Error()
	} else {
		row.Permanent, row.Daily, row.Available = pe.p.Permanent, pe.p.Daily, pe.p.Available
	}
	if f.Disabled || strings.EqualFold(f.Status, "disabled") {
		row.State = "disabled"
	} else if row.State == "ok" && !f.NextRetryAfter.IsZero() && f.NextRetryAfter.After(time.Now()) {
		row.State, row.CooldownTo = "cooldown", f.NextRetryAfter.UnixMilli()
		row.StateDetail = f.StatusMessage
	} else if row.State == "ok" && f.Unavailable && f.StatusMessage != "" {
		row.StateDetail = f.StatusMessage
	}
	return row
}

// ---- requests / models ----

func handleRequestsQuery(req mgmtRequest) []byte {
	atoi := func(k string) int64 { n, _ := strconv.ParseInt(req.q(k), 10, 64); return n }
	q := reqQuery{
		Since: atoi("since"), Until: atoi("until"),
		Account: req.q("account"), Model: req.q("model"), Status: req.q("status"), Keyword: req.q("q"),
		Page: int(atoi("page")), Size: int(atoi("size")),
	}
	if req.q("format") == "csv" {
		page := queryRequests(q, true)
		buf := &bytes.Buffer{}
		buf.WriteString("\ufeff")
		w := csv.NewWriter(buf)
		_ = w.Write([]string{"时间", "账号", "模型", "流式", "状态", "耗时ms", "首字ms", "输入tokens", "输出tokens", "错误"})
		for _, r := range page.Items {
			_ = w.Write([]string{
				time.UnixMilli(r.TS).Format("2006-01-02 15:04:05"), r.Account, r.Model, strconv.FormatBool(r.Stream),
				strconv.Itoa(r.Status), strconv.FormatInt(r.MS, 10), strconv.FormatInt(r.TTFB, 10),
				strconv.FormatInt(r.PromptTokens, 10), strconv.FormatInt(r.CompletionTokens, 10), r.Error,
			})
		}
		w.Flush()
		return mgmtRaw(200, "text/csv; charset=utf-8", buf.Bytes())
	}
	return mgmtJSON(200, queryRequests(q, false))
}

type modelRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Context  int64  `json:"context"`
	Tools    bool   `json:"tools"`
	Vision   bool   `json:"vision"`
	Multiple string `json:"multiple"`
	Free     bool   `json:"free"`
}

func handleModelsList(force bool) []byte {
	files, err := loomyFiles()
	if err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	var lastErr error
	for _, f := range files {
		if f.Disabled {
			continue
		}
		auth, _, _, errGet := getLoomyAuth(f.AuthIndex)
		if errGet != nil {
			continue
		}
		models, errFetch := fetchGatewayModels(auth.Session, force)
		if errFetch != nil {
			lastErr = errFetch
			continue
		}
		out := make([]modelRow, 0, len(models))
		for _, m := range models {
			r := modelRow{ID: m.ID, Name: m.Name, Context: m.ContextLength, Tools: m.Capabilities.FunctionCalling, Vision: m.Capabilities.Vision}
			r.Multiple, r.Free = parseMultiple(m.Name)
			out = append(out, r)
		}
		return mgmtJSON(200, map[string]any{"items": out, "prefix": modelPrefix})
	}
	msg := "没有可用账号来拉取模型清单"
	if lastErr != nil {
		msg += "：" + lastErr.Error()
	}
	return mgmtJSON(502, map[string]any{"error": msg})
}

// parseMultiple reads "(x2.0)" / "限时免费" markers out of the display name.
func parseMultiple(name string) (string, bool) {
	n := strings.NewReplacer("（", "(", "）", ")", " ", "", "\u00a0", "").Replace(name)
	if strings.Contains(n, "限时免费") || strings.Contains(n, "免费") {
		return "0", true
	}
	start := strings.LastIndex(n, "(x")
	if start < 0 {
		return "", false
	}
	end := strings.Index(n[start:], ")")
	if end < 0 {
		return "", false
	}
	val := strings.TrimPrefix(n[start:start+end], "(x")
	if f, err := strconv.ParseFloat(val, 64); err == nil && f == 0 {
		return val, true
	}
	return val, false
}

// ---- response helpers ----

func mgmtJSON(status int, payload any) []byte {
	raw, _ := json.Marshal(payload)
	return mgmtRaw(status, "application/json; charset=utf-8", raw)
}

func mgmtRaw(status int, ctype string, body []byte) []byte {
	return okEnvelope(map[string]any{
		"StatusCode": status,
		"Headers": map[string][]string{
			"content-type":           {ctype},
			"cache-control":          {"no-store"},
			"x-content-type-options": {"nosniff"},
		},
		"Body": body,
	})
}

func mgmtHTML(page string) []byte {
	return okEnvelope(map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"content-type":            {"text/html; charset=utf-8"},
			"cache-control":           {"no-store"},
			"x-content-type-options":  {"nosniff"},
			"referrer-policy":         {"no-referrer"},
			"content-security-policy": {"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'"},
		},
		"Body": []byte(page),
	})
}
