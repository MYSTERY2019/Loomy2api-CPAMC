package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Loomy reward activities (endpoints confirmed by the MIT project
// MYCF711/magpie-loomy-auth against real accounts, 2026-10):
//
//   POST /points/first-login          daily check-in: refreshes the 5000/day pool
//                                     (idempotent, alreadyProcessed=true when done)
//   GET  /onboarding/tasks            {tasks:{key:bool}, earned, total}
//   POST /onboarding/tasks/complete   {key} -> {alreadyCompleted, balance}
//   GET  /invitation-codes            {list:[{inviteCode, usedCount, maxUses, status}]}
//   GET  /points/activation           {activated, appliedInvitationCode}
//   POST /points/activation           {inviteCode} bind someone's code (once per account)
//
// The daily pool is NOT topped up by itself: without the check-in the
// dailyBalance stays at 0 after the business day rolls over (00:05 CST).

var cst = time.FixedZone("CST", 8*3600)

type onboardingTask struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Points int    `json:"points"`
}

var onboardingCatalog = []onboardingTask{
	{"share_soul", "分享搭子给朋友", 2000},
	{"generate_ppt", "生成第一份 PPT", 1500},
	{"install_skill", "安装一个技能", 1500},
	{"create_soul", "创建第一个搭子", 1500},
	{"pick_skill", "选择一个技能", 1000},
	{"set_schedule", "设置定时任务", 1000},
	{"configure_remote", "配置远程控制", 1000},
	{"first_message", "发送第一条消息", 500},
}

// gatewayCall performs a JSON request against the gateway and returns the
// unwrapped "data" object. Business error codes become errors.
func gatewayCall(method, path, session string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, gateway(path), reader)
	if err != nil {
		return nil, err
	}
	setAuthHeaders(req, session)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if code, desc := appErr(raw); code != "" {
		if isAuthCode(code) {
			return nil, &authError{code: code, msg: desc}
		}
		return nil, fmt.Errorf("%s %s", code, desc)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 160))
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应不是 JSON")
	}
	return unwrapData(env), nil
}

func unwrapData(env map[string]any) map[string]any {
	cur := env
	for i := 0; i < 2; i++ {
		if d, ok := cur["data"].(map[string]any); ok {
			cur = d
		} else {
			break
		}
	}
	return cur
}

// ---- check-in ----

type checkinResult struct {
	AuthIndex string  `json:"auth_index"`
	Phone     string  `json:"phone"`
	File      string  `json:"file"`
	OK        bool    `json:"ok"`
	Already   bool    `json:"already"`
	Quota     float64 `json:"daily_quota"`
	Balance   float64 `json:"daily_balance"`
	Cycle     string  `json:"cycle,omitempty"`
	Error     string  `json:"error,omitempty"`
	Skipped   bool    `json:"skipped,omitempty"`
	TS        int64   `json:"ts"`
}

var (
	checkinMu   sync.Mutex
	lastCheckin = map[string]checkinResult{} // auth_index -> last result
)

func checkinOne(f hostAuthEntry) checkinResult {
	res := checkinResult{AuthIndex: f.AuthIndex, File: f.Name, TS: time.Now().UnixMilli()}
	auth, _, _, err := getLoomyAuth(f.AuthIndex)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Phone = maskPhone(auth.Phone)
	data, err := gatewayCall(http.MethodPost, "/points/first-login", auth.Session, map[string]any{})
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
		res.Already = data["alreadyProcessed"] == true
		res.Quota = toFloat(data["dailyQuota"])
		res.Balance = toFloat(data["dailyBalance"])
		res.Cycle, _ = data["dailyCycleDate"].(string)
		invalidatePoints(f.AuthIndex)
	}
	checkinMu.Lock()
	lastCheckin[f.AuthIndex] = res
	checkinMu.Unlock()
	return res
}

func checkinAll(authIndex string, auto bool) ([]checkinResult, error) {
	files, err := loomyFiles()
	if err != nil {
		return nil, err
	}
	var out []checkinResult
	for _, f := range files {
		if authIndex != "" && f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled && authIndex == "" {
			out = append(out, checkinResult{AuthIndex: f.AuthIndex, File: f.Name, Skipped: true, Error: "已停用，跳过", TS: time.Now().UnixMilli()})
			continue
		}
		out = append(out, checkinOne(f)) // serial on purpose: the vendor dislikes bursts
	}
	ok, already, fail := 0, 0, 0
	for _, r := range out {
		switch {
		case r.Skipped:
		case !r.OK:
			fail++
		case r.Already:
			already++
		default:
			ok++
		}
	}
	who := "手动"
	if auto {
		who = "自动"
	}
	level := "info"
	detail := ""
	if fail > 0 {
		level = "warn"
		for _, r := range out {
			if !r.OK && !r.Skipped {
				detail += r.Phone + "：" + r.Error + "\n"
			}
		}
	}
	if len(out) > 0 {
		logEvent(level, "checkin", fmt.Sprintf("%s签到：%d 个已领取，%d 个今天已签过，%d 个失败", who, ok, already, fail), strings.TrimSpace(detail))
	}
	return out, nil
}

