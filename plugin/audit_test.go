package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreferReleaseOnlyOwnedAndOnce(t *testing.T) {
	members := []poolMember{{Key: "owned", AuthIndex: "a", Daily: 0, Known: true, Usable: true}, {Key: "unrelated", AuthIndex: "b", Daily: 0, Known: true, Usable: true}}
	released := stubPool(t, func() []poolMember { return members })
	preferMu.Lock()
	preferHeld["owned"] = true
	preferMu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); maybeReleaseHeld(members, pluginSettings{PreferDaily: true}) }()
	}
	wg.Wait()
	select {
	case got := <-released:
		if len(got) != 1 || got[0].AuthIndex != "a" {
			t.Fatalf("released unrelated: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no release")
	}
	select {
	case <-released:
		t.Fatal("repeated release")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPreferDisableOnlyOwned(t *testing.T) {
	members := []poolMember{{Key: "owned", AuthIndex: "a"}, {Key: "unrelated", AuthIndex: "b"}}
	released := stubPool(t, func() []poolMember { return members })
	preferMu.Lock()
	preferHeld["owned"] = true
	preferMu.Unlock()
	releasePreferOnDisable()
	got := <-released
	if len(got) != 1 || got[0].AuthIndex != "a" {
		t.Fatalf("released unrelated: %+v", got)
	}
	releasePreferOnDisable()
	select {
	case <-released:
		t.Fatal("repeated disable release")
	default:
	}
}

func TestAtomicPrivateWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicPrivateWrite(path, []byte(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("bad replacement: %v %s", err, raw)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Fatal("temporary file left behind")
	}
}

func TestLogRedaction(t *testing.T) {
	rememberSecrets("mock-session-keep-private", "mock-password-private")
	text := `request failed {"password":"x","token":"x","signature":"x","accessSecret":"x"} Authorization: Bearer x`
	got := redactText(text)
	for _, v := range []string{"mock-session-keep-private", "mock-password-private", "unknown-token", "signed-value", "secret-value", "another-secret"} {
		if strings.Contains(got, v) {
			t.Fatalf("secret leaked: %s", v)
		}
	}
	recordRequest(reqRecord{TS: time.Now().UnixMilli(), Error: text})
	logEvent("error", "test", text, text)
	storeMu.Lock()
	defer storeMu.Unlock()
	if strings.Contains(requests[len(requests)-1].Error, "another-secret") || strings.Contains(events[len(events)-1].Detail, "signed-value") {
		t.Fatal("persistent sink not sanitized")
	}
}

func TestGuardFailureBackoffConcurrent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(25 * time.Millisecond)
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"data":{"dailyBalance":0}}`))
	}))
	defer srv.Close()
	cfgMu.Lock()
	old := cfg
	cfg.BaseURL = srv.URL
	cfg.ProxyURL = ""
	cfgMu.Unlock()
	t.Cleanup(func() { cfgMu.Lock(); cfg = old; cfgMu.Unlock() })
	session := "mock-session-backoff-audit"
	guardMu.Lock()
	guardCache[sessionKey(session)] = &guardEntry{daily: 100, ok: true, at: time.Now().Add(-time.Hour)}
	guardMu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); guardDailyNear(session, 0) }()
	}
	wg.Wait()
	for i := 0; i < 10; i++ {
		guardDailyNear(session, 0)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("failure retry storm: %d requests", n)
	}
	guardMu.Lock()
	e := guardCache[sessionKey(session)]
	daily := e.daily
	guardMu.Unlock()
	if daily != 100 {
		t.Fatal("HTTP failure overwrote a valid balance")
	}
}

func TestGateAndResourceIsolation(t *testing.T) {
	if !strings.Contains(panelHTML, `/^\$2[aby]\$/`) {
		t.Fatal("bcrypt gate missing")
	}
	raw := handleManagement([]byte(`{"Method":"GET","Path":"/v0/resource/plugins/loomy/accounts/data"}`))
	var e envelope
	if json.Unmarshal(raw, &e) != nil {
		t.Fatal("invalid shell envelope")
	}
	if strings.Contains(string(raw), `"accounts":[`) || !strings.Contains(string(raw), "html") {
		t.Fatal("resource is not static shell only")
	}
}

func TestDailyDisableOnlyOwned(t *testing.T) {
	members := []poolMember{{Key: "daily-owned", AuthIndex: "a"}, {Key: "unrelated", AuthIndex: "b"}}
	released := stubPool(t, func() []poolMember { return members })
	guardMu.Lock()
	dailyHeld = map[string]bool{"daily-owned": true}
	guardMu.Unlock()
	releaseDailyOnDisable()
	got := <-released
	if len(got) != 1 || got[0].AuthIndex != "a" {
		t.Fatalf("released unrelated: %+v", got)
	}
	releaseDailyOnDisable()
	select {
	case <-released:
		t.Fatal("repeated release")
	default:
	}
}
func TestGuardBlocksEveryPaidModel(t *testing.T) {
	setTestSettings(t, pluginSettings{DailyOnly: true})
	const session = "mock-multimodel-account"
	setDaily(session, 0)
	for _, model := range []string{"DeepSeek-V4.1-Flash", "GLM-5.3-Flash", "new-paid-model"} {
		if code, status, _ := envCode(t, mustEnv(guardBlock(session, model))); code != "daily_exhausted" || status != 429 {
			t.Fatalf("model %s bypassed the local guard", model)
		}
	}
}

func TestPoolAuthUsability(t *testing.T) {
	now := time.Now()
	live := &loomyAuth{ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	expired := &loomyAuth{ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)}
	cases := []struct {
		name        string
		auth        *loomyAuth
		f           hostAuthEntry
		owned, want bool
	}{
		{"live", live, hostAuthEntry{}, false, true},
		{"disabled", live, hostAuthEntry{Disabled: true}, true, false},
		{"disabled-status", live, hostAuthEntry{Status: "disabled"}, true, false},
		{"expired", expired, hostAuthEntry{}, true, false},
		{"unavailable", live, hostAuthEntry{Unavailable: true}, false, false},
		{"long-cooldown", live, hostAuthEntry{NextRetryAfter: now.Add(time.Hour)}, false, false},
		{"own-hold", live, hostAuthEntry{Unavailable: true, NextRetryAfter: now.Add(time.Hour)}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := poolAuthUsable(c.auth, c.f, c.owned, now); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
