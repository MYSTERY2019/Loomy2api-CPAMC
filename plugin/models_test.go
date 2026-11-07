package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func withCatalogServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cfgMu.Lock()
	old := cfg
	cfg.BaseURL = srv.URL
	cfg.ProxyURL = ""
	cfgMu.Unlock()
	modelCacheMu.Lock()
	oldCaches := modelCaches
	modelCaches = map[string]*catalogCache{}
	modelCacheMu.Unlock()
	t.Cleanup(func() {
		srv.Close()
		cfgMu.Lock()
		cfg = old
		cfgMu.Unlock()
		modelCacheMu.Lock()
		modelCaches = oldCaches
		modelCacheMu.Unlock()
	})
}

func TestCatalogSessionIsolationAndCoalescing(t *testing.T) {
	var hits atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("token") == "mock-b" {
			_, _ = w.Write([]byte(`[{"id":"shared"},{"id":"only-b"}]`))
		} else {
			_, _ = w.Write([]byte(`[{"id":"shared"}]`))
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := "mock-a"
			want := 1
			if i%2 == 1 {
				session = "mock-b"
				want = 2
			}
			models, err := fetchGatewayModels(session, false)
			if err != nil || len(models) != want {
				t.Errorf("session catalogue: %d %v", len(models), err)
			}
		}(i)
	}
	wg.Wait()
	if hits.Load() != 2 {
		t.Fatalf("got %d calls, want one per session", hits.Load())
	}
	if len(cachedModelUnion()) != 2 {
		t.Fatal("union lost account-only model")
	}
	_, _ = fetchGatewayModels("mock-b", true)
	_, _ = fetchGatewayModels("mock-a", false)
	if hits.Load() != 3 {
		t.Fatal("force refreshed another account")
	}
}

func TestModelsForAuthDoesNotBorrowAnotherAccount(t *testing.T) {
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		id := "only-a"
		if r.Header.Get("token") == "mock-b" {
			id = "only-b"
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": id}})
	})
	for _, id := range []string{"a", "b", "a"} {
		auth, _ := json.Marshal(map[string]string{"session": "mock-" + id})
		req, _ := json.Marshal(map[string]any{"StorageJSON": auth})
		var env envelope
		_ = json.Unmarshal(handleModelsForAuth(req), &env)
		var result struct{ Models []struct{ ID string } }
		_ = json.Unmarshal(env.Result, &result)
		if len(result.Models) != 1 || result.Models[0].ID != "lm/only-"+id {
			t.Fatalf("wrong account routing models: %s", env.Result)
		}
	}
}

func TestExecuteRejectsUnsupportedAccountBeforeChat(t *testing.T) {
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`[{"id":"supported"}]`))
	})
	auth, _ := json.Marshal(map[string]string{"session": "mock-execute"})
	req, _ := json.Marshal(execRequest{StorageJSON: auth, Model: "lm/not-supported", Payload: []byte(`{"model":"lm/not-supported"}`)})
	var env envelope
	_ = json.Unmarshal(handleExecute(req, false), &env)
	if env.Error == nil || env.Error.Code != "model_not_found" || posts.Load() != 0 {
		t.Fatalf("unsupported request forwarded: %s posts=%d", handleExecute(req, false), posts.Load())
	}
}

