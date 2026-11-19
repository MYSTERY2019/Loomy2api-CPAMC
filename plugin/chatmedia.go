package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// The Loomy gateway is stricter than an OpenAI client expects in two ways, both of
// which surface to the caller as an opaque "400 请求有误，请重试；若多次出现请更新应用版本":
//
//   - a user message whose text is empty (an image-only turn, "" or []),
//   - a tool message without tool_call_id,
//   - an image part whose URL is not an inline data URL (https URLs are refused).
//
// It also accepts image parts on models that have no vision and silently swaps them
// for a "[IMAGE: base64-encoded — not readable by this text-only model ...]" marker,
// which makes the model invent an answer about a picture it never saw. The catalogue
// keeps advertising vision for such models, so the plugin cannot trust it.
//
// normalizeChatPayload rewrites only those shapes, and only the ones the gateway
// refuses or cannot use; everything else is passed through byte-for-byte.

const (
	noteEmptyText     = "[空消息]"
	noteImageDropped  = "[图片]"
	noteImageNoVision = "[图片：当前模型不支持读图，内容已省略]"
	noteImageBadURL   = "[图片链接：仅接受内联 data URL，已省略]"

	// visionDegradedMarker is what the gateway substitutes for an image when the
	// selected account model has no vision. Seeing it is the only reliable runtime
	// signal that the catalogue's vision claim is wrong.
	visionDegradedMarker = "not readable by this text-only model"
)

var (
	visionMu      sync.Mutex
	visionDropped = map[string]bool{} // model id -> upstream dropped an image at least once
)

func visionDegraded(model string) bool {
	visionMu.Lock()
	defer visionMu.Unlock()
	return visionDropped[model]
}

// noteVisionDegraded records that the upstream refused to read an image for this
// model. Returns true only on the first observation so the event log stays quiet.
func noteVisionDegraded(model string) bool {
	if model == "" {
		return false
	}
	visionMu.Lock()
	first := !visionDropped[model]
	visionDropped[model] = true
	visionMu.Unlock()
	if first {
		logEvent("warn", "vision", model+" 上游实际不支持读图，后续请求将省略图片", "")
	}
	return first
}

// observeVisionMarker inspects an upstream body (JSON or SSE chunk) for the
// degradation marker.
func observeVisionMarker(body []byte, model string) {
	if bytes.Contains(body, []byte(visionDegradedMarker)) {
		noteVisionDegraded(model)
	}
}

// normalizeChatPayload returns the body the gateway can actually serve. imageOK
// reports whether the selected model is believed to read inline images; when it is
// false, image parts become an explicit text note instead of a wasted upload.
func normalizeChatPayload(raw []byte, imageOK bool) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return raw
	}
	rawMsgs, ok := body["messages"]
	if !ok {
		return raw
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return raw
	}

	changed := false
	out := make([]map[string]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		role := stringField(m["role"])
		switch role {
		case "tool":
			if strings.TrimSpace(stringField(m["tool_call_id"])) == "" {
				changed = true // unusable upstream, and the result has no owner to attach to
				continue
			}
		case "user":
			content, did := normalizeContent(m["content"], imageOK)
			if did {
				m["content"] = content
				changed = true
			}
		}
		out = append(out, m)
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	body["messages"] = encoded
	merged, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return merged
}

func normalizeContent(raw json.RawMessage, imageOK bool) (json.RawMessage, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return rawText(noteEmptyText), true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) != "" {
			return raw, false
		}
		return rawText(noteEmptyText), true
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, false // not a shape this function understands
	}
	kept := make([]map[string]json.RawMessage, 0, len(parts))
	changed, sawImage := false, false
	for _, p := range parts {
		switch partType(p) {
		case "text", "input_text":
			if strings.TrimSpace(stringField(p["text"])) == "" {
				changed = true
				continue
			}
			kept = append(kept, p)
		case "image_url", "input_image", "image":
			sawImage = true
			url := imageURLOf(p)
			if imageOK && strings.HasPrefix(url, "data:") {
				// Only the classic chat shape is accepted upstream: an `input_image`
				// part (the Responses-API spelling some clients emit) is answered with
				// the opaque 400 envelope, measured against iflytek-spark-loomy-2.
				if partType(p) == "image_url" {
					kept = append(kept, p)
				} else {
					kept = append(kept, imagePart(url))
					changed = true
				}
				continue
			}
			changed = true
			if strings.HasPrefix(url, "data:") {
				kept = append(kept, textPart(noteImageNoVision))
			} else {
				kept = append(kept, textPart(noteImageBadURL))
			}
		default:
			changed = true // the gateway refuses part types it does not know
		}
	}
	if len(kept) == 0 {
		if sawImage {
			return rawText(noteImageDropped), true
		}
		return rawText(noteEmptyText), true
	}
	if !changed {
		return raw, false
	}
	return marshalParts(kept), true
}

func partType(part map[string]json.RawMessage) string {
	return strings.ToLower(strings.TrimSpace(stringField(part["type"])))
}

