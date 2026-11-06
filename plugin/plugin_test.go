package main

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStripModelPrefix(t *testing.T) {
	out := stripModelPrefix([]byte(`{"model":"lm/spark-x2.6","messages":[]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["model"] != "spark-x2.6" {
		t.Fatalf("got %v", m["model"])
	}
	raw := []byte(`{"model":"other","x":1}`)
	if string(stripModelPrefix(raw)) != string(raw) {
		t.Fatal("non-prefixed body must be untouched")
	}
}

func TestEscapeNonASCII(t *testing.T) {
	got := string(escapeNonASCII([]byte(`{"c":"你好😀"}`)))
	want := `{"c":"\u4f60\u597d\ud83d\ude00"}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(got), &v); err != nil || v["c"] != "你好😀" {
		t.Fatalf("round trip failed: %v %v", err, v)
	}
}

func TestAppErr(t *testing.T) {
	if c, _ := appErr([]byte(`{"code":"100002","desc":"session invalid"}`)); c != "100002" {
		t.Fatal("string code")
	}
	if c, _ := appErr([]byte(`{"code":100002,"desc":"x"}`)); c != "100002" {
		t.Fatal("numeric code")
	}
	if c, _ := appErr([]byte(`{"id":"chatcmpl","choices":[]}`)); c != "" {
		t.Fatal("normal completion flagged")
	}
	if c, _ := appErr([]byte(`{"code":"000000","data":{}}`)); c != "" {
		t.Fatal("success code flagged")
	}
	if businessStatus("100001", "") != 401 || businessStatus("300", "积分不足") != 429 || businessStatus("500", "x") != 502 {
		t.Fatal("status mapping")
	}
}

func TestReadSSEEvent(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("data: {\"a\":1}\r\n\r\ndata: [DONE]\n\n"))
	ev, err := readSSEEvent(r)
	if err != nil || string(ev) != "data: {\"a\":1}\n\n" {
		t.Fatalf("ev1 %q %v", ev, err)
	}
	ev, err = readSSEEvent(r)
	if err != nil || string(ev) != "data: [DONE]\n\n" {
		t.Fatalf("ev2 %q %v", ev, err)
	}
	if string(sseData([]byte("data: {\"code\":\"1\"}\n\n"))) != `{"code":"1"}` {
		t.Fatal("sseData")
	}
}

func TestUsage(t *testing.T) {
	p, c := usageFrom([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":5}}`))
	if p != 3 || c != 5 {
		t.Fatal("json usage")
	}
	p, c = usageFrom([]byte("data: {\"x\":1}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"))
	if p != 7 || c != 9 {
		t.Fatalf("sse usage %d %d", p, c)
	}
}

func TestMergeAuthKeepsUnknownFields(t *testing.T) {
	orig := []byte(`{"type":"loomy","session":"old","phone":"13800001234","note":"主力","priority":5,"disabled":true}`)
	a, err := parseAuthJSON(orig)
	if err != nil {
		t.Fatal(err)
	}
	stampNewSession(a, "new-session")
	out := mergeAuthJSON(orig, a)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["session"] != "new-session" || m["note"] != "主力" || m["priority"] != float64(5) || m["disabled"] != true || m["expires_at"] == nil {
		t.Fatalf("merged: %s", out)
	}
	if _, err := parseAuthJSON([]byte(`{"type":"codex","session":"x"}`)); err == nil {
		t.Fatal("foreign credential accepted")
	}
}

func TestParseMultiple(t *testing.T) {
	cases := map[string][2]any{"DeepSeek（x2.0）": {"2.0", false}, "GLM 限时免费": {"0", true}, "spark (x0.0)": {"0.0", true}, "plain": {"", false}}
	for in, want := range cases {
		m, f := parseMultiple(in)
		if m != want[0] || f != want[1] {
			t.Fatalf("%s -> %s %v", in, m, f)
		}
	}
}

func TestStoreQuery(t *testing.T) {
	cfgMu.Lock()
	cfg.DataDir = t.TempDir()
	cfg.RetentionDays = 30
	cfgMu.Unlock()
	storeDir = ""
	initStore()
	now := time.Now().UnixMilli()
	for i := 0; i < 25; i++ {
		st := 200
		if i%5 == 0 {
			st = 502
		}
		recordRequest(reqRecord{TS: now - int64(i)*1000, Account: "138****0001", Model: "spark-x2.6", Status: st, Error: map[bool]string{true: "boom"}[st != 200]})
	}
	p := queryRequests(reqQuery{Status: "fail", Page: 1, Size: 10}, false)
	if p.Total != 5 || len(p.Items) != 5 {
		t.Fatalf("fail filter %d", p.Total)
	}
	p = queryRequests(reqQuery{Page: 3, Size: 10}, false)
	if p.Total != 25 || len(p.Items) != 5 {
		t.Fatalf("paging %d %d", p.Total, len(p.Items))
	}
	// reload from disk
	storeDir = ""
	initStore()
	if queryRequests(reqQuery{}, true).Total != 25 {
		t.Fatal("persistence")
	}
	st := computeStats()
	if st.Today == 0 {
		t.Fatal("stats")
	}
}

func TestParseConfigEnvelope(t *testing.T) {
	yaml := "enabled: true\nbase-url: \"http://127.0.0.1:9900/api/v1/\"\nproxy-url: socks5://127.0.0.1:1080 # comment\nstream-mode: buffered\n"
	env, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml), "schema_version": 10})
	parseConfig(env)
	c := currentConfig()
	if c.BaseURL != "http://127.0.0.1:9900/api/v1" || c.ProxyURL != "socks5://127.0.0.1:1080" || c.StreamMode != "buffered" {
		t.Fatalf("%+v", c)
	}
	parseConfig([]byte(`{"config_yaml":""}`))
	if currentConfig().BaseURL != defaultBase {
		t.Fatal("default")
	}
}
