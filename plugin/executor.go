package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const maxStreamDuration = 15 * time.Minute

type execRequest struct {
	AuthID       string
	Model        string
	Stream       bool
	Payload      []byte
	StorageJSON  []byte
	StreamID     string `json:"stream_id"`
	Format       string
	SourceFormat string
	Headers      http.Header
	Metadata     map[string]any
}

func handleExecute(body []byte, stream bool) []byte {
	var req execRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errorEnvelope("bad_request", err.Error(), 400)
	}
	if isImageExecution(req) {
		return handleImageExecute(req, stream)
	}

	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil || auth.Session == "" {
		return errorEnvelope("invalid_auth", "selected auth has no session", 401)
	}

	rec := &reqRecord{
		TS:      time.Now().UnixMilli(),
		Account: maskPhone(auth.Phone),
		AuthID:  req.AuthID,
		Model:   strings.TrimPrefix(req.Model, modelPrefix),
		Stream:  stream,
		Status:  200,
	}
	started := time.Now()
	finish := func(status int, errMsg string) {
		if status == 200 {
			markGuardStale(auth.Session)
		}
		rec.Status = status
		rec.Error = truncate(errMsg, 300)
		rec.MS = time.Since(started).Milliseconds()
		recordRequest(*rec)
	}

	// An open pool-wide breaker must not cause a fresh catalogue request either.
	if pauseRemaining() > 0 {
		if env, msg := guardBlock(auth.Session, rec.Model); env != nil {
			rec.Local = true
			finish(429, msg)
			return env
		}
	}

	// CPA's model.for_auth registration is account-bound. Recheck here too, since
	// the host can retain a static or stale registration until its next refresh.
	models, catalogErr := fetchGatewayModels(auth.Session, false)
	if catalogErr != nil {
		rec.Local = true
		var upstream *catalogRequestError
		if errors.As(catalogErr, &upstream) && (upstream.status == 401 || upstream.status == 403 || upstream.status == 429) {
			finish(upstream.status, "模型目录鉴权或上游限制")
			return upstreamHTTPError(upstream.status, []byte("model catalogue authentication or upstream limit"))
		}
		finish(502, "无法确认当前账号支持的模型")
		return errorEnvelope("model_catalog_unavailable", "cannot verify selected account model catalogue", 502)
	}
	supported := false
	for _, m := range models {
		if m.ID == rec.Model && !strings.EqualFold(m.Type, "image") {
			supported = true
			break
		}
	}
	if !supported {
		rec.Local = true
		finish(404, "当前账号不支持此模型")
		return errorEnvelope("model_not_found", "selected account does not support this chat model", 404)
	}

	if env, msg := guardBlock(auth.Session, rec.Model); env != nil {
		rec.Local = true
		finish(429, msg)
		return env
	}

	payload := escapeNonASCII(applyDefaultEffort(stripModelPrefix(req.Payload)))
	ctx, cancel := context.WithTimeout(context.Background(), maxStreamDuration)
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway(chatPath), bytes.NewReader(payload))
	if err != nil {
		cancel()
		finish(500, err.Error())
		return errorEnvelope("bad_request", err.Error(), 500)
	}
	upReq.Header.Set("Content-Type", "application/json")
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	setAuthHeaders(upReq, auth.Session)

	client := httpClient(300 * time.Second)
	if stream {
		client = httpClient(0) // overall lifetime bounded by ctx instead
	}
	resp, err := client.Do(upReq)
	if err != nil {
		cancel()
		finish(502, err.Error())
		return errorEnvelope("upstream_error", err.Error(), 502)
	}

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		cancel()
		env := upstreamHTTPError(resp.StatusCode, msg)
		code, _ := appErr(msg)
		if isRiskRejection(resp.StatusCode, code, string(msg)) {
			noteRisk(rec.Account, string(msg))
		}
		finish(resp.StatusCode, string(msg))
		return env
	}

	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
	if stream && isSSE && req.StreamID != "" && currentConfig().StreamMode == "live" {
		// Peek the first event synchronously so business errors still map to a
		// proper HTTP status (and CPA cooldown) before we commit to streaming.
		reader := bufio.NewReaderSize(resp.Body, 64*1024)
		first, errFirst := readSSEEvent(reader)
		if errFirst != nil && len(first) == 0 {
			resp.Body.Close()
			cancel()
			finish(502, "empty upstream stream: "+errFirst.Error())
			return errorEnvelope("upstream_error", "empty upstream stream", 502)
		}
		if code, desc := appErr(sseData(first)); code != "" {
			if isRiskRejection(0, code, desc) {
				noteRisk(rec.Account, code+" "+desc)
			}
			resp.Body.Close()
			cancel()
			finish(businessStatus(code, desc), code+" "+desc)
			return businessError(code, desc)
		}
		rec.TTFB = time.Since(started).Milliseconds()
		goSafe("stream-pump", func() {
			defer cancel()
			defer resp.Body.Close()
			status, errMsg := pumpStream(req.StreamID, first, reader, rec)
			finish(status, errMsg)
		})
		return okEnvelope(map[string]any{"headers": map[string][]string{"content-type": {"text/event-stream"}}})
	}

	defer cancel()
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		finish(502, err.Error())
		return errorEnvelope("read_error", err.Error(), 502)
	}
	rec.TTFB = time.Since(started).Milliseconds()
	if code, desc := appErr(firstJSON(raw)); code != "" {
		if isRiskRejection(0, code, desc) {
			noteRisk(rec.Account, code+" "+desc)
		}
		finish(businessStatus(code, desc), code+" "+desc)
		return businessError(code, desc)
	}
	rec.PromptTokens, rec.CompletionTokens = usageFrom(raw)

	if !stream {
		finish(200, "")
		return okEnvelope(map[string]any{
			"Payload": raw,
			"Headers": map[string][]string{"content-type": {contentType(resp)}},
		})
	}
	// buffered fallback (stream-mode: buffered, or host without stream_id)
	chunks := make([]map[string]any, 0, 16)
	for _, part := range splitSSE(raw) {
		chunks = append(chunks, map[string]any{"Payload": part})
	}
	finish(200, "")
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"content-type": {"text/event-stream"}},
		"chunks":  chunks,
	})
}

