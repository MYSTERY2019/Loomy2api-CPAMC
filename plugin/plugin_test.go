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
	orig := []byte(`{"type":"loomy","session":"old","phone":"test-phone-0001","note":"test fixture","priority":5,"disabled":true}`)
	a, err := parseAuthJSON(orig)
	if err != nil {
		t.Fatal(err)
	}
	stampNewSession(a, "new-session")
	out := mergeAuthJSON(orig, a)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["session"] != "new-session" || m["note"] != "test fixture" || m["priority"] != float64(5) || m["disabled"] != true || m["expires_at"] == nil {
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
		recordRequest(reqRecord{TS: now - int64(i)*1000, Account: "test-account-0001", Model: "spark-x2.6", Status: st, Error: map[bool]string{true: "boom"}[st != 200]})
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

func TestReplyText(t *testing.T) {
	if got := replyText([]byte(`{"choices":[{"message":{"content":"OK"}}]}`)); got != "OK" {
		t.Fatalf("json: %q", got)
	}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"K\"}}]}\n\ndata: [DONE]\n\n"
	if got := replyText([]byte(sse)); got != "OK" {
		t.Fatalf("sse: %q", got)
	}
}

func TestUnwrapData(t *testing.T) {
	var env map[string]any
	_ = json.Unmarshal([]byte(`{"code":0,"data":{"data":{"alreadyProcessed":true}}}`), &env)
	if unwrapData(env)["alreadyProcessed"] != true {
		t.Fatal("nested data not unwrapped")
	}
	_ = json.Unmarshal([]byte(`{"code":0,"data":{"activated":false}}`), &env)
	if v, ok := unwrapData(env)["activated"]; !ok || v != false {
		t.Fatal("single data not unwrapped")
	}
}

func TestRiskRejection(t *testing.T) {
	if !isRiskRejection(403, "", `{"error_code":"TM.00000001"}`) {
		t.Fatal("403 must be risk")
	}
	if !isRiskRejection(200, "TM.00000001", "") {
		t.Fatal("TM code must be risk")
	}
	if isRiskRejection(200, "100002", "session invalid") {
		t.Fatal("expired session is not risk")
	}
}

func TestNextDailyReset(t *testing.T) {
	at := time.Date(2026, 11, 7, 0, 3, 0, 0, cst)
	if got := nextDailyReset(at); !got.Equal(time.Date(2026, 11, 7, 0, 10, 0, 0, cst)) {
		t.Fatalf("before reset: %v", got)
	}
	at = time.Date(2026, 11, 7, 15, 0, 0, 0, cst)
	if got := nextDailyReset(at); !got.Equal(time.Date(2026, 11, 8, 0, 10, 0, 0, cst)) {
		t.Fatalf("after reset: %v", got)
	}
}

func TestBusinessDay(t *testing.T) {
	if d := businessDay(time.Date(2026, 11, 7, 0, 4, 0, 0, cst)); d != "2026-11-06" {
		t.Fatal(d)
	}
	if d := businessDay(time.Date(2026, 11, 7, 0, 6, 0, 0, cst)); d != "2026-11-07" {
		t.Fatal(d)
	}
}

func TestDeviceIDStable(t *testing.T) {
	a := deviceID(&loomyAuth{Phone: "test-phone-a"})
	b := deviceID(&loomyAuth{Phone: "test-phone-a"})
	c := deviceID(&loomyAuth{Phone: "test-phone-b"})
	if a != b || a == c || len(a) != 36 || a[14] != '4' {
		t.Fatalf("%s %s %s", a, b, c)
	}
}

func TestThinkingLevels(t *testing.T) {
	got := thinkingLevels([]string{"none", " LOW", "low", "", "xhigh"})
	if strings.Join(got, ",") != "none,low,xhigh" {
		t.Fatal(got)
	}
	e := modelEntry("m", 1, false, "", "none", "high")
	th, _ := e["Thinking"].(map[string]any)
	if th == nil || th["ZeroAllowed"] != true {
		t.Fatal(e)
	}
	if _, ok := modelEntry("m", 1, false, "")["Thinking"]; ok {
		t.Fatal("no levels -> no Thinking")
	}
}
