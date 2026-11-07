package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---- helpers ----

func setTestSettings(t *testing.T, s pluginSettings) {
	t.Helper()
	storeMu.Lock()
	old := settings
	settings = s
	storeMu.Unlock()
	t.Cleanup(func() {
		storeMu.Lock()
		settings = old
		storeMu.Unlock()
	})
}

func setDaily(session string, daily float64) {
	guardMu.Lock()
	guardCache[sessionKey(session)] = &guardEntry{daily: daily, ok: true, at: time.Now()}
	guardMu.Unlock()
}

func stubPool(t *testing.T, members func() []poolMember) chan []poolMember {
	t.Helper()
	released := make(chan []poolMember, 4)
	oldPool, oldRelease := poolMembersFn, releaseFn
	poolMembersFn = members
	releaseFn = func(m []poolMember, _ float64) { released <- m }
	resetPreferState()
	poolMu.Lock()
	poolCache = nil
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMembersFn, releaseFn = oldPool, oldRelease
		resetPreferState()
		poolMu.Lock()
		poolCache = nil
		poolMu.Unlock()
	})
	return released
}

func envCode(t *testing.T, env []byte) (string, int, int) {
	t.Helper()
	if env == nil {
		return "", 0, 0
	}
	var e envelope
	if err := json.Unmarshal(env, &e); err != nil || e.Error == nil {
		t.Fatalf("bad envelope %s", env)
	}
	return e.Error.Code, e.Error.HTTPStatus, e.Error.RetryAfterSeconds
}

// ---- pure decision ----

func TestPreferDailyHold(t *testing.T) {
	members := []poolMember{
		{Key: "a", Daily: 0, Known: true, Usable: true},
		{Key: "b", Daily: 3000, Known: true, Usable: true},
		{Key: "c", Daily: 4000, Known: true, Usable: false}, // disabled / long cooldown
		{Key: "d", Daily: 4000, Known: false, Usable: true}, // unknown balance
	}
	if hold, n := preferDailyHold("a", 0, members, 0); !hold || n != 1 {
		t.Fatalf("a must be held for b, got %v %d", hold, n)
	}
	if hold, _ := preferDailyHold("b", 3000, members, 0); hold {
		t.Fatal("an account with daily left is never held")
	}
	if hold, _ := preferDailyHold("a", 0, members[:1], 0); hold {
		t.Fatal("alone: spend permanent")
	}
	if hold, _ := preferDailyHold("a", 0, []poolMember{members[0], members[2], members[3]}, 0); hold {
		t.Fatal("unusable or unknown accounts do not hold anyone")
	}
	// floor: b at 300 with floor 500 counts as spent
	m2 := []poolMember{{Key: "a", Daily: 100, Known: true, Usable: true}, {Key: "b", Daily: 300, Known: true, Usable: true}}
	if hold, _ := preferDailyHold("a", 100, m2, 500); hold {
		t.Fatal("both below the floor: nobody held")
	}
	if hold, _ := preferDailyHold("a", 100, m2, 200); !hold {
		t.Fatal("b above floor 200: a held")
	}
}

func TestPoolOutOfDaily(t *testing.T) {
	if poolOutOfDaily(nil, 0) {
		t.Fatal("empty pool is not 'out of daily'")
	}
	m := []poolMember{{Daily: 0, Known: true, Usable: true}, {Daily: 5000, Known: true, Usable: false}}
	if !poolOutOfDaily(m, 0) {
		t.Fatal("only unusable has daily: out")
	}
	m = append(m, poolMember{Known: false, Usable: true})
	if poolOutOfDaily(m, 0) {
		t.Fatal("unknown balance must not trigger a release")
	}
}

// ---- guardBlock integration ----

