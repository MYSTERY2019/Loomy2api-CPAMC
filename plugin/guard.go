package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pool protection, borrowed from workbuddy2api-hub:
//
//  1. Risk circuit breaker: when two or more different accounts hit an upstream
//     risk-control rejection (HTTP 403 / TM.* codes) inside riskWindow, every
//     request is held locally for RiskPauseSec so the whole pool is not dragged
//     into the same ban wave. 0 disables the breaker.
//  2. Points guard ("daily pool only"): a paid model request is answered with a
//     local 429 when that account's daily balance is at or below the floor, so
//     the one-off permanent points are never spent. Free models pass through.
//     prefer_daily (prefer.go) is the softer variant: permanent points are
//     only held back while another account still has daily points.
//  3. Per-account device id: a stable UUID derived from the phone number, sent
//     where the desktop client sends one (invitation activation).

const (
	defaultRiskPauseSec = 1800
	riskWindow          = 10 * time.Minute
	riskAccounts        = 2
	guardTTL            = 60 * time.Second
)

// ---- settings helpers ----

func riskPauseSec(s pluginSettings) int {
	if s.RiskPauseSec == nil {
		return defaultRiskPauseSec
	}
	return *s.RiskPauseSec
}

// ---- 1. risk circuit breaker ----

var (
	riskMu   sync.Mutex
	riskHits = map[string]time.Time{} // account -> last risk rejection
)

// isRiskRejection reports upstream answers that look like risk control rather
// than an expired session (Loomy signals expiry as HTTP 200 + code 1000xx).
func isRiskRejection(status int, code, body string) bool {
	if status == http.StatusForbidden {
		return true
	}
	if strings.HasPrefix(strings.ToUpper(code), "TM.") {
		return true
	}
	return strings.Contains(body, "TM.0000") || strings.Contains(body, "风控") || strings.Contains(body, "异常行为")
}

// noteRisk records a rejection and trips the breaker when enough distinct
// accounts were hit inside the window.
func noteRisk(account, detail string) {
	now := time.Now()
	riskMu.Lock()
	riskHits[account] = now
	n := 0
	for acc, t := range riskHits {
		if now.Sub(t) > riskWindow {
			delete(riskHits, acc)
			continue
		}
		n++
	}
	riskMu.Unlock()
	logEvent("warn", "risk", account+" 被上游风控拒绝", truncate(detail, 300))
	s := settingsSnapshot()
	sec := riskPauseSec(s)
	if sec <= 0 || n < riskAccounts {
		return
	}
	until := now.Add(time.Duration(sec) * time.Second).UnixMilli()
	if s.PausedUntil >= until-60_000 {
		return // already paused (roughly) that long
	}
	updateSettings(func(s *pluginSettings) {
		s.PausedUntil = until
		s.PauseReason = fmt.Sprintf("%d 个账号在 %d 分钟内被风控拒绝", n, int(riskWindow.Minutes()))
	})
	logEvent("error", "risk", fmt.Sprintf("风控熔断：%d 个账号在 %d 分钟内被拒绝，暂停派发 %d 分钟", n, int(riskWindow.Minutes()), sec/60), "")
}

// pauseRemaining returns how long the breaker still holds requests.
func pauseRemaining() time.Duration {
	s := settingsSnapshot()
	if s.PausedUntil == 0 {
		return 0
	}
	left := time.Until(time.UnixMilli(s.PausedUntil))
	if left <= 0 {
		return 0
	}
	return left
}

// releasePause lifts the breaker and clears the CPA cooldowns it caused.
func releasePause(manual bool) {
	updateSettings(func(s *pluginSettings) { s.PausedUntil = 0; s.PauseReason = "" })
	riskMu.Lock()
	riskHits = map[string]time.Time{}
	riskMu.Unlock()
	resetAllCooldowns()
	if manual {
		logEvent("info", "risk", "已手动解除风控熔断", "")
	}
}

// resetAllCooldowns clears every account. Per-model cooldowns are not visible
// in host.auth.list, so filtering on NextRetryAfter would miss them.
func resetAllCooldowns() {
	files, err := loomyFiles()
	if err != nil {
		return
	}
	for _, f := range files {
		if !f.Disabled {
			_, _ = callHost("host.routing.reset_cooldown", map[string]any{"auth_index": f.AuthIndex})
		}
	}
}

// ---- 2. points guard ----

