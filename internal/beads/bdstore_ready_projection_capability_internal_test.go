package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// bdEmbeddedSQLRefusal is the verbatim failure a bd serving a non-Dolt backend
// returns for `bd sql`, as gc's runner composes it: bd writes
// `Error: 'bd sql' is not yet supported in embedded mode` to stderr
// (cmd/bd/sql.go HandleError) and classifyBDExecResult wraps it onto the exit
// status.
const bdEmbeddedSQLRefusal = "exit status 1: Error: 'bd sql' is not yet supported in embedded mode"

// embeddedSQLRunner answers `bd version` like bd 1.1.0 and refuses `bd sql` the
// way a bd serving a backend it cannot open a SQL session against does.
func embeddedSQLRunner() *recordingRunner {
	r := &recordingRunner{}
	r.reply = func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "version":
			return []byte("bd version 1.1.0\n"), nil
		case len(args) > 0 && args[0] == "sql":
			return nil, errors.New(bdEmbeddedSQLRefusal)
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	return r
}

func writeScopeMetadata(t *testing.T, scope string, meta map[string]any) {
	t.Helper()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", beadsDir, err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), data, 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func activeWorkBeads() []Bead {
	return []Bead{
		{ID: "mc-1", Type: "task", Status: "open"},
		{ID: "mc-2", Type: "task", Status: "open"},
	}
}

// TestReadyProjectionLatchesOnTheEmbeddedModeRefusal is the live maintainer-city
// defect: bd serves that city's work class from hosted Postgres, `bd sql` is not
// implemented there, and the capability gate only ever asked bd its VERSION — so
// every cache prime and every reconcile spent a guaranteed-failing 6-16s
// subprocess, forever.
//
// The refusal is a permanent property of the ledger in front of the process, so
// it is latched: bd is asked exactly once.
func TestReadyProjectionLatchesOnTheEmbeddedModeRefusal(t *testing.T) {
	runner := embeddedSQLRunner()
	notices := &bytes.Buffer{}
	s := NewBdStore(t.TempDir(), runner.run, WithBdStoreNoticeSink(notices))

	first, err := s.enrichReadyProjectionForCache(activeWorkBeads())
	if !errors.Is(err, ErrReadyProjectionUnsupported) {
		t.Fatalf("first enrich error = %v, want ErrReadyProjectionUnsupported", err)
	}
	if !strings.Contains(err.Error(), "not yet supported in embedded mode") {
		t.Errorf("degrade does not carry bd's cause: %v", err)
	}
	for _, b := range first {
		if b.IsBlocked != nil {
			t.Errorf("bead %s was enriched from a failed projection: %v", b.ID, *b.IsBlocked)
		}
	}

	var laterErrs []error
	for i := 2; i <= 5; i++ {
		if _, err := s.enrichReadyProjectionForCache(activeWorkBeads()); err != nil {
			laterErrs = append(laterErrs, fmt.Errorf("enrich #%d: %w", i, err))
		}
	}

	sqlCalls := 0
	for _, call := range runner.calls {
		if len(call) > 1 && call[1] == "sql" {
			sqlCalls++
		}
	}
	if sqlCalls != 1 {
		t.Fatalf("bd sql ran %d times across 5 enrichments (calls=%v); the latch must spend exactly one", sqlCalls, runner.calls)
	}
	if len(laterErrs) != 0 {
		t.Fatalf("the latched projection kept reporting failures instead of degrading quietly: %v", laterErrs)
	}
	if got := strings.Count(notices.String(), "ready-projection enrichment disabled"); got != 1 {
		t.Fatalf("operator notice printed %d times, want exactly 1:\n%s", got, notices.String())
	}
	if !strings.Contains(notices.String(), "not yet supported in embedded mode") {
		t.Errorf("operator notice does not name the cause:\n%s", notices.String())
	}
}

