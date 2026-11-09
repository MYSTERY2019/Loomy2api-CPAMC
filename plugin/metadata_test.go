package main

import (
	"regexp"
	"strings"
	"testing"
)

// CPA drops a plugin whose registration metadata is incomplete — the host log
// says "returned invalid metadata or no capabilities" and the plugin never
// loads. An empty GitHubRepository is enough to trigger it, which is how
// v1.0.2 and v1.0.3 shipped broken. Keep this guard.
func TestRegistrationMetadataIsComplete(t *testing.T) {
	reg := registration()
	meta, ok := reg["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("registration has no metadata map: %T", reg["metadata"])
	}

	for _, field := range []string{"Name", "Version", "Author", "GitHubRepository", "Logo"} {
		val, _ := meta[field].(string)
		if strings.TrimSpace(val) == "" {
			t.Fatalf("metadata.%s is empty; CPA will refuse to load the plugin", field)
		}
	}

	repo, _ := meta["GitHubRepository"].(string)
	if !regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+$`).MatchString(repo) {
		t.Fatalf("metadata.GitHubRepository must be https://github.com/{owner}/{repo}, got %q", repo)
	}

	version, _ := meta["Version"].(string)
	if strings.HasPrefix(version, "v") {
		t.Fatalf("metadata.Version must not start with v, got %q", version)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(version) {
		t.Fatalf("metadata.Version must be dotted numeric, got %q", version)
	}

	caps, ok := reg["capabilities"].(map[string]any)
	if !ok || len(caps) == 0 {
		t.Fatal("registration has no capabilities; CPA treats that as a broken plugin")
	}
	for _, want := range []string{"auth_provider", "executor", "management_api", "model_registrar", "quota_provider"} {
		if v, _ := caps[want].(bool); !v {
			t.Fatalf("capability %q missing or false", want)
		}
	}
}
