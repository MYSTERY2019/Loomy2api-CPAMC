package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func mockImageRequest() execRequest {
	return execRequest{Model: "lm/qwen-image-3.0-pro", Format: "openai-image", SourceFormat: "openai-image", Metadata: map[string]any{"request_path": "/v1/images/generations"}, Payload: []byte(`{"model":"lm/qwen-image-3.0-pro","prompt":"山水","n":1,"size":"1024x1024","response_format":"url"}`), StorageJSON: []byte(`{"session":"mock-image-session","phone":"test-account-0001"}`)}
}

func TestImagePayloadAndRegistration(t *testing.T) {
	req := mockImageRequest()
	raw, err := imagePayload(req, false)
	if err != nil || strings.Contains(string(raw), "山") || strings.Contains(string(raw), `lm/`) || !strings.Contains(string(raw), `\u5c71`) {
		t.Fatalf("payload %s %v", raw, err)
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data["model"] != "qwen-image-3.0-pro" || data["n"] != float64(1) {
		t.Fatal("bad payload")
	}
	for _, id := range []string{"Hy-Image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro"} {
		m := imageModelEntry(gatewayModel{ID: id, Type: "image"})
		if m["Type"] != "openai-image" || m["ID"] != "lm/"+id {
			t.Fatalf("bad registration %v", m)
		}
	}
	req.Payload = []byte(`{"prompt":"x","response_format":"url"}`)
	absent, e := imagePayload(req, false)
	if e != nil || strings.Contains(string(absent), `"size"`) {
		t.Fatalf("invented size: %s %v", absent, e)
	}
	models, _, err := parseGatewayModels([]byte(`[{"id":"qwen-image-3.0-pro","type":"image"},{"id":"unknown-image","type":"image"},{"id":"Hy-Image-3.5-preview","type":"chat"},{"id":"chat","type":"chat"}]`))
	if err != nil || len(models) != 2 || models[0].Capabilities.FunctionCalling || len(models[0].ReasoningEfforts) != 0 {
		t.Fatalf("catalog %v %v", models, err)
	}
}

func TestImageRejectsUnsupportedBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	tests := []struct {
		name   string
		alter  func(*execRequest)
		stream bool
	}{
		{"edits", func(r *execRequest) { r.Metadata["request_path"] = "/v1/images/edits" }, false},
		{"unknown-path", func(r *execRequest) { r.Metadata = nil }, false},
		{"chat", func(r *execRequest) { r.Format = "chat-completions"; r.SourceFormat = "chat-completions" }, false},
		{"stream", func(r *execRequest) {}, true},
		{"batch", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x","n":2}`) }, false},
		{"reference", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x","image":"https://invalid.test/ref"}`) }, false},
		{"unknown-field", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x","quality":"high"}`) }, false},
		{"b64", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x","response_format":"b64_json"}`) }, false},
		{"default-b64", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x"}`) }, false},
		{"empty", func(r *execRequest) { r.Payload = []byte(`{"prompt":" "}`) }, false},
		{"mismatch", func(r *execRequest) { r.Payload = []byte(`{"prompt":"x","model":"Hy-Image-3.5-preview"}`) }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mockImageRequest()
			tc.alter(&r)
			var env envelope
			_ = json.Unmarshal(handleImageExecute(r, tc.stream), &env)
			if env.OK || env.Error == nil || env.Error.HTTPStatus != 400 {
				t.Fatalf("not rejected: %+v", env)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatal("unsupported request reached network")
	}
}

func TestImageDailyOnlyBlocksBeforeNetwork(t *testing.T) {
	setTestSettings(t, pluginSettings{DailyOnly: true})
	var hits atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	var env envelope
	_ = json.Unmarshal(handleImageExecute(mockImageRequest(), false), &env)
	if env.OK || env.Error == nil || env.Error.Code != "image_daily_budget_unknown" || hits.Load() != 0 {
		t.Fatalf("daily-only not safe: %+v", env)
	}
}

func TestImageGenerationMockProtocol(t *testing.T) {
	setTestSettings(t, pluginSettings{})
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == modelsPath {
			io.WriteString(w, `[{"id":"qwen-image-3.0-pro","type":"image"}]`)
			return
		}
		if r.URL.Path != imagesPath || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("User-Agent") != "Loomy|Desktop|Electron|macOS" || r.Header.Get("token") != "mock-image-session" || strings.Contains(string(raw), "lm/") || strings.Contains(string(raw), "山") {
			t.Errorf("unsafe forwarding %s", raw)
		}
		io.WriteString(w, `{"created":123,"data":[{"url":"https://example.invalid/fake.png"}],"points_consumed":110}`)
	})
	req, _ := json.Marshal(mockImageRequest())
	var env struct {
		OK     bool
		Result struct{ Payload []byte }
	}
	raw := handleExecute(req, false)
	if json.Unmarshal(raw, &env) != nil || !env.OK || posts.Load() != 1 {
		t.Fatalf("generation: %s", raw)
	}
	var result struct {
		Data  []map[string]string
		Usage map[string]float64
	}
	if json.Unmarshal(env.Result.Payload, &result) != nil || len(result.Data) != 1 || result.Usage["points_consumed"] != 110 {
		t.Fatalf("response: %s", env.Result.Payload)
	}
}

func TestImageFailuresNeverRetryGeneration(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"server", 502, `bad gateway`, 400},
		{"malformed", 200, `invalid`, 400},
		{"empty-output", 200, `{"data":[]}`, 400},
		{"wrong-output-format", 200, `{"data":[{"b64_json":"ZmFrZQ=="}]}`, 400},
		{"unexpected-batch", 200, `{"data":[{"url":"https://example.invalid/1.png"},{"url":"https://example.invalid/2.png"}]}`, 400},
		{"unsafe-url", 200, `{"data":[{"url":"javascript:alert(1)"}]}`, 400},
		{"unknown-business", 200, `{"code":"999999","desc":"unknown"}`, 400},
		{"expired-session", 200, `{"code":"100002","desc":"expired"}`, 401},
		{"rate-limit", 429, `rejected`, 400},
		{"request-timeout", 408, `timeout`, 400},
		{"redirect", 307, ``, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTestSettings(t, pluginSettings{})
			var posts atomic.Int32
			withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == modelsPath {
					io.WriteString(w, `[{"id":"qwen-image-3.0-pro","type":"image"}]`)
					return
				}
				posts.Add(1)
				w.Header().Set("Location", "/should-not-follow")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			var env envelope
			raw := handleImageExecute(mockImageRequest(), false)
			_ = json.Unmarshal(raw, &env)
			if env.OK || env.Error == nil || env.Error.HTTPStatus != tc.want || posts.Load() != 1 {
				t.Fatalf("%s posts=%d", raw, posts.Load())
			}
		})
	}
}

func TestImageRequiresOwnLiveCapability(t *testing.T) {
	setTestSettings(t, pluginSettings{})
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		io.WriteString(w, `[{"id":"chat","type":"chat"}]`)
	})
	var env envelope
	_ = json.Unmarshal(handleImageExecute(mockImageRequest(), false), &env)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 404 || posts.Load() != 0 {
		t.Fatalf("unsupported model forwarded: %+v", env)
	}
}

