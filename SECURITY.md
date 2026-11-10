# Security policy

## Reporting a vulnerability

Please report security issues privately, not in a public issue:

- Open a draft security advisory: <https://github.com/MYSTERY2019/Loomy2api-CPAMC/security/advisories/new>
- If that is not available to you, open an issue that says only that you have a report and how to reach you — no details.

Please include what you need to make the report reproducible: the plugin version, the affected route or function, and whether a real Loomy account or only a mock was involved. Reports about the CLIProxyAPI host itself belong to <https://github.com/router-for-me/CLIProxyAPI>.

## Supported versions

Only the latest release receives fixes. The plugin is a shared library loaded by CLIProxyAPI; there is no separate patch line for older tags.

## What counts as a vulnerability

- Disclosure of a Loomy session, account password, or access key to a party that should not see it.
- A request reaching a host that is not the configured gateway or account service.
- Bypassing the point guard so that a request is sent when it should have been blocked.
- Local privilege issues in the management API or data directory (for example files written world-readable, or a route that answers without the host's management authentication).

## Known, accepted behaviour

- **The Loomy session travels in a custom `token` header.** This is not a choice: the upstream gateway returns `code 100002` "缺少 token" on `/models` and `/points/*` without it, while `/chat/completions` accepts `Authorization: Bearer` alone. The plugin therefore keeps the header and constrains where requests may go instead — a redirect policy that refuses cross-host hops and a transport-level host guard that only allows the configured gateway/account hosts.
- **`auths/loomy-*.json` contains the phone number and password in clear text**, by design, because password-backed sessions are what allow automatic renewal. The plugin does not write these files; CLIProxyAPI owns them. Keep the directory `0700` and the files `0600`.
- **Using the plugin to resell, rent out, or mass-register accounts is out of scope and against the upstream terms.** See the disclaimer in the READMEs.

## How the last two findings were handled

| Finding | Fix |
|---|---|
| Session copied to a redirect target (custom headers survive a redirect; only `Authorization`/`Cookie` are stripped) | `CheckRedirect` refuses cross-host hops and `https`→`http` downgrades; `hostGuard` refuses any request to an unconfigured host. Regression test: `TestCrossHostRedirectDoesNotLeakSession` |
| `metadata.GitHubRepository` blanked by a history rewrite, so CLIProxyAPI refused to load the plugin | Field restored; `plugin/metadata_test.go` fails CI if any required metadata field is empty |