// ---- daily scheduler ----

var (
	schedOnce sync.Once
	schedStop = make(chan struct{})
)

func startScheduler() {
	schedOnce.Do(func() {
		startPreferWatchdog()
		goSafe("checkin-scheduler", func() {
			// small delay so CPA finishes loading auth files first
			select {
			case <-time.After(30 * time.Second):
			case <-schedStop:
				return
			}
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				maybeAutoCheckin()
				select {
				case <-t.C:
				case <-schedStop:
					return
				}
			}
		})
	})
}

func stopScheduler() {
	defer func() { _ = recover() }()
	close(schedStop)
}

// maybeAutoCheckin runs once per business day after 00:10 CST.
func maybeAutoCheckin() {
	s := settingsSnapshot()
	if s.CheckinOff {
		return
	}
	now := time.Now().In(cst)
	today := now.Format("2006-01-02")
	if s.LastAutoCheckin == today || now.Hour() == 0 && now.Minute() < 10 {
		return
	}
	updateSettings(func(s *pluginSettings) { s.LastAutoCheckin = today })
	_, _ = checkinAll("", true)
}

// ---- activities overview ----

type activityRow struct {
	AuthIndex   string          `json:"auth_index"`
	File        string          `json:"file"`
	Phone       string          `json:"phone"`
	Note        string          `json:"note,omitempty"`
	Disabled    bool            `json:"disabled"`
	Checkin     *checkinResult  `json:"checkin,omitempty"`
	TasksDone   map[string]bool `json:"tasks_done,omitempty"`
	TasksEarned int             `json:"tasks_earned"`
	TasksLeft   int             `json:"tasks_left"`
	InviteCode  string          `json:"invite_code,omitempty"`
	InviteUsed  int             `json:"invite_used"`
	InviteMax   int             `json:"invite_max"`
	Activated   *bool           `json:"activated,omitempty"`
	Error       string          `json:"error,omitempty"`
}

var (
	actCacheMu sync.Mutex
	actCache   = map[string]activityRow{}
	actCacheAt = map[string]time.Time{}
)

func invalidateActivity(authIndex string) {
	actCacheMu.Lock()
	if authIndex == "" {
		actCache, actCacheAt = map[string]activityRow{}, map[string]time.Time{}
	} else {
		delete(actCacheAt, authIndex)
	}
	actCacheMu.Unlock()
}

func loadActivity(f hostAuthEntry, force bool) activityRow {
	actCacheMu.Lock()
	if row, ok := actCache[f.AuthIndex]; ok && !force && time.Since(actCacheAt[f.AuthIndex]) < 10*time.Minute {
		actCacheMu.Unlock()
		row.Disabled = f.Disabled
		return row
	}
	actCacheMu.Unlock()

	row := activityRow{AuthIndex: f.AuthIndex, File: f.Name, Disabled: f.Disabled}
	auth, _, _, err := getLoomyAuth(f.AuthIndex)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Phone, row.Note = maskPhone(auth.Phone), auth.Note
	var errs []string
	if d, err := gatewayCall(http.MethodGet, "/onboarding/tasks", auth.Session, nil); err == nil {
		row.TasksDone = map[string]bool{}
		raw, _ := d["tasks"].(map[string]any)
		for _, t := range onboardingCatalog {
			done := raw[t.Key] == true
			row.TasksDone[t.Key] = done
			if done {
				row.TasksEarned += t.Points
			} else {
				row.TasksLeft += t.Points
			}
		}
	} else {
		errs = append(errs, "任务："+err.Error())
	}
	if d, err := gatewayCall(http.MethodGet, "/invitation-codes", auth.Session, nil); err == nil {
		if list, ok := d["list"].([]any); ok {
			for _, it := range list {
				m, _ := it.(map[string]any)
				code, _ := m["inviteCode"].(string)
				if code == "" {
					continue
				}
				status, _ := m["status"].(string)
				if row.InviteCode == "" || status == "active" {
					row.InviteCode = strings.TrimSpace(code)
					row.InviteUsed, row.InviteMax = int(toFloat(m["usedCount"])), int(toFloat(m["maxUses"]))
				}
			}
		}
	} else {
		errs = append(errs, "邀请码："+err.Error())
	}
	if d, err := gatewayCall(http.MethodGet, "/points/activation", auth.Session, nil); err == nil {
		v := d["activated"] == true
		row.Activated = &v
	} else {
		errs = append(errs, "邀请状态："+err.Error())
	}
	row.Error = strings.Join(errs, "；")
	actCacheMu.Lock()
	actCache[f.AuthIndex], actCacheAt[f.AuthIndex] = row, time.Now()
	actCacheMu.Unlock()
	return row
}

