package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Points consumption, read from the vendor ledger (GET /points/records).
// Every debit row carries direction, pointsActual, consumeSource (daily |
// permanent) and dailyCycleDate (business day, rolls over at 00:05 CST) —
// fields confirmed by MYCF711/magpie-loomy-auth on real accounts. The ledger
// has no model field, so per-model numbers come from our own request log.

const (
	ledgerPageSize = 50
	ledgerMaxPages = 6
	usageDays      = 7
)

func businessDay(t time.Time) string {
	return t.In(cst).Add(-5 * time.Minute).Format("2006-01-02")
}

type ledgerDay struct {
	Day       string  `json:"day"`
	Daily     float64 `json:"daily"`
	Permanent float64 `json:"permanent"`
}

type usageAccount struct {
	AuthIndex      string      `json:"auth_index"`
	Phone          string      `json:"phone"`
	Note           string      `json:"note,omitempty"`
	Daily          float64     `json:"daily"`
	Permanent      float64     `json:"permanent"`
	TodayDaily     float64     `json:"today_daily"`
	TodayPermanent float64     `json:"today_permanent"`
	Days           []ledgerDay `json:"days"`
	PaidRequests   int64       `json:"paid_requests"`
	PerRequest     float64     `json:"per_request"`
	RunsLeft       int64       `json:"runs_left"` // -1 = unknown
	Partial        bool        `json:"partial,omitempty"`
	Error          string      `json:"error,omitempty"`
	At             int64       `json:"at"`
}

var (
	usageMu    sync.Mutex
	usageCache = map[string]usageAccount{}
)

func fetchLedger(session string, days map[string]*ledgerDay, oldest string) (bool, error) {
	seen := 0
	for page := 1; page <= ledgerMaxPages; page++ {
		path := fmt.Sprintf("%s?pageNo=%d&pageSize=%d&recordType=all", pointsPath, page, ledgerPageSize)
		d, err := gatewayCall(http.MethodGet, path, session, nil)
		if err != nil {
			return false, err
		}
		list, _ := d["list"].([]any)
		total := int(toFloat(d["total"]))
		older := 0
		for _, it := range list {
			r, _ := it.(map[string]any)
			if r == nil {
				continue
			}
			day := strings.TrimSpace(fmt.Sprint(r["dailyCycleDate"]))
			if day == "" || day == "<nil>" {
				if ts := recordTime(r); !ts.IsZero() {
					day = businessDay(ts)
				}
			}
			if day != "" && day < oldest {
				older++
				continue
			}
			if r["direction"] != "debit" {
				continue
			}
			ld := days[day]
			if ld == nil {
				continue
			}
			pts := toFloat(r["pointsActual"])
			if pts <= 0 {
				pts = toFloat(r["points"])
			}
			if r["consumeSource"] == "daily" {
				ld.Daily += pts
			} else {
				ld.Permanent += pts
			}
		}
		seen += len(list)
		// newest first: once a whole page is older than the window we are done
		if len(list) == 0 || seen >= total || older == len(list) {
			return true, nil
		}
	}
	return false, nil
}

// recordTime reads whichever timestamp field the ledger row carries.
func recordTime(r map[string]any) time.Time {
	for _, k := range []string{"createTime", "createdAt", "createAt", "gmtCreate", "recordTime", "time"} {
		switch v := r[k].(type) {
		case float64:
			if v > 1e12 {
				return time.UnixMilli(int64(v))
			}
			if v > 1e9 {
				return time.Unix(int64(v), 0)
			}
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 1e12 {
				return time.UnixMilli(n)
			}
			for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05"} {
				if t, err := time.ParseInLocation(layout, v, cst); err == nil {
					return t
				}
			}
		}
	}
	return time.Time{}
}

