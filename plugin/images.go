package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// These generators have catalogue and user-reported upstream generation evidence.
// Model identity, tariff and session validity are still upstream-controlled.
// Registration still requires this credential's own image entry.
func isLoomyImageModel(id string) bool {
	switch strings.TrimPrefix(strings.TrimSpace(id), modelPrefix) {
	case "Hy-Image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro":
		return true
	}
	return false
}

func imageModelEntry(m gatewayModel) map[string]any {
	return map[string]any{
		"ID": modelPrefix + m.ID, "Object": "model", "OwnedBy": providerKey,
		"Type": "openai-image", "Name": m.ID, "DisplayName": m.Name,
		"SupportedInputModalities":   []string{"text"},
		"SupportedOutputModalities":  []string{"image"},
		"SupportedGenerationMethods": []string{"images.generations"},
	}
}

func isImageExecution(req execRequest) bool {
	if req.Format == "openai-image" || req.SourceFormat == "openai-image" {
		return true
	}
	if p, _ := req.Metadata["request_path"].(string); strings.HasPrefix(p, "/v1/images/") {
		return true
	}
	return isLoomyImageModel(req.Model)
}

// imagePayload is intentionally text-to-image only. Multipart edits, reference
// images, streaming and unmeasured extension fields must fail before spending.
// n=1 is the measured protocol, not a vendor-wide claimed batch limit.
func imagePayload(req execRequest, stream bool) ([]byte, error) {
	if stream {
		return nil, fmt.Errorf("Loomy image streaming is not supported; use stream=false")
	}
	if req.Format != "openai-image" && req.SourceFormat != "openai-image" {
		return nil, fmt.Errorf("image models require /v1/images/generations, not chat completions")
	}
	path, _ := req.Metadata["request_path"].(string)
	if path != "/v1/images/generations" {
		return nil, fmt.Errorf("only /v1/images/generations is supported; edits and unknown image paths are not served")
	}
	if !isLoomyImageModel(req.Model) {
		return nil, fmt.Errorf("unsupported Loomy image model")
	}
	if len(req.Payload) > 1<<20 {
		return nil, fmt.Errorf("image request exceeds 1 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("image request must be a JSON object")
	}
	allowed := map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "response_format": true, "stream": true}
	for k := range fields {
		if !allowed[k] {
			return nil, fmt.Errorf("unsupported image field %q; only text-to-image is implemented", k)
		}
	}
	var prompt string
	if json.Unmarshal(fields["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	var payloadModel string
	if raw, ok := fields["model"]; ok {
		if json.Unmarshal(raw, &payloadModel) != nil || strings.TrimPrefix(payloadModel, modelPrefix) != strings.TrimPrefix(req.Model, modelPrefix) {
			return nil, fmt.Errorf("payload model does not match selected model")
		}
	}
	if raw, ok := fields["n"]; ok {
		var n int
		if json.Unmarshal(raw, &n) != nil || n != 1 {
			return nil, fmt.Errorf("only n=1 has been implemented")
		}
	}
	if raw, ok := fields["stream"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) != nil || b {
			return nil, fmt.Errorf("image streaming is not supported")
		}
	}
	if raw, ok := fields["response_format"]; !ok {
		// CPA defaults omitted response_format to b64_json and would download the
		// generated URL. Refuse before spending rather than fail after generation.
		return nil, fmt.Errorf("specify response_format=url; base64 conversion is not supported")
	} else {
		var format string
		if json.Unmarshal(raw, &format) != nil || format != "url" {
			return nil, fmt.Errorf("only response_format=url is supported; no image URL is downloaded by the plugin")
		}
	}
	if raw, ok := fields["size"]; ok {
		var size string
		if json.Unmarshal(raw, &size) != nil || len(size) > 64 || strings.TrimSpace(size) == "" {
			return nil, fmt.Errorf("size must be a nonempty string")
		}
	}
	fields["model"], _ = json.Marshal(strings.TrimPrefix(req.Model, modelPrefix))
	fields["n"] = json.RawMessage(`1`)
	delete(fields, "stream")
	delete(fields, "response_format") // measured gateway protocol returns URLs by default
	raw, err := json.Marshal(fields)
	return escapeNonASCII(raw), err
}

// A timeout, 5xx or malformed success may follow a billed generation. Returning
// HTTP 400 prevents CPA 8.0.53's credential/round retries for request faults.
// This is an uncertain outcome, not evidence of a failed/non-billed generation.
func imageUnknown() []byte {
	return errorEnvelope("image_outcome_unknown", "image outcome unknown; it may have consumed points. Check the Loomy ledger before retrying; automatic retry is unsafe", 400)
}

func handleImageExecute(req execRequest, stream bool) []byte {
	payload, err := imagePayload(req, stream)
	if err != nil {
		return errorEnvelope("unsupported_image_request", err.Error(), 400)
	}
	auth, err := parseAuthJSON(req.StorageJSON)
	if err != nil || auth.Session == "" {
		return errorEnvelope("invalid_auth", "selected auth has no session", 401)
	}
	rec := reqRecord{TS: time.Now().UnixMilli(), Account: maskPhone(auth.Phone), AuthID: req.AuthID, Model: strings.TrimPrefix(req.Model, modelPrefix)}
	started := time.Now()
	finish := func(status int, msg string) {
		rec.Status = status
		rec.MS = time.Since(started).Milliseconds()
		rec.Error = msg
		recordRequest(rec)
	}
	// No dependable per-image quote/cap is available. Do not treat the historical
	// 110-point observation as a price or allow chat-style fail-open here.
	if settingsSnapshot().DailyOnly {
		rec.Local = true
		finish(400, "仅每日额度模式下绘图费用上限未知，已本地拦截")
		return errorEnvelope("image_daily_budget_unknown", "仅每日额度模式下暂不允许绘图：缺少可靠单次费用上限", 400)
	}
	if env, msg := guardBlock(auth.Session, rec.Model); env != nil {
		rec.Local = true
		finish(429, msg)
		return env
	}
	models, err := fetchGatewayModels(auth.Session, true)
	if err != nil {
		rec.Local = true
		status := 502
		if e, ok := err.(*catalogRequestError); ok && (e.status == 401 || e.status == 403 || e.status == 429) {
			status = e.status
		}
		finish(status, "无法确认绘图模型能力")
		return errorEnvelope("image_catalog_unavailable", "cannot verify selected account image catalogue", status)
	}
	supported := false
	for _, m := range models {
		if m.ID == rec.Model && strings.EqualFold(m.Type, "image") {
			supported = true
			break
		}
	}
	if !supported {
		rec.Local = true
		finish(404, "当前账号不支持此绘图模型")
		return errorEnvelope("model_not_found", "selected account does not support this image model", 404)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway(imagesPath), bytes.NewReader(payload))
	if err != nil {
		rec.Local = true
		finish(400, "无法创建绘图请求")
		return errorEnvelope("invalid_image_request", "cannot build image request", 400)
	}
	up.Header.Set("Content-Type", "application/json;charset=utf-8")
	up.Header.Set("Accept", "application/json")
	setAuthHeaders(up, auth.Session)
	// No prompt, output URL, reference image or raw response is persisted.
	client := *httpClient(300 * time.Second)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(up)
	markGuardStale(auth.Session)
	if err != nil {
		finish(400, "绘图结果未知，可能已扣积分；查询账单后再决定是否重试")
		return imageUnknown()
	}
	defer resp.Body.Close()
	rec.TTFB = time.Since(started).Milliseconds()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if readErr != nil || len(raw) > 32<<20 {
		finish(400, "绘图响应不完整，结果与扣费未知")
		return imageUnknown()
	}
	if resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode < 200 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		finish(400, "绘图上游服务错误，结果与扣费未知")
		return imageUnknown()
	}
	if resp.StatusCode >= 400 {
		if isRiskRejection(resp.StatusCode, "", string(raw)) {
			noteRisk(rec.Account, "绘图被上游风控拒绝")
		}
		finish(resp.StatusCode, "绘图被上游拒绝")
		// Keep auth/rate/risk semantics without echoing raw upstream secrets.
		return upstreamHTTPError(resp.StatusCode, []byte("Loomy image request rejected"))
	}
	if code, desc := appErr(raw); code != "" {
		if isRiskRejection(0, code, desc) {
			noteRisk(rec.Account, "绘图被上游风控拒绝")
		}
		status := businessStatus(code, desc)
		if status >= 500 || status == 408 || status == 429 {
			finish(400, "绘图业务结果未知，可能已扣积分")
			return imageUnknown()
		}
		finish(status, "绘图业务错误 "+code)
		return errorEnvelope("image_upstream_rejected", "Loomy image request rejected", status)
	}
	var result struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
		Points *float64 `json:"points_consumed"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Data) != 1 {
		finish(400, "绘图响应缺少图片，结果与扣费未知")
		return imageUnknown()
	}
	for _, d := range result.Data {
		if d.URL != "" {
			u, err := url.Parse(d.URL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
				finish(400, "绘图响应含无效图片地址，结果与扣费未知")
				return imageUnknown()
			}
		}
		if d.URL == "" {
			finish(400, "绘图响应缺少图片，结果与扣费未知")
			return imageUnknown()
		}
	}
	if result.Points != nil && *result.Points >= 0 && !math.IsNaN(*result.Points) && !math.IsInf(*result.Points, 0) {
		rec.PointsConsumed = result.Points
	}
	finish(200, "")
	// CPA normalizes the image response and drops unknown top-level properties.
	// Mirror only the measured charge into usage so clients can retain it too.
	if rec.PointsConsumed != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			var usage map[string]json.RawMessage
			_ = json.Unmarshal(obj["usage"], &usage)
			if usage == nil {
				usage = map[string]json.RawMessage{}
			}
			usage["points_consumed"], _ = json.Marshal(*rec.PointsConsumed)
			obj["usage"], _ = json.Marshal(usage)
			raw, _ = json.Marshal(obj)
		}
	}
	return okEnvelope(map[string]any{"Payload": raw, "Headers": map[string][]string{"content-type": {"application/json"}}})
}
