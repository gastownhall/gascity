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

// TestRemoteCapabilityRequirementsPinnedToDesign pins the class requirement
// table to DESIGN.md's (mc-http-switch, "Class requirement table"), plus the
// two rows this slice proved REQUIRED because gc has no fallback for them
// (issues.related: DepList's dependents leg; stats.get: Ping). Deleting a
// required row, demoting it to optional, or slipping an unlisted row into
// the table fails here, so the table can only move with the design.
func TestRemoteCapabilityRequirementsPinnedToDesign(t *testing.T) {
	want := map[string]RemoteCapabilityClass{
		"issues.get":          RemoteCapabilityRequired,
		"issues.list":         RemoteCapabilityRequired,
		"issues.query":        RemoteCapabilityRequired,
		"issues.count":        RemoteCapabilityRequired,
		"issues.create":       RemoteCapabilityRequired,
		"issues.update":       RemoteCapabilityRequired,
		"issues.close":        RemoteCapabilityRequired,
		"issues.reopen":       RemoteCapabilityRequired,
		"issues.delete":       RemoteCapabilityRequired,
		"issues.claim":        RemoteCapabilityRequired,
		"issues.release":      RemoteCapabilityRequired,
		"issues.casMetadata":  RemoteCapabilityRequired,
		"issues.batchApply":   RemoteCapabilityRequired,
		"issues.batchClose":   RemoteCapabilityRequired,
		"ready.list":          RemoteCapabilityRequired,
		"ready.count":         RemoteCapabilityRequired,
		"dependencies.add":    RemoteCapabilityRequired,
		"dependencies.remove": RemoteCapabilityRequired,
		"dependencies.list":   RemoteCapabilityRequired,
		"config.get":          RemoteCapabilityRequired,
		"issues.related":      RemoteCapabilityRequired,
		"stats.get":           RemoteCapabilityRequired,

		"issues.batchGet":        RemoteCapabilityOptional,
		"issues.count.scope":     RemoteCapabilityOptional,
		"issues.batchApplyLarge": RemoteCapabilityOptional,
		"issues.reclaim":         RemoteCapabilityOptional,
		"issues.update.claim":    RemoteCapabilityOptional,
	}
	got := map[string]RemoteCapabilityRequirement{}
	for _, row := range RemoteCapabilityRequirements() {
		if _, dup := got[row.Token]; dup {
			t.Errorf("token %q appears twice in the requirement table", row.Token)
		}
		got[row.Token] = row
	}
	for token, class := range want {
		row, ok := got[token]
		switch {
		case !ok:
			t.Errorf("design row %q (%s) is missing from the requirement table", token, class)
		case row.Class != class:
			t.Errorf("token %q is %s, design says %s", token, row.Class, class)
		case row.Class == RemoteCapabilityOptional && strings.TrimSpace(row.Fallback) == "":
			t.Errorf("optional token %q records no fallback", token)
		}
	}
	for token := range got {
		if _, ok := want[token]; !ok {
			t.Errorf("token %q is in the requirement table but not in the design's", token)
		}
	}
}

// TestEvaluateWireCompatFailsEachMissingRequiredToken: every required row
// FAILs wire_compat by name when the server does not advertise it, and every
// optional row only WARNs. A row whose class the check ignored would pass
// here no matter what the table said.
func TestEvaluateWireCompatFailsEachMissingRequiredToken(t *testing.T) {
	for _, row := range RemoteCapabilityRequirements() {
		handshake := remoteTestHandshake()
		handshake.Snapshot.Capabilities = remoteTestCapabilities(row.Token)
		got := EvaluateWireCompat("examplehttp", "", handshake, nil)
		want := PreflightCheckFail
		if row.Class == RemoteCapabilityOptional {
			want = PreflightCheckWarn
		}
		if got.State != want || !strings.Contains(got.Summary, row.Token) {
			t.Errorf("missing %s token %q: state %s summary %q, want %s naming it", row.Class, row.Token, got.State, got.Summary, want)
		}
	}
}

// TestLoadMetadataStateAcceptsARegisteredRemoteBackend: the metadata shape
// `bd connect` and bdhttp.Attach write (the backend selection alone, the
// server pinned in the sidecar) parses once the linked beads library has the
// backend registered as remote, and is refused by name while it is not. The
// loader asks the registry; it names no backend.
func TestLoadMetadataStateAcceptsARegisteredRemoteBackend(t *testing.T) {
	const name = "gcloadfakeremote"
	fs := fsys.NewFake()
	fs.Files["/scope/.beads/metadata.json"] = []byte(`{"backend":"` + name + `","database":"beads"}`)

	if _, _, err := LoadMetadataState(fs, "/scope/.beads/metadata.json"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("unregistered remote backend: error = %v, want ErrUnknownBackend", err)
	}

	open := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("opens nothing")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: open, OpenReadOnly: open, WorkspaceIsBeadsDir: true, Remote: true})
	t.Cleanup(func() { beadsbackend.Deregister(name) })

	state, ok, err := LoadMetadataState(fs, "/scope/.beads/metadata.json")
	if err != nil || !ok {
		t.Fatalf("registered remote backend: LoadMetadataState = (%v, %v), want accepted", ok, err)
	}
	if state.Backend != name {
		t.Fatalf("Backend = %q, want %q", state.Backend, name)
	}

	// A registered backend that is NOT remote is still refused: the loader's
	// widening is for remote backends only.
	const local = "gcloadfakelocal"
	beadsbackend.Register(local, beadsbackend.Backend{Open: open, OpenReadOnly: open})
	t.Cleanup(func() { beadsbackend.Deregister(local) })
	fs.Files["/local/.beads/metadata.json"] = []byte(`{"backend":"` + local + `"}`)
	if _, _, err := LoadMetadataState(fs, "/local/.beads/metadata.json"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("registered non-remote backend: error = %v, want ErrUnknownBackend", err)
	}
}
