# Changelog

## v1.0.7 - 2026-11-20

### Vision
- **`lm/*` image turns now reach the model.** The gateway routes each request by its
  business-purpose header, `X-Loomy-Request-Purpose`. Without it, an inline image is
  replaced by a `[IMAGE: base64-encoded — not readable by this text-only model]` note
  and the turn is answered from text alone — which is why every `lm/*` model looked
  blind while the same picture worked through another provider. The plugin now sends
  `kb.vision` whenever the request body carries an inline image.
  Measured on `DeepSeek-V4.1-Flash` with one 512px JPEG: `prompt_tokens` 63 → 168, the
  model reads the code in the picture, first token in ~2s instead of ~80s.
- Unsupported part types are normalised before a request is spent: an `input_image`
  part (the Responses-API spelling) is rewritten to the classic `image_url` shape.
  Measured against `iflytek-spark-loomy-2`, that part type alone answers the opaque
  `400 请求有误`. Empty user text becomes `[空消息]`, a `tool` message without
  `tool_call_id` is dropped, and a non-`data:` image URL becomes a note instead of a
  guaranteed 400.
- Added the `<image_base64>` tag form other Loomy gateways fall back to: when a 400
  carries the image-shape refusal, the same body is retried once in tag form
  (`image-tag-retry`, default on), and `image-tag-models` pins chosen models to it.
  That retry is a second billed request, so it only runs on a real refusal.
- If the upstream still degrades an image, the model is remembered for the life of the
  process and later turns send a note instead of another wasted upload (`vision` event
  in the panel).

### Notes
- Image turns are declared under the `kb.vision` purpose — the same one the official
  desktop client uses for its own image analysis, so billing follows the vendor's
  existing split rather than inventing a new path.
- Recommended host config for interactive clients: `stream-mode: live`. With
  `buffered`, tokens are collected upstream and released when the turn finishes, which
  reads as "stuck, then a wall of text".

## v1.0.6 - 2026-11-17

### Reliability
- `lm/*` requests no longer answer `502 model_catalog_unavailable` when the
  upstream model catalogue cannot be refreshed. A catalogue that verified before
  keeps routing for a 60s grace window (`staleCatalogueGrace`), so a burst of TLS
  handshake timeouts or gateway 5xx answers no longer breaks requests for a model
  the account already supports. Auth (401/403) and rate-limit (429) answers still
  surface, and a cold cache still fails closed.
- The verified catalogue is reused for 30 minutes instead of 10
  (`catalogueFreshFor`), cutting catalogue refreshes - and the exposure to the
  gateway's intermittent TLS failures - by roughly two thirds.
- Tests cover both sides: a chat is forwarded when the catalogue refresh answers
  502 while that account's cache is warm, and the cold-cache path still fails
  closed with `model_catalog_unavailable`.

## v1.0.5 - 2026-11-15

- Per-account points are cached and shown in the console overview.
- Upstream points timeouts no longer surface as account errors.
- `verify-prod.sh` falls back to a real BASE_URL.
