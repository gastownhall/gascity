package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// slingHandoffCase builds the hand-off scenario: a task bead that already
// carries one live attached molecule, slung to an agent whose
// default_sling_formula implies a formula attach. `assignee` is the ONLY thing
// callers vary by default, so any difference in outcome is attributable to it
// alone; the optional agentMut hooks exist solely for the pool case, where the
// target's routing identity is itself the variable under test.
//
// Both the bead and its molecule are seeded into deps.Store as well as the
// child querier: CheckNoMoleculeChildren discovers the attachment through the
// querier, but CloseAttachedSubtree burns it through the store, so a
// store-only-holds-the-parent setup makes the burn fail for a reason that has
// nothing to do with the behavior under test.
func slingHandoffCase(t *testing.T, beadID, moleculeID, assignee string, agentMut ...func(*config.Agent)) (slingOpts, slingDeps, *fakeChildQuerier, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	// The target carries a default_sling_formula, so the formula attach is
	// IMPLIED rather than requested. That is what enables the silent
	// fallback-to-plain-routing path; an explicit --on hard-fails instead
	// (see TestOnFormulaExistingMoleculeErrors).
	a := config.Agent{
		Name:                "builder",
		MaxActiveSessions:   intPtr(1),
		DefaultSlingFormula: strPtr("code-review"),
	}
	for _, mut := range agentMut {
		mut(&a)
	}

	parent := beads.Bead{
		ID: beadID, Title: beadID, Type: "task", Status: "open",
		Assignee: assignee, Metadata: map[string]string{},
	}
	mol := beads.Bead{
		ID: moleculeID, Title: moleculeID, Type: "molecule", Status: "open",
		ParentID: beadID, Metadata: map[string]string{},
	}

	q := newFakeChildQuerier()
	q.beadsByID[beadID] = parent
	q.childrenOf[beadID] = []beads.Bead{mol}

	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{parent, mol}, nil)

	return testOpts(a, beadID), deps, q, stdout, stderr
}

// TestDefaultFormulaFallbackOnAssignedBeadIsSilentlyUndeliverable reproduces
// gm-2kyaqy: gascity/reviewer slung ga-a6xcq1 to gascity/builder at
// 2026-09-15T02:18:14Z, the command printed "Slung ga-a6xcq1 → gascity/builder"
// and exited 0, and the bead was never picked up — it sat in_progress, assigned
// to gascity/reviewer, with no molecule, for ~4h.
//
// The mechanism, from the reviewer's own captured stderr:
//
//	warning: bead ga-a6xcq1 already assigned to "gascity/reviewer"
//	skipped attaching default formula "mol-tdd-build" on ga-a6xcq1: bead
//	  ga-a6xcq1 already has attached molecule ga-23ecf3; routed as a plain
//	  bead instead
//	Slung ga-a6xcq1 → gascity/builder
//
// checkNoMoleculeChildren (internal/sling/sling_attachment.go:304,330) only
// auto-burns a stale attachment when the parent is UNASSIGNED. That gate is
// deliberate — burning a live worker's molecule would destroy in-flight work,
// and sling_test.go:3286 pins it. But when it declines, the implicit
// default-formula path (sling_core.go:618-629) falls back to plain routing and
// returns a nil error, so the sling reports success having produced a state no
// consumer can reach: assigned to a non-target, and with no molecule to drive
// the target.
//
// The contrasting success 3.5h later (ga-cks4ct, 05:49:06Z) differed in exactly
// one respect — the bead was unassigned, so the same stale molecule WAS burned
// ("Auto-burned stale molecule ga-8c6ce3") and mol-tdd-build attached normally.
func TestDefaultFormulaFallbackOnAssignedBeadIsSilentlyUndeliverable(t *testing.T) {
	const (
		beadID      = "BL-42"
		holder      = "gascity/reviewer"
		moleculeID  = "MOL-1"
		targetAgent = "builder"
	)

	// The bead is still claimed by the agent doing the slinging, and still
	// carries that agent's own live molecule — exactly the reviewer's state at
	// hand-off time.
	opts, deps, q, stdout, stderr := slingHandoffCase(t, beadID, moleculeID, holder)
	code := doSling(opts, deps, q, stdout, stderr)

	errText := stderr.String()
	outText := stdout.String()

	// --- Part 1: the observed production behavior (passes today). ----------
	// Documents the repro so a future reader can see the shape without
	// re-deriving it from transcripts.
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 — repro requires the silent-success path; stderr=%s", code, errText)
	}
	if !strings.Contains(errText, "already has attached molecule "+moleculeID) {
		t.Fatalf("stderr = %q, want the attachment conflict that triggers the fallback", errText)
	}
	if !strings.Contains(errText, "routed as a plain bead instead") {
		t.Fatalf("stderr = %q, want the plain-bead fallback", errText)
	}
	if strings.Contains(errText, "Auto-burned stale molecule") {
		t.Fatalf("stderr = %q, an assigned parent must NOT have its molecule burned", errText)
	}

	// --- Part 2: the invariant that is violated (fails today = RED). -------
	// The bead is now routed to `targetAgent` but is still assigned to
	// `holder` and has no molecule. No query the target runs can reach it:
	// `bd ready` excludes assigned/in_progress work, and there is no wisp to
	// drive it. A sling that ends here has not delivered anything, so it must
	// not report unqualified success.
	//
	// Either signal satisfies the invariant:
	//   (a) a non-zero exit code, or
	//   (b) an explicit warning naming the blocking assignee and the fact that
	//       the target cannot see the bead.
	strandedWarning := strings.Contains(errText, "undeliverable") ||
		strings.Contains(errText, "will not be picked up") ||
		strings.Contains(errText, "still assigned to")

	if code == 0 && !strandedWarning {
		t.Errorf(`sling reported success on an undeliverable outcome.

bead %s is now routed to %q but remains assigned to %q with no molecule
attached; nothing the target queries can return it.

exit code = %d (want non-zero, or a warning naming the blocking assignee)
stdout    = %q
stderr    = %q

Want one of: a non-zero exit, or stderr naming the bead as undeliverable /
"still assigned to %s" / "will not be picked up".`,
			beadID, targetAgent, holder, code, outText, errText, holder)
	}
}

