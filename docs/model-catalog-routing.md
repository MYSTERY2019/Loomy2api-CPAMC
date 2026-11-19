# Model catalog and routing

## Catalog behavior

- Model caches are isolated by account session. Concurrent requests for the same account share a fetch; a manual refresh can bypass the cache.
- The management model list merges results from enabled accounts, removes duplicate model IDs, and reports partial failures. A built-in fallback is used only when all account requests fail.
- `model.for_auth` remains scoped to the selected account. A pool-wide catalog does not grant each account the capabilities of every other account.
- Static registration is not proof of live support. When support cannot be established, the plugin should not send a chat request; when an account does not support a model, the request is rejected locally.

## Image input

**The cause was a missing request header, not a missing capability.**

The desktop client tags every request with a business purpose (`X-Loomy-Request-Purpose`,
17 fixed values: `chat.message`, `kb.vision`, `image.generate`, …) and the gateway routes by
that tag. Measured against `loomyad.xunfei.cn/api/v1` with one 512px JPEG and
`DeepSeek-V4.1-Flash`:

| purpose header | prompt_tokens | answer to "which code is in the picture?" | latency |
|---|---|---|---|
| *(none)* | 63 | blind ("1") | 82 s |
| `chat.message` | 63 | blind ("无") | 44 s |
| **`kb.vision`** | **168** | **`ZQ-731` (correct)** | **2 s** |
| `kb.vision` + ChatId/MsgId/InvokeOrigin/traceparent | **168** | **`ZQ-731`** | 1 s |

So the plugin sends the purpose header whenever the body carries an inline image
(`hasImagePart`), which puts the request on the vision route. Everything else — the model
list, the account, the endpoint — was already right.

Consequences worth knowing:

- The earlier "text-only model" placeholder and the opaque `400 请求有误` were both symptoms of
  the same missing routing signal (plus unsupported part types, now normalised).
- The catalogue's `vision: true` was telling the truth all along; only the request lacked the
  tag that selects the vision route.
- The client also uses the tag to *bill* the call (`kb.vision` is its own purpose), so sending
  it for image turns matches how the official client pays for the same work.


So the plugin does not promise vision it cannot deliver:

- `normalizeChatPayload` rewrites only the shapes the gateway rejects or cannot use: an empty
  user message text becomes `[空消息]`, a `tool` message without `tool_call_id` is dropped, an
  image on a model without vision becomes an explicit note, and a non-`data:` image URL becomes
  a note instead of a guaranteed `400`. An `input_image` (Responses-API spelling) part is
  rewritten to the classic `image_url` chat shape — measured against
  `iflytek-spark-loomy-2`, that part type alone returns the opaque 400.
- The first time a response carries the degradation marker, that model is remembered for the
  life of the process (`vision` event in the panel) and later requests send a note instead of
  the image.

What this cannot fix: if the account really does have no vision route, the models still cannot
see pictures. Use a model from a provider that does, or OCR locally and send the text.

### `<image_base64>` fallback

The other Loomy gateways in the market converged on a second image shape. Measured by
`magpie-loomy-auth` (2026-11-08, eight models × two shapes, backends read from
`error.metadata.provider_name`):

| backend | native `image_url` data URI | `<image_base64>` text tag |
|---|---|---|
| `iflytek-spark-loomy` | **400** | 200 |
| `alibaba-dashscope` | **400** `暂不支持该图片格式` | 200 |
| DeepSeek / GLM / mimo | 200 (native vision) | 200 |

Which backend serves a model name drifts, so the plugin does not hard-code a list:

- `image-tag-retry` (default **on**): when a 400 carries the image-shape refusal — the envelope's
  `error.message` must *start* with `请求有误，请重试` or `暂不支持该图片格式`, so a user pasting
  that sentence into the chat cannot trigger it — the same body is converted to the tag form and
  sent **once** more. That retry is a second billed request, which is why it is spent only on a
  real refusal.
- `image-tag-models: model-a,model-b`: pins the tag form for models known to sit on a first-party
  backend, saving the refused round trip. Empty by default.

Note the tag form is text: it makes the request acceptable, not the model sighted. A model whose
route is text-only still cannot read the picture.


## Image generation models

Image generation models cannot use the chat-completions execution path. They require the host's
image route, model type and execution adapter. This plugin's chat handler does not make an image
model usable merely by listing it in the catalog.

