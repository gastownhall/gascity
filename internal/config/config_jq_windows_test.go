//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedCorpusDoesNotNeedExternalJqOnWindows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".EXE;.COM;.CMD;.BAT")
	if _, err := os.Stat(filepath.Join(dir, "jq.exe")); !os.IsNotExist(err) {
		t.Fatalf("test PATH unexpectedly contains jq.exe: %v", err)
	}

	cases := []struct {
		name string
		test func(*testing.T)
	}{
		{"migration-root", TestEffectiveWorkQueryClaimsRunTargetOnlyRootDuringMigration},
		{"divergent-route", TestEffectiveWorkQueryIgnoresDivergentRunTargetWhenRoutedToPresent},
		{"demand-routed", TestEffectivePoolDemandQueryCountsRoutedTo},
		{"demand-legacy", TestEffectivePoolDemandQueryCountsRunTargetOnlyRootDuringMigration},
		{"demand-divergent", TestEffectivePoolDemandQueryIgnoresDivergentRunTargetWhenRoutedToPresent},
		{"demand-empty", TestEffectivePoolDemandQueryTreatsEmptyReadyOutputAsZero},
		{"demand-dedup", TestEffectivePoolDemandQueryDedupsMigrationOverlap},
	}
	for _, tc := range cases {
		var sub *testing.T
		t.Run(tc.name, func(t *testing.T) {
			sub = t
			tc.test(t)
		})
		if sub == nil {
			t.Fatalf("%s did not run", tc.name)
		}
		if sub.Skipped() {
			t.Errorf("%s skipped with no external jq on PATH", tc.name)
		}
	}
}
