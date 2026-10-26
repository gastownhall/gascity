package acceptancehelpers

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin when a failed `gc rig add` may be retried: once, and only
// when the failure is the bd v1.3.1 backend port race, proved by the Dolt line
// the init wrapper captured. They are pure: the classifier takes text, and the
// retry policy takes fakes for the add and for the filesystem question.

const (
	pingFailure = "gc rig add: provider-owned beads init: [mysql] packets.go:58 unexpected EOF\n" +
		"Error: failed to open uow provider: uow: ping db: dial tcp 127.0.0.1:43599: connect: connection refused\n"
	timeoutFailure = "gc rig add: provider-owned beads init: Error: timeout waiting for proxy to become ready on its OS-assigned port\n"
)

// diag renders one wrapper section the way the wrapper prints it.
func diag(path, body string) string {
	return fmt.Sprintf("--- bd init diagnostics: %s (%d bytes) ---\n%s--- end bd init diagnostics: %s ---\n", path, len(body), body, path)
}

func serverLogPath(scope string) string {
	return filepath.Join(scope, ".beads", "dolt", "server.log")
}

func TestPortCollisionProof(t *testing.T) {
	rig := "/work/rig"
	otherRig := "/work/other"
	good := diag(serverLogPath(rig), "Starting server\n"+portConflictLine+"\n")
	roots := []string{rig}

	for _, tc := range []struct {
		name      string
		out       string
		err       error
		roots     []string
		wantProof string
		wantErr   string
	}{
		{"ping with proof", pingFailure + good, errors.New("exit status 1"), roots, portConflictLine, ""},
		{"proxy-ready timeout with proof", timeoutFailure + good, errors.New("exit status 1"), roots, portConflictLine, ""},
		{
			"hard timeout with proof", "gc rig add: provider-owned beads init: Error: hard timeout (2m0s) waiting for proxy on its OS-assigned port\n" + good,
			errors.New("exit status 1"), roots, portConflictLine, "",
		},
		{"resolved scope root matches", pingFailure + good, errors.New("exit status 1"), []string{"/private" + rig, rig}, portConflictLine, ""},
		{"no error is never a proof", pingFailure + good, nil, roots, "", "no failure"},
		{"bare EOF and refused without a log section", pingFailure, errors.New("exit status 1"), roots, "", "no server.log"},
		{"timeout without a log section", timeoutFailure, errors.New("exit status 1"), roots, "", "no server.log"},
		{"proof line only in the surrounding text", pingFailure + portConflictLine + "\n", errors.New("exit status 1"), roots, "", "no server.log"},
		{"unrelated error with proof", "gc rig add: some other failure\n" + good, errors.New("exit status 1"), roots, "", "not a provider-owned init failure"},
		{
			"provider init failure of another kind", "gc rig add: provider-owned beads init: Error: schema mismatch\n" + good,
			errors.New("exit status 1"), roots, "", "not a provider-owned init failure",
		},
		{
			"ping text without the provider-owned init prefix", "Error: failed to open uow provider: uow: ping db: refused\n" + good,
			errors.New("exit status 1"), roots, "", "not a provider-owned init failure",
		},
		{
			"server.log without the conflict line", pingFailure + diag(serverLogPath(rig), "Starting server\nlistening\n"),
			errors.New("exit status 1"), roots, "", "no exact",
		},
		{
			"near-miss conflict wording", pingFailure + diag(serverLogPath(rig), "Port 43599 already in use\n"),
			errors.New("exit status 1"), roots, "", "no exact",
		},
		{
			"conflict line embedded in a longer line", pingFailure + diag(serverLogPath(rig), "level=error msg=\""+portConflictLine+"\"\n"),
			errors.New("exit status 1"), roots, "", "no exact",
		},
		{
			"stale proof from another scope's server.log", pingFailure + diag(serverLogPath(otherRig), portConflictLine+"\n"),
			errors.New("exit status 1"), roots, "", "not this rig's",
		},
		{
			"proof only in proxy.log", pingFailure + diag(filepath.Join(rig, ".beads", "dolt", "proxy.log"), portConflictLine+"\n"),
			errors.New("exit status 1"), roots, "", "no server.log",
		},
		{
			"unavailable server.log", pingFailure + "--- bd init diagnostics: " + serverLogPath(rig) + " (unavailable: not a readable file) ---\n--- end bd init diagnostics: " + serverLogPath(rig) + " ---\n",
			errors.New("exit status 1"), roots, "", "no exact",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := portCollisionProof(tc.out, tc.err, tc.roots)
			if tc.wantErr == "" {
				if err != nil || proof != tc.wantProof {
					t.Fatalf("portCollisionProof = (%q, %v), want (%q, nil)", proof, err, tc.wantProof)
				}
				return
			}
			if err == nil || proof != "" {
				t.Fatalf("portCollisionProof = (%q, %v), want an error containing %q", proof, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Only the fresh, default, local, bd-owned proxied shape may recover.
func TestRigAddRecoveryEligibility(t *testing.T) {
	for _, topo := range BeadsTopologies() {
		t.Run(topo.Name, func(t *testing.T) {
			err := recoverableShape(topo, nil)
			if topo.Name == "M1-proxied-local" {
				if err != nil {
					t.Fatalf("M1 must be recoverable: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s is not the fresh local managed proxied shape and must not recover", topo.Name)
			}
		})
	}

	var m1 BeadsTopology
	for _, topo := range BeadsTopologies() {
		if topo.Name == "M1-proxied-local" {
			m1 = topo
		}
	}
	for _, env := range [][]string{
		{"BEADS_DOLT_SERVER_HOST=10.0.0.5"},
		{"BEADS_DOLT_SERVER_PORT=3307"},
		{"BD_DOLT_PORT=3307"},
		{"GC_DOLT_PORT=3307"},
		{"GC_DOLT_HOST=db.example"},
	} {
		if err := recoverableShape(m1, env); err == nil {
			t.Errorf("an operator-pinned endpoint %v must not recover", env)
		}
	}
	if err := recoverableShape(m1, []string{"GC_DOLT=skip", "BD_DOLT_SHARED_SERVER=false", "PATH=/bin"}); err != nil {
		t.Errorf("harness switches are not endpoints: %v", err)
	}
	pinned := m1
	pinned.Env = map[string]string{"BEADS_DOLT_SERVER_HOST": "10.0.0.5"}
	if err := recoverableShape(pinned, nil); err == nil {
		t.Error("a topology that pins an endpoint in its own Env must not recover")
	}
}

type addStep struct {
	out string
	err error
}

// recoveryHarness drives RigAddRecovery with scripted attempts and a scripted
// view of the rig's .beads.
type recoveryHarness struct {
	rig      string
	steps    []addStep
	calls    int
	existing map[string]bool
	// afterAdd, when set, replaces the store state after attempt n (1-based).
	afterAdd map[int]map[string]bool
	logs     []string
}

func (h *recoveryHarness) run(topo BeadsTopology) (string, error) {
	rec := RigAddRecovery{
		Topology: topo,
		RigDir:   h.rig,
		Add: func() (string, error) {
			if h.calls >= len(h.steps) {
				return "", errors.New("unscripted extra gc rig add")
			}
			s := h.steps[h.calls]
			h.calls++
			if next, ok := h.afterAdd[h.calls]; ok {
				h.existing = next
			}
			return s.out, s.err
		},
		StoreExists: func(path string) bool { return h.existing[path] },
		Logf:        func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
	}
	return rec.Run()
}

func m1Topology(t *testing.T) BeadsTopology {
	t.Helper()
	for _, topo := range BeadsTopologies() {
		if topo.Name == "M1-proxied-local" {
			return topo
		}
	}
	t.Fatal("no M1 topology")
	return BeadsTopology{}
}

func TestRigAddRecoveryPolicy(t *testing.T) {
	rig := "/work/rig"
	store := filepath.Join(rig, ".beads")
	proof := pingFailure + diag(serverLogPath(rig), portConflictLine+"\n")
	timeoutProof := timeoutFailure + diag(serverLogPath(rig), portConflictLine+"\n")
	fail := errors.New("exit status 1")
	ok := addStep{out: "added rig\n"}

	t.Run("a first-attempt success never recovers", func(t *testing.T) {
		h := &recoveryHarness{rig: rig, steps: []addStep{ok}}
		out, err := h.run(m1Topology(t))
		if err != nil || out != "added rig\n" || h.calls != 1 || len(h.logs) != 0 {
			t.Fatalf("out=%q err=%v calls=%d logs=%v", out, err, h.calls, h.logs)
		}
	})

	for name, first := range map[string]addStep{
		"ping with proof":    {out: proof, err: fail},
		"timeout with proof": {out: timeoutProof, err: fail},
	} {
		t.Run(name+" recovers exactly once", func(t *testing.T) {
			h := &recoveryHarness{rig: rig, steps: []addStep{first, ok}}
			out, err := h.run(m1Topology(t))
			if err != nil || out != "added rig\n" {
				t.Fatalf("out=%q err=%v, want the second attempt's success", out, err)
			}
			if h.calls != 2 {
				t.Fatalf("gc rig add ran %d times, want exactly 2", h.calls)
			}
			joined := strings.Join(h.logs, "\n")
			for _, want := range []string{portConflictLine, "exit status 1", "recover"} {
				if !strings.Contains(joined, want) {
					t.Errorf("recovery log does not retain %q:\n%s", want, joined)
				}
			}
		})
	}

	t.Run("a second collision is the failure and is not retried", func(t *testing.T) {
		h := &recoveryHarness{rig: rig, steps: []addStep{{out: proof, err: fail}, {out: proof + "second\n", err: fail}, ok}}
		out, err := h.run(m1Topology(t))
		if err == nil || !strings.HasSuffix(out, "second\n") {
			t.Fatalf("out=%q err=%v, want the second failure", out, err)
		}
		if h.calls != 2 {
			t.Fatalf("gc rig add ran %d times, want 2 (one retry only)", h.calls)
		}
	})

	for _, tc := range []struct {
		name     string
		first    addStep
		existing map[string]bool
		after    map[int]map[string]bool
	}{
		{"missing proof", addStep{out: pingFailure, err: fail}, nil, nil},
		{"bare refused", addStep{out: "Error: connection refused\n", err: fail}, nil, nil},
		{"wrong proof", addStep{out: pingFailure + diag(serverLogPath(rig), "Port 1 already in use\n"), err: fail}, nil, nil},
		{"stale proof", addStep{out: pingFailure + diag(serverLogPath("/work/other"), portConflictLine+"\n"), err: fail}, nil, nil},
		{"unrelated error", addStep{out: "gc rig add: rig name taken\n", err: fail}, nil, nil},
		{"pre-existing store", addStep{out: proof, err: fail}, map[string]bool{store: true}, nil},
		{"incomplete rollback", addStep{out: proof, err: fail}, nil, map[int]map[string]bool{1: {store: true}}},
	} {
		t.Run(tc.name+" fails without another attempt", func(t *testing.T) {
			h := &recoveryHarness{rig: rig, steps: []addStep{tc.first, ok}, existing: tc.existing, afterAdd: tc.after}
			out, err := h.run(m1Topology(t))
			if err == nil || out != tc.first.out {
				t.Fatalf("out=%q err=%v, want the first failure returned unchanged", out, err)
			}
			if h.calls != 1 {
				t.Fatalf("gc rig add ran %d times, want 1", h.calls)
			}
		})
	}

	t.Run("non-M1 shapes fail without another attempt", func(t *testing.T) {
		for _, topo := range BeadsTopologies() {
			if topo.Name == "M1-proxied-local" {
				continue
			}
			h := &recoveryHarness{rig: rig, steps: []addStep{{out: proof, err: fail}, ok}}
			if _, err := h.run(topo); err == nil || h.calls != 1 {
				t.Errorf("%s: err=%v calls=%d, want the failure after one attempt", topo.Name, err, h.calls)
			}
		}
	})
}
