package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request log, event log and settings, persisted as JSONL under data-dir
// (default ~/.loomy-plugin, mode 0700). Survives CPA restarts.

type reqRecord struct {
	TS               int64  `json:"ts"` // unix ms
	Account          string `json:"account"`
	AuthID           string `json:"auth_id,omitempty"`
	Model            string `json:"model"`
	Stream           bool   `json:"stream,omitempty"`
	Status           int    `json:"status"`
	MS               int64  `json:"ms"`
	TTFB             int64  `json:"ttfb,omitempty"`
	PromptTokens     int64  `json:"pt,omitempty"`
	CompletionTokens int64  `json:"ct,omitempty"`
	Error            string `json:"error,omitempty"`
}

type eventRecord struct {
	TS     int64  `json:"ts"`
	Level  string `json:"level"` // info | warn | error
	Kind   string `json:"kind"`  // renew | probe | account | panic | system
	Msg    string `json:"msg"`
	Detail string `json:"detail,omitempty"`
}

type pluginSettings struct {
	RenewPaused bool `json:"renew_paused"`
}

const (
	maxReqMemory   = 50000
	maxEventMemory = 500
	compactBytes   = 16 << 20
)

var (
	storeMu   sync.Mutex
	storeDir  string
	requests  []reqRecord // oldest first
	events    []eventRecord
	settings  pluginSettings
	reqFile   *os.File
	evFile    *os.File
	lastPrune time.Time
)

func initStore() {
	dir := currentConfig().DataDir
	storeMu.Lock()
	defer storeMu.Unlock()
	if dir == storeDir && reqFile != nil {
		return
	}
	closeFilesLocked()
	storeDir = dir
	requests, events = nil, nil
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.Chmod(dir, 0o700)
	cutoff := retentionCutoff()
	readJSONL(filepath.Join(dir, "requests.jsonl"), func(line []byte) {
		var r reqRecord
		if json.Unmarshal(line, &r) == nil && r.TS >= cutoff {
			requests = append(requests, r)
		}
	})
	if len(requests) > maxReqMemory {
		requests = append([]reqRecord(nil), requests[len(requests)-maxReqMemory:]...)
	}
	readJSONL(filepath.Join(dir, "events.jsonl"), func(line []byte) {
		var e eventRecord
		if json.Unmarshal(line, &e) == nil {
			events = append(events, e)
		}
	})
	if len(events) > maxEventMemory {
		events = append([]eventRecord(nil), events[len(events)-maxEventMemory:]...)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.Unmarshal(raw, &settings)
	}
	reqFile, _ = os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	evFile, _ = os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	compactLocked(true)
}

func readJSONL(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			fn(line)
		}
	}
}

func retentionCutoff() int64 {
	return time.Now().Add(-time.Duration(currentConfig().RetentionDays) * 24 * time.Hour).UnixMilli()
}

func closeFilesLocked() {
	if reqFile != nil {
		_ = reqFile.Close()
		reqFile = nil
	}
	if evFile != nil {
		_ = evFile.Close()
		evFile = nil
	}
}

func flushStore() {
	storeMu.Lock()
	defer storeMu.Unlock()
	if reqFile != nil {
		_ = reqFile.Sync()
	}
	if evFile != nil {
		_ = evFile.Sync()
	}
}

func recordRequest(r reqRecord) {
	storeMu.Lock()
	defer storeMu.Unlock()
	requests = append(requests, r)
	if len(requests) > maxReqMemory {
		requests = append([]reqRecord(nil), requests[len(requests)-maxReqMemory*9/10:]...)
	}
	if reqFile != nil {
		line, _ := json.Marshal(r)
		_, _ = reqFile.Write(append(line, '\n'))
	}
	if time.Since(lastPrune) > time.Hour {
		compactLocked(false)
	}
}