// TestReadyProjectionRefusesAnUnimplementedBackendWithoutSpawningBd is the
// capability gate proper. `bd sql` support is a property of the BACKEND, not of
// the bd release, so a scope whose metadata names a backend this build does not
// implement never gets asked — not even for its version.
func TestReadyProjectionRefusesAnUnimplementedBackendWithoutSpawningBd(t *testing.T) {
	scope := t.TempDir()
	writeScopeMetadata(t, scope, map[string]any{
		"database":   "dolt",
		"backend":    "postgres",
		"dolt_mode":  "server",
		"project_id": "d2e95604-e869-478c-ad1a-ddee6e8bc3fc",
	})
	runner := embeddedSQLRunner()
	notices := &bytes.Buffer{}
	s := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(notices))

	for i := 1; i <= 3; i++ {
		out, err := s.enrichReadyProjectionForCache(activeWorkBeads())
		if i == 1 {
			if !errors.Is(err, ErrReadyProjectionUnsupported) {
				t.Fatalf("first enrich error = %v, want ErrReadyProjectionUnsupported", err)
			}
			if !strings.Contains(err.Error(), `unsupported backend "postgres"`) {
				t.Errorf("degrade does not name the backend: %v", err)
			}
		} else if err != nil {
			t.Fatalf("enrich #%d after the gate latched: %v", i, err)
		}
		for _, b := range out {
			if b.IsBlocked != nil {
				t.Errorf("bead %s was enriched by an unsupported backend", b.ID)
			}
		}
	}

	if len(runner.calls) != 0 {
		t.Fatalf("an unimplemented backend was asked %v; the gate must spend no subprocess", runner.calls)
	}
	if got := strings.Count(notices.String(), "ready-projection enrichment disabled"); got != 1 {
		t.Fatalf("operator notice printed %d times, want exactly 1:\n%s", got, notices.String())
	}
}

// TestReadyProjectionOnAnImplementedBackendIsUnchanged is the byte-identity
// guard for the five Dolt cities. Their metadata names a backend this build
// implements, so the gate is inert: the exact same two commands run, in the same
// order, and the rows come back enriched.
//
// Mutation proof: flipping "dolt" to any backend this build does not register
// empties runner.calls (the sibling test above), and deleting the latch in
// fetchReadyProjection re-runs `bd sql` on every call
// (TestReadyProjectionLatchesOnTheEmbeddedModeRefusal).
func TestReadyProjectionOnAnImplementedBackendIsUnchanged(t *testing.T) {
	for _, backend := range []string{"dolt", "doltlite", ""} {
		t.Run("backend="+backend, func(t *testing.T) {
			scope := t.TempDir()
			meta := map[string]any{"database": "dolt", "dolt_mode": "server", "dolt_database": "hq"}
			if backend != "" {
				meta["backend"] = backend
			}
			writeScopeMetadata(t, scope, meta)
			runner := readyProjectionRunner(`[{"id":"mc-1","is_blocked":false},{"id":"mc-2","is_blocked":true}]`)
			notices := &bytes.Buffer{}
			s := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(notices))

			out, err := s.enrichReadyProjectionForCache(activeWorkBeads())
			if err != nil {
				t.Fatalf("enrichReadyProjectionForCache: %v", err)
			}
			want := [][]string{
				{"bd", "version"},
				{"bd", "sql", readyProjectionSQL(), "--json"},
			}
			if !reflect.DeepEqual(runner.calls, want) {
				t.Fatalf("bd invocations = %v, want %v", runner.calls, want)
			}
			byID := make(map[string]Bead, len(out))
			for _, b := range out {
				byID[b.ID] = b
			}
			for id, wantBlocked := range map[string]bool{"mc-1": false, "mc-2": true} {
				got := byID[id].IsBlocked
				if got == nil || *got != wantBlocked {
					t.Errorf("bead %s is_blocked = %v, want &%v", id, got, wantBlocked)
				}
			}
			if notices.Len() != 0 {
				t.Errorf("an implemented backend printed a degrade notice:\n%s", notices.String())
			}
		})
	}
}

// TestReadyProjectionUnreadableMetadataFallsThroughToTheLatch keeps the gate
// fail-open: metadata gc cannot read is not evidence about the backend, so the
// version probe still runs and the runtime latch is what catches the refusal.
func TestReadyProjectionUnreadableMetadataFallsThroughToTheLatch(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	runner := embeddedSQLRunner()
	s := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(&bytes.Buffer{}))

	if _, err := s.enrichReadyProjectionForCache(activeWorkBeads()); !errors.Is(err, ErrReadyProjectionUnsupported) {
		t.Fatalf("enrich error = %v, want the runtime latch verdict", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %v, want the version probe and one refused sql", runner.calls)
	}
}