func handleActivities(force bool) []byte {
	files, err := loomyFiles()
	if err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	rows := make([]activityRow, len(files))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		i, f := i, f
		goSafe("activity", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = loadActivity(f, force)
		})
	}
	wg.Wait()
	checkinMu.Lock()
	for i := range rows {
		if r, ok := lastCheckin[rows[i].AuthIndex]; ok {
			r := r
			rows[i].Checkin = &r
		}
	}
	checkinMu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].File < rows[j].File })
	s := settingsSnapshot()
	tasks := make([]onboardingTask, len(onboardingCatalog))
	copy(tasks, onboardingCatalog)
	return mgmtJSON(200, map[string]any{
		"items":             rows,
		"tasks":             tasks,
		"auto_checkin":      !s.CheckinOff,
		"last_auto_checkin": s.LastAutoCheckin,
		"today":             time.Now().In(cst).Format("2006-01-02"),
	})
}

// ---- claim onboarding tasks ----

func handleClaimTasks(body []byte) []byte {
	var in struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(body, &in)
	files, err := loomyFiles()
	if err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	type claimRes struct {
		Phone   string   `json:"phone"`
		File    string   `json:"file"`
		Claimed []string `json:"claimed"`
		Points  int      `json:"points"`
		Failed  []string `json:"failed,omitempty"`
		Error   string   `json:"error,omitempty"`
	}
	var out []claimRes
	total := 0
	for _, f := range files {
		if in.AuthIndex != "" && f.AuthIndex != in.AuthIndex || in.AuthIndex == "" && f.Disabled {
			continue
		}
		res := claimRes{File: f.Name}
		auth, _, _, err := getLoomyAuth(f.AuthIndex)
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		res.Phone = maskPhone(auth.Phone)
		row := loadActivity(f, true)
		if row.TasksDone == nil {
			res.Error = "读取任务失败：" + row.Error
			out = append(out, res)
			continue
		}
		for _, t := range onboardingCatalog { // highest value first, serial
			if row.TasksDone[t.Key] {
				continue
			}
			if _, err := gatewayCall(http.MethodPost, "/onboarding/tasks/complete", auth.Session, map[string]any{"key": t.Key}); err != nil {
				res.Failed = append(res.Failed, t.Title+"："+err.Error())
				continue
			}
			res.Claimed = append(res.Claimed, t.Title)
			res.Points += t.Points
		}
		total += res.Points
		invalidateActivity(f.AuthIndex)
		invalidatePoints(f.AuthIndex)
		out = append(out, res)
	}
	if total > 0 || len(out) > 0 {
		logEvent("info", "tasks", fmt.Sprintf("领取新手任务：共 +%d 积分", total), "")
	}
	return mgmtJSON(200, map[string]any{"items": out, "points": total})
}

// ---- invitation ----

func handleBindInvite(body []byte) []byte {
	var in struct {
		AuthIndex string `json:"auth_index"`
		Code      string `json:"code"`
	}
	if json.Unmarshal(body, &in) != nil || in.AuthIndex == "" || !inviteRe.MatchString(strings.TrimSpace(in.Code)) {
		return mgmtJSON(400, map[string]any{"error": "需要账号和 6 位邀请码"})
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	auth, _, _, err := getLoomyAuth(in.AuthIndex)
	if err != nil {
		return mgmtJSON(404, map[string]any{"error": err.Error()})
	}
	if d, err := gatewayCall(http.MethodGet, "/points/activation", auth.Session, nil); err == nil && d["activated"] == true {
		return mgmtJSON(409, map[string]any{"error": "该账号已经用过邀请码（每个账号只能用一次）"})
	}
	d, err := gatewayCall(http.MethodPost, "/points/activation", auth.Session, map[string]any{"inviteCode": code, "deviceId": deviceID(auth)})
	if err != nil {
		logEvent("warn", "invite", maskPhone(auth.Phone)+" 绑定邀请码失败", err.Error())
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	invalidateActivity("")
	invalidatePoints(in.AuthIndex)
	logEvent("info", "invite", maskPhone(auth.Phone)+" 已绑定邀请码 "+code, "")
	return mgmtJSON(200, map[string]any{"ok": true, "activated": d["activated"] == true || d["alreadyActivated"] == true})
}