// pumpStream forwards SSE events to the host one by one.
func pumpStream(streamID string, first []byte, reader *bufio.Reader, rec *reqRecord) (int, string) {
	emit := func(ev []byte) error {
		_, err := callHost("host.stream.emit", map[string]any{"stream_id": streamID, "payload": ev})
		return err
	}
	closeStream := func(errMsg string, status int) {
		payload := map[string]any{"stream_id": streamID}
		if errMsg != "" {
			payload["error"] = errMsg
			payload["error_detail"] = envelopeError{Code: "upstream_stream_error", Message: errMsg, HTTPStatus: status}
		}
		_, _ = callHost("host.stream.close", payload)
	}
	ev := first
	for {
		if len(bytes.TrimSpace(ev)) > 0 {
			if p, c := usageFrom(sseData(ev)); p > 0 || c > 0 {
				rec.PromptTokens, rec.CompletionTokens = p, c
			}
			if err := emit(ev); err != nil {
				return 499, "client closed: " + err.Error()
			}
		}
		next, err := readSSEEvent(reader)
		if err != nil {
			if len(bytes.TrimSpace(next)) > 0 {
				_ = emit(next)
			}
			if errors.Is(err, io.EOF) {
				closeStream("", 0)
				return 200, ""
			}
			closeStream(err.Error(), 502)
			return 502, "stream interrupted: " + err.Error()
		}
		ev = next
	}
}

// readSSEEvent reads one "\n\n"-terminated event (tolerating \r\n) and returns
// it normalised with a trailing blank line.
func readSSEEvent(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(trimmed) == 0 {
				if buf.Len() > 0 {
					buf.WriteByte('\n')
					return buf.Bytes(), nil
				}
			} else {
				buf.Write(trimmed)
				buf.WriteByte('\n')
			}
		}
		if err != nil {
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			return buf.Bytes(), err
		}
	}
}

// sseData extracts the joined data: payload of one event (or the raw bytes if
// the "event" is actually a bare JSON body).
func sseData(ev []byte) []byte {
	t := bytes.TrimSpace(ev)
	if len(t) > 0 && t[0] == '{' {
		return t
	}
	var out [][]byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, bytes.TrimSpace(line[5:]))
		}
	}
	return bytes.Join(out, []byte("\n"))
}

func firstJSON(raw []byte) []byte {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' {
		return t
	}
	parts := splitSSE(raw)
	if len(parts) > 0 {
		return sseData(parts[0])
	}
	return nil
}

