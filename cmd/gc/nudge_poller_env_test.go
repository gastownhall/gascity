package main

import (
	"slices"
	"strings"
	"testing"
)

// Kills: the nudge poller spawn site passing the session's GC_SESSION_ID or
// GC_RUNTIME_EPOCH to its detached child (CONTRACT C4).
func TestNudgePollerCommandDropsSessionIdentity(t *testing.T) {
	cmd := newNudgePollerCommand("/bin/gc", []string{"PATH=/usr/bin", "GC_SESSION_ID=gc-1", "GC_RUNTIME_EPOCH=4"}, "/city", "worker", "mayor")
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "GC_SESSION_ID=") || strings.HasPrefix(entry, "GC_RUNTIME_EPOCH=") {
			t.Fatalf("poller env keeps %q: %q", entry, cmd.Env)
		}
	}
	if !slices.Contains(cmd.Env, "PATH=/usr/bin") {
		t.Fatalf("poller env lost PATH: %q", cmd.Env)
	}
}
