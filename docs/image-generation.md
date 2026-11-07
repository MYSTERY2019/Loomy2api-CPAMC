# Image generation support

## Route and request support

When an account's live image catalogue includes a supported model, the plugin registers it as an OpenAI-compatible image model for CPA. Requests use the JSON text-to-image route with `n: 1` and `response_format: "url"`. Image edits, reference-image input, streaming, and base64 output are not supported.

The plugin refreshes the selected account's image catalogue before generation. Daily-only mode blocks image generation because the cost cannot be capped reliably per call. Other point policies are safeguards, not spending guarantees.

## Billing and model-list behavior

Image responses report the charge in `usage.points_consumed`, for example:

```json
{"created": 0, "data": [{"url": "https://…"}], "usage": {"points_consumed": 1}}
```

The standard CPA `/v1/models` response exposes OpenAI-compatible fields and may not expose CPA's internal image type. A model appearing in a static registration list does not guarantee that an account currently supports it.

## Testing

Local tests use a mock upstream and do not generate images or spend points. For live checks, use the deployment's approved test process and check the account ledger before retrying an ambiguous request; the cost can vary.