// TestDefaultFormulaFallbackControlUnassignedBeadAttaches is the control for
// the test above: identical setup, identical live molecule, ONLY the assignee
// differs. It passes on current code and must keep passing.
//
// This isolates the assignee as the single differentiator, and disproves the
// hypothesis that the reviewer's own still-live mol-code-review convoy is what
// blocked the hand-off. A live attached molecule is present in BOTH the failing
// and the succeeding case — in production, ga-23ecf3 was still open 65s after
// the failing sling and ga-8c6ce3 was still open 13s after the succeeding one —
// so "a live convoy was attached" cannot be the cause. Being assigned is.
//
// It also rules out the second candidate, an idempotent-skip in
// resolveConvoyRecovery: that path prints "already routed … skipping
// (idempotent)" and creates nothing, whereas both the failing production sling
// and the failing test above ran to completion, minted an Auto-convoy, and
// printed "Slung … → …".
func TestDefaultFormulaFallbackControlUnassignedBeadAttaches(t *testing.T) {
	const (
		beadID     = "BL-43"
		moleculeID = "MOL-2"
	)

	// Byte-identical to the failing case except for the empty assignee.
	opts, deps, q, stdout, stderr := slingHandoffCase(t, beadID, moleculeID, "")
	code := doSling(opts, deps, q, stdout, stderr)

	errText := stderr.String()
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr=%s", code, errText)
	}
	if !strings.Contains(errText, "Auto-burned stale molecule "+moleculeID) {
		t.Errorf("stderr = %q, want the stale molecule auto-burned on an unassigned parent", errText)
	}
	if strings.Contains(errText, "routed as a plain bead instead") {
		t.Errorf("stderr = %q, an unassigned parent must not fall back to plain routing", errText)
	}
}

// TestDefaultFormulaFallbackPoolSessionClaimIsNotUndeliverable guards the
// identity the undeliverable warning compares against. A pool target routes
// under its POOL name (agentutil.RoutedToIdentity collapses a pool instance
// back to its template), and a pool session claims work as "<pool>-<id>", so a
// bead held by one of the target pool's own sessions IS deliverable -- the
// claim belongs to the target, not to a third party. Comparing against
// QualifiedName() instead would read "builders-1" as a foreign holder and warn
// spuriously on every pool hand-off. CheckBeadState
// (internal/sling/sling_attachment.go) already makes exactly this carve-out.
//
// The molecule conflict is real either way, so its warning must still appear;
// only the undeliverable warning is suppressed.
func TestDefaultFormulaFallbackPoolSessionClaimIsNotUndeliverable(t *testing.T) {
	const (
		beadID     = "BL-44"
		moleculeID = "MOL-3"
		poolName   = "builders"
		holder     = poolName + "-1"
	)

	opts, deps, q, stdout, stderr := slingHandoffCase(t, beadID, moleculeID, holder, func(a *config.Agent) {
		a.PoolName = poolName
		a.MaxActiveSessions = intPtr(3)
	})
	code := doSling(opts, deps, q, stdout, stderr)

	errText := stderr.String()
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (the molecule conflict still falls back to plain routing); stderr=%s", code, errText)
	}
	if !strings.Contains(errText, "already has attached molecule "+moleculeID) {
		t.Fatalf("stderr = %q, want the attachment conflict that triggers the fallback", errText)
	}
	if !strings.Contains(errText, "routed as a plain bead instead") {
		t.Fatalf("stderr = %q, want the plain-bead fallback", errText)
	}

	if strings.Contains(errText, "undeliverable") || strings.Contains(errText, "still assigned to") {
		t.Errorf(`sling warned that %s is undeliverable, but %q is a session of the target pool %q.

A pool session's claim is the target's own claim, so the hand-off is
deliverable and the warning is spurious.

stderr = %q`, beadID, holder, poolName, errText)
	}
}
