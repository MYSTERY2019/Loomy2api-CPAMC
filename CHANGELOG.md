# Changelog

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
