# Model catalog and routing

## Catalog behavior

- Model caches are isolated by account session. Concurrent requests for the same account share a fetch; a manual refresh can bypass the cache.
- The management model list merges results from enabled accounts, removes duplicate model IDs, and reports partial failures. A built-in fallback is used only when all account requests fail.
- `model.for_auth` remains scoped to the selected account. A pool-wide catalog does not grant each account the capabilities of every other account.
- Static registration is not proof of live support. When support cannot be established, the plugin should not send a chat request; when an account does not support a model, the request is rejected locally.

## Image models

Image models cannot use the chat-completions execution path. They require the host's image route, model type and execution adapter. This plugin's chat handler does not make an image model usable merely by listing it in the catalog.
