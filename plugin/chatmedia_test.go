package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return v
}

func TestNormalizeChatPayloadShapes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		imageOK bool
		want    string
	}{
		{
			name: "empty user text becomes a note",
			in:   `{"messages":[{"role":"user","content":""}]}`,
			want: `{"messages":[{"role":"user","content":"` + noteEmptyText + `"}]}`,
		},
		{
			name: "null user content becomes a note",
			in:   `{"messages":[{"role":"user","content":null}]}`,
			want: `{"messages":[{"role":"user","content":"` + noteEmptyText + `"}]}`,
		},
		{
			name: "image only turn without vision keeps a placeholder",
			in:   `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
			want: `{"messages":[{"role":"user","content":[{"type":"text","text":"` + noteImageNoVision + `"}]}]}`,
		},
		{
			name:    "empty text part next to an image is dropped",
			in:      `{"messages":[{"role":"user","content":[{"type":"text","text":""},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
			imageOK: true,
			want:    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
		},
		{
			name:    "https image url is refused by the gateway",
			in:      `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
			imageOK: true,
			want:    `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"text","text":"` + noteImageBadURL + `"}]}]}`,
		},
		{
			name:    "vision model keeps the inline image",
			in:      `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
			imageOK: true,
			want:    `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
		},
		{
			name: "text only payload is not rewritten",
			in:   `{"messages":[{"role":"user","content":"你好"}]}`,
			want: `{"messages":[{"role":"user","content":"你好"}]}`,
		},
		{
			name: "tool message without id is dropped",
			in:   `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]},{"role":"tool","content":"ok"}]}`,
			want: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}]}`,
		},
		{
			name: "tool message with id is kept",
			in:   `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"}]}`,
			want: `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"}]}`,
		},
		{
			name: "assistant empty content is legal",
			in:   `{"messages":[{"role":"assistant","content":""}]}`,
			want: `{"messages":[{"role":"assistant","content":""}]}`,
		},
		{
			name: "unknown part types are dropped",
			in:   `{"messages":[{"role":"user","content":[{"type":"text","text":"正文"},{"type":"file","file":{"id":"f1"}}]}]}`,
			want: `{"messages":[{"role":"user","content":[{"type":"text","text":"正文"}]}]}`,
		},
		{
			name: "anthropic style source part is dropped",
			in:   `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAA"}}]}]}`,
			want: `{"messages":[{"role":"user","content":[{"type":"text","text":"` + noteImageBadURL + `"}]}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeChatPayload([]byte(tc.in), tc.imageOK)
			if !reflect.DeepEqual(mustJSON(t, string(got)), mustJSON(t, tc.want)) {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestNormalizeChatPayloadPreservesOtherFields(t *testing.T) {
	in := `{"model":"qwen3.8-flash","stream":true,"tools":[{"type":"function","function":{"name":"t"}}],"messages":[{"role":"user","content":""}]}`
	got := normalizeChatPayload([]byte(in), false)
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "qwen3.8-flash" || body["stream"] != true || body["tools"] == nil {
		t.Fatalf("unrelated fields were lost: %s", got)
	}
	msgs := body["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if msg["content"] != noteEmptyText {
		t.Fatalf("content not normalised: %s", got)
	}
}

func TestNormalizeChatPayloadLeavesGarbageAlone(t *testing.T) {
	for _, in := range []string{`not json`, `{"messages":"nope"}`, `{"nope":1}`} {
		if got := normalizeChatPayload([]byte(in), false); string(got) != in {
			t.Fatalf("input %q was rewritten to %q", in, got)
		}
	}
}

func TestVisionDegradedTracking(t *testing.T) {
	visionMu.Lock()
	visionDropped = map[string]bool{}
	visionMu.Unlock()

	if visionDegraded("m1") {
		t.Fatal("fresh model must not be degraded")
	}
	observeVisionMarker([]byte(`{"choices":[{"message":{"content":"[IMAGE: base64-encoded — not readable by this text-only model; visual analysis skipped]"}}]}`), "m1")
	if !visionDegraded("m1") {
		t.Fatal("marker must mark the model degraded")
	}
	if visionDegraded("m2") {
		t.Fatal("degradation is per model")
	}
	if noteVisionDegraded("m1") {
		t.Fatal("second observation must not report first-time")
	}
	observeVisionMarker([]byte(`{"choices":[{"message":{"content":"图片里是一个报错"}}]}`), "m3")
	if visionDegraded("m3") {
		t.Fatal("clean response must not degrade")
	}
}

func TestRewriteImagesToTags(t *testing.T) {
	in := `{"model":"qwen3.8-flash","messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	got, ok := rewriteImagesToTags([]byte(in))
	if !ok {
		t.Fatal("inline image must be rewritten")
	}
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "qwen3.8-flash" {
		t.Fatalf("model lost: %s", got)
	}
	want := "看图\n<image_base64>AAAA</image_base64>"
	if body.Messages[0].Content != want {
		t.Fatalf("content=%q want %q", body.Messages[0].Content, want)
	}
}

func TestRewriteImagesToTagsIgnoresBodiesWithoutImages(t *testing.T) {
	for _, in := range []string{
		`{"messages":[{"role":"user","content":"纯文本"}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		`not json`,
	} {
		if _, ok := rewriteImagesToTags([]byte(in)); ok {
			t.Fatalf("must not rewrite %q", in)
		}
	}
}

func TestImageFormatRefusal(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"请求有误，请重试；若多次出现请更新应用版本","type":"invalid_request_error","code":400}}`, true},
		{400, `{"error":{"message":"暂不支持该图片格式，请更换图片后再试","type":"invalid_request_error"}}`, true},
		{400, `{"error":{"message":"内容太长了，请精简输入或开启新对话","type":"invalid_request_error"}}`, false},
		{400, `{"error":{"message":"前面的正文里提到了请求有误，请重试这句话","type":"invalid_request_error"}}`, false},
		{400, `{"error":{"message":"上游账号不可用"}}`, false},
		{400, `{"message":"请求有误，请重试"}`, false},
		{200, `{"error":{"message":"请求有误，请重试；若多次出现请更新应用版本","type":"invalid_request_error"}}`, false},
		{400, `请求有误，请重试`, false},
	}
	for _, tc := range cases {
		if got := imageFormatRefusal(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("status=%d body=%s → %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestPrefersImageTag(t *testing.T) {
	cfgMu.Lock()
	old := cfg.ImageTagModels
	cfg.ImageTagModels = []string{"qwen3.8-flash", "minimax-m3"}
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfg.ImageTagModels = old
		cfgMu.Unlock()
	}()

	for model, want := range map[string]bool{
		"qwen3.8-flash":       true,
		"Qwen3.8-Flash":       true,
		"group/qwen3.8-flash": true,
		"MiniMax-M3":          true,
		"qwen-3.8-max":        false,
		"GLM-5.3-Flash":       false,
		"":                    false,
	} {
		if got := prefersImageTag(model); got != want {
			t.Fatalf("prefersImageTag(%q)=%v want %v", model, got, want)
		}
	}
}

func TestInputImagePartIsRewrittenToChatShape(t *testing.T) {
	in := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	want := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	got := normalizeChatPayload([]byte(in), true)
	if !reflect.DeepEqual(mustJSON(t, string(got)), mustJSON(t, want)) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Without a vision route the same part degrades to a note instead.
	got = normalizeChatPayload([]byte(in), false)
	if !strings.Contains(string(got), noteImageNoVision) {
		t.Fatalf("visionless model must get a note: %s", got)
	}
}

func TestHasImagePart(t *testing.T) {
	withImage := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if !hasImagePart([]byte(withImage)) {
		t.Fatal("inline image must be detected")
	}
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"纯文本"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"messages":"nope"}`,
		`not json`,
	} {
		if hasImagePart([]byte(body)) {
			t.Fatalf("must not report an image for %q", body)
		}
	}
}