func TestImageRowsAndSelftestSeparation(t *testing.T) {
	rows := toModelRows([]gatewayModel{{ID: "qwen-image-3.0-pro", Type: "image", Name: "Qwen", ContextLength: 1000}})
	if len(rows) != 1 || rows[0].Type != "image" || len(rows[0].Efforts) != 0 || rows[0].Context != 0 || rows[0].Free || rows[0].Multiple != "" {
		t.Fatalf("image mislabelled as chat: %+v", rows)
	}
	raw := handleSelfTest([]byte(`{"model":"lm/qwen-image-3.0-pro"}`))
	var env struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	_ = json.Unmarshal(raw, &env)
	if env.Result.StatusCode != 400 || !strings.Contains(string(env.Result.Body), "一键测试") {
		t.Fatalf("image selftest accepted: %s", raw)
	}
}

func TestImageConnectionLossHasUnknownOutcomeNoRetry(t *testing.T) {
	setTestSettings(t, pluginSettings{})
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == modelsPath {
			io.WriteString(w, `[{"id":"qwen-image-3.0-pro","type":"image"}]`)
			return
		}
		posts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // request was received; response/charge is now unknown
	})
	var env envelope
	_ = json.Unmarshal(handleImageExecute(mockImageRequest(), false), &env)
	if env.OK || env.Error == nil || env.Error.Code != "image_outcome_unknown" || env.Error.HTTPStatus != 400 || posts.Load() != 1 {
		t.Fatalf("unsafe connection loss handling: %+v, posts=%d", env, posts.Load())
	}
}

func TestImageMissingChargeStaysUnknown(t *testing.T) {
	setTestSettings(t, pluginSettings{})
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == modelsPath {
			io.WriteString(w, `[{"id":"qwen-image-3.0-pro","type":"image"}]`)
			return
		}
		io.WriteString(w, `{"created":123,"data":[{"url":"https://example.invalid/fake.png"}]}`)
	})
	var env struct {
		OK     bool
		Result struct{ Payload []byte }
	}
	_ = json.Unmarshal(handleImageExecute(mockImageRequest(), false), &env)
	if !env.OK || strings.Contains(string(env.Result.Payload), "points_consumed") {
		t.Fatalf("invented image charge: %s", env.Result.Payload)
	}
}
