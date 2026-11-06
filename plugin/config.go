package main

import (
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
	c := &http.Client{Timeout: timeout, Transport: transport}
	clientCache[timeout] = c
	return c
}
