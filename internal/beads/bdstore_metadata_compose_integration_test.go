//go:build integration

package beads_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/test/toolhome"
)

// TestBdStoreConcurrentDifferentKeySetMetadataComposesOnSharedServer is the
// #1596 regression row: two writers concurrently `bd update --set-metadata`
// DIFFERENT keys of one bead, and both writes must land. bd <= v1.2.2 merged
// the pair into a client-side snapshot of the whole metadata document and
// wrote that back, so the later commit erased the earlier writer's sibling
// key while both exited 0 — one round in three on a shared dolt sql-server.
// bd >= v1.3.0 (beads #5191) resolves the pair against the row re-read
// inside the write transaction, so different keys compose and only a
// same-key race is last-writer-wins. BdStore.SetMetadata is one such bd
// update per call, so what this row pins is that the store's own plumbing
// composes under that floor.
//
// The scope must be in server mode against a shared dolt sql-server. An
// embedded scope serializes writers on the scope-local database and the row
// goes vacuous — even bd 1.2.2 loses nothing there — so the test asserts
// dolt_mode=server in .beads/metadata.json before the first round. The
// bd_conditional_release_contract_test cell pins the current bd release;
// bd_metadata_compose_floor_test runs this same row against BD_PREV_VERSION
// (v1.3.0, the fix's first release) so the floor cannot quietly regress.
func TestBdStoreConcurrentDifferentKeySetMetadataComposesOnSharedServer(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	port := beads.StartTestDoltServerForTest(t)

	// A real bd scope in server mode: git init (bd commits to git on init),
	// then bd init pointing at the shared server. The runner pins BEADS_DIR
	// to the scope and strips every ambient BEADS_* and dolt-server knob, so
	// bd resolves the server from the scope's own state, exactly as the
	// production store does out of a developer's shell.
	scope := beadstest.GuardedTempDir(t)
	git := exec.Command("git", "init", "--quiet", scope)
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
	runner := serverScopeRunner(t, scope)
	if out, err := runner(scope, "bd", "init", "-p", "tst",
		"--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port),
		"--server-user", "root", "--external", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("bd init server mode: %v\n%s", err, out)
	}
	if out, err := runner(scope, "bd", "version"); err != nil {
		t.Logf("bd version probe failed: %v", err)
	} else {
		t.Logf("bd under test: %s", strings.TrimSpace(string(out)))
	}

	// The mode check is part of the contract, not decoration: an embedded
	// fallback makes every round below vacuous, so refuse to run rather than
	// pass green over a harness that stopped exercising the bug.
	metaRaw, err := os.ReadFile(filepath.Join(scope, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read scope metadata.json: %v", err)
	}
	var scopeMeta map[string]any
	if err := json.Unmarshal(metaRaw, &scopeMeta); err != nil {
		t.Fatalf("parse scope metadata.json %s: %v", metaRaw, err)
	}
	if mode, _ := scopeMeta["dolt_mode"].(string); mode != "server" {
		t.Fatalf("scope dolt_mode = %q, want %q; the concurrency row is vacuous in any other mode", mode, "server")
	}

	// Two store values are two independent writers: each SetMetadata is its
	// own bd process doing its own read-modify-write, the reporter's shape.
	writers := []*beads.BdStore{beads.NewBdStore(scope, runner), beads.NewBdStore(scope, runner)}
	created, err := writers[0].Create(beads.Bead{Title: "different-key set-metadata composition probe"})
	if err != nil {
		t.Fatalf("Create probe bead: %v", err)
	}

	// A fixed value per round hides every loss after the first, so each
	// round writes its own. One barrier releases both writers together; a
	// nil error plus the key check is also the retry-budget contract: a
	// writer that exhausts bd's serialization retries must fail loudly, not
	// report nil with a missing key.
	var lost []string
	for round := 1; round <= 50; round++ {
		value := fmt.Sprintf("v%d", round)
		if msgs := raceSetMetadata(t, writers, created.ID, [2]string{"k1", "k2"}, [2]string{value, value}); len(msgs) > 0 {
			lost = append(lost, fmt.Sprintf("round %d: %s", round, strings.Join(msgs, ", ")))
		}
	}
	if len(lost) > 0 {
		t.Fatalf("%d of 50 rounds lost a concurrent different-key --set-metadata write (bd merged one writer's whole document over the other's):\n%s",
			len(lost), strings.Join(lost, "\n"))
	}

	// Same-key races have no composition to check — last writer wins — but
	// both writers must still report nil and leave one of the two values.
	if msgs := raceSetMetadata(t, writers, created.ID, [2]string{"k1", "k1"}, [2]string{"same-a", "same-b"}); len(msgs) > 0 {
		t.Fatalf("same-key race: %s", strings.Join(msgs, ", "))
	}

	// A multi-key SetMetadataBatch (one bd update carrying every pair) must
	// compose with a single-key writer the same way: all four keys land.
	const batchValue = "batched"
	batchKeys := [3]string{"b1", "b2", "b3"}
	batch := map[string]string{}
	for _, k := range batchKeys {
		batch[k] = batchValue
	}
	barrier := make(chan struct{})
	var batchErr, singleErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-barrier
		batchErr = writers[0].SetMetadataBatch(created.ID, batch)
	}()
	go func() {
		defer wg.Done()
		<-barrier
		singleErr = writers[1].SetMetadata(created.ID, "s1", batchValue)
	}()
	close(barrier)
	wg.Wait()
	for i, err := range []error{batchErr, singleErr} {
		if err != nil {
			t.Fatalf("batch-vs-single race writer %d: %v", i, err)
		}
	}
	got, err := writers[0].Get(created.ID)
	if err != nil {
		t.Fatalf("Get after batch race: %v", err)
	}
	for _, k := range append(batchKeys[:], "s1") {
		if got.Metadata[k] != batchValue {
			t.Fatalf("after batch-vs-single race, key %s = %q, want %q (metadata %v)", k, got.Metadata[k], batchValue, got.Metadata)
		}
	}
}

