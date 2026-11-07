package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// "Prefer expiring points" (settings.prefer_daily, default off).
//
// The 5000/day pool is wiped at 00:05 CST; permanent points never expire. With
// this switch on, an account whose daily pool is spent (daily <= DailyFloor)
// does not touch its permanent points while any other usable account still has
// daily points left: the request is soft-rejected with a local 429 whose
// Retry-After runs to the next day roll-over, so CPA cools that account and
// routes to the next one. Only when every usable account has spent its daily
// pool do permanent points start being used, and the accounts held so far are
// released at that moment.
//
// Weights (CPA "priority") compose naturally: CPA only picks from the highest
// priority tier that still has an available account, so the high tier spends
// its daily pools first, the held accounts drop out of that tier, the next tier
// spends its daily pools, and only after that does the highest tier come back
// (released) to spend permanent points.
//
// daily_only is stricter (never spend permanent points) and wins when both are
// on. Free (x0.0) models are never held.

const (
	poolTTL        = 20 * time.Second
	longCooldown   = 10 * time.Minute // a cooldown longer than this means "not coming back soon"
	preferWatchdog = 60 * time.Second
)

type poolMember struct {
	AuthIndex string
	Key       string // sessionKey(session)
	session   string // never logged or serialised
	Priority  int    // CPA priority; <= 0 shown as weight 1
	Daily     float64
	Known     bool // daily balance known
	Usable    bool // enabled, not invalid, not in a long cooldown
}

// dailyLeft reports a member that can still spend daily points.
func (m poolMember) dailyLeft(floor float64) bool {
	return m.Usable && m.Known && m.Daily > floor
}

// preferDailyHold decides whether self must be held: its own daily pool is spent
// and at least one other usable account still has daily points.
func preferDailyHold(selfKey string, selfDaily float64, members []poolMember, floor float64) (bool, int) {
	if selfDaily > floor {
		return false, 0
	}
	others := 0
	for _, m := range members {
		if m.Key != selfKey && m.dailyLeft(floor) {
			others++
		}
	}
	return others > 0, others
}

// poolOutOfDaily reports that no usable account has daily points left, i.e.
// the pool is in the "permanent points" phase.
func poolOutOfDaily(members []poolMember, floor float64) bool {
	any := false
	for _, m := range members {
		if !m.Usable {
			continue
		}
		any = true
		if !m.Known || m.Daily > floor {
			return false // unknown counts as "maybe has daily": do not release on a guess
		}
	}
	return any
}

var (
	poolMu    sync.Mutex
	poolCache []poolMember
	poolAt    time.Time
	poolBusy  bool

	preferMu       sync.Mutex
	preferReleased string // business day the held accounts were last released
	preferHeld     = map[string]bool{}

	// overridable in tests
	poolMembersFn = loadPoolMembers
	releaseFn     = releasePreferHeld
)

// loadPoolMembers reads the pool membership through host callbacks. Balances
// are not cached here: withBalances fills them from the guard cache each time.
func loadPoolMembers() []poolMember {
	files, err := loomyFiles()
	if err != nil {
		return nil
	}
	now := time.Now()
	out := make([]poolMember, 0, len(files))
	for _, f := range files {
		auth, _, _, errGet := getLoomyAuth(f.AuthIndex)
		if errGet != nil || auth.Session == "" {
			continue
		}
		m := poolMember{AuthIndex: f.AuthIndex, Key: sessionKey(auth.Session), Priority: f.Priority, session: auth.Session}
		preferMu.Lock()
		ownedHold := preferHeld[m.Key]
		preferMu.Unlock()
		m.Usable = poolAuthUsable(auth, f, ownedHold, now)
		out = append(out, m)
	}
	return out
}

// Own holds are included in the pool so they can be released, but never
// resurrect a disabled or expired credential.
func poolAuthUsable(auth *loomyAuth, f hostAuthEntry, ownedHold bool, now time.Time) bool {
	days, _ := sessionDaysLeft(auth, f.ModTime)
	lifetimeKnown := auth.ExpiresAt != "" || !f.ModTime.IsZero()
	return (!lifetimeKnown || days > 0) && !f.Disabled && !strings.EqualFold(f.Status, "disabled") &&
		(ownedHold || (!f.Unavailable && !f.NextRetryAfter.After(now.Add(longCooldown))))
}

// poolSnapshot returns a cached view; a stale one is refreshed in the
// background so the request path never waits on more than the first load.
func poolSnapshot() []poolMember {
	poolMu.Lock()
	fresh := poolCache != nil && time.Since(poolAt) < poolTTL
	have := poolCache != nil
	out := poolCache
	start := !fresh && !poolBusy
	if start {
		poolBusy = true
	}
	poolMu.Unlock()
	if fresh {
		return out
	}
	load := func() []poolMember {
		m := poolMembersFn()
		poolMu.Lock()
		poolBusy = false
		if m == nil {
			m = []poolMember{}
		}
		poolCache, poolAt = m, time.Now()
		poolMu.Unlock()
		return m
	}
	if !start {
		return out
	}
	if have {
		goSafe("pool-refresh", func() { load() })
		return out
	}
	return load()
}