func TestGuardBlockPreferDaily(t *testing.T) {
	const sA, sB = "session-aaaaaaaaaaaaaaaa", "session-bbbbbbbbbbbbbbbb"
	daily := map[string]float64{sA: 0, sB: 2500}
	pool := func() []poolMember {
		out := []poolMember{}
		for _, s := range []string{sA, sB} {
			out = append(out, poolMember{AuthIndex: s[8:9], Key: sessionKey(s), Daily: daily[s], Known: true, Usable: true})
		}
		return out
	}
	released := stubPool(t, pool)
	setDaily(sA, daily[sA])
	setDaily(sB, daily[sB])

	// off by default: nothing held
	setTestSettings(t, pluginSettings{})
	if env, _ := guardBlock(sA, "DeepSeek-V4.1-Flash"); env != nil {
		t.Fatal("default settings must not hold")
	}

	setTestSettings(t, pluginSettings{PreferDaily: true})
	env, msg := guardBlock(sA, "DeepSeek-V4.1-Flash")
	code, status, retry := envCode(t, env)
	if code != "daily_preferred" || status != 429 {
		t.Fatalf("want soft 429, got %s %d (%s)", code, status, msg)
	}
	until := int(time.Until(nextDailyReset(time.Now())).Seconds())
	if retry < until-5 || retry > until+5 {
		t.Fatalf("cooldown must run to the next roll-over: %d vs %d", retry, until)
	}
	if env, _ := guardBlock(sB, "DeepSeek-V4.1-Flash"); env != nil {
		t.Fatal("B still has daily points")
	}
	for _, free := range []string{"spark-x", "spark-x2.6"} {
		if env, _ := guardBlock(sA, free); env != nil {
			t.Fatalf("free model %s must pass", free)
		}
	}

	// daily_only wins: never permanent, even with nobody else left
	setTestSettings(t, pluginSettings{PreferDaily: true, DailyOnly: true})
	if code, _, _ := envCode(t, mustEnv(guardBlock(sA, "GLM-5.3-Flash"))); code != "daily_exhausted" {
		t.Fatalf("daily_only must win, got %s", code)
	}

	// B runs out too -> permanent phase: A passes and held accounts are released once
	setTestSettings(t, pluginSettings{PreferDaily: true})
	daily[sB] = 0
	setDaily(sB, 0)
	invalidatePool()
	poolMu.Lock()
	poolCache = nil
	poolMu.Unlock()
	if env, _ := guardBlock(sA, "DeepSeek-V4.1-Flash"); env != nil {
		t.Fatal("whole pool out of daily: permanent points allowed")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("held accounts were not released")
	}
	if env, _ := guardBlock(sB, "DeepSeek-V4.1-Flash"); env != nil {
		t.Fatal("B passes in the permanent phase")
	}
	select {
	case <-released:
		t.Fatal("release must happen once")
	case <-time.After(100 * time.Millisecond):
	}
}

func mustEnv(env []byte, _ string) []byte { return env }

// TestPreferDailyWithWeights simulates CPA's tiered selection (highest priority
// with an available account first) over a day of paid requests and checks the
// spending order: high tier daily -> low tier daily -> permanent (high tier first).
func TestPreferDailyWithWeights(t *testing.T) {
	type acct struct {
		name      string
		session   string
		priority  int
		daily     float64
		permanent float64
		cooled    bool
	}
	accts := []*acct{
		{name: "H1", session: "session-h1-xxxxxxxxxxxxx", priority: 2, daily: 2000, permanent: 10000},
		{name: "H2", session: "session-h2-xxxxxxxxxxxxx", priority: 2, daily: 1000, permanent: 10000},
		{name: "L1", session: "session-l1-xxxxxxxxxxxxx", priority: 1, daily: 3000, permanent: 10000},
	}
	pool := func() []poolMember {
		out := []poolMember{}
		for _, a := range accts {
			out = append(out, poolMember{AuthIndex: a.name, Key: sessionKey(a.session), Priority: a.priority, Daily: a.daily, Known: true, Usable: !a.cooled})
		}
		return out
	}
	stubPool(t, pool)
	releasedCh := make(chan struct{}, 8)
	releaseFn = func([]poolMember, float64) { releasedCh <- struct{}{} }
	setTestSettings(t, pluginSettings{PreferDaily: true})

	pick := func() *acct {
		best := -1 << 30
		var tier []*acct
		for _, a := range accts {
			if a.cooled {
				continue
			}
			if a.priority > best {
				best, tier = a.priority, nil
			}
			if a.priority == best {
				tier = append(tier, a)
			}
		}
		if len(tier) == 0 {
			return nil
		}
		sort.Slice(tier, func(i, j int) bool { return tier[i].daily > tier[j].daily })
		return tier[0]
	}
	const cost = 500.0
	var log []string
	for step := 0; step < 40; step++ {
		var served *acct
		for tries := 0; tries < len(accts) && served == nil; tries++ {
			a := pick()
			if a == nil {
				t.Fatalf("step %d: no account available (deadlock)", step)
			}
			setDaily(a.session, a.daily)
			poolMu.Lock()
			poolCache = nil
			poolMu.Unlock()
			if env, _ := guardBlock(a.session, "DeepSeek-V4.1-Flash"); env != nil {
				a.cooled = true // CPA cooldown until the roll-over
				continue
			}
			served = a
		}
		if served == nil {
			t.Fatalf("step %d: request failed", step)
		}
		// release runs in a goroutine (reset_cooldown on every held account)
		select {
		case <-releasedCh:
			for _, a := range accts {
				a.cooled = false
			}
		case <-time.After(5 * time.Millisecond):
		}
		if served.daily >= cost {
			served.daily -= cost
			log = append(log, served.name+":daily")
		} else {
			served.permanent -= cost
			log = append(log, served.name+":perm")
		}
	}
	joined := strings.Join(log, " ")
	t.Log(joined)
	firstPerm := strings.Index(joined, ":perm")
	if firstPerm < 0 {
		t.Fatalf("permanent never used: %s", joined)
	}
	if strings.Contains(joined[firstPerm:], ":daily") {
		t.Fatalf("daily spent after permanent started: %s", joined)
	}
	lastHighDaily := max(strings.LastIndex(joined, "H1:daily"), strings.LastIndex(joined, "H2:daily"))
	if firstLow := strings.Index(joined, "L1:daily"); firstLow < lastHighDaily {
		t.Fatalf("low tier daily used before high tier daily was spent: %s", joined)
	}
	// the request that discovers the permanent phase may be served by the
	// account that saw it; after the release CPA is back on the high tier
	perms := []string{}
	for _, e := range log {
		if strings.HasSuffix(e, ":perm") {
			perms = append(perms, e)
		}
	}
	if len(perms) < 3 || !strings.HasPrefix(perms[1], "H") || !strings.HasPrefix(perms[2], "H") {
		t.Fatalf("after the release permanent points must come from the high tier: %v", perms)
	}
	for _, a := range accts {
		if a.daily >= cost {
			t.Fatalf("%s still has daily %v when permanent started", a.name, a.daily)
		}
	}
}