func TestDiagnosticCatalogUnion(t *testing.T) {
	// Only model IDs and metadata from A276105 are retained; no auth filenames,
	// phone numbers, credentials or session lengths are included in the fixture.
	raw, err := os.ReadFile("testdata/model-catalogs.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalogs [][]map[string]any
	if err = json.Unmarshal(raw, &catalogs); err != nil {
		t.Fatal(err)
	}
	var parsed [][]gatewayModel
	for i, list := range catalogs {
		b, _ := json.Marshal(list)
		models, _, e := parseGatewayModels(b)
		if e != nil {
			t.Fatal(e)
		}
		want := 9
		if i == 0 {
			want = 8
		}
		if len(models) != want {
			t.Fatalf("account %d: %d", i, len(models))
		}
		parsed = append(parsed, models)
	}
	union := mergeGatewayModels(parsed...)
	if len(union) != 9 {
		t.Fatalf("union %d, want 9", len(union))
	}
	for _, m := range union {
		if m.ID == "auto" {
			t.Fatal("auto model leaked")
		}
		if isLoomyImageModel(m.ID) && m.Type != "image" {
			t.Fatal("image model advertised as chat")
		}
	}
	if len(mergeGatewayModels(parsed[0], parsed[0])) != 8 {
		t.Fatal("duplicate IDs")
	}
}

func TestPoolCatalogMergePartialAndDisabled(t *testing.T) {
	var hits atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("token") == "bad" {
			w.WriteHeader(502)
			return
		}
		id := "a"
		if r.Header.Get("token") == "b" {
			id = "b"
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": id}})
	})
	files := []hostAuthEntry{{AuthIndex: "a"}, {AuthIndex: "b"}, {AuthIndex: "bad"}, {AuthIndex: "disabled", Disabled: true}}
	models, loaded, failed, err := collectModelCatalog(files, false, func(index string) (*loomyAuth, error) { return &loomyAuth{Session: index}, nil })
	if len(models) != 2 || loaded != 2 || failed != 1 || err == nil || hits.Load() != 3 {
		t.Fatalf("pool union: %v %d %d %v hits=%d", models, loaded, failed, err, hits.Load())
	}
}

func TestCatalogFailureBackoffAndExecuteFailClosed(t *testing.T) {
	var gets, posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		} else {
			gets.Add(1)
		}
		w.WriteHeader(502)
	})
	auth, _ := json.Marshal(map[string]string{"session": "mock-failure"})
	req, _ := json.Marshal(execRequest{StorageJSON: auth, Model: "lm/a"})
	for i := 0; i < 5; i++ {
		var env envelope
		_ = json.Unmarshal(handleExecute(req, false), &env)
		if env.Error == nil || env.Error.Code != "model_catalog_unavailable" {
			t.Fatalf("bad failure: %s", handleExecute(req, false))
		}
	}
	if gets.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("catalog retry storm/forwarded chat: gets=%d posts=%d", gets.Load(), posts.Load())
	}
	_, _ = fetchGatewayModels("mock-failure", true)
	if gets.Load() != 2 {
		t.Fatal("force did not bypass backoff")
	}
}

func TestCatalogRefreshReplacesRemovedModelsAndPricesStayAccountBound(t *testing.T) {
	var changed atomic.Bool
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		name := "shared(x2.0)"
		if r.Header.Get("token") == "free" {
			name = "shared(限时免费)"
		}
		rows := []map[string]string{{"id": "shared", "name": name}}
		if !changed.Load() {
			rows = append(rows, map[string]string{"id": "retired"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	_, _ = fetchGatewayModels("free", false)
	_, _ = fetchGatewayModels("paid", false)
	if !isFreeModelForSession("free", "shared") || isFreeModelForSession("paid", "shared") {
		t.Fatal("price flag crossed account boundary")
	}
	changed.Store(true)
	models, err := fetchGatewayModels("paid", true)
	if err != nil || len(models) != 1 {
		t.Fatal("refresh accumulated old entries")
	}
}

func TestCatalogPreflightPreservesSessionExpiry401(t *testing.T) {
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`{"code":100002,"message":"session expired"}`))
	})
	auth, _ := json.Marshal(map[string]string{"session": "mock-expired"})
	req, _ := json.Marshal(execRequest{StorageJSON: auth, Model: "lm/a"})
	var env envelope
	_ = json.Unmarshal(handleExecute(req, false), &env)
	if env.Error == nil || env.Error.HTTPStatus != 401 || posts.Load() != 0 {
		t.Fatalf("session expiry lost in catalogue preflight: %+v", env.Error)
	}
}