// withBalances copies the members and fills each balance from the guard
// cache. Unknown balances of usable accounts are fetched now (in parallel,
// bounded by the client timeout) so the first decision of the day is right.
func withBalances(members []poolMember, skipKey string) []poolMember {
	out := make([]poolMember, len(members))
	copy(out, members)
	var wg sync.WaitGroup
	for i := range out {
		m := &out[i]
		if m.session == "" { // test stubs carry their own balances
			continue
		}
		m.Daily, m.Known = guardDailyPeek(m.session)
		if m.Known || !m.Usable || m.Key == skipKey {
			continue
		}
		wg.Add(1)
		goSafe("pool-balance", func() {
			defer wg.Done()
			m.Daily, m.Known = guardDaily(m.session)
		})
	}
	wg.Wait()
	return out
}

func invalidatePool() {
	poolMu.Lock()
	poolAt = time.Time{}
	poolMu.Unlock()
}

// preferBlock is the prefer_daily step of guardBlock (daily_only off, paid model).
func preferBlock(session string, s pluginSettings) ([]byte, string) {
	daily, ok := guardDailyNear(session, s.DailyFloor)
	if !ok || daily > s.DailyFloor {
		return nil, ""
	}
	key := sessionKey(session)
	members := withBalances(poolSnapshot(), key)
	hold, others := preferDailyHold(key, daily, members, s.DailyFloor)
	if !hold {
		maybeReleaseHeld(members, s)
		return nil, ""
	}
	preferMu.Lock()
	first := !preferHeld[key]
	preferHeld[key] = true
	preferMu.Unlock()
	reset := nextDailyReset(time.Now())
	msg := fmt.Sprintf("今日额度已用完（剩 %.0f，底线 %.0f），还有 %d 个账号有每日额度，先用它们；本号 %s 前不动永久积分", daily, s.DailyFloor, others, reset.Format("01-02 15:04"))
	if first {
		logEvent("info", "guard", "优先用快到期积分：暂停 1 个已用完每日额度的账号", msg)
	}
	return rateLimited("daily_preferred", msg, int(time.Until(reset).Seconds())+1), msg
}

// maybeReleaseHeld lifts the cooldowns once per business day when the whole
// pool has run out of daily points, so permanent points can be spent across
// every account (highest weight tier first, as CPA picks).
func maybeReleaseHeld(members []poolMember, s pluginSettings) {
	if !s.PreferDaily || s.DailyOnly || !poolOutOfDaily(members, s.DailyFloor) {
		return
	}
	day := businessDay(time.Now())
	preferMu.Lock()
	if preferReleased == day && len(preferHeld) == 0 {
		preferMu.Unlock()
		return
	}
	preferReleased = day
	held := ownedPreferMembers(members)
	preferHeld = map[string]bool{}
	preferMu.Unlock()
	if len(held) == 0 {
		return
	}
	goSafe("prefer-release", func() { releaseFn(held, s.DailyFloor) })
	logEvent("info", "guard", fmt.Sprintf("所有可用账号的每日额度都已用完，开始使用永久积分（释放 %d 个本功能暂停的账号）", len(held)), "")
}

// ownedPreferMembers requires preferMu. Never infer ownership from a low balance.
func ownedPreferMembers(members []poolMember) []poolMember {
	var held []poolMember
	for _, m := range members {
		if preferHeld[m.Key] {
			held = append(held, m)
		}
	}
	return held
}

// The host API is credential-wide (not model/reason-scoped). Limit its use to
// credentials actually held here; the host API does not expose finer-grained control.
func releasePreferHeld(members []poolMember, _ float64) {
	for _, m := range members {
		if m.AuthIndex != "" {
			_, _ = callHost("host.routing.reset_cooldown", map[string]any{"auth_index": m.AuthIndex})
		}
	}
	invalidatePool()
}

// releasePreferOnDisable does not touch accounts held for unrelated reasons.
func releasePreferOnDisable() {
	members := poolMembersFn()
	preferMu.Lock()
	held := ownedPreferMembers(members)
	preferHeld = map[string]bool{}
	preferReleased = ""
	preferMu.Unlock()
	if len(held) > 0 {
		releaseFn(held, 0)
	}
	invalidatePool()
}

// preferTick runs every minute: keeps balances warm for the pool view and
// releases held accounts when no request would reach the plugin any more
// (every remaining account held or gone).
func preferTick() {
	s := settingsSnapshot()
	if !s.PreferDaily || s.DailyOnly {
		return
	}
	invalidatePool()
	members := poolMembersFn()
	poolMu.Lock()
	poolCache, poolAt = members, time.Now()
	poolMu.Unlock()
	maybeReleaseHeld(withBalances(members, ""), s)
}

func startPreferWatchdog() {
	goSafe("prefer-watchdog", func() {
		t := time.NewTicker(preferWatchdog)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				preferTick()
			case <-schedStop:
				return
			}
		}
	})
}

// resetPreferState forgets holds (switch turned off, or tests).
func resetPreferState() {
	preferMu.Lock()
	preferHeld = map[string]bool{}
	preferReleased = ""
	preferMu.Unlock()
	invalidatePool()
}
