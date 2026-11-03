//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
)

// molDoWorkDrainScript returns the fenced bash block of the embedded
// mol-do-work drain step: the exact text a seat runs.
func molDoWorkDrainScript(t *testing.T) string {
	t.Helper()
	data, err := core.PackFS.ReadFile("formulas/mol-do-work.toml")
	if err != nil {
		t.Fatalf("reading mol-do-work: %v", err)
	}
	var f struct {
		Steps []struct {
			ID          string `toml:"id"`
			Description string `toml:"description"`
		} `toml:"steps"`
	}
	if _, err := toml.Decode(string(data), &f); err != nil {
		t.Fatalf("decoding mol-do-work: %v", err)
	}
	for _, step := range f.Steps {
		if step.ID != "drain" {
			continue
		}
		_, rest, ok := strings.Cut(step.Description, "```bash\n")
		if !ok {
			t.Fatalf("drain step has no bash block:\n%s", step.Description)
		}
		block, _, ok := strings.Cut(rest, "\n```")
		if !ok {
			t.Fatalf("drain step bash block is unterminated:\n%s", step.Description)
		}
		return block
	}
	t.Fatal("mol-do-work has no drain step")
	return ""
}

// jsonUnmarshalPayload decodes the JSON payload in a command's combined output.
func jsonUnmarshalPayload(out string, v any) error {
	return json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), v)
}

// setupGraphWorkflowCityWithoutAgents inits a bd+dolt-backed city that has no
// agents, so nothing but the test touches its beads.
func setupGraphWorkflowCityWithoutAgents(t *testing.T) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	cityToml := fmt.Sprintf("[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n", cityName)
	configPath := filepath.Join(t.TempDir(), "drain-city.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city config: %v", err)
	}
	if out, err := runGCDoltWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir); err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
		cleanupTestCityDir(cityDir)
	})
	return cityDir
}

// drainSeat is one seat running the drain block against a real bd+dolt city.
type drainSeat struct {
	cityDir   string
	sessionID string
	ackLog    string
	env       []string
	script    string
}

