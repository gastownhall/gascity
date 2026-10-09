package contract

import (
	"context"
	"errors"
	"strings"
	"testing"

	beadsbackend "github.com/steveyegge/beads/backend"

	"github.com/gastownhall/gascity/internal/fsys"
)

func remoteTestCapabilities(drop ...string) []string {
	skip := map[string]bool{}
	for _, token := range drop {
		skip[token] = true
	}
	var tokens []string
	for _, row := range RemoteCapabilityRequirements() {
		if !skip[row.Token] {
			tokens = append(tokens, row.Token)
		}
	}
	return tokens
}

func remoteTestHandshake() PreflightWireHandshake {
	return PreflightWireHandshake{
		Snapshot:        PreflightWireSnapshot{APIVersion: "v0", BdVersion: "1.3.1", WireRevision: 2, ProjectID: "gc-remote", Capabilities: remoteTestCapabilities()},
		TargetProjectID: "gc-remote",
	}
}

// remoteTestChecker is a checker over a fake scope whose metadata names a
// remote backend. Every Dolt reader fails the test when called.
func remoteTestChecker(t *testing.T, handshake PreflightWireHandshake, handshakeErr error) (PreflightChecker, *int) {
	t.Helper()
	fs := fsys.NewFake()
	writeSkipTestMetadata(fs, "/city", `{"backend": "examplehttp", "project_id": "gc-remote"}`)
	dials := 0
	return PreflightChecker{
		FS:            fs,
		Provider:      "bd",
		RemoteBackend: func(backend string) bool { return backend == "examplehttp" },
		BDContext: func(string) (PreflightBDContext, error) {
			t.Fatal("bd context consulted on the remote route")
			return PreflightBDContext{}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			t.Fatal("database identity probed on the remote route")
			return "", false, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			t.Fatal("schema cursors probed on the remote route")
			return PreflightSchemaCursors{}, false, nil
		},
		WireHandshake: func(string) (PreflightWireHandshake, error) {
			dials++
			return handshake, handshakeErr
		},
	}, &dials
}

func checkByID(t *testing.T, result PreflightResult, id PreflightCheckID) PreflightCheckResult {
	t.Helper()
	for _, check := range result.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("no %s check in %+v", id, result.Checks)
	return PreflightCheckResult{}
}

func TestPreflightRemoteRouteTakesNoDoltProbeAndPasses(t *testing.T) {
	checker, dials := remoteTestChecker(t, remoteTestHandshake(), nil)
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Verdict != PreflightVerdictEligible || !result.NativeStoreEligible {
		t.Fatalf("verdict = %s, want ELIGIBLE: %+v", result.Verdict, result.Checks)
	}
	if *dials != 1 {
		t.Fatalf("handshakes = %d, want 1", *dials)
	}
	wantOrder := []PreflightCheckID{
		PreflightCheckProviderContract, PreflightCheckMetadataBackend, PreflightCheckBDContextAgreement,
		PreflightCheckDoltModeSafe, PreflightCheckIdentityMatch, PreflightCheckVersionCompat,
		PreflightCheckBDVersionHint, PreflightCheckContractShape, PreflightCheckWireCompat,
	}
	if len(result.Checks) != len(wantOrder) {
		t.Fatalf("checks = %d, want %d", len(result.Checks), len(wantOrder))
	}
	for i, id := range wantOrder {
		check := result.Checks[i]
		if check.ID != id {
			t.Fatalf("check %d = %s, want %s", i, check.ID, id)
		}
		if i >= 2 && i < 8 && !strings.Contains(check.Summary, "not consulted (remote backend)") {
			t.Errorf("%s summary = %q, want it reported as not consulted (remote backend)", id, check.Summary)
		}
	}
	if got := checkByID(t, result, PreflightCheckWireCompat).State; got != PreflightCheckPass {
		t.Fatalf("wire_compat = %s, want PASS", got)
	}
}

// TestPreflightRemoteProviderBlockSkipsHandshake: an already-blocked scope is
// not dialed.
func TestPreflightRemoteProviderBlockSkipsHandshake(t *testing.T) {
	checker, dials := remoteTestChecker(t, remoteTestHandshake(), nil)
	checker.Provider = "custom"
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Verdict != PreflightVerdictBlocked || *dials != 0 {
		t.Fatalf("verdict = %s, handshakes = %d; want BLOCKED with no dial", result.Verdict, *dials)
	}
}