type guardEntry struct {
	daily   float64
	ok      bool
	at      time.Time
	busy    bool
	retryAt time.Time // brief failure backoff, prevents request-path retry storms
}

var (
	guardMu    sync.Mutex
	guardCache = map[string]*guardEntry{} // session hash -> daily balance
	dailyHeld  = map[string]bool{}        // cooldown ownership, not inferred from the balance
)

func sessionKey(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:8])
}

// guardDaily returns the cached daily balance. A stale value triggers an async
// refresh; an unknown one is fetched synchronously (bounded by the client
// timeout). ok=false means "unknown", and the guard then lets the request pass.
func guardDaily(session string) (float64, bool) {
	key := sessionKey(session)
	guardMu.Lock()
	e := guardCache[key]
	if e == nil {
		e = &guardEntry{}
		guardCache[key] = e
	}
	fresh := e.ok && time.Since(e.at) < guardTTL
	known := e.ok
	daily := e.daily
	start := !fresh && !e.busy && !time.Now().Before(e.retryAt)
	if start {
		e.busy = true
	}
	guardMu.Unlock()
	if fresh {
		return daily, true
	}
	refresh := func() {
		p, err := fetchPoints(session)
		guardMu.Lock()
		e.busy = false
		if err == nil {
			e.daily, e.ok, e.at = p.Daily, true, time.Now()
			e.retryAt = time.Time{}
		} else {
			e.retryAt = time.Now().Add(5 * time.Second)
		}
		guardMu.Unlock()
	}
	if known {
		if start {
			goSafe("guard-refresh", refresh)
		}
		return daily, true
	}
	if start {
		refresh()
	}
	guardMu.Lock()
	defer guardMu.Unlock()
	return e.daily, e.ok
}

// nearMargin: below floor+nearMargin a stale balance is re-read before deciding,
// so a nearly spent account is not let through on a minute-old number.
const nearMargin = 2000

// guardDailyNear is guardDaily, but a stale cached balance close to the floor
// is refreshed synchronously.
func guardDailyNear(session string, floor float64) (float64, bool) {
	key := sessionKey(session)
	guardMu.Lock()
	e := guardCache[key]
	stale := e != nil && e.ok && time.Since(e.at) >= guardTTL && e.daily <= floor+nearMargin && !e.busy && !time.Now().Before(e.retryAt)
	if stale {
		e.busy = true
	}
	guardMu.Unlock()
	if stale {
		p, err := fetchPoints(session)
		guardMu.Lock()
		e.busy = false
		if err == nil {
			e.daily, e.ok, e.at = p.Daily, true, time.Now()
			e.retryAt = time.Time{}
		} else {
			e.retryAt = time.Now().Add(5 * time.Second)
		}
		guardMu.Unlock()
	}
	return guardDaily(session)
}

// guardDailyPeek returns the cached balance without waiting; a missing or
// stale entry is refreshed in the background.
func guardDailyPeek(session string) (float64, bool) {
	key := sessionKey(session)
	guardMu.Lock()
	e := guardCache[key]
	if e == nil {
		e = &guardEntry{}
		guardCache[key] = e
	}
	fresh := e.ok && time.Since(e.at) < guardTTL
	daily, ok := e.daily, e.ok
	start := !fresh && !e.busy && !time.Now().Before(e.retryAt)
	if start {
		e.busy = true
	}
	guardMu.Unlock()
	if start {
		goSafe("guard-peek", func() {
			p, err := fetchPoints(session)
			guardMu.Lock()
			e.busy = false
			if err == nil {
				e.daily, e.ok, e.at = p.Daily, true, time.Now()
				e.retryAt = time.Time{}
			} else {
				e.retryAt = time.Now().Add(5 * time.Second)
			}
			guardMu.Unlock()
		})
	}
	return daily, ok
}

// markGuardStale makes the next request re-read the balance after spending.
func markGuardStale(session string) {
	guardMu.Lock()
	if e := guardCache[sessionKey(session)]; e != nil {
		e.at = time.Time{}
	}
	guardMu.Unlock()
}

var defaultFreeModels = map[string]bool{"spark-x2.6": true, "spark-x": true}

// isFreeModel uses the gateway catalogue ("(限时免费)" / "(x0.0)" in the display
// name) and falls back to the two models known to be free.
func isFreeModel(id string) bool {
	list := cachedModelUnion()
	for _, m := range list {
		if strings.EqualFold(m.ID, id) {
			mult, free := parseMultiple(m.Name)
			// a catalogue name without any marker keeps the known-free default
			return free || mult == "" && defaultFreeModels[strings.ToLower(id)]
		}
	}
	return defaultFreeModels[strings.ToLower(id)]
}