func loadUsage(f hostAuthEntry, force bool) usageAccount {
	usageMu.Lock()
	if u, ok := usageCache[f.AuthIndex]; ok && !force && time.Since(time.UnixMilli(u.At)) < 10*time.Minute {
		usageMu.Unlock()
		return u
	}
	usageMu.Unlock()
	u := usageAccount{AuthIndex: f.AuthIndex, Note: f.Note, RunsLeft: -1, At: time.Now().UnixMilli()}
	auth, _, _, err := getLoomyAuth(f.AuthIndex)
	if err != nil {
		u.Error = err.Error()
		return u
	}
	u.Phone = maskPhone(auth.Phone)
	if u.Note == "" {
		u.Note = auth.Note
	}
	now := time.Now()
	days := map[string]*ledgerDay{}
	order := make([]string, 0, usageDays)
	for i := usageDays - 1; i >= 0; i-- {
		d := businessDay(now.AddDate(0, 0, -i))
		days[d] = &ledgerDay{Day: d}
		order = append(order, d)
	}
	complete, err := fetchLedger(auth.Session, days, order[0])
	if err != nil {
		u.Error = err.Error()
	}
	u.Partial = !complete
	for _, d := range order {
		u.Days = append(u.Days, *days[d])
	}
	today := days[order[len(order)-1]]
	u.TodayDaily, u.TodayPermanent = today.Daily, today.Permanent
	if p, errP := fetchPoints(auth.Session); errP == nil {
		u.Daily, u.Permanent = p.Daily, p.Permanent
	} else if u.Error == "" {
		u.Error = errP.Error()
	}
	usageMu.Lock()
	usageCache[f.AuthIndex] = u
	usageMu.Unlock()
	return u
}

type modelUsage struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	OK       int64  `json:"ok"`
	Tokens   int64  `json:"tokens"`
	Free     bool   `json:"free"`
	Multiple string `json:"multiple,omitempty"`
}

func handleUsage(force bool) []byte {
	files, err := loomyFiles()
	if err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	rows := make([]usageAccount, len(files))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		i, f := i, f
		goSafe("usage", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = loadUsage(f, force)
		})
	}
	wg.Wait()
	ensureModelCache(files)

	// today's paid requests per account and per-model counts, from our log
	dayStart := todayStartMS()
	paid := map[string]int64{}
	models := map[string]*modelUsage{}
	storeMu.Lock()
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		if r.TS < dayStart {
			break
		}
		if r.Local {
			continue
		}
		m := models[r.Model]
		if m == nil {
			m = &modelUsage{Model: r.Model}
			models[r.Model] = m
		}
		m.Requests++
		m.Tokens += r.PromptTokens + r.CompletionTokens
		if r.Status == 200 {
			m.OK++
		}
	}
	storeMu.Unlock()
	names := map[string]string{}
	for _, gm := range cachedModelUnion() {
		names[strings.ToLower(gm.ID)] = gm.Name
	}
	modelList := make([]modelUsage, 0, len(models))
	for _, m := range models {
		if n, ok := names[strings.ToLower(m.Model)]; ok {
			m.Multiple, m.Free = parseMultiple(n)
		} else {
			m.Free = defaultFreeModels[strings.ToLower(m.Model)]
		}
		modelList = append(modelList, *m)
	}
	sort.Slice(modelList, func(i, j int) bool { return modelList[i].Requests > modelList[j].Requests })

	storeMu.Lock()
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		if r.TS < dayStart {
			break
		}
		if r.Local || r.Status != 200 || isFreeModel(r.Model) {
			continue
		}
		paid[r.Account]++
	}
	storeMu.Unlock()

	var sumDaily, sumPerm, sumToday float64
	for i := range rows {
		u := &rows[i]
		u.PaidRequests = paid[u.Phone]
		spent := u.TodayDaily + u.TodayPermanent
		if u.PaidRequests > 0 && spent > 0 {
			u.PerRequest = spent / float64(u.PaidRequests)
			u.RunsLeft = int64(u.Daily / u.PerRequest)
		}
		sumDaily += u.TodayDaily
		sumPerm += u.TodayPermanent
		sumToday += u.Daily
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Phone < rows[j].Phone })
	week := make([]ledgerDay, usageDays)
	for _, u := range rows {
		for i, d := range u.Days {
			if i < len(week) {
				week[i].Day = d.Day
				week[i].Daily += d.Daily
				week[i].Permanent += d.Permanent
			}
		}
	}
	return mgmtJSON(200, map[string]any{
		"items":  rows,
		"models": modelList,
		"week":   week,
		"summary": map[string]any{
			"today_daily": sumDaily, "today_permanent": sumPerm, "daily_left": sumToday,
		},
		"business_day": businessDay(time.Now()),
	})
}

// todayStartMS is the start of the current business day (00:05 CST), so the
// request counts line up with the ledger's dailyCycleDate.
func todayStartMS() int64 {
	t := time.Now().In(cst).Add(-5 * time.Minute)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 5, 0, 0, cst).UnixMilli()
}

// ensureModelCache loads every enabled account; a first success is not a pool catalogue.
func ensureModelCache(files []hostAuthEntry) {
	for _, f := range files {
		if f.Disabled {
			continue
		}
		if auth, _, _, err := getLoomyAuth(f.AuthIndex); err == nil {
			_, _ = fetchGatewayModels(auth.Session, false)
		}
	}
}
