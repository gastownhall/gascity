//go:build integration || dolt_integration

package dolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real-Dolt counterpart of compact_rig_endpoint_test.go. A real managed
// sql-server answers a query for a database it does not serve with Error 1049,
// and a same-named copy it does serve carries real history that a flatten
// would destroy.
func TestCompactRealDoltSkipsRigPinnedToExternalDoltEndpoint(t *testing.T) {
	cases := []struct {
		name string
		// managedCopy leaves a cloudrig database on the managed server, as a
		// rig that moved to the cloud endpoint leaves its old store behind.
		managedCopy bool
	}{
		{name: "rig database absent from the managed server"},
		{name: "managed server holds a same-named copy", managedCopy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHistoryProtectionCity(t)
			seedTeamHistory(t, c.dolt, c.dbDir(), 0)
			copyDir := filepath.Join(c.dataDir, "cloudrig")
			if tc.managedCopy {
				seedTeamHistory(t, c.dolt, copyDir, historyProtectionThreshold+5)
				// Mark the copy's whole history as grown here, so on main the
				// compactor stamps the watermark on root and flattens it.
				if err := os.WriteFile(filepath.Join(copyDir, ".compact-full-history"), nil, 0o644); err != nil {
					t.Fatalf("write full-history marker: %v", err)
				}
			}
			c.start()

			cloudRig := writeCompactRigScope(t, t.TempDir(), "cloudrig", compactCloudPinConfig("cr"))
			fakeBin := t.TempDir()
			writeExecutable(t, filepath.Join(fakeBin, "gc"), fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  printf '%%s\n' '{"rigs":[{"name":"cloudrig","path":%q,"hq":false}]}'
fi
exit 0
`, cloudRig))
			copyCommits := ""
			if tc.managedCopy {
				copyCommits = c.cell("SELECT COUNT(*) FROM cloudrig.dolt_log")
			}

			out, err := c.compactCommand("PATH=" + strings.Join([]string{fakeBin, filepath.Dir(c.dolt), os.Getenv("PATH")}, string(os.PathListSeparator)))
			if err != nil {
				t.Fatalf("a rig pinned to an external Dolt endpoint must not fail the run: %v\n%s", err, out)
			}
			if strings.Contains(out, "database not found") {
				t.Fatalf("the compactor asked the managed server for the pinned rig's database:\n%s", out)
			}
			assertCompactPinnedScopeSkipLine(t, out, "cloudrig", cloudRig, compactCloudDoltEndpoint)
			if c.tag() == "" {
				t.Fatalf("the city's own database was not reached (no gc-compact-base stamp):\n%s", out)
			}
			if !tc.managedCopy {
				return
			}
			if got := c.cell("SELECT COUNT(*) FROM cloudrig.dolt_log"); got != copyCommits {
				t.Fatalf("managed copy commits = %s, want %s: the copy was flattened\n%s", got, copyCommits, out)
			}
			if got := c.cell("SELECT COUNT(*) FROM cloudrig.dolt_tags WHERE tag_name = 'gc-compact-base'"); got != strconv.Itoa(0) {
				t.Fatalf("managed copy got a gc-compact-base stamp; the compactor must not touch it:\n%s", out)
			}
		})
	}
}
