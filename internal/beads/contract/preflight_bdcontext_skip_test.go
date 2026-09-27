package contract

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	skipTestPostgresMetadata = `{"backend": "postgres", "project_id": "gc-rig"}`
	skipTestDoltMetadata     = `{"backend": "dolt", "dolt_mode": "server", "dolt_database": "db", "project_id": "gc-rig"}`
)

// countingBDContext returns a bd-context reader that counts invocations per
// scope and answers with a healthy dolt server context.
func countingBDContext(calls map[string]int) func(string) (PreflightBDContext, error) {
	return func(scope string) (PreflightBDContext, error) {
		calls[scope]++
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4", SchemaVersion: 1}, nil
	}
}

func skipTestChecker(fs *fsys.Fake, provider string, bdContext func(string) (PreflightBDContext, error)) PreflightChecker {
	return PreflightChecker{
		FS:                  fs,
		Provider:            provider,
		BeadsLibraryVersion: "1.0.4",
		BDContext:           bdContext,
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-rig", true, nil
		},
	}
}

func writeSkipTestMetadata(fs *fsys.Fake, scope, metadata string) {
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(metadata)
}

// TestPreflightBDContextSubprocessCountPerScopeSet pins the production shape
// that motivated skipping bd context: a postgres city with six dolt rigs.
// Before the skip every scope ran `bd context --json` (7 subprocesses); the
// postgres city is BLOCKED by metadata_backend whatever bd says, so only the
// six rigs whose verdict bd context can still change consult it.
func TestPreflightBDContextSubprocessCountPerScopeSet(t *testing.T) {
	fs := fsys.NewFake()
	scopes := []string{"/city"}
	writeSkipTestMetadata(fs, "/city", skipTestPostgresMetadata)
	for i := 0; i < 6; i++ {
		rig := fmt.Sprintf("/rigs/r%d", i)
		writeSkipTestMetadata(fs, rig, skipTestDoltMetadata)
		scopes = append(scopes, rig)
	}
	calls := map[string]int{}
	checker := skipTestChecker(fs, "bd", countingBDContext(calls))

	for _, scope := range scopes {
		result, err := checker.Check(scope)
		if err != nil {
			t.Fatalf("Check(%s) error = %v", scope, err)
		}
		assertCheckOrder(t, result)
		wantEligible := scope != "/city"
		if result.NativeStoreEligible != wantEligible {
			t.Fatalf("Check(%s) eligible = %v, want %v; checks=%+v", scope, result.NativeStoreEligible, wantEligible, result.Checks)
		}
	}

	total := 0
	for _, n := range calls {
		total += n
	}
	if total != 6 {
		t.Fatalf("bd context invocations = %d (%v), want 6 (one per dolt rig, none for the blocked postgres city)", total, calls)
	}
	if calls["/city"] != 0 {
		t.Fatalf("bd context ran %d time(s) for the already-blocked postgres city", calls["/city"])
	}
}

// TestPreflightNonBDProviderDoesNotConsultBDContext covers the other blocker
// that precedes bd context: a provider without the bd contract.
func TestPreflightNonBDProviderDoesNotConsultBDContext(t *testing.T) {
	fs := fsys.NewFake()
	writeSkipTestMetadata(fs, "/city", skipTestDoltMetadata)
	calls := map[string]int{}
	result, err := skipTestChecker(fs, "file", countingBDContext(calls)).Check("/city")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckProviderContract, PreflightCheckFail)
	if calls["/city"] != 0 {
		t.Fatalf("bd context invocations = %d, want 0 for a non-bd provider", calls["/city"])
	}
}

