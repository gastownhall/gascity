package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSupervisorPreForkOwnershipWarning exercises
// supervisorPreForkOwnershipWarning, which warns before a bare
// `gc supervisor start` fork when gc's own systemd user unit is installed
// but not currently active — the shape that leaves the forked process
// outside the unit's Restart=always policy (ga-9pjtoy).
func TestSupervisorPreForkOwnershipWarning(t *testing.T) {
	cases := []struct {
		name       string
		writeUnit  bool
		unitActive bool
		wantEmpty  bool
	}{
		{
			name:      "no unit installed",
			writeUnit: false,
			wantEmpty: true,
		},
		{
			name:       "unit installed and active",
			writeUnit:  true,
			unitActive: true,
			wantEmpty:  true,
		},
		{
			name:       "unit installed but inactive",
			writeUnit:  true,
			unitActive: false,
			wantEmpty:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)

			unitPath := supervisorSystemdServicePath()
			if tc.writeUnit {
				if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=test\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldActive := supervisorSystemctlActive
			supervisorSystemctlActive = func(_ string) bool { return tc.unitActive }
			t.Cleanup(func() { supervisorSystemctlActive = oldActive })

			got := supervisorPreForkOwnershipWarning()

			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("supervisorPreForkOwnershipWarning() = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("supervisorPreForkOwnershipWarning() = empty, want non-empty warning")
			}
			unitName := supervisorSystemdServiceName()
			for _, want := range []string{unitName, "Restart=always", "reset-failed"} {
				if !strings.Contains(got, want) {
					t.Fatalf("supervisorPreForkOwnershipWarning() = %q, want it to mention %q", got, want)
				}
			}
		})
	}
}
