package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Atomic replacement, with private permissions even if the old file was public.
func atomicPrivateWrite(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".loomy-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, e := os.Open(filepath.Dir(path)); e == nil {
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

var secretMu sync.RWMutex
var knownSecrets = map[string]struct{}{}
var secretField = regexp.MustCompile(`(?i)(["']?(?:session|password|passwd|token|access[_-]?(?:key|secret)|secret[_-]?key|signature|authorization|sign)["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|[^\s,;&}]+)`)
var bearerSecret = regexp.MustCompile(`(?i)\bbearer\s+[^\s,"';}]+`)

func rememberSecrets(values ...string) {
	secretMu.Lock()
	defer secretMu.Unlock()
	for _, v := range values {
		if len(v) >= 4 {
			knownSecrets[v] = struct{}{}
		}
	}
}

// Sanitize before truncation/persistence, not just in the browser. Known values
// cover unlabelled upstream echoes; field rules also cover unknown credentials.
func redactText(s string) string {
	secretMu.RLock()
	values := make([]string, 0, len(knownSecrets))
	for v := range knownSecrets {
		values = append(values, v)
	}
	secretMu.RUnlock()
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	s = bearerSecret.ReplaceAllString(s, "Bearer [REDACTED]")
	return secretField.ReplaceAllString(s, `${1}"[REDACTED]"`)
}