// newDrainSeat starts a bd-backed city and a session bead for the seat. `gc
// runtime drain-ack` is the one command it does not run for real: the shim in
// front of gc logs the call and succeeds, because the controller's reaction to
// an ack is not what this test is about; every other gc command is the
// production binary over the real store.
func newDrainSeat(t *testing.T) *drainSeat {
	t.Helper()
	cityDir := setupGraphWorkflowCityWithoutAgents(t)

	out, err := bdDolt(cityDir, "create", "--json", "--type", "session", "seat")
	if err != nil {
		t.Fatalf("creating session bead: %v\noutput: %s", err, out)
	}
	var seat graphBead
	if err := jsonUnmarshalPayload(out, &seat); err != nil {
		t.Fatalf("session bead: %v\noutput: %s", err, out)
	}

	shimDir := t.TempDir()
	ackLog := filepath.Join(shimDir, "ack.log")
	shim := fmt.Sprintf("#!/bin/bash\nif [ \"$1 $2\" = \"runtime drain-ack\" ]; then echo ack >> %q; exit 0; fi\nexec %q \"$@\"\n", ackLog, gcBinary)
	if err := os.WriteFile(filepath.Join(shimDir, "gc"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing gc shim: %v", err)
	}

	env := commandEnvForDir(cityDir, true)
	env = filterEnvMany(env, "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_CITY_RUNTIME_DIR", "GC_BEAD_ID", "GC_SESSION_ID")
	env = append(env,
		"GC_CITY="+cityDir,
		"GC_CITY_PATH="+cityDir,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(cityDir, ".gc", "runtime"),
		"GC_SESSION_ID="+seat.ID,
	)
	if port, ok := ensureManagedDoltPortForTest(cityDir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
	}
	env = replaceEnv(env, "PATH", prependPath(shimDir, parseEnvList(env)["PATH"]))

	return &drainSeat{cityDir: cityDir, sessionID: seat.ID, ackLog: ackLog, env: env, script: molDoWorkDrainScript(t)}
}

// bead creates a bead carrying the given metadata and returns its id.
func (s *drainSeat) bead(t *testing.T, title, status string, meta map[string]string) string {
	t.Helper()
	out, err := bdDolt(s.cityDir, "create", "--json", title)
	if err != nil {
		t.Fatalf("creating %s: %v\noutput: %s", title, err, out)
	}
	var b graphBead
	if err := jsonUnmarshalPayload(out, &b); err != nil {
		t.Fatalf("%s: %v\noutput: %s", title, err, out)
	}
	args := []string{"update", b.ID, "--status", status}
	for k, v := range meta {
		args = append(args, "--set-metadata", k+"="+v)
	}
	if out, err := bdDolt(s.cityDir, args...); err != nil {
		t.Fatalf("updating %s: %v\noutput: %s", b.ID, err, out)
	}
	return b.ID
}

// claim stamps beadID as the seat's current claim, as `gc hook --claim` does.
func (s *drainSeat) claim(t *testing.T, beadID string) {
	t.Helper()
	if out, err := bdDolt(s.cityDir, "update", s.sessionID, "--set-metadata", "current_claim_bead_id="+beadID); err != nil {
		t.Fatalf("stamping claim: %v\noutput: %s", err, out)
	}
}

// run executes the drain block and reports its combined output and error.
func (s *drainSeat) run(t *testing.T) (string, error) {
	t.Helper()
	return runCommand(s.cityDir, s.env, 2*time.Minute, "bash", "-c", "set -o pipefail\n"+s.script)
}

func (s *drainSeat) acked(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(s.ackLog)
	return err == nil && strings.Contains(string(data), "ack")
}

func (s *drainSeat) status(t *testing.T, id string) string {
	t.Helper()
	return showBead(t, s.cityDir, id).Status
}

const (
	drainRef  = "mol-do-work.drain"
	doWorkRef = "mol-do-work.do-work"
)

// TestMolDoWorkDrainClosesTheContinuationOfItsOwnWorkflow: the seat's claim is
// its do-work step, and the one ready drain bead of the same workflow closes
// while a stale open drain bead of an earlier workflow is left alone.
func TestMolDoWorkDrainClosesTheContinuationOfItsOwnWorkflow(t *testing.T) {
	seat := newDrainSeat(t)
	stale := seat.bead(t, "stale drain", "open", map[string]string{"gc.root_bead_id": "old-root", "gc.step_ref": drainRef})
	step := seat.bead(t, "do-work", "in_progress", map[string]string{"gc.root_bead_id": "new-root", "gc.step_ref": doWorkRef})
	drain := seat.bead(t, "drain", "open", map[string]string{"gc.root_bead_id": "new-root", "gc.step_ref": drainRef})
	seat.claim(t, step)

	if out, err := seat.run(t); err != nil {
		t.Fatalf("drain block failed: %v\noutput: %s", err, out)
	}
	if got := seat.status(t, drain); got != "closed" {
		t.Errorf("own continuation %s is %q, want closed", drain, got)
	}
	if got := seat.status(t, stale); got != "open" {
		t.Errorf("stale drain bead %s of another workflow is %q, want it left open", stale, got)
	}
	if !seat.acked(t) {
		t.Error("drain block did not reach `gc runtime drain-ack`")
	}
}

// TestMolDoWorkDrainClosesAClaimedDrainBeadDirectly pins the current-claim
// branch (ga-v4vyto risk 1): when the seat's claim is itself an open drain
// bead, that bead closes even though another workflow has an open drain bead
// too. The claim is the only source of the seat's workflow, so the block cannot
// second-guess it; a stale claim stamp is `gc hook --claim`'s to overwrite.
func TestMolDoWorkDrainClosesAClaimedDrainBeadDirectly(t *testing.T) {
	seat := newDrainSeat(t)
	other := seat.bead(t, "other drain", "open", map[string]string{"gc.root_bead_id": "other-root", "gc.step_ref": drainRef})
	claimed := seat.bead(t, "claimed drain", "open", map[string]string{"gc.root_bead_id": "my-root", "gc.step_ref": drainRef})
	seat.claim(t, claimed)

	if out, err := seat.run(t); err != nil {
		t.Fatalf("drain block failed: %v\noutput: %s", err, out)
	}
	if got := seat.status(t, claimed); got != "closed" {
		t.Errorf("claimed drain bead %s is %q, want closed", claimed, got)
	}
	if got := seat.status(t, other); got != "open" {
		t.Errorf("unclaimed drain bead %s is %q, want it left open", other, got)
	}
}

// TestMolDoWorkDrainWithoutAContinuationExitsBeforeTheAck pins ga-v4vyto risk
// 3: when no continuation resolves, the block exits 1 BEFORE `gc runtime
// drain-ack`, so the seat is not acked and nothing is closed. The controller's
// drain deadline then force-stops the seat (cmd/gc/session_wake.go), leaving
// the continuation bead open for the pool.
func TestMolDoWorkDrainWithoutAContinuationExitsBeforeTheAck(t *testing.T) {
	seat := newDrainSeat(t)
	step := seat.bead(t, "do-work", "in_progress", map[string]string{"gc.root_bead_id": "my-root", "gc.step_ref": doWorkRef})
	seat.claim(t, step)
	// No drain bead of my-root exists.

	out, err := seat.run(t)
	if err == nil {
		t.Fatalf("drain block succeeded with no continuation bead; output: %s", out)
	}
	if !strings.Contains(out, "could not resolve one ready continuation bead for root my-root") {
		t.Errorf("output does not name the unresolved root: %s", out)
	}
	if seat.acked(t) {
		t.Error("drain block reached `gc runtime drain-ack` after failing to resolve a continuation")
	}
	if got := seat.status(t, step); got != "in_progress" {
		t.Errorf("do-work step is %q after a failed drain, want it untouched", got)
	}
}

// TestMolDoWorkDrainRefusesTwoReadyContinuations: two ready drain beads of one
// workflow are ambiguous, so the block closes neither.
func TestMolDoWorkDrainRefusesTwoReadyContinuations(t *testing.T) {
	seat := newDrainSeat(t)
	step := seat.bead(t, "do-work", "in_progress", map[string]string{"gc.root_bead_id": "my-root", "gc.step_ref": doWorkRef})
	a := seat.bead(t, "drain a", "open", map[string]string{"gc.root_bead_id": "my-root", "gc.step_ref": drainRef})
	b := seat.bead(t, "drain b", "open", map[string]string{"gc.root_bead_id": "my-root", "gc.step_ref": drainRef})
	seat.claim(t, step)

	if out, err := seat.run(t); err == nil {
		t.Fatalf("drain block succeeded with two ready continuations; output: %s", out)
	}
	for _, id := range []string{a, b} {
		if got := seat.status(t, id); got != "open" {
			t.Errorf("ambiguous continuation %s is %q, want left open", id, got)
		}
	}
}
