package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A points lookup that fails for network reasons must not read as a broken
// account: the row keeps the last known balance and says it is stale.
func TestPointsStateStaleInsteadOfError(t *testing.T) {
	good := pointsInfo{Permanent: 12000, Daily: 3400, Available: 15400}

	cases := []struct {
		name      string
		entry     pointsCacheEntry
		wantState string
		wantPerm  float64
		wantInDetail string
	}{
		{
			name:      "success is ok",
			entry:     pointsCacheEntry{p: good},
			wantState: "ok",
			wantPerm:  12000,
		},
		{
			name:      "auth failure invalidates the session",
			entry:     pointsCacheEntry{err: &authError{code: "100002", msg: "token expired"}},
			wantState: "invalid",
			wantInDetail: "100002",
		},
		{
			name: "upstream failure with a previous reading goes stale",
			entry: pointsCacheEntry{
				err:      errors.New("context deadline exceeded"),
				lastGood: good,
				lastOK:   time.Now().Add(-3 * time.Minute),
			},
			wantState:    "stale",
			wantPerm:     12000,
			wantInDetail: "3 分钟前",
		},
		{
			name:      "upstream failure with nothing to fall back on stays error",
			entry:     pointsCacheEntry{err: errors.New("context deadline exceeded")},
			wantState: "error",
			wantInDetail: "deadline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, p, detail := pointsState(tc.entry)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if p.Permanent != tc.wantPerm {
				t.Fatalf("permanent = %v, want %v", p.Permanent, tc.wantPerm)
			}
			if !strings.Contains(detail, tc.wantInDetail) {
				t.Fatalf("detail = %q, want it to contain %q", detail, tc.wantInDetail)
			}
		})
	}
}

// The stale numbers must not be presentable as current, so the age wording has
// to stay readable at every scale.
func TestShortAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{12 * time.Second, "12 秒"},
		{59 * time.Second, "59 秒"},
		{3 * time.Minute, "3 分钟"},
		{59 * time.Minute, "59 分钟"},
		{90 * time.Minute, "1 小时"},
		{2 * time.Hour, "2 小时"},
	}
	for _, tc := range cases {
		if got := shortAge(tc.d); got != tc.want {
			t.Fatalf("shortAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
