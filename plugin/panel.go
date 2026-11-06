package main

import _ "embed"

// panelHTML is the console shell served on the unauthenticated resource route.
// It carries no data: every call it makes goes to /v0/management/... with the
// CPA management key the operator types in (kept in this browser only).
//
//go:embed panel.html
var panelHTML string
