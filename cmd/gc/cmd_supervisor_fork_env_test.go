package main

import (
	"strings"
	"testing"
)

// TestSupervisorForkEnv exercises supervisorForkEnv, which derives the
// environment for a forked `gc supervisor run` child (ga-9pjtoy): the child
// must not inherit GC_SESSION_ID (otherwise proctable's orphan scan treats it
// as the launching agent's own runtime once reparented), and must carry
// supervisorPreserveSessionsOnSignalEnv=1 so a later `systemctl restart`
// still preserves agent sessions on SIGTERM.
func TestSupervisorForkEnv(t *testing.T) {
	cases := []struct {
		name   string
		parent []string
		check  func(t *testing.T, got []string)
	}{
		{
			name:   "drops GC_SESSION_ID",
			parent: []string{"PATH=/bin", "GC_SESSION_ID=ga-abc123", "HOME=/home/x"},
			check: func(t *testing.T, got []string) {
				for _, kv := range got {
					if strings.HasPrefix(kv, "GC_SESSION_ID=") {
						t.Fatalf("supervisorForkEnv result %v still carries GC_SESSION_ID", got)
					}
				}
			},
		},
		{
			name:   "adds preserve-var when absent",
			parent: []string{"PATH=/bin"},
			check: func(t *testing.T, got []string) {
				if !containsExactly(got, supervisorPreserveSessionsOnSignalEnv+"=1") {
					t.Fatalf("supervisorForkEnv result %v does not carry %s=1", got, supervisorPreserveSessionsOnSignalEnv)
				}
			},
		},
		{
			name: "replaces preserve-var in place rather than duplicating",
			parent: []string{
				"PATH=/bin",
				supervisorPreserveSessionsOnSignalEnv + "=0",
				"HOME=/home/x",
			},
			check: func(t *testing.T, got []string) {
				var matches []string
				for _, kv := range got {
					if strings.HasPrefix(kv, supervisorPreserveSessionsOnSignalEnv+"=") {
						matches = append(matches, kv)
					}
				}
				if len(matches) != 1 {
					t.Fatalf("supervisorForkEnv result %v has %d entries for %s, want exactly 1", got, len(matches), supervisorPreserveSessionsOnSignalEnv)
				}
				if matches[0] != supervisorPreserveSessionsOnSignalEnv+"=1" {
					t.Fatalf("supervisorForkEnv result carries %q, want value 1", matches[0])
				}
			},
		},
		{
			name:   "preserves unrelated vars in order",
			parent: []string{"PATH=/bin", "GC_SESSION_ID=ga-abc123", "HOME=/home/x", "LANG=C"},
			check: func(t *testing.T, got []string) {
				var order []string
				for _, kv := range got {
					if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "LANG=") {
						order = append(order, kv)
					}
				}
				want := []string{"PATH=/bin", "HOME=/home/x", "LANG=C"}
				if len(order) != len(want) {
					t.Fatalf("supervisorForkEnv result %v, want unrelated vars %v preserved", got, want)
				}
				for i := range want {
					if order[i] != want[i] {
						t.Fatalf("supervisorForkEnv reordered unrelated vars: got %v, want %v", order, want)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := supervisorForkEnv(tc.parent)
			tc.check(t, got)
		})
	}
}

func containsExactly(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