// A free offer can differ by account: never borrow another account's price flag.
func isFreeModelForSession(session, id string) bool {
	e := accountCatalogCache(session)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.models {
		if m.ID == id {
			mult, free := parseMultiple(m.Name)
			return free || mult == "" && defaultFreeModels[strings.ToLower(id)]
		}
	}
	return defaultFreeModels[strings.ToLower(id)]
}

// nextDailyReset is the next 00:10 CST, a little after the 00:05 roll-over and
// when the auto check-in refills the pool.
func nextDailyReset(now time.Time) time.Time {
	t := now.In(cst)
	r := time.Date(t.Year(), t.Month(), t.Day(), 0, 10, 0, 0, cst)
	if !r.After(t) {
		r = r.AddDate(0, 0, 1)
	}
	return r
}

// guardBlock decides whether a request must be held locally. It returns the
// envelope to send back, or nil to continue.
func guardBlock(session, model string) ([]byte, string) {
	if left := pauseRemaining(); left > 0 {
		retry := int(left.Seconds())
		if retry > 60 {
			retry = 60 // short CPA cooldown; a manual release takes effect within a minute
		}
		msg := fmt.Sprintf("风控熔断中，约 %d 分钟后恢复（可在面板解除）", int(left.Minutes())+1)
		return rateLimited("risk_paused", msg, max(retry, 1)), msg
	}
	s := settingsSnapshot()
	if isFreeModelForSession(session, model) || !s.DailyOnly && !s.PreferDaily {
		return nil, ""
	}
	if !s.DailyOnly {
		return preferBlock(session, s)
	}
	daily, ok := guardDailyNear(session, s.DailyFloor)
	if !ok || daily > s.DailyFloor {
		return nil, ""
	}
	guardMu.Lock()
	dailyHeld[sessionKey(session)] = true
	guardMu.Unlock()
	reset := nextDailyReset(time.Now())
	msg := fmt.Sprintf("今日积分剩 %.0f（底线 %.0f），已开启“只用每日额度”，%s 后恢复", daily, s.DailyFloor, reset.Format("01-02 15:04"))
	return rateLimited("daily_exhausted", msg, int(time.Until(reset).Seconds())+1), msg
}

// Release only credentials actually blocked by daily_only in this process.
// Host reset remains credential-wide; see the audit's ABI limitation.
func releaseDailyOnDisable() {
	members := poolMembersFn()
	guardMu.Lock()
	var held []poolMember
	for _, m := range members {
		if dailyHeld[m.Key] {
			held = append(held, m)
		}
	}
	dailyHeld = map[string]bool{}
	guardMu.Unlock()
	if len(held) > 0 {
		releaseFn(held, 0)
	}
	invalidatePool()
}

func rateLimited(code, msg string, retry int) []byte {
	raw, _ := jsonMarshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: msg, HTTPStatus: http.StatusTooManyRequests, RetryAfterSeconds: retry}})
	return raw
}

// ---- 3. device id ----

// deviceID is stable per account so the vendor sees one device per account
// instead of one shared device for the whole pool.
func deviceID(a *loomyAuth) string {
	seed := a.Phone
	if seed == "" {
		seed = a.UserID
	}
	if seed == "" {
		seed = a.Session
	}
	sum := sha256.Sum256([]byte("loomy-cpa-device:" + seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func guardState() map[string]any {
	s := settingsSnapshot()
	left := pauseRemaining()
	riskMu.Lock()
	recent := 0
	for _, t := range riskHits {
		if time.Since(t) <= riskWindow {
			recent++
		}
	}
	riskMu.Unlock()
	st := map[string]any{
		"risk_pause_sec": riskPauseSec(s),
		"paused":         left > 0,
		"paused_left":    int(left.Seconds()),
		"pause_reason":   s.PauseReason,
		"recent_risk":    recent,
		"daily_only":     s.DailyOnly,
		"daily_floor":    s.DailyFloor,
		"prefer_daily":   s.PreferDaily,
		"next_reset":     nextDailyReset(time.Now()).UnixMilli(),
	}
	if left > 0 {
		st["paused_until"] = s.PausedUntil
	}
	return st
}
