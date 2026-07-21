package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// scopedGateBead builds a shipped work bead whose flat gc.work_* keys are
// POISONED — the shape a pool session parked on a shared checkout produces.
func scopedGateBead(id, scopeRaw string) beads.Bead {
	return beads.Bead{
		ID:   id,
		Type: "task",
		Metadata: beads.StringMap{
			beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
			beadmeta.WorkCommitMetadataKey:  "cafef00d",
			beadmeta.WorkBranchMetadataKey:  "parked-shared-branch",
			beadmeta.WorkDirMetadataKey:     "/shared/parked/root",
			beadmeta.TargetScopeMetadataKey: scopeRaw,
		},
	}
}

func gateEnvelope(t *testing.T) (cityPath string, envelope targetscope.Envelope) {
	t.Helper()
	cityPath = t.TempDir()
	return cityPath, targetscope.Envelope{CityPath: cityPath, StorePath: cityPath}
}

// §11 #12 — the gate's git probe must receive the DECLARED branch and worktree,
// never the claim-stamped poison.
func TestCloseGateProbesDeclaredScopeNotPoisonedFlatKeys(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)
	declaredWorktree := filepath.Join(cityPath, "worktrees", "T")

	scope, err := targetscope.Marshal(targetscope.Scope{V: 1, Branch: "release", Worktree: declaredWorktree})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	bead := scopedGateBead("wr-scoped", scope)
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)

	repoDir, declaredBranch, violation := workRecordCloseLocation(store, bead, cityPath, envelope)

	if violation != "" {
		t.Fatalf("unexpected scope violation: %s", violation)
	}
	if declaredBranch != "release" {
		t.Fatalf("declaredBranch = %q, want release (not the parked flat value)", declaredBranch)
	}
	if repoDir != declaredWorktree {
		t.Fatalf("repoDir = %q, want the declared worktree %q (not the shared parked root)", repoDir, declaredWorktree)
	}
}

// The probe the validator actually calls must carry the declared branch, and
// the violation message must name it — otherwise an operator reading the gate
// output would go looking on the wrong branch.
func TestCloseGateValidatesCommitAgainstDeclaredBranch(t *testing.T) {
	var probedBranch string
	violations := validateWorkRecordOnClose(
		scopedGateBead("wr-scoped", ""),
		"release",
		func(_, branch string) bool {
			probedBranch = branch
			return false
		},
	)

	if probedBranch != "release" {
		t.Fatalf("probed branch = %q, want the declared release", probedBranch)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly the unreachable-commit violation", violations)
	}
	if got := violations[0]; !strings.Contains(got, beadmeta.TargetScopeMetadataKey) || !strings.Contains(got, "release") {
		t.Fatalf("violation %q should name the declared scope branch it probed", got)
	}
}

// §11 #14(iv) — an ESCAPING worktree makes the scoped read fail. The gate must
// report a violation, NOT silently fall back to the flat keys: falling back is
// the defect reopening.
func TestCloseGateRefusesFlatFallbackWhenScopeEscapesEnvelope(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)

	scope, err := targetscope.Marshal(targetscope.Scope{V: 1, Branch: "release", Worktree: "/etc"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	bead := scopedGateBead("wr-escape", scope)
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)

	repoDir, declaredBranch, violation := workRecordCloseLocation(store, bead, cityPath, envelope)

	if violation == "" {
		t.Fatal("an escaping worktree must be a violation, not a silent flat-key fallback")
	}
	if repoDir == "/shared/parked/root" || declaredBranch == "parked-shared-branch" {
		t.Fatalf("gate fell back to the poisoned flat keys: repoDir=%q branch=%q", repoDir, declaredBranch)
	}
}

// A corrupt scope object is likewise a failed scoped read, never permission to
// use the claim-time values.
func TestCloseGateRefusesFlatFallbackWhenScopeIsCorrupt(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)
	bead := scopedGateBead("wr-corrupt", "{not json")
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)

	_, _, violation := workRecordCloseLocation(store, bead, cityPath, envelope)

	if violation == "" {
		t.Fatal("a corrupt gc.target_scope must be a violation, not a silent flat-key fallback")
	}
}

// §13(b) — a valid but field-empty scope means "declared unknown". The gate must
// still not reach for the flat keys; the process's own scopeRoot is the fallback.
func TestCloseGateUnknownScopeDoesNotFallBackToFlatKeys(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)

	scope, err := targetscope.Marshal(targetscope.Unknown())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	bead := scopedGateBead("wr-unknown", scope)
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)

	repoDir, declaredBranch, violation := workRecordCloseLocation(store, bead, cityPath, envelope)

	if violation != "" {
		t.Fatalf("unknown is a legitimate declared value, not a violation: %s", violation)
	}
	if repoDir != cityPath {
		t.Fatalf("repoDir = %q, want the scopeRoot %q — never the poisoned flat work_dir", repoDir, cityPath)
	}
	if declaredBranch != "" {
		t.Fatalf("declaredBranch = %q, want empty for an unknown scope", declaredBranch)
	}
}

// Legacy behavior is preserved exactly: with no scope anywhere, the gate reads
// the flat keys as it always did.
func TestCloseGateAbsentScopeKeepsLegacyFlatRead(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)
	bead := scopedGateBead("wr-legacy", "")
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)

	repoDir, declaredBranch, violation := workRecordCloseLocation(store, bead, cityPath, envelope)

	if violation != "" {
		t.Fatalf("absent scope must not be a violation: %s", violation)
	}
	if repoDir != "/shared/parked/root" {
		t.Fatalf("repoDir = %q, want the flat gc.work_dir (legacy path unchanged)", repoDir)
	}
	if declaredBranch != "" {
		t.Fatalf("declaredBranch = %q, want empty so the validator uses the flat branch", declaredBranch)
	}
}

// §11 #5 — a formula stage carries only gc.root_bead_id. The scope governing it
// lives on the root, so the gate must reach it through the inherited walk.
func TestCloseGateResolvesScopeInheritedFromRoot(t *testing.T) {
	cityPath, envelope := gateEnvelope(t)

	scope, err := targetscope.Marshal(targetscope.Scope{V: 1, Branch: "release"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	root := beads.Bead{ID: "root-1", Type: "task", Metadata: beads.StringMap{
		beadmeta.TargetScopeMetadataKey: scope,
	}}
	stage := scopedGateBead("wr-stage", "")
	stage.Metadata[beadmeta.RootBeadIDMetadataKey] = "root-1"
	store := beads.NewMemStoreFrom(1, []beads.Bead{root, stage}, nil)

	_, declaredBranch, violation := workRecordCloseLocation(store, stage, cityPath, envelope)

	if violation != "" {
		t.Fatalf("unexpected violation: %s", violation)
	}
	if declaredBranch != "release" {
		t.Fatalf("declaredBranch = %q, want release inherited from the root", declaredBranch)
	}
}
