package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// withGatewayBase points the plugin at a test server and restores it afterwards.
func withGatewayBase(t *testing.T, base string) {
	t.Helper()
	cfgMu.Lock()
	old := cfg
	cfg.BaseURL = base
	cfg.ProxyURL = ""
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = old
		cfgMu.Unlock()
	})
}

// TestCrossHostRedirectDoesNotLeakSession is the regression for the audit
// finding: the Loomy session rides in a custom "token" header, which Go copies
// to the redirect target. A cross-host redirect must therefore never be
// followed, and the target must never see the session.
func TestCrossHostRedirectDoesNotLeakSession(t *testing.T) {
	var leaked atomic.Int32
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("token") != "" || r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":"0"}`))
	}))
	defer sink.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()

	withGatewayBase(t, origin.URL)

	_, _, err := gatewayGet("/points/balance", "session-must-not-leak", 5*time.Second, 1<<16)
	if err == nil {
		t.Fatal("cross-host redirect was followed; expected an error")
	}
	if hits.Load() != 0 {
		t.Fatalf("sink host received %d request(s); session may have leaked", hits.Load())
	}
	if leaked.Load() != 0 {
		t.Fatalf("session header reached the redirected host %d time(s)", leaked.Load())
	}
}

// Same-host redirects are still allowed so ordinary path canonicalisation keeps
// working; only the host (and a https downgrade) is refused.
func TestSameHostRedirectAllowed(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, srv.URL+"/new", http.StatusFound)
			return
		}
		if r.Header.Get("token") != "same-host-session" {
			t.Errorf("token header missing after same-host redirect: %q", r.Header.Get("token"))
		}
		_, _ = w.Write([]byte(`{"code":"0","data":{"ok":true}}`))
	}))
	defer srv.Close()

	withGatewayBase(t, srv.URL)

	body, status, err := gatewayGet("/old", "same-host-session", 5*time.Second, 1<<16)
	if err != nil {
		t.Fatalf("same-host redirect failed: %v", err)
	}
	if status != http.StatusOK || len(body) == 0 {
		t.Fatalf("unexpected response: status=%d body=%q", status, body)
	}
}

func TestSafeRedirectPolicy(t *testing.T) {
	mustURL := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("bad url %q: %v", raw, err)
		}
		return u
	}
	via := func(raw string) []*http.Request {
		return []*http.Request{{URL: mustURL(raw)}}
	}

	cases := []struct {
		name    string
		target  string
		via     []*http.Request
		wantErr bool
	}{
		{"same host different path", "https://loomyad.xunfei.cn/api/v1/other", via("https://loomyad.xunfei.cn/api/v1"), false},
		{"cross host", "https://evil.example/steal", via("https://loomyad.xunfei.cn/api/v1"), true},
		{"https downgrade", "http://loomyad.xunfei.cn/api/v1", via("https://loomyad.xunfei.cn/api/v1"), true},
		{"same host different port", "https://loomyad.xunfei.cn:8443/x", via("https://loomyad.xunfei.cn/api"), true},
		{"too many hops", "https://loomyad.xunfei.cn/x", []*http.Request{
			{URL: mustURL("https://loomyad.xunfei.cn/1")},
			{URL: mustURL("https://loomyad.xunfei.cn/2")},
			{URL: mustURL("https://loomyad.xunfei.cn/3")},
			{URL: mustURL("https://loomyad.xunfei.cn/4")},
			{URL: mustURL("https://loomyad.xunfei.cn/5")},
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := safeRedirect(&http.Request{URL: mustURL(tc.target)}, tc.via)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

// The image executor copies the shared client; the copy must keep the policy.
func TestImageClientKeepsRedirectPolicy(t *testing.T) {
	c := httpClient(0)
	if c.CheckRedirect == nil {
		t.Fatal("shared client has no CheckRedirect policy")
	}
}