// TestEvaluateWireCompatNamesTheGap pins the wire_compat rule: a FAIL names the
// missing required token, the revision, or the project; an absent optional
// token WARNs without degrading eligibility.
func TestEvaluateWireCompatNamesTheGap(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(*PreflightWireHandshake)
		err         error
		metadataPID string
		wantState   PreflightCheckState
		wantInText  string
	}{
		{name: "pass", mutate: func(*PreflightWireHandshake) {}, wantState: PreflightCheckPass},
		{name: "absent revision is zero and accepted", mutate: func(h *PreflightWireHandshake) { h.Snapshot.WireRevision = 0 }, wantState: PreflightCheckPass},
		{name: "required token missing", mutate: func(h *PreflightWireHandshake) {
			h.Snapshot.Capabilities = remoteTestCapabilities("issues.casMetadata")
		}, wantState: PreflightCheckFail, wantInText: "issues.casMetadata"},
		{name: "optional token missing", mutate: func(h *PreflightWireHandshake) {
			h.Snapshot.Capabilities = remoteTestCapabilities("issues.batchGet")
		}, wantState: PreflightCheckWarn, wantInText: "issues.batchGet"},
		{name: "revision too new", mutate: func(h *PreflightWireHandshake) { h.Snapshot.WireRevision = RemoteWireRevisionMax + 1 }, wantState: PreflightCheckFail, wantInText: "wire_revision"},
		{name: "revision negative", mutate: func(h *PreflightWireHandshake) { h.Snapshot.WireRevision = RemoteWireRevisionMin - 1 }, wantState: PreflightCheckFail, wantInText: "wire_revision"},
		{name: "project mismatch", mutate: func(h *PreflightWireHandshake) { h.Snapshot.ProjectID = "someone-else" }, wantState: PreflightCheckFail, wantInText: "someone-else"},
		{name: "metadata pin used when activation pins none", mutate: func(h *PreflightWireHandshake) { h.TargetProjectID = "" }, metadataPID: "gc-other", wantState: PreflightCheckFail, wantInText: "gc-other"},
		{name: "no pin at all", mutate: func(h *PreflightWireHandshake) { h.TargetProjectID = "" }, wantState: PreflightCheckFail, wantInText: "no project id"},
		{name: "typed handshake failure", mutate: func(*PreflightWireHandshake) {}, err: &PreflightWireError{Reason: PreflightWireUnauthenticated, Err: errors.New("401")}, wantState: PreflightCheckFail, wantInText: "unauthenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := remoteTestHandshake()
			tc.mutate(&h)
			got := EvaluateWireCompat("examplehttp", tc.metadataPID, h, tc.err)
			if got.ID != PreflightCheckWireCompat || got.State != tc.wantState {
				t.Fatalf("got %s %s (%q), want %s", got.ID, got.State, got.Summary, tc.wantState)
			}
			if tc.wantInText != "" && !strings.Contains(got.Summary, tc.wantInText) {
				t.Fatalf("summary %q does not name %q", got.Summary, tc.wantInText)
			}
		})
	}
}

// TestPreflightRemoteOptionalGapStaysEligible: an optional fallback is
// recorded, never a reason to refuse the native store.
func TestPreflightRemoteOptionalGapStaysEligible(t *testing.T) {
	h := remoteTestHandshake()
	h.Snapshot.Capabilities = remoteTestCapabilities("issues.reclaim", "issues.batchGet")
	checker, _ := remoteTestChecker(t, h, nil)
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.NativeStoreEligible || checkByID(t, result, PreflightCheckWireCompat).State != PreflightCheckWarn {
		t.Fatalf("verdict = %s, wire_compat = %+v; want ELIGIBLE with a WARN", result.Verdict, checkByID(t, result, PreflightCheckWireCompat))
	}
}

// TestPreflightRemoteRequiredGapBlocks: a required gap blocks and names the
// token as the fallback reason.
func TestPreflightRemoteRequiredGapBlocks(t *testing.T) {
	h := remoteTestHandshake()
	h.Snapshot.Capabilities = remoteTestCapabilities("ready.list")
	checker, _ := remoteTestChecker(t, h, nil)
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.NativeStoreEligible || !strings.Contains(result.FallbackReason, "ready.list") {
		t.Fatalf("verdict = %s reason = %q, want BLOCKED naming ready.list", result.Verdict, result.FallbackReason)
	}
	if len(result.RepairSteps) == 0 || result.RepairSteps[0].CheckID != PreflightCheckWireCompat {
		t.Fatalf("repair steps = %+v, want a wire_compat step", result.RepairSteps)
	}
}

// TestPreflightRemoteWithoutHandshakeReaderFails: an unverified server is not
// eligible.
func TestPreflightRemoteWithoutHandshakeReaderFails(t *testing.T) {
	checker, _ := remoteTestChecker(t, remoteTestHandshake(), nil)
	checker.WireHandshake = nil
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.NativeStoreEligible {
		t.Fatal("remote scope eligible without a handshake")
	}
}

// TestBackendIsRemoteReadsTheLibraryRegistry: the default classification is
// the beads registry's Remote bit, never a spelling.
func TestBackendIsRemoteReadsTheLibraryRegistry(t *testing.T) {
	const name = "gccontractfakeremote"
	if BackendIsRemote(name) {
		t.Fatal("unregistered backend reported remote")
	}
	open := func(context.Context, string) (beadsbackend.DoltStorage, error) { return nil, errors.New("no store") }
	beadsbackend.Register(name, beadsbackend.Backend{Open: open, OpenReadOnly: open, Remote: true})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
	if !BackendIsRemote(name) || !BackendIsRemote("  "+name+" ") {
		t.Fatal("registered remote backend not reported remote")
	}
	if BackendIsRemote("dolt") || BackendIsRemote("") {
		t.Fatal("dolt or unset reported remote")
	}
}

// TestPreflightRemoteHandshakeFailureBlocks: an unreachable or refusing server
// blocks, and the typed reason reaches the fallback text.
func TestPreflightRemoteHandshakeFailureBlocks(t *testing.T) {
	checker, dials := remoteTestChecker(t, PreflightWireHandshake{}, &PreflightWireError{Reason: PreflightWireProjectMismatch, Err: errors.New("server owns another project")})
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.NativeStoreEligible || *dials != 1 || !strings.Contains(result.FallbackReason, string(PreflightWireProjectMismatch)) {
		t.Fatalf("verdict = %s reason = %q dials = %d, want BLOCKED naming project_mismatch", result.Verdict, result.FallbackReason, *dials)
	}
}
