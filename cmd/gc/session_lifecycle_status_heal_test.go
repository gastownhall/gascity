package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestSessionLifecycleStatusHealProductionWiring is a deliberately bounded
// cutover canary for the helper and its two production callers.
func TestSessionLifecycleStatusHealProductionWiring(t *testing.T) {
	const (
		applyName   = "applySessionLifecycleStatusHeal"
		plannerName = "planSessionLifecycleStatus"
		writerName  = "healStateWithRollbackInfo"
	)
	wantRefs := map[string]map[string]int{
		"session_lifecycle_status_heal.go": {
			applyName:   1, // declaration
			plannerName: 1, // direct call
			writerName:  1, // direct call
		},
		"session_reconciler.go": {
			applyName: 2, // the orphan and desired direct calls
		},
	}
	wantDirectCalls := map[string]map[string]int{
		"session_lifecycle_status_heal.go": {
			plannerName: 1,
			writerName:  1,
		},
		"session_reconciler.go": {
			applyName: 2,
		},
	}
	wantContexts := map[string]map[string]string{
		"sessionLifecycleStatusHealSiteOrphan": {
			"Site":              "sessionLifecycleStatusHealSiteOrphan",
			"RuntimeObserved":   "livenessErr == nil",
			"RuntimeAlive":      "providerAlive",
			"LoadedRevision":    "loadedRevisionByID[id]",
			"RollbackAvailable": "!storeQueryPartial",
		},
		"sessionLifecycleStatusHealSiteDesired": {
			"Site":              "sessionLifecycleStatusHealSiteDesired",
			"RuntimeObserved":   `sp != nil && strings.TrimSpace(name) != ""`,
			"RuntimeAlive":      "alive",
			"LoadedRevision":    "loadedRevisionByID[id]",
			"RollbackAvailable": "true",
		},
	}
	seenContexts := make(map[string]int, len(wantContexts))

	for _, filename := range []string{"session_lifecycle_status_heal.go", "session_reconciler.go"} {
		source, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filename, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		if filename == "session_lifecycle_status_heal.go" {
			helperCalls := make(map[string]int)
			helperDecls := 0
			for _, declaration := range parsed.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || fn.Name.Name != applyName {
					continue
				}
				helperDecls++
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					callee, ok := call.Fun.(*ast.Ident)
					if ok && (callee.Name == plannerName || callee.Name == writerName) {
						helperCalls[callee.Name]++
					}
					return true
				})
			}
			if helperDecls != 1 || !reflect.DeepEqual(helperCalls, wantDirectCalls[filename]) {
				t.Fatalf("%s helper declarations/calls = %d/%#v, want 1/%#v", filename, helperDecls, helperCalls, wantDirectCalls[filename])
			}
		}
		refs := make(map[string]int)
		directCalls := make(map[string]int)
		ast.Inspect(parsed, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok {
				switch ident.Name {
				case applyName, plannerName, writerName:
					refs[ident.Name]++
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch callee.Name {
			case applyName, plannerName, writerName:
				directCalls[callee.Name]++
			default:
				return true
			}
			if filename != "session_reconciler.go" || callee.Name != applyName {
				return true
			}
			if len(call.Args) != 7 {
				t.Fatalf("%s apply helper arguments = %d, want exact seven-argument wiring", filename, len(call.Args))
			}
			contextLiteral, ok := call.Args[2].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s apply helper argument 3 = %T, want sessionLifecycleStatusHealContext literal", filename, call.Args[2])
			}
			contextType, ok := contextLiteral.Type.(*ast.Ident)
			if !ok || contextType.Name != "sessionLifecycleStatusHealContext" {
				t.Fatalf("%s apply helper argument 3 type = %#v, want sessionLifecycleStatusHealContext", filename, contextLiteral.Type)
			}
			var remainingArgs []string
			for _, argument := range call.Args[3:] {
				var rendered bytes.Buffer
				if err := format.Node(&rendered, fset, argument); err != nil {
					t.Fatalf("format %s apply helper argument: %v", filename, err)
				}
				remainingArgs = append(remainingArgs, rendered.String())
			}
			wantRemainingArgs := []string{"sessFront", "clk", "startupTimeout", "reconcileOpts.statusComparisonObserver"}
			if !reflect.DeepEqual(remainingArgs, wantRemainingArgs) {
				t.Fatalf("%s apply helper trailing arguments = %#v, want %#v", filename, remainingArgs, wantRemainingArgs)
			}
			fields := make(map[string]string, len(contextLiteral.Elts))
			for _, element := range contextLiteral.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					t.Fatalf("%s context element = %T, want keyed field", filename, element)
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok {
					t.Fatalf("%s context key = %T, want identifier", filename, field.Key)
				}
				var rendered bytes.Buffer
				if err := format.Node(&rendered, fset, field.Value); err != nil {
					t.Fatalf("format %s context field %s: %v", filename, key.Name, err)
				}
				fields[key.Name] = rendered.String()
			}
			site := fields["Site"]
			want, ok := wantContexts[site]
			if !ok {
				t.Fatalf("%s context site = %q, want orphan or desired", filename, site)
			}
			if !reflect.DeepEqual(fields, want) {
				t.Fatalf("%s %s context = %#v, want %#v", filename, site, fields, want)
			}
			seenContexts[site]++
			return true
		})
		if !reflect.DeepEqual(refs, wantRefs[filename]) {
			t.Fatalf("%s identifier references = %#v, want exact declaration/direct-call refs %#v", filename, refs, wantRefs[filename])
		}
		if !reflect.DeepEqual(directCalls, wantDirectCalls[filename]) {
			t.Fatalf("%s direct calls = %#v, want %#v", filename, directCalls, wantDirectCalls[filename])
		}
	}
	for site := range wantContexts {
		if seenContexts[site] != 1 {
			t.Fatalf("%s context count = %d, want 1", site, seenContexts[site])
		}
	}
}

