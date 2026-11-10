package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBase = "https://loomyad.xunfei.cn/api/v1"

type pluginConfig struct {
	BaseURL       string
	AccountURL    string
	ProxyURL      string
	DataDir       string
	StreamMode    string // live | buffered
	RetentionDays int
	DefaultEffort string // none|low|medium|high|xhigh; empty = leave client's choice
}

var (
	cfgMu sync.RWMutex
	cfg   = pluginConfig{BaseURL: defaultBase, AccountURL: defaultAccountBase, StreamMode: "live", RetentionDays: 30}

	clientMu    sync.Mutex
	clientCache = map[time.Duration]*http.Client{}
)

func currentConfig() pluginConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func gateway(path string) string { return currentConfig().BaseURL + path }

// parseConfig reads the flat "key: value" lines CPA hands to plugin.register.
// The host may also send JSON; both are accepted.
func parseConfig(raw []byte) {
	next := pluginConfig{BaseURL: defaultBase, StreamMode: "live", RetentionDays: 30}
	set := func(key, val string) {
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.ReplaceAll(strings.TrimSpace(key), "_", "-") {
		case "base-url":
			if val != "" {
				next.BaseURL = strings.TrimRight(val, "/")
			}
		case "account-base-url":
			if val != "" {
				next.AccountURL = strings.TrimRight(val, "/")
			}
		case "proxy-url":
			next.ProxyURL = val
		case "data-dir":
			next.DataDir = val
		case "stream-mode":
			if val == "buffered" || val == "live" {
				next.StreamMode = val
			}
		case "log-retention-days":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 365 {
				next.RetentionDays = n
			}
		case "default-reasoning-effort", "default-effort":
			switch strings.ToLower(val) {
			case "none", "low", "medium", "high", "xhigh":
				next.DefaultEffort = strings.ToLower(val)
			case "", "off", "client":
				next.DefaultEffort = ""
			}
		}
	}
	// CPA sends {"config_yaml": <base64 YAML of plugins.configs.<id>>, "schema_version": N}.
	// (v1 line-parsed this JSON envelope directly, so its config was never applied.)
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "{") {
		var env struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if jsonUnmarshal([]byte(t), &env) == nil {
			raw = env.ConfigYAML
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if i := strings.Index(trimmed, " #"); i > 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		kv := strings.SplitN(trimmed, ":", 2)
		if len(kv) == 2 {
			set(kv[0], kv[1])
		}
	}
	if next.DataDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			next.DataDir = filepath.Join(home, ".loomy-plugin")
		} else {
			next.DataDir = filepath.Join(os.TempDir(), "loomy-plugin")
		}
	}
	if next.AccountURL == "" {
		next.AccountURL = defaultAccountBase
	}
	if _, err := url.ParseRequestURI(next.BaseURL); err != nil {
		next.BaseURL = defaultBase
	}
	cfgMu.Lock()
	changedProxy := cfg.ProxyURL != next.ProxyURL
	cfg = next
	cfgMu.Unlock()
	if changedProxy {
		clientMu.Lock()
		for _, c := range clientCache {
			c.CloseIdleConnections()
		}
		clientCache = map[time.Duration]*http.Client{}
		clientMu.Unlock()
	}
}

// hostGuard refuses any outbound request whose host is not the configured
// gateway or account host. The redirect policy already stops a cross-host hop,
// but this covers the other ways a credential-bearing request could end up
// somewhere unexpected: a mistyped base-url, a future code path, a redirect
// that slipped through a proxy.
type hostGuard struct{ base http.RoundTripper }

func (h hostGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("host guard: nil request")
	}
	if !allowedHost(req.URL.Host) {
		return nil, fmt.Errorf("host guard: refusing request to %q, which is not the configured gateway or account host", req.URL.Host)
	}
	return h.base.RoundTrip(req)
}

func allowedHost(host string) bool {
	cfg := currentConfig()
	for _, base := range []string{cfg.BaseURL, cfg.AccountURL, defaultBase, defaultAccountBase} {
		if base == "" {
			continue
		}
		if u, err := url.Parse(base); err == nil && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return false
}

// safeRedirect keeps credential-bearing requests on the host they were meant
// for. Go copies non-standard headers (our "token" header carries the Loomy
// session) to the redirect target; only Authorization, WWW-Authenticate,
// Cookie and Cookie2 are stripped on a cross-domain hop. Refusing the hop
// outright is the only reliable guard, because the session is not in
// Authorization for this upstream.
func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	prev := via[len(via)-1].URL
	if !strings.EqualFold(req.URL.Host, prev.Host) {
		return fmt.Errorf("refusing cross-host redirect from %s to %s", prev.Host, req.URL.Host)
	}
	if prev.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing https to %s redirect", req.URL.Scheme)
	}
	return nil
}

// httpClient returns a shared client per timeout so connections are reused
// (the v1 code built a fresh Transport per call and leaked idle sockets).
func httpClient(timeout time.Duration) *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c, ok := clientCache[timeout]; ok {
		return c
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if p := currentConfig().ProxyURL; p != "" {
		if parsed, err := url.Parse(p); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	c := &http.Client{Timeout: timeout, Transport: hostGuard{base: transport}, CheckRedirect: safeRedirect}
	clientCache[timeout] = c
	return c
}
