//go:build integration

package beads_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestBdStoreConditionalWriterConformance is the S2-T12 integration row: the
// ConditionalWriter conformance suite over a REAL bd binary. It is the
// authoritative guard for the provisional conditional-write machine codes
// ("precondition-failed" / "conditional-write-unsupported") and the
// "revision" wire key, all assumed ahead of beads#4682 landing — a rename in
// the shipped bd fails here loudly instead of drifting silently
// (bdstore_conditional.go's classifier note points at this row).
//
// Against today's bd (v1.1.0, no --if-revision) the conformance leg SKIPS
// via the store's own production capability probe — the skip decision is the
// exact decision production makes before degrading. The scaffold leg still
// runs against any bd so the scope recipe cannot rot while the row waits.
//
// Of the three adversarial classifier inputs the classifier build recorded
// as integration-row obligations, only (A) — a capable bd's cobra usage
// echo naming --if-revision while reporting a DIFFERENT unknown flag — is
// reliably producible against a live bd, and it runs here. (B) a policy gate
// refusal carrying an informational current_revision and (C) a coded refusal
// whose message contains "not found" require server-side policy state a
// stock bd does not expose on demand; they remain white-box fakeRunner cells
// in bdstore_conditional_internal_test.go (gate-refusal and code-dominance
// subtests) by design.
func TestBdStoreConditionalWriterConformance(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}

	t.Run("scaffold_roundtrip_any_bd", func(t *testing.T) {
		// Always runs (pre- and post-#4682): proves the git init + bd init +
		// NewBdStore recipe yields a working store, so the conformance leg's
		// scaffolding is exercised in CI long before it stops skipping.
		store, _ := newConditionalIntegrationBdStore(t)
		created, err := store.Create(beads.Bead{Title: "conditional-row scaffold probe"})
		if err != nil {
			t.Fatalf("Create against real bd: %v", err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get(%s) against real bd: %v", created.ID, err)
		}
		if got.Title != "conditional-row scaffold probe" {
			t.Fatalf("roundtrip title = %q", got.Title)
		}
	})

	probeStore, _ := newConditionalIntegrationBdStore(t)
	capable, err := beads.ConditionalWritesCapableForIntegration(probeStore)
	if err != nil {
		t.Skipf("capability probe against real bd failed (bd broken?): %v", err)
	}
	if !capable {
		t.Skip("installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly " +
			"and becomes the authoritative guard for the provisional body codes once #4682 lands")
	}

	t.Run("conformance", func(t *testing.T) {
		// Known likely first failure once #4682 lands and this leg goes live:
		// the contention subtest bursts ~30 concurrent bd subprocesses at one
		// EMBEDDED (serverless) scope, and embedded dolt lock/busy errors are
		// not in isBdTransientWriteError's serialization class — a failure
		// with lock/busy text there is a retry-classifier gap or a
		// server-mode-scope problem, NOT a #4682 wire-code contract break.
		// Widen the serialization class (production-relevant) or move this
		// scope to server mode before reading such a failure as the codes
		// being wrong.
		beadstest.RunConditionalWriterConformanceWithOptions(t, "BdStore",
			func(t *testing.T) beads.Store {
				store, _ := newConditionalIntegrationBdStore(t)
				return store
			},
			beadstest.ConditionalWriterOptions{
				// bd's precondition body carries current_revision (#4682);
				// asserting Current here is part of the wire-key guard.
				SuppliesCurrent: true,
				// BdStore has no constructor toggle — incapability is a
				// runtime latch — so the disable-toggle leg is absent, not
				// skipped (mirrors the harness doc).
				OpenDisabled: nil,
			})
	})

	t.Run("capable_usage_echo_must_not_classify_unsupported", func(t *testing.T) {
		// MUST stay below the capability skip above: a pre-#4682 bd rejects
		// --if-revision ITSELF ("unknown flag: --if-revision"), which is a
		// correct unsupported classification — running this cell against an
		// old bd would false-fail it. Only a capable bd's echo (unknown flag
		// = the bogus one, usage listing --if-revision) must stay unlatched.
		//
		// Build-spec adversarial input (A), driven against the REAL bd: a
		// capable bd given an unknown OTHER flag echoes usage that lists
		// --if-revision. If the classifier keyed on a floating substring
		// match (the F1 hazard), this real echo would latch a perfectly
		// capable store incapable and silently degrade every future fenced
		// write under auto.
		store, dir := newConditionalIntegrationBdStore(t)
		created, err := store.Create(beads.Bead{Title: "usage echo target"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		runner := newConditionalIntegrationRunner(dir)
		out, runErr := runner(dir, "bd", "update", created.ID,
			"--if-revision", "1", "--gc-integration-bogus-flag", "--json")
		if runErr == nil {
			t.Skip("bd accepted an unknown flag; the usage-echo cell is not drivable against this bd")
		}
		echo := string(out) + " " + runErr.Error()
		if !strings.Contains(echo, "if-revision") {
			t.Skipf("bd's unknown-flag error does not echo usage listing --if-revision; cell not drivable: %s", echo)
		}
		classified := beads.ClassifyConditionalWriteResultForIntegration(out, runErr)
		if beads.IsConditionalWriteUnsupported(classified) {
			t.Fatalf("a capable bd's usage echo classified as unsupported — this latches a capable store incapable: %v", classified)
		}
	})
}

// newConditionalIntegrationBdStore stands up a REAL bd scope in a fresh
// TempDir — git init + `bd init` in embedded (serverless) mode — and returns
// the production BdStore over it plus the scope root. Environment is pinned
// per scope (BEADS_DIR into the scope's own .beads) so an ambient BEADS_DIR
// from the invoking shell can never redirect the store to a live database,
// mirroring the libstore env-pinning precedent.
func newConditionalIntegrationBdStore(t *testing.T) (*beads.BdStore, string) {
	t.Helper()
	dir := t.TempDir()
	git := exec.Command("git", "init", "--quiet", dir)
	// GIT_DIR/GIT_WORK_TREE from the invoking shell would redirect init away
	// from the TempDir; strip them for this one call (setting them to the
	// empty string is not "unset" to git — it is an invalid path).
	env := git.Environ()
	kept := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			continue
		}
		kept = append(kept, kv)
	}
	git.Env = kept
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runner := newConditionalIntegrationRunner(dir)
	if out, err := runner(dir, "bd", "init", "-p", "tst", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("bd init: %v\n%s", err, out)
	}
	return beads.NewBdStore(dir, runner), dir
}

// newConditionalIntegrationRunner pins BEADS_DIR to the scope so every bd
// invocation resolves the scope-local embedded database, and force-clears the
// dolt-server env knobs: a dev shell with BEADS_DOLT_SERVER_HOST/PORT (a live
// deployment's dolt server) or BEADS_DOLT_AUTO_START=1 exported must never make
// this row write a tst database into a live server or leave a dolt sql-server
// running in the TempDir. (CI's packages shard runs under env -i and is safe
// either way; this guards local runs.)
func newConditionalIntegrationRunner(scopeDir string) beads.CommandRunner {
	return beads.ExecCommandRunnerWithEnv(map[string]string{
		"BEADS_DIR":              filepath.Join(scopeDir, ".beads"),
		"BEADS_DOLT_AUTO_START":  "0",
		"BEADS_DOLT_SERVER_HOST": "",
		"BEADS_DOLT_SERVER_PORT": "",
	})
}
