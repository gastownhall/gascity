package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestPreflightBlocksNativeOnMetadataPostgres(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "postgres",
		"postgres_host": "db.example.com",
		"postgres_port": "5432",
		"postgres_user": "operator",
		"postgres_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "postgres"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckOrder(t, result)
	assertCheckState(t, result, PreflightCheckMetadataBackend, PreflightCheckFail)
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckContractShape, PreflightCheckPass)
	assertPreflightReadOnly(t, checker.FS.(*fsys.Fake))
}

func TestPreflightRedactsPostgresDSN(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "postgres",
		"postgres_dsn": "postgres://operator:swordfish@db.example.com/gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "postgres"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckMetadataBackend, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckContractShape, PreflightCheckWarn)
	check := findPreflightCheck(t, result, PreflightCheckMetadataBackend)
	if check.Details.PostgresDSNRedacted != "postgres://[REDACTED]" {
		t.Fatalf("PostgresDSNRedacted = %q, want redacted DSN", check.Details.PostgresDSNRedacted)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(data), "swordfish") || strings.Contains(string(data), "operator:swordfish") {
		t.Fatalf("serialized result leaked DSN secret: %s", data)
	}
}

func TestPreflightBlocksNativeOnContextDisagreement(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "postgres"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckFail)
}

// An UNREACHABLE bd context (e.g. a non-git city root where `bd context` cannot
// run) is not evidence of a backend disagreement — it only means the native
// store's bd-context cross-checks cannot be verified. The bd-context-derived
// checks report WARN, not FAIL. When gc has INDEPENDENTLY confirmed the dolt
// backend by connecting to the server and matching project_id (identity_match
// PASS), that direct verification is stronger evidence than bd context's
// cross-check, so eligibility is upgraded to ELIGIBLE rather than falling back
// to per-call bd. (A real disagreement, with a readable bd context, still
// blocks — see TestPreflightBlocksNativeOnContextDisagreement.)
func TestPreflightEligibleOnUnreachableBDContextWhenIdentityVerified(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{}, errors.New("bd context unavailable: not a git repository")
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	// Unreachable bd context + independent identity proof => ELIGIBLE.
	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	// The bd-context cross-checks still report WARN; they are informational —
	// the verdict is upgraded on the strength of the independent identity match.
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckPass)
	// The result flags that eligibility came via the identity-fallback path.
	if !result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = false, want true on the identity-verified upgrade")
	}
}

// Without independent identity proof, an unreachable bd context must stay
// DEGRADED (per-call bd fallback): gc has no other evidence that the native
// store would read the correct dolt backend.
func TestPreflightDegradesOnUnreachableBDContextWithoutIdentityProof(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{}, errors.New("bd context unavailable: not a git repository")
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "", false, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	// Unreachable bd context, no independent proof => DEGRADED, never BLOCKED.
	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckWarn)
	// No upgrade happened, so the identity-fallback flag stays false.
	if result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = true, want false when the verdict stays DEGRADED")
	}
}

func TestPreflightBlocksNativeOnIdentityMismatch(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
	check := findPreflightCheck(t, result, PreflightCheckIdentityMatch)
	if check.Details.MetadataProjectID != "metadata-id" || check.Details.DBProjectID != "database-id" {
		t.Fatalf("identity details = %+v, want both project ids visible", check.Details)
	}
}

func TestPreflightPassesOnHealthyDolt(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	for _, check := range result.Checks {
		if check.State != PreflightCheckPass {
			t.Fatalf("check %s state = %s, want PASS in healthy case: %+v", check.ID, check.State, result.Checks)
		}
	}
	if result.Fallback != PreflightFallbackNone {
		t.Fatalf("Fallback = %q, want none", result.Fallback)
	}
}

func TestPreflightAcceptsExecGcBeadsBdProviderPath(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local")
	checker.Provider = "exec:/tmp/gc-beads-bd.sh"

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckProviderContract, PreflightCheckPass)
}

func TestProviderUsesBDContract(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "", want: true},
		{provider: "bd", want: true},
		{provider: " file ", want: false},
		{provider: "exec:gc-beads-bd", want: true},
		{provider: "exec:/tmp/gc-beads-bd", want: true},
		{provider: "exec:/tmp/gc-beads-bd.sh", want: true},
		{provider: "exec:/tmp/gc-beads-k8s", want: false},
		{provider: "exec:/tmp/custom", want: false},
	}
	for _, tt := range tests {
		if got := ProviderUsesBDContract(tt.provider); got != tt.want {
			t.Fatalf("ProviderUsesBDContract(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

func TestPreflightRespectsSkipOverrideAsRecoveryOnly(t *testing.T) {
	t.Setenv("BEADS_SKIP_IDENTITY_CHECK", "1")
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
}

func TestPreflightWarnsWhenDatabaseIdentityUnavailable(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "")
	checker.DatabaseProjectID = func(string) (string, bool, error) {
		return "", false, errors.New("dial dolt")
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckWarn)
}

// TestPreflightDefersIdentityToNativeOpenForExternalEndpoint covers hosted
// beads-gateway endpoints: the direct project_id SQL probe (managedDoltOpenDatabase)
// connects as root over plaintext and cannot authenticate the EIA-as-username +
// TLS gateway, so it never confirms project_id. For an external endpoint the
// authoritative database _project_id is verified by beadslib at native-open time
// (verifyProjectIdentity over the authenticated connection), so the identity
// check defers to that gate and keeps the scope native-eligible instead of
// degrading to the shell BdStore — without claiming a control-plane confirmation
// it cannot make.
func TestPreflightDefersIdentityToNativeOpenForExternalEndpoint(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "bd_prj_c069247fbac36e2b",
		"project_id": "prj_c069247fbac36e2b"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "")
	// Direct DB probe fails to authenticate the hosted gateway (root/plaintext)...
	checker.DatabaseProjectID = func(string) (string, bool, error) {
		return "", false, errors.New("dial hosted gateway: access denied")
	}
	// ...and the scope resolves to an external endpoint, so identity is deferred
	// to beadslib's native-open verification rather than degraded.
	checker.DeferIdentityToNativeOpen = func(string) bool { return true }

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckPass)
}