// TestPreflightBlockedOutcomeIndependentOfBDContext is the safety argument
// for the skip: for a scope metadata_backend already blocks, every answer bd
// context could give — agreement, disagreement, embedded mode, version skew,
// missing schema, or an error — yields the same verdict, gate (first FAIL)
// and fallback reason as not asking at all. (Run against the pre-skip code,
// every verdict/gate/reason assertion here passes; only the repair-step
// assertion differs, because the old code also listed bd-context repair
// steps for a scope metadata had already blocked. RepairSteps has no consumer
// outside this package; the store factory reads only verdict, gate, reason.)
func TestPreflightBlockedOutcomeIndependentOfBDContext(t *testing.T) {
	type bdAnswer struct {
		ctx   PreflightBDContext
		err   error
		unset bool
	}
	answers := map[string]bdAnswer{
		"agree":     {ctx: PreflightBDContext{Backend: "postgres", BDVersion: "1.0.4", SchemaVersion: 1}},
		"disagree":  {ctx: PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4", SchemaVersion: 1}},
		"embedded":  {ctx: PreflightBDContext{Backend: "dolt", DoltMode: "embedded", BDVersion: "1.0.4", SchemaVersion: 1}},
		"skew":      {ctx: PreflightBDContext{Backend: "postgres", BDVersion: "9.9.9", SchemaVersion: 1}},
		"no-schema": {ctx: PreflightBDContext{Backend: "postgres", BDVersion: "1.0.4"}},
		"error":     {err: errors.New("bd context unavailable")},
		"unwired":   {unset: true},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			fs := fsys.NewFake()
			writeSkipTestMetadata(fs, "/city", skipTestPostgresMetadata)
			var reader func(string) (PreflightBDContext, error)
			if !answer.unset {
				reader = func(string) (PreflightBDContext, error) { return answer.ctx, answer.err }
			}
			result, err := skipTestChecker(fs, "bd", reader).Check("/city")
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
			assertCheckOrder(t, result)
			var gate PreflightCheckID
			for _, check := range result.Checks {
				if check.State == PreflightCheckFail {
					gate = check.ID
					break
				}
			}
			if gate != PreflightCheckMetadataBackend {
				t.Fatalf("gate = %q, want %q", gate, PreflightCheckMetadataBackend)
			}
			if want := `Metadata backend "postgres" is unsupported; the native store serves dolt only`; result.FallbackReason != want {
				t.Fatalf("FallbackReason = %q, want %q", result.FallbackReason, want)
			}
			if result.Fallback != PreflightFallbackBdStore {
				t.Fatalf("Fallback = %q, want %q", result.Fallback, PreflightFallbackBdStore)
			}
			for _, step := range result.RepairSteps {
				if isBDContextDependentCheck(step.CheckID) {
					t.Fatalf("repair step from %s depends on bd context: %+v", step.CheckID, result.RepairSteps)
				}
			}
		})
	}
}

// TestPreflightConsultsBDContextFreshOnEveryCheck pins the long-lived
// controller contract: nothing is cached across Check calls. A scope whose
// metadata changes from blocked to dolt consults bd context on the next
// check, and a bd context error is re-asked (never remembered) next time.
func TestPreflightConsultsBDContextFreshOnEveryCheck(t *testing.T) {
	fs := fsys.NewFake()
	writeSkipTestMetadata(fs, "/rig", skipTestPostgresMetadata)
	calls := 0
	fail := false
	checker := skipTestChecker(fs, "bd", func(string) (PreflightBDContext, error) {
		calls++
		if fail {
			return PreflightBDContext{}, errors.New("bd context unavailable")
		}
		return PreflightBDContext{Backend: "dolt", DoltMode: "embedded", BDVersion: "1.0.4", SchemaVersion: 1}, nil
	})

	check := func() PreflightResult {
		t.Helper()
		result, err := checker.Check("/rig")
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		return result
	}

	if result := check(); calls != 0 || result.NativeStoreEligible {
		t.Fatalf("postgres metadata: calls=%d eligible=%v, want 0/false", calls, result.NativeStoreEligible)
	}

	// Operator re-points the scope at dolt: the very next check asks bd, and
	// bd's answer (embedded) decides.
	writeSkipTestMetadata(fs, "/rig", skipTestDoltMetadata)
	result := check()
	if calls != 1 {
		t.Fatalf("after metadata change: bd context calls = %d, want 1", calls)
	}
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckFail)

	// An error is reported, then re-asked rather than replayed.
	fail = true
	result = check()
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	fail = false
	result = check()
	if calls != 3 {
		t.Fatalf("bd context calls = %d, want 3 (no caching of results or errors)", calls)
	}
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckFail)

	// And back to blocked metadata: bd context is skipped again.
	writeSkipTestMetadata(fs, "/rig", skipTestPostgresMetadata)
	check()
	if calls != 3 {
		t.Fatalf("after reverting to postgres: bd context calls = %d, want 3", calls)
	}
}