// imageURLOf reads the URL out of the part shapes seen in the wild:
// {"image_url":{"url":"..."}}, {"image_url":"..."}, {"url":"..."}.
func imageURLOf(part map[string]json.RawMessage) string {
	if raw, ok := part["image_url"]; ok {
		var nested struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &nested) == nil && nested.URL != "" {
			return nested.URL
		}
		if s := stringField(raw); s != "" {
			return s
		}
	}
	return stringField(part["url"])
}

func stringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func rawText(text string) json.RawMessage {
	encoded, _ := json.Marshal(text)
	return encoded
}

func textPart(text string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"type": json.RawMessage(`"text"`),
		"text": rawText(text),
	}
}

// imagePart builds the one image shape the gateway accepts.
func imagePart(url string) map[string]json.RawMessage {
	nested, _ := json.Marshal(map[string]string{"url": url})
	return map[string]json.RawMessage{
		"type":      json.RawMessage(`"image_url"`),
		"image_url": nested,
	}
}

func marshalParts(parts []map[string]json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(parts)
	if err != nil {
		return nil
	}
	return encoded
}

// ---- <image_base64> tag form -------------------------------------------------

// Measured by the magpie-loomy-auth gateway (2026-11-08, eight models × two
// shapes): an inline `image_url` data URI is answered with the opaque 400
// envelope as soon as the request lands on a first-party backend
// (iflytek-spark-loomy / alibaba-dashscope), while the same bytes as an
// `<image_base64>` text tag return 200 on every model. Which backend serves a
// model name drifts over time, so the tag form is a fallback rather than a rule:
// the plugin keeps the native shape and only converts when the upstream rejects
// the request *for the image shape*, because that retry is a second billed request.

const (
	imageTagOpen  = "<image_base64>"
	imageTagClose = "</image_base64>"

	refusalRequestBad  = "请求有误，请重试"
	refusalImageFormat = "暂不支持该图片格式"

	// purposeHeader is the gateway's business-purpose declaration. Measured against
	// loomyad: an image sent under the default purpose lands on a text-only route
	// (prompt_tokens 63, the image replaced by a placeholder note), while the same
	// request marked kb.vision is routed to the vision route (prompt_tokens 168,
	// the model reads the image). This is the whole difference between "cannot see
	// images" and "sees them".
	purposeHeader = "X-Loomy-Request-Purpose"
	purposeVision = "kb.vision"
)

// hasImagePart reports whether a chat body carries an inline image part, which is
// what decides the purpose header below.
func hasImagePart(raw []byte) bool {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(body["messages"], &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if stringField(m["role"]) != "user" {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(m["content"], &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch partType(p) {
			case "image_url", "input_image", "image":
				if _, ok := inlineBase64(imageURLOf(p)); ok {
					return true
				}
			}
		}
	}
	return false
}

// imageFormatRefusal reports whether a 400 was produced by the image shape.
// Anchored on the error envelope's message, never on the body text: a user who
// pastes this very message into the chat would otherwise make any unrelated 400
// trigger a second, billed request.
func imageFormatRefusal(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(firstJSON(body), &env) != nil || env.Error == nil || env.Error.Type == "" {
		return false
	}
	msg := strings.TrimSpace(env.Error.Message)
	return strings.HasPrefix(msg, refusalRequestBad) || strings.HasPrefix(msg, refusalImageFormat)
}

// rewriteImagesToTags folds every inline image of a user message into the
// message text as "<image_base64>…</image_base64>". It reports false when the
// body carries no inline image, which is also the signal that a retry would be
// pointless.
func rewriteImagesToTags(raw []byte) ([]byte, bool) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return raw, false
	}
	rawMsgs, ok := body["messages"]
	if !ok {
		return raw, false
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return raw, false
	}

	changed := false
	for _, m := range msgs {
		if stringField(m["role"]) != "user" {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(m["content"], &parts) != nil {
			continue // plain string content cannot carry an image
		}
		var text strings.Builder
		images := 0
		for _, p := range parts {
			switch partType(p) {
			case "text", "input_text":
				text.WriteString(stringField(p["text"]))
			case "image_url", "input_image", "image":
				b64, ok := inlineBase64(imageURLOf(p))
				if !ok {
					continue
				}
				images++
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(imageTagOpen)
				text.WriteString(b64)
				text.WriteString(imageTagClose)
			}
		}
		if images == 0 {
			continue
		}
		m["content"], _ = json.Marshal(text.String())
		changed = true
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		return raw, false
	}
	body["messages"] = encoded
	merged, err := json.Marshal(body)
	if err != nil {
		return raw, false
	}
	return merged, true
}

// inlineBase64 returns the payload of a `data:…;base64,` URL.
func inlineBase64(url string) (string, bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", false
	}
	i := strings.Index(url, ";base64,")
	if i < 0 {
		return "", false
	}
	b64 := strings.TrimSpace(url[i+len(";base64,"):])
	if b64 == "" {
		return "", false
	}
	return b64, true
}

// prefersImageTag reports whether the operator pinned this model to the tag form.
// The measured backend drift makes a hard-coded list unsafe, so the default is off
// and the automatic refusal retry does the work instead.
func prefersImageTag(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "group/")
	for _, want := range currentConfig().ImageTagModels {
		if want != "" && model == want {
			return true
		}
	}
	return false
}