// ---- weight ----

func TestWeightOf(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 2: 2, 4: 4, 9: 4} {
		if got := weightOf(in); got != want {
			t.Fatalf("weightOf(%d)=%d want %d", in, got, want)
		}
	}
	doc := setRawField([]byte(`{"type":"loomy","session":"s","note":"n","disabled":false,"x":{"y":1}}`), "priority", 3)
	var m map[string]any
	_ = json.Unmarshal(doc, &m)
	if m["priority"] != float64(3) || m["note"] != "n" || m["x"] == nil || m["disabled"] != false {
		t.Fatalf("priority write lost fields: %s", doc)
	}
}

// ---- models ----

func TestParseGatewayModelsTolerant(t *testing.T) {
	raw := []byte(`{"data":[
	 {"id":"spark-x","name":"Spark-X2.5（限时免费）","type":"chat","context_length":"1048576","max_output_tokens":"65536","reasoning_efforts":["none","low"],"capabilities":{"vision":false,"function_calling":true}},
	 {"id":"Kimi-k2.6","name":"Kimi k2.6(x6.5)","context_length":262144,"capabilities":{"input_modalities":["text","image"]}},
	 {"id":"bad","context_length":{"weird":1}},
	 "not an object",
	 {"name":"no id"},
	 {"id":"Hy-Image-3.5-preview","type":"image"},
	 {"id":"auto","type":"chat"}
	]}`)
	out, skipped, err := parseGatewayModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || skipped != 2 {
		t.Fatalf("got %d models, %d skipped: %+v", len(out), skipped, out)
	}
	if out[0].ContextLength != 1048576 || out[0].MaxOutput != 65536 || len(out[0].ReasoningEfforts) != 2 {
		t.Fatalf("string numbers: %+v", out[0])
	}
	if !out[1].Capabilities.Vision || !out[1].Capabilities.FunctionCalling {
		t.Fatalf("modalities / default tools: %+v", out[1])
	}
	if out[2].ID != "bad" || out[2].ContextLength != 0 {
		t.Fatalf("odd field type must not drop the record: %+v", out[2])
	}
	if arr, _, err := parseGatewayModels([]byte(`[{"id":"mimo-v2.5","context_length":1048576}]`)); err != nil || len(arr) != 1 {
		t.Fatalf("bare array: %v %v", arr, err)
	}
	if _, _, err := parseGatewayModels([]byte(`<html>502</html>`)); err == nil {
		t.Fatal("garbage must be an error")
	}
}

func TestStaticModelsSnapshot(t *testing.T) {
	ms := staticGatewayModels()
	if len(ms) != 9 {
		t.Fatalf("want 9 snapshot models, got %d", len(ms))
	}
	seen := map[string]gatewayModel{}
	for _, m := range ms {
		seen[m.ID] = m
		if m.ContextLength <= 0 {
			t.Fatalf("%s has no context", m.ID)
		}
	}
	if _, bad := seen["deepseek-v4-flash-0731"]; bad {
		t.Fatal("0731 is not in the upstream catalogue")
	}
	if !seen["Kimi-k2.6"].Capabilities.Vision || seen["qwen-3.8-max"].Capabilities.Vision || seen["Kimi-k2.6"].ContextLength != 262144 {
		t.Fatal("vision / context mismatch with the measured snapshot")
	}
	rows := toModelRows(ms)
	free := map[string]bool{}
	for _, r := range rows {
		if r.Free {
			free[r.ID] = true
		}
	}
	if !free["spark-x"] || !free["spark-x2.6"] || len(free) != 2 {
		t.Fatalf("free set: %v", free)
	}
	if len(staticChatModels()) != 9 {
		t.Fatal("CPA fallback list must use the same snapshot")
	}
	if !isFreeModel("spark-x") || !isFreeModel("SPARK-X2.6") || isFreeModel("GLM-5.3-Flash") {
		t.Fatal("isFreeModel fallback")
	}
}