// compactLocked drops expired records from memory and rewrites the file when
// it has grown too large.
func compactLocked(force bool) {
	lastPrune = time.Now()
	cutoff := retentionCutoff()
	i := sort.Search(len(requests), func(i int) bool { return requests[i].TS >= cutoff })
	if i > 0 {
		requests = append([]reqRecord(nil), requests[i:]...)
	}
	if storeDir == "" || reqFile == nil {
		return
	}
	st, err := reqFile.Stat()
	if err != nil || (!force && st.Size() < compactBytes) || (force && st.Size() < compactBytes/4) {
		return
	}
	path := filepath.Join(storeDir, "requests.jsonl")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, r := range requests {
		line, _ := json.Marshal(r)
		_, _ = w.Write(append(line, '\n'))
	}
	_ = w.Flush()
	_ = f.Close()
	_ = reqFile.Close()
	_ = os.Rename(tmp, path)
	reqFile, _ = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func logEvent(level, kind, msg, detail string) {
	e := eventRecord{TS: time.Now().UnixMilli(), Level: level, Kind: kind, Msg: truncate(msg, 300), Detail: truncate(detail, 2000)}
	storeMu.Lock()
	defer storeMu.Unlock()
	events = append(events, e)
	if len(events) > maxEventMemory {
		events = append([]eventRecord(nil), events[len(events)-maxEventMemory:]...)
	}
	if evFile != nil {
		line, _ := json.Marshal(e)
		_, _ = evFile.Write(append(line, '\n'))
		if st, err := evFile.Stat(); err == nil && st.Size() > 4<<20 {
			// keep the tail only
			path := filepath.Join(storeDir, "events.jsonl")
			_ = evFile.Close()
			buf := &bytes.Buffer{}
			for _, ev := range events {
				l, _ := json.Marshal(ev)
				buf.Write(append(l, '\n'))
			}
			_ = os.WriteFile(path, buf.Bytes(), 0o600)
			evFile, _ = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		}
	}
}

func settingsSnapshot() pluginSettings {
	storeMu.Lock()
	defer storeMu.Unlock()
	return settings
}

func updateSettings(fn func(*pluginSettings)) pluginSettings {
	storeMu.Lock()
	defer storeMu.Unlock()
	fn(&settings)
	if storeDir != "" {
		raw, _ := json.MarshalIndent(settings, "", "  ")
		_ = os.WriteFile(filepath.Join(storeDir, "settings.json"), raw, 0o600)
	}
	return settings
}

// ---- queries ----

type reqQuery struct {
	Since, Until int64
	Account      string
	Model        string
	Status       string // ok | fail | ""
	Keyword      string
	Page, Size   int
}

func (q reqQuery) match(r reqRecord) bool {
	if q.Since > 0 && r.TS < q.Since {
		return false
	}
	if q.Until > 0 && r.TS > q.Until {
		return false
	}
	if q.Account != "" && r.Account != q.Account {
		return false
	}
	if q.Model != "" && r.Model != q.Model {
		return false
	}
	switch q.Status {
	case "ok":
		if r.Status != 200 {
			return false
		}
	case "fail":
		if r.Status == 200 {
			return false
		}
	}
	if q.Keyword != "" && !strings.Contains(strings.ToLower(r.Error+" "+r.Model+" "+r.Account), strings.ToLower(q.Keyword)) {
		return false
	}
	return true
}

type reqPage struct {
	Items    []reqRecord `json:"items"`
	Total    int         `json:"total"`
	Page     int         `json:"page"`
	Size     int         `json:"size"`
	Accounts []string    `json:"accounts"`
	Models   []string    `json:"models"`
}

func queryRequests(q reqQuery, all bool) reqPage {
	storeMu.Lock()
	defer storeMu.Unlock()
	if q.Size <= 0 || q.Size > 200 {
		q.Size = 20
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	accSet, modelSet := map[string]bool{}, map[string]bool{}
	matched := make([]reqRecord, 0, 64)
	for i := len(requests) - 1; i >= 0; i-- { // newest first
		r := requests[i]
		accSet[r.Account] = true
		modelSet[r.Model] = true
		if q.match(r) {
			matched = append(matched, r)
		}
	}
	page := reqPage{Total: len(matched), Page: q.Page, Size: q.Size, Accounts: keys(accSet), Models: keys(modelSet)}
	if all {
		page.Items = matched
		return page
	}
	start := (q.Page - 1) * q.Size
	if start < len(matched) {
		page.Items = matched[start:min(start+q.Size, len(matched))]
	} else {
		page.Items = []reqRecord{}
	}
	return page
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

type acctStat struct {
	Today, TodayFail int64
	LastTS           int64
	LastError        string
}

type reqStats struct {
	Today, TodayOK int64
	TodayTokens    int64
	AvgMS          int64
	PerAccount     map[string]*acctStat
	Days           []dayStat
}

type dayStat struct {
	Day  string `json:"day"`
	OK   int64  `json:"ok"`
	Fail int64  `json:"fail"`
}

func computeStats() reqStats {
	storeMu.Lock()
	defer storeMu.Unlock()
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixMilli()
	st := reqStats{PerAccount: map[string]*acctStat{}}
	dayIdx := map[string]int{}
	for d := 6; d >= 0; d-- {
		key := now.AddDate(0, 0, -d).Format("01-02")
		dayIdx[key] = len(st.Days)
		st.Days = append(st.Days, dayStat{Day: key})
	}
	weekAgo := now.AddDate(0, 0, -7).UnixMilli()
	var msSum, msN int64
	for _, r := range requests {
		if r.TS < weekAgo {
			continue
		}
		key := time.UnixMilli(r.TS).Format("01-02")
		if i, ok := dayIdx[key]; ok {
			if r.Status == 200 {
				st.Days[i].OK++
			} else {
				st.Days[i].Fail++
			}
		}
		a := st.PerAccount[r.Account]
		if a == nil {
			a = &acctStat{}
			st.PerAccount[r.Account] = a
		}
		a.LastTS = r.TS
		if r.Status != 200 && r.Error != "" {
			a.LastError = r.Error
		}
		if r.TS < midnight {
			continue
		}
		st.Today++
		a.Today++
		if r.Status == 200 {
			st.TodayOK++
			msSum += r.MS
			msN++
		} else {
			a.TodayFail++
		}
		st.TodayTokens += r.PromptTokens + r.CompletionTokens
	}
	if msN > 0 {
		st.AvgMS = msSum / msN
	}
	return st
}

func snapshotEvents(level string, limit int) []eventRecord {
	storeMu.Lock()
	defer storeMu.Unlock()
	out := make([]eventRecord, 0, min(limit, len(events)))
	for i := len(events) - 1; i >= 0 && len(out) < limit; i-- {
		if level == "" || events[i].Level == level {
			out = append(out, events[i])
		}
	}
	return out
}
