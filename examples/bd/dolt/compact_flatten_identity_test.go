//go:build unix

package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactDoltCall is one fake-dolt invocation from the compact fixture log.
type compactDoltCall struct {
	db    string
	query string
}

// readCompactDoltCalls parses the fake dolt log into its invocations. A
// multi-line query (the flatten) continues until the next "db=" entry.
func readCompactDoltCalls(t *testing.T, logPath string) []compactDoltCall {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	var calls []compactDoltCall
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "db="); ok {
			if db, query, found := strings.Cut(rest, " query="); found {
				calls = append(calls, compactDoltCall{db: db, query: query})
				continue
			}
		}
		if len(calls) > 0 {
			calls[len(calls)-1].query += "\n" + line
		}
	}
	return calls
}

// A writer that commits right after the flatten's DOLT_COMMIT (a same-count
// UPDATE visible only in the live database) neither quarantines nor rewrites
// the flatten identity: verification reads the flatten commit, the run records
// that commit, and full GC is deferred to the next quiet run (#8502).
func TestCompactScriptLaterWriterAfterFlattenDefersWithoutQuarantine(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_after_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("later writer after the flatten must exit 0: %v\n%s", err, out)
	}
	for _, unwanted := range []string{"value hash changed", "INTEGRITY check failed", "quarantine and investigate"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("later writer's live change was verified as flatten damage (%q):\n%s", unwanted, out)
		}
	}
	for _, want := range []string{
		"flatten_HEAD=" + compactFakeFlattenCommit,
		"final_verify_HEAD=writercommit",
		"deferring full GC until the next quiet run",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(quarantine); !os.IsNotExist(statErr) {
		t.Fatalf("later writer must not quarantine; stat=%v", statErr)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if reason := compactMarkerValue(t, pendingGC, "reason"); reason != "writer race during flatten deferred full GC" {
		t.Fatalf("pending-GC reason = %q, want the writer-race defer", reason)
	}
	for _, call := range readCompactDoltCalls(t, fixture.doltLog) {
		if strings.Contains(call.query, "DOLT_GC") {
			t.Fatalf("full GC ran while a later writer was active: %+v", call)
		}
	}
}