// TestPreflightExternalEndpointStillBlocksOnProbeMismatch guards the deferral:
// deferring to native-open verification only applies when the direct probe is
// UNAVAILABLE. If the probe does reach the database and reports a project_id that
// disagrees with metadata, that is a genuine cross-project mismatch and must
// still block native activation even for an external endpoint.
func TestPreflightExternalEndpointStillBlocksOnProbeMismatch(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")
	checker.DeferIdentityToNativeOpen = func(string) bool { return true }

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
}

func TestPreflightUnreadableScopeReturnsError(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Errors[filepath.Join(scope, ".beads", "metadata.json")] = os.ErrPermission
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	if _, err := checker.Check(scope); err == nil || !strings.Contains(err.Error(), "read preflight metadata") {
		t.Fatalf("Check() error = %v, want unreadable metadata error", err)
	}
	assertPreflightReadOnly(t, fs)
}

func testPreflightChecker(metadata string, ctx PreflightBDContext, dbProjectID string) PreflightChecker {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(metadata)
	if ctx.BDVersion == "" {
		ctx.BDVersion = "1.0.4"
	}
	if ctx.SchemaVersion == 0 {
		ctx.SchemaVersion = 1
	}
	return PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return ctx, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return dbProjectID, dbProjectID != "", nil
		},
	}
}

func preflightMetadataJSON(body string) string {
	return strings.ReplaceAll(body, "\t", "")
}

func assertPreflightVerdict(t *testing.T, result PreflightResult, want PreflightVerdict, wantEligible bool) {
	t.Helper()
	if result.Verdict != want {
		t.Fatalf("Verdict = %q, want %q; checks=%+v", result.Verdict, want, result.Checks)
	}
	if result.NativeStoreEligible != wantEligible {
		t.Fatalf("NativeStoreEligible = %v, want %v", result.NativeStoreEligible, wantEligible)
	}
}

func assertCheckOrder(t *testing.T, result PreflightResult) {
	t.Helper()
	want := []PreflightCheckID{
		PreflightCheckProviderContract,
		PreflightCheckMetadataBackend,
		PreflightCheckBDContextAgreement,
		PreflightCheckDoltModeSafe,
		PreflightCheckIdentityMatch,
		PreflightCheckVersionCompat,
		PreflightCheckContractShape,
	}
	if len(result.Checks) != len(want) {
		t.Fatalf("Checks len = %d, want %d: %+v", len(result.Checks), len(want), result.Checks)
	}
	for i, id := range want {
		if result.Checks[i].ID != id {
			t.Fatalf("Checks[%d].ID = %q, want %q; checks=%+v", i, result.Checks[i].ID, id, result.Checks)
		}
	}
}

func assertCheckState(t *testing.T, result PreflightResult, id PreflightCheckID, want PreflightCheckState) {
	t.Helper()
	check := findPreflightCheck(t, result, id)
	if check.State != want {
		t.Fatalf("check %s state = %q, want %q; check=%+v", id, check.State, want, check)
	}
}

func findPreflightCheck(t *testing.T, result PreflightResult, id PreflightCheckID) PreflightCheckResult {
	t.Helper()
	for _, check := range result.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing check %s in %+v", id, result.Checks)
	return PreflightCheckResult{}
}

func assertPreflightReadOnly(t *testing.T, fs *fsys.Fake) {
	t.Helper()
	for _, call := range fs.Calls {
		switch call.Method {
		case "WriteFile", "MkdirAll", "Rename", "Remove", "Chmod":
			t.Fatalf("preflight checker must be read-only; saw %s on %s", call.Method, call.Path)
		}
	}
}

// TestCheckVersionCompatSourceBuild verifies that a source (local-path/replace)
// build of the linked beads library — which reports "(devel)" as its module
// version — does not take the native store offline. The schema version is the
// real compatibility signal; only a *confirmed* version mismatch should fail.
func TestCheckVersionCompatSourceBuild(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 50}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"source build reports (devel) — schema is the signal, pass", "(devel)", validCtx("1.0.5"), PreflightCheckPass},
		{"confirmed version mismatch still fails", "1.0.5", validCtx("1.0.4"), PreflightCheckFail},
		{"matching versions pass", "1.0.5", validCtx("1.0.5"), PreflightCheckPass},
		{"missing bd version is unconfirmable — warn", "1.0.5", validCtx(""), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.ID != PreflightCheckVersionCompat {
				t.Fatalf("ID = %q, want %q", got.ID, PreflightCheckVersionCompat)
			}
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}
