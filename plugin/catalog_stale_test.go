package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A verified catalogue must keep routing when the upstream refresh fails with a
// transport or gateway (5xx) error, while auth answers still surface.
func TestStaleCatalogueSurvivesRefreshFailure(t *testing.T) {
	var mode atomic.Int32 // 0 = ok, 1 = http 502, 2 = http 401
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			http.Error(w, "gateway down", http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"100002","desc":"login expired"}`))
		default:
			_, _ = w.Write([]byte(`[{"id":"DeepSeek-V4.1-Flash"}]`))
		}
	})

	const session = "session-stale-fallback"
	if _, err := fetchGatewayModels(session, true); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}

	cache := accountCatalogCache(session)
	cache.mu.Lock()
	cache.at = time.Now().Add(-11 * time.Minute)
	cache.mu.Unlock()

	mode.Store(1)
	models, err := fetchGatewayModels(session, false)
	if err != nil {
		t.Fatalf("a failing refresh must reuse the verified catalogue, got %v", err)
	}
	if len(models) != 1 || models[0].ID != "DeepSeek-V4.1-Flash" {
		t.Fatalf("unexpected stale catalogue: %+v", models)
	}

	mode.Store(2)
	if _, err := fetchGatewayModels(session, true); err == nil {
		t.Fatal("an auth failure must not be masked by the stale catalogue")
	}
}

// A request must still reach the model when the catalogue refresh fails but this
// account verified its catalogue before: the executor may not turn a gateway
// hiccup into a 502 for a model the account already supports.
func TestExecuteSurvivesCatalogOutageWithVerifiedCache(t *testing.T) {
	var modelsFail atomic.Bool
	var forwarded atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			forwarded.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
			return
		}
		if modelsFail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"a"}]`))
	})

	const session = "mock-outage"
	auth, _ := json.Marshal(map[string]string{"session": session})
	if _, err := fetchGatewayModels(session, true); err != nil {
		t.Fatalf("warm catalogue: %v", err)
	}
	cache := accountCatalogCache(session)
	cache.mu.Lock()
	cache.at = time.Now().Add(-11 * time.Minute)
	cache.mu.Unlock()

	modelsFail.Store(true)
	req, _ := json.Marshal(execRequest{StorageJSON: auth, Model: "lm/a"})
	var env envelope
	_ = json.Unmarshal(handleExecute(req, false), &env)
	if env.Error != nil {
		t.Fatalf("catalogue outage must reuse the verified catalogue, got code=%s", env.Error.Code)
	}
	if forwarded.Load() == 0 {
		t.Fatal("chat request was not forwarded upstream")
	}
}