// serverScopeRunner is newConditionalIntegrationRunner for a scope bound to
// a shared dolt sql-server. Same isolation (bd re-homed, every ambient
// BEADS_* dropped, BEADS_DIR pinned to the scope, dolt auto-start off) but
// with BEADS_TEST_MODE forced to "0": bd's storage layer reads that flag as
// a hard guard and resolves a server-bound workspace to the 127.0.0.1:1
// sentinel instead of its recorded port while it is "1" (see
// beadstest.EnvBeadsTestMode), so the default the embedded-scope runner
// re-applies would send every write to a port nothing listens on.
func serverScopeRunner(t *testing.T, scopeDir string) beads.CommandRunner {
	t.Helper()
	home := filepath.Join(filepath.Dir(scopeDir), "server-tool-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create bd tool home: %v", err)
	}
	env := map[string]string{}
	for _, kv := range toolhome.Environ(os.Environ(), home) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	env["BEADS_DIR"] = filepath.Join(scopeDir, ".beads")
	env["BEADS_DOLT_AUTO_START"] = "0"
	env[beadstest.EnvBeadsTestMode] = "0"
	// toolhome.Environ drops every BEADS_* name, so BdSubprocessEnv re-applies the
	// test-mode default that keeps bd's detached metrics flusher from racing the
	// TempDir removal (ga-aik16g); the explicit "0" above wins over it.
	return beads.ExecCommandRunnerWithExactEnvContext(context.Background(), beadstest.BdSubprocessEnv(env))
}

// raceSetMetadata releases two SetMetadata writers on one bead from a shared
// barrier, requires both to return nil, and returns the ways the bead's
// metadata disagrees with want. Each writer i sets key i to value i; values
// may repeat for a same-key race, where the final value may be either
// writer's.
func raceSetMetadata(t *testing.T, writers []*beads.BdStore, id string, keys, values [2]string) []string {
	t.Helper()
	barrier := make(chan struct{})
	errs := make([]error, len(writers))
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			errs[i] = writers[i].SetMetadata(id, keys[i], values[i])
		}(i)
	}
	close(barrier)
	wg.Wait()
	var problems []string
	for i, err := range errs {
		if err != nil {
			problems = append(problems, fmt.Sprintf("writer %d SetMetadata(%s=%s): %v", i, keys[i], values[i], err))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	got, err := writers[0].Get(id)
	if err != nil {
		return []string{fmt.Sprintf("Get after race: %v", err)}
	}
	for i := range keys {
		switch got.Metadata[keys[i]] {
		case values[i]:
			continue
		case values[1-i]:
			// The other writer's value: only legal in a same-key race.
			if keys[i] == keys[1-i] {
				continue
			}
		}
		problems = append(problems, fmt.Sprintf("key %s = %q, want %q (metadata %v)", keys[i], got.Metadata[keys[i]], values[i], got.Metadata))
	}
	return problems
}
