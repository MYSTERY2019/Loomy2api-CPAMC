package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// One-click self test: for each loomy account, send one tiny real chat request
// straight to the gateway (bypassing CPA routing) and report status, latency
// and the reply. Results are not written to the request log.

const testPrompt = "这是连通性测试，请只回复：OK"

type testResult struct {
	AuthIndex string `json:"auth_index"`
	File      string `json:"file"`
	Phone     string `json:"phone"`
	Model     string `json:"model"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	MS        int64  `json:"ms"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

func handleSelfTest(body []byte) []byte {
	var in struct {
		AuthIndex       string `json:"auth_index"`
		Model           string `json:"model"`
		IncludeDisabled bool   `json:"include_disabled"`
	}
	_ = json.Unmarshal(body, &in)
	model := strings.TrimPrefix(strings.TrimSpace(in.Model), modelPrefix)
	if model == "" {
		model = "spark-x2.6"
	}
	if isLoomyImageModel(model) {
		return mgmtJSON(400, map[string]any{"error": "一键测试仅支持聊天模型；绘图需要独立请求并可能消耗积分"})
	}
	files, err := loomyFiles()
	if err != nil {
		return mgmtJSON(502, map[string]any{"error": err.Error()})
	}
	targets := make([]hostAuthEntry, 0, len(files))
	for _, f := range files {
		if in.AuthIndex != "" && f.AuthIndex != in.AuthIndex {
			continue
		}
		targets = append(targets, f)
	}
	if in.AuthIndex != "" && len(targets) == 0 {
		return mgmtJSON(404, map[string]any{"error": "account not found"})
	}

	results := make([]testResult, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, f := range targets {
		results[i] = testResult{AuthIndex: f.AuthIndex, File: f.Name, Model: model}
		if f.Disabled && !in.IncludeDisabled && in.AuthIndex == "" {
			results[i].Skipped = true
			results[i].Error = "已停用，跳过"
			continue
		}
		wg.Add(1)
		i, f := i, f
		goSafe("selftest", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = testAccount(f, model)
		})
	}
	wg.Wait()

	pass, fail, skip := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Skipped:
			skip++
		case r.OK:
			pass++
		default:
			fail++
		}
	}
	level := "info"
	if fail > 0 {
		level = "warn"
	}
	logEvent(level, "test", "一键测试："+itoa(pass)+" 通过，"+itoa(fail)+" 失败"+map[bool]string{true: "，" + itoa(skip) + " 跳过", false: ""}[skip > 0], "model "+model)
	return mgmtJSON(200, map[string]any{"model": modelPrefix + model, "pass": pass, "fail": fail, "skipped": skip, "items": results})
}

func testAccount(f hostAuthEntry, model string) (res testResult) {
	res = testResult{AuthIndex: f.AuthIndex, File: f.Name, Model: model}
	if isLoomyImageModel(model) {
		res.Status, res.Error = 400, "一键测试仅支持聊天模型；绘图需要独立请求并可能消耗积分"
		return
	}
	auth, _, _, err := getLoomyAuth(f.AuthIndex)
	if err != nil {
		res.Error = err.Error()
		return
	}
	res.Phone = maskPhone(auth.Phone)
	if auth.Session == "" {
		res.Status, res.Error = 401, "没有 session"
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 16,
		"messages":   []map[string]string{{"role": "user", "content": testPrompt}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway(chatPath), bytes.NewReader(escapeNonASCII(payload)))
	if err != nil {
		res.Error = err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setAuthHeaders(req, auth.Session)
	started := time.Now()
	resp, err := httpClient(60 * time.Second).Do(req)
	if err != nil {
		res.MS, res.Status, res.Error = time.Since(started).Milliseconds(), 502, err.Error()
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.MS = time.Since(started).Milliseconds()
	res.Status = resp.StatusCode
	if resp.StatusCode >= 400 {
		res.Error = truncate(strings.TrimSpace(string(raw)), 200)
		return
	}
	if code, desc := appErr(firstJSON(raw)); code != "" {
		res.Status = businessStatus(code, desc)
		res.Error = code + " " + desc
		return
	}
	res.Reply = truncate(strings.TrimSpace(replyText(raw)), 120)
	res.OK = true
	if res.Reply == "" {
		res.Reply = "(空回复)"
	}
	return
}

// replyText extracts assistant text from a JSON completion or an SSE body.
func replyText(raw []byte) string {
	var one struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	trim := bytes.TrimSpace(raw)
	if len(trim) > 0 && trim[0] == '{' && json.Unmarshal(trim, &one) == nil && len(one.Choices) > 0 {
		return one.Choices[0].Message.Content + one.Choices[0].Delta.Content
	}
	var sb strings.Builder
	for _, ev := range splitSSE(raw) {
		data := sseData(ev)
		if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			continue
		}
		one.Choices = nil
		if json.Unmarshal(data, &one) == nil && len(one.Choices) > 0 {
			sb.WriteString(one.Choices[0].Delta.Content)
			sb.WriteString(one.Choices[0].Message.Content)
		}
	}
	return sb.String()
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