func usageFrom(raw []byte) (int64, int64) {
	if !bytes.Contains(raw, []byte(`"usage"`)) {
		return 0, 0
	}
	var p struct {
		Usage *struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '{' {
		if json.Unmarshal(t, &p) == nil && p.Usage != nil {
			return p.Usage.Prompt, p.Usage.Completion
		}
		return 0, 0
	}
	// buffered SSE: last event carrying usage wins
	var pr, co int64
	for _, part := range splitSSE(raw) {
		if a, b := usageFrom(sseData(part)); a > 0 || b > 0 {
			pr, co = a, b
		}
	}
	return pr, co
}

func upstreamHTTPError(status int, msg []byte) []byte {
	text := strings.TrimSpace(string(msg))
	if code, desc := appErr(msg); code != "" {
		text = code + " " + desc
	}
	switch {
	case status == http.StatusForbidden:
		// risk control, not an expired session: let CPA cool the account down
		// instead of triggering a re-login (which would add more traffic)
		return errorEnvelope("risk_blocked", text, status)
	case status == http.StatusUnauthorized:
		return errorEnvelope("auth_failed", text, status)
	case status == http.StatusTooManyRequests:
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "rate_limited", Message: text, HTTPStatus: 429, RetryAfterSeconds: 60}})
		return raw
	default:
		return errorEnvelope("upstream_error", text, status)
	}
}

// businessStatus maps gateway business codes to the HTTP status CPA acts on:
// 401 → credential refresh/cooldown, 429 → rotate to the next account.
func businessStatus(code, desc string) int {
	if isAuthCode(code) {
		return http.StatusUnauthorized
	}
	if strings.Contains(desc, "积分") || strings.Contains(desc, "余额") || strings.Contains(desc, "额度") ||
		strings.Contains(desc, "频繁") || strings.Contains(strings.ToLower(desc), "limit") {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func businessError(code, desc string) []byte {
	status := businessStatus(code, desc)
	e := &envelopeError{Code: "upstream_" + code, Message: fmt.Sprintf("loomy %s: %s", code, desc), HTTPStatus: status}
	if status == http.StatusTooManyRequests {
		e.RetryAfterSeconds = 600
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: e})
	return raw
}

// applyDefaultEffort injects reasoning_effort when the client did not set one.
// Configured with plugins.configs.loomy.default-reasoning-effort; empty means
// the gateway decides. A client that already sent reasoning_effort or a
// thinking block always wins.
func applyDefaultEffort(raw []byte) []byte {
	effort := strings.ToLower(strings.TrimSpace(currentConfig().DefaultEffort))
	if effort == "" {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	if _, ok := fields["reasoning_effort"]; ok {
		return raw
	}
	if _, ok := fields["thinking"]; ok {
		return raw
	}
	encoded, err := json.Marshal(effort)
	if err != nil {
		return raw
	}
	fields["reasoning_effort"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// stripModelPrefix removes the "lm/" routing prefix from the payload model.
// Only "model" is touched.
func stripModelPrefix(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	modelRaw, ok := fields["model"]
	if !ok {
		return raw
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil || !strings.HasPrefix(model, modelPrefix) {
		return raw
	}
	bare, _ := json.Marshal(strings.TrimPrefix(model, modelPrefix))
	fields["model"] = bare
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// escapeNonASCII rewrites raw UTF-8 into \uXXXX escapes. The Loomy gateway
// answers 400 "请求参数错误" for raw multibyte characters in the JSON body.
// Non-ASCII bytes only legally appear inside JSON strings, so this is safe.
func escapeNonASCII(in []byte) []byte {
	ascii := true
	for _, b := range in {
		if b >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return in
	}
	var out bytes.Buffer
	out.Grow(len(in) * 2)
	for i := 0; i < len(in); {
		if in[i] < 0x80 {
			out.WriteByte(in[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(in[i:])
		i += size
		if r == utf8.RuneError && size == 1 {
			out.WriteString(`\ufffd`)
			continue
		}
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", hi, lo)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}

func splitSSE(raw []byte) [][]byte {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	parts := bytes.Split(raw, []byte("\n\n"))
	out := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(bytes.TrimSpace(part)) == 0 {
			continue
		}
		out = append(out, append(append([]byte{}, part...), '\n', '\n'))
	}
	if len(out) == 0 && len(bytes.TrimSpace(raw)) > 0 {
		out = append(out, raw)
	}
	return out
}

func contentType(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json"
}