func TestApplySessionLifecycleStatusHealMatchesLegacyPatchAndFold(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		context        sessionLifecycleStatusHealContext
		startupTimeout time.Duration
		lastWokeAt     time.Duration
		wantPatch      sessionpkg.MetadataPatch
		wantClearLease bool
		wantCandidate  sessionLifecycleStatusPlan
	}{
		{
			name: "orphan stale creating with partial store inventory preserves lease",
			context: sessionLifecycleStatusHealContext{
				Site:              sessionLifecycleStatusHealSiteOrphan,
				RuntimeObserved:   true,
				RuntimeAlive:      false,
				RollbackAvailable: false,
			},
			wantPatch: sessionpkg.MetadataPatch{
				"state": string(sessionpkg.StateStartPending),
			},
			wantCandidate: sessionLifecycleStatusPlan{
				Outcome: sessionLifecycleStatusHeal,
				Reason:  sessionLifecycleStatusReasonHeal,
				Patch: sessionpkg.MetadataPatch{
					"state": string(sessionpkg.StateStartPending),
				},
			},
		},
		{
			name: "desired stale creating with complete store inventory rolls back",
			context: sessionLifecycleStatusHealContext{
				Site:              sessionLifecycleStatusHealSiteDesired,
				RuntimeObserved:   true,
				RuntimeAlive:      false,
				RollbackAvailable: true,
			},
			wantClearLease: true,
			wantPatch: sessionpkg.MetadataPatch{
				"continuation_reset_pending": "true",
				"pending_create_claim":       "",
				"pending_create_started_at":  "",
				"primed_at":                  "",
				"priming_attempted_at":       "",
				"prompt_hash":                "",
				"session_key":                "",
				"sleep_reason":               string(sessionpkg.SleepReasonRuntimeMissing),
				"started_config_hash":        "",
				"state":                      string(sessionpkg.StateAsleep),
			},
			wantCandidate: sessionLifecycleStatusPlan{
				Outcome: sessionLifecycleStatusHeal,
				Reason:  sessionLifecycleStatusReasonHeal,
				Patch: sessionpkg.MetadataPatch{
					"continuation_reset_pending": "true",
					"pending_create_claim":       "",
					"pending_create_started_at":  "",
					"primed_at":                  "",
					"priming_attempted_at":       "",
					"prompt_hash":                "",
					"session_key":                "",
					"sleep_reason":               string(sessionpkg.SleepReasonRuntimeMissing),
					"started_config_hash":        "",
					"state":                      string(sessionpkg.StateAsleep),
				},
			},
		},
		// #4944 completed the intent the orphan site's own rollbackAvailable
		// comment states — never apply HALF a pending-create rollback — by
		// keeping state and claim on one decision: while the configured Start
		// lease is in flight the row does not move at all, where this case
		// previously pinned the half-applied form (state→asleep with the lease
		// left behind). The rollback twin above is the other half of the pair:
		// once the lease expires the whole transition lands in one batch.
		{
			name: "desired configured startup timeout preserves in-flight lease",
			context: sessionLifecycleStatusHealContext{
				Site:              sessionLifecycleStatusHealSiteDesired,
				RuntimeObserved:   true,
				RuntimeAlive:      false,
				RollbackAvailable: true,
			},
			startupTimeout: 5 * time.Minute,
			lastWokeAt:     -90 * time.Second,
			wantPatch:      nil,
			wantCandidate: sessionLifecycleStatusPlan{
				Outcome: sessionLifecycleStatusNoop,
				Reason:  sessionLifecycleStatusReasonConverged,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]string{
				"state":                     string(sessionpkg.StateCreating),
				"pending_create_claim":      "true",
				"pending_create_started_at": now.Add(-20 * time.Minute).Format(time.RFC3339),
			}
			if tt.lastWokeAt != 0 {
				metadata["last_woke_at"] = now.Add(tt.lastWokeAt).Format(time.RFC3339)
			}
			info, front := statusHealFixture(t, "status-heal", now.Add(-20*time.Minute), metadata)
			tick := newReconcileTick([]sessionpkg.Info{info})
			clk := &clock.Fake{Time: now}
			var comparisons []sessionLifecycleStatusComparison

			got, err := applySessionLifecycleStatusHeal(tick, info.ID, tt.context, front, clk, tt.startupTimeout, func(comparison sessionLifecycleStatusComparison) {
				comparisons = append(comparisons, comparison)
			})
			if err != nil {
				t.Fatalf("apply status heal: %v", err)
			}
			if !sameSessionLifecycleStatusPatch(got, tt.wantPatch) {
				t.Fatalf("legacy patch = %#v, want %#v", got, tt.wantPatch)
			}
			for _, key := range []string{"pending_create_claim", "pending_create_started_at"} {
				value, exists := got[key]
				if tt.wantClearLease && (!exists || value != "") {
					t.Fatalf("legacy patch %q = %q (present %t), want explicit clear", key, value, exists)
				}
				if !tt.wantClearLease && exists {
					t.Fatalf("legacy patch unexpectedly contains %q: %#v", key, got)
				}
			}
			wantInfo := info.ApplyPatch(tt.wantPatch)
			if !reflect.DeepEqual(tick.infoByID[info.ID], wantInfo) {
				t.Fatalf("tick info = %#v, want input.ApplyPatch(patch) %#v", tick.infoByID[info.ID], wantInfo)
			}
			persisted, err := front.Get(info.ID)
			if err != nil {
				t.Fatalf("front.Get(%s): %v", info.ID, err)
			}
			if !reflect.DeepEqual(persisted, wantInfo) {
				t.Fatalf("persisted reread = %#v, want %#v", persisted, wantInfo)
			}
			if len(comparisons) != 1 {
				t.Fatalf("comparisons = %#v, want one", comparisons)
			}
			gotComparison := comparisons[0]
			tt.wantCandidate.SessionID = info.ID
			if gotComparison.Site != tt.context.Site || gotComparison.Outcome != sessionLifecycleStatusComparisonMatched || gotComparison.Reason != sessionLifecycleStatusComparisonReasonEquivalent || !reflect.DeepEqual(gotComparison.Candidate, tt.wantCandidate) || !reflect.DeepEqual(gotComparison.LegacyPatch, tt.wantPatch) {
				t.Fatalf("comparison = %#v, want site=%q matched/equivalent candidate=%#v legacy=%#v", gotComparison, tt.context.Site, tt.wantCandidate, tt.wantPatch)
			}
		})
	}
}

