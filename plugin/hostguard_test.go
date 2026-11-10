package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The guard is belt-and-braces on top of the redirect policy: no code path may
// send a credential-bearing request to a host that is not the configured
// gateway or account service.
func TestHostGuardRefusesUnknownHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://evil.example/steal", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("token", "session-must-not-leave")

	_, err = httpClient(time.Second).Do(req)
	if err == nil {
		t.Fatal("request to an unconfigured host was allowed")
	}
	if !strings.Contains(err.Error(), "host guard") {
		t.Fatalf("expected the host guard to refuse, got: %v", err)
	}
}

func TestHostGuardAllowsConfiguredHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != "ok-session" {
			t.Errorf("token header missing on the allowed host")
		}
		_, _ = w.Write([]byte(`{"code":"0"}`))
	}))
	defer srv.Close()

	withGatewayBase(t, srv.URL)

	body, status, err := gatewayGet("/models", "ok-session", 5*time.Second, 1<<16)
	if err != nil {
		t.Fatalf("allowed host was refused: %v", err)
	}
	if status != http.StatusOK || len(body) == 0 {
		t.Fatalf("unexpected response: status=%d body=%q", status, body)
	}
}

func TestAllowedHostMatching(t *testing.T) {
	withGatewayBase(t, "https://loomyad.xunfei.cn/api/v1")

	cases := map[string]bool{
		"loomyad.xunfei.cn":      true,
		"LOOMYAD.XUNFEI.CN":      true,
		"account.xfinfr.com":     true,
		"evil.example":           false,
		"loomyad.xunfei.cn:8443": false,
	}
	for host, want := range cases {
		if got := allowedHost(host); got != want {
			t.Fatalf("allowedHost(%q) = %v, want %v", host, got, want)
		}
	}
}