func TestApplySessionLifecycleStatusHealRejectsInvalidTickIdentity(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	bead := statusHealBead("status-heal-valid", now, map[string]string{
		"state": string(sessionpkg.StateAsleep),
	})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
	writer := &statusHealAttemptStore{Store: mem}
	front := sessionpkg.NewStore(beads.SessionStore{Store: writer})
	info, err := front.Get(bead.ID)
	if err != nil {
		t.Fatalf("front.Get(%s): %v", bead.ID, err)
	}

	tests := []struct {
		name            string
		tick            *reconcileTick
		request         string
		wantErrContains string
	}{
		{
			name:            "missing tick key",
			tick:            newReconcileTick(nil),
			request:         info.ID,
			wantErrContains: `session "status-heal-valid" missing from reconcile tick`,
		},
		{
			name: "mismatched info ID",
			tick: &reconcileTick{infoByID: map[string]sessionpkg.Info{info.ID: func() sessionpkg.Info {
				mismatched := info
				mismatched.ID = "status-heal-actual"
				return mismatched
			}()}},
			request:         info.ID,
			wantErrContains: `requested session ID "status-heal-valid", tick info ID "status-heal-actual"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeInfo, beforeExists := tt.tick.infoByID[tt.request]
			beforeLen := len(tt.tick.infoByID)
			observerCalls := 0

			patch, err := applySessionLifecycleStatusHeal(tt.tick, tt.request, sessionLifecycleStatusHealContext{
				Site:              sessionLifecycleStatusHealSiteDesired,
				RuntimeObserved:   true,
				RuntimeAlive:      true,
				RollbackAvailable: true,
			}, front, &clock.Fake{Time: now}, 0, func(sessionLifecycleStatusComparison) {
				observerCalls++
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErrContains)
			}
			if patch != nil {
				t.Fatalf("patch = %#v, want nil", patch)
			}
			if observerCalls != 0 {
				t.Fatalf("observer calls = %d, want 0", observerCalls)
			}
			if writer.attempts != 0 {
				t.Fatalf("writer attempts = %d, want 0", writer.attempts)
			}
			afterInfo, afterExists := tt.tick.infoByID[tt.request]
			if len(tt.tick.infoByID) != beforeLen || afterExists != beforeExists || !reflect.DeepEqual(afterInfo, beforeInfo) {
				t.Fatalf("tick map changed: len=%d entry=(%#v,%t), want len=%d entry=(%#v,%t)", len(tt.tick.infoByID), afterInfo, afterExists, beforeLen, beforeInfo, beforeExists)
			}
		})
	}
}

type statusHealAttemptStore struct {
	beads.Store
	attempts int
}

func (s *statusHealAttemptStore) SetMetadataBatch(id string, patch map[string]string) error {
	s.attempts++
	return s.Store.SetMetadataBatch(id, patch)
}

// TestApplySessionLifecycleStatusHealUnknownRuntimeKeepsLegacyWriter pins the
// ownership invariant applySessionLifecycleStatusHeal's doc comment states: the
// legacy write is authoritative until the rollout gate transfers ownership, so a
// PARKED candidate never suppresses it.
//
// The row is drained rather than the runtime-liveness row this used to seed.
// #5138 stopped the legacy writer hardcoding Runtime.Observed=true, so with the
// observation genuinely unknown the projection now returns the compat state for
// every runtime-driven row and there is nothing left to write — a fixture that
// can no longer tell "legacy ran" from "legacy was suppressed". The drained
// branch reads BaseState, which is metadata-derived and does not consult the
// observation, so it still exercises a real legacy write while the candidate
// parks on the same unknown observation.
func TestApplySessionLifecycleStatusHealUnknownRuntimeKeepsLegacyWriter(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	info, front := statusHealFixture(t, "status-heal-unknown", now, map[string]string{
		"state": string(sessionpkg.StateDrained),
	})
	tick := newReconcileTick([]sessionpkg.Info{info})
	var comparisons []sessionLifecycleStatusComparison

	patch, err := applySessionLifecycleStatusHeal(tick, info.ID, sessionLifecycleStatusHealContext{
		Site:              sessionLifecycleStatusHealSiteOrphan,
		RuntimeObserved:   false,
		RuntimeAlive:      false,
		RollbackAvailable: false,
	}, front, &clock.Fake{Time: now}, 0, func(comparison sessionLifecycleStatusComparison) {
		comparisons = append(comparisons, comparison)
	})
	if err != nil {
		t.Fatalf("apply unknown-runtime status heal: %v", err)
	}
	if patch["state"] != string(sessionpkg.StateAsleep) || patch["sleep_reason"] != string(sessionpkg.SleepReasonDrained) {
		t.Fatalf("legacy patch = %#v, want asleep/drained despite parked candidate", patch)
	}
	if tick.infoByID[info.ID].MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("tick state = %q, want asleep legacy patch folded", tick.infoByID[info.ID].MetadataState)
	}
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatalf("front.Get(%s): %v", info.ID, err)
	}
	if !reflect.DeepEqual(persisted, tick.infoByID[info.ID]) {
		t.Fatalf("persisted reread = %#v, want folded legacy asleep state %#v", persisted, tick.infoByID[info.ID])
	}
	if len(comparisons) != 1 {
		t.Fatalf("comparison count = %d, want 1", len(comparisons))
	}
	if comparisons[0].Candidate.Outcome != sessionLifecycleStatusPark ||
		comparisons[0].Outcome != sessionLifecycleStatusComparisonIncomparable ||
		comparisons[0].Reason != sessionLifecycleStatusComparisonReasonShadowParked {
		t.Fatalf("comparison = %+v, want parked/incomparable", comparisons[0])
	}
}

func TestApplySessionLifecycleStatusHealWriteFailureDoesNotFold(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	bead := statusHealBead("status-heal-write-failure", now, map[string]string{
		"state": string(sessionpkg.StateAsleep),
	})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
	writeErr := errors.New("apply then error")
	front := sessionpkg.NewStore(beads.SessionStore{Store: &applyThenErrorStatusHealStore{Store: mem, err: writeErr}})
	info, err := front.Get(bead.ID)
	if err != nil {
		t.Fatalf("front.Get(%s): %v", bead.ID, err)
	}
	tick := newReconcileTick([]sessionpkg.Info{info})
	before := tick.infoByID[info.ID]

	patch, err := applySessionLifecycleStatusHeal(tick, info.ID, sessionLifecycleStatusHealContext{
		Site:              sessionLifecycleStatusHealSiteDesired,
		RuntimeObserved:   true,
		RuntimeAlive:      true,
		RollbackAvailable: true,
	}, front, &clock.Fake{Time: now}, 0, nil)
	if err == nil {
		t.Fatal("apply status heal error = nil, want apply-then-error failure")
	}
	if patch != nil {
		t.Fatalf("patch = %#v, want nil after failed write", patch)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want %v", err, writeErr)
	}
	if !reflect.DeepEqual(tick.infoByID[info.ID], before) {
		t.Fatalf("tick projection advanced after failed write: got %#v, want %#v", tick.infoByID[info.ID], before)
	}
	persisted, getErr := front.Get(info.ID)
	if getErr != nil {
		t.Fatalf("front.Get(%s): %v", info.ID, getErr)
	}
	wantPersisted := before.ApplyPatch(sessionpkg.MetadataPatch{"state": string(sessionpkg.StateAwake)})
	if !reflect.DeepEqual(persisted, wantPersisted) {
		t.Fatalf("durable row = %#v, want committed legacy patch %#v", persisted, wantPersisted)
	}
}

func statusHealFixture(t *testing.T, id string, createdAt time.Time, metadata map[string]string) (sessionpkg.Info, *sessionpkg.Store) {
	t.Helper()
	front, _ := sessiontest.Store(t, statusHealBead(id, createdAt, metadata))
	info, err := front.Get(id)
	if err != nil {
		t.Fatalf("front.Get(%s): %v", id, err)
	}
	return info, front
}

func statusHealBead(id string, createdAt time.Time, metadata map[string]string) beads.Bead {
	clonedMetadata := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clonedMetadata[key] = value
	}
	return beads.Bead{
		ID:        id,
		Type:      sessionpkg.BeadType,
		Labels:    []string{sessionpkg.LabelSession},
		CreatedAt: createdAt,
		Metadata:  clonedMetadata,
	}
}

type applyThenErrorStatusHealStore struct {
	beads.Store
	err error
}

func (s *applyThenErrorStatusHealStore) SetMetadataBatch(id string, patch map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, patch); err != nil {
		return err
	}
	return s.err
}
