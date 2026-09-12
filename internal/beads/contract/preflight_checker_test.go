package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestPreflightBlocksNativeOnABackendGCDoesNotImplement pins the native-store
// gate for the shape that replaced postgres: the metadata names a backend gc
// has no vocabulary for, and every check that can decide says so by name.
//
// The connection keys in the fixture are deliberate. They are the shape a
// linked beads library reads and gc does not, and their presence must change
// nothing here — the verdict comes from the backend name alone.
func TestPreflightBlocksNativeOnABackendGCDoesNotImplement(t *testing.T) {
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
	assertCheckState(t, result, PreflightCheckContractShape, PreflightCheckFail)
	for _, id := range []PreflightCheckID{PreflightCheckMetadataBackend, PreflightCheckContractShape} {
		if summary := findPreflightCheck(t, result, id).Summary; !strings.Contains(summary, `"postgres"`) {
			t.Errorf("%s summary = %q, want it to name the backend", id, summary)
		}
	}
	assertPreflightReadOnly(t, checker.FS.(*fsys.Fake))
}

// TestPreflightRedactsADSNDiagnostic keeps the redaction rule keyed on the
// shape of the value rather than on any one backend's name: a *_dsn diagnostic
// is a connection string with a userinfo section whoever wrote it.
func TestPreflightRedactsADSNDiagnostic(t *testing.T) {
	details := PreflightDetails{
		MetadataBackend: "postgres",
		AdditionalDiagnostics: []PreflightDetailField{
			{Key: "storage_dsn", Value: "postgres://operator:swordfish@db.example.com/gascity"},
		},
	}
	check := NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "shape refused", details)
	if got := check.Details.AdditionalDiagnostics[0].Value; got != "postgres://[REDACTED]" {
		t.Fatalf("dsn diagnostic = %q, want %q", got, "postgres://[REDACTED]")
	}
	data, err := json.Marshal(check)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(data), "swordfish") {
		t.Fatalf("serialized check leaked the DSN secret: %s", data)
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
		replaced   bool
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"source build reports (devel) — schema is the signal, pass", "(devel)", false, validCtx("1.0.5"), PreflightCheckPass},
		{"confirmed version mismatch still fails", "1.0.5", false, validCtx("1.0.4"), PreflightCheckFail},
		{"matching versions pass", "1.0.5", false, validCtx("1.0.5"), PreflightCheckPass},
		{"missing bd version is unconfirmable — warn", "1.0.5", false, validCtx(""), PreflightCheckWarn},

		// The live defect (ga-40qh1). This fork replaces the beads module with
		// the enterprise build, so the linked module reports the replacement's
		// pseudo-version while bd reports its release. Those two strings can
		// never be equal, so the compare answered "mismatch" for a question it
		// was never able to ask.
		{"enterprise replacement pseudo-version vs bd release — unknown, pass", "0.0.0-20260810084121-1aa7bf160786", true, validCtx("1.1.0"), PreflightCheckPass},
		// Same shape without a replace: origin/main pins beads at an untagged
		// commit, so even the unreplaced OSS build reports a pseudo-version.
		{"pseudo-version without a replace — unknown, pass", "1.1.1-0.20260805093327-bf97b73749ac", false, validCtx("1.1.0"), PreflightCheckPass},
		// A replacement module may carry a real tag. Its numbering belongs to a
		// different module, so it is still not comparable to bd's release.
		{"tagged replacement module — unknown, pass", "2.4.0", true, validCtx("1.1.0"), PreflightCheckPass},
		// A local-path replace records no version at all.
		{"replaced with no recorded version — unknown, pass", "", true, validCtx("1.1.0"), PreflightCheckPass},

		// The other direction: an unreplaced release pair that genuinely
		// disagrees must still fail, including against the live bd version.
		{"unreplaced release skew still fails", "1.1.1", false, validCtx("1.1.0"), PreflightCheckFail},
		{"unreplaced exact match still passes", "1.1.0", false, validCtx("1.1.0"), PreflightCheckPass},
		{"v-prefixed unreplaced release skew still fails", "v1.2.0", false, validCtx("v1.1.0"), PreflightCheckFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion, BeadsLibraryReplaced: tt.replaced}
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

// TestCheckVersionCompatSummariesAreStableWhereItAlreadyPassed pins the exact
// wording of every outcome that existed before ga-40qh1. Widening the "unknown"
// set must be invisible to the scopes the check already answered — a summary
// drift here would change `gc doctor` output and the recorded fallback reason
// for scopes that were never broken.
func TestCheckVersionCompatSummariesAreStableWhereItAlreadyPassed(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 50}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		err        error
		want       string
	}{
		{"unreachable bd context", "1.0.5", PreflightBDContext{}, errors.New("not a git repository"), "bd context is unreachable; cannot confirm bd/beads version compatibility"},
		{"no schema version", "1.0.5", PreflightBDContext{BDVersion: "1.0.5"}, nil, "bd context did not report a schema version"},
		{"missing bd version", "1.0.5", validCtx(""), nil, "bd/beads version compatibility could not be confirmed"},
		{"source build", "(devel)", validCtx("1.0.5"), nil, "bd/beads schema compatible; linked library version unconfirmed (source build)"},
		{"matching releases", "1.0.5", validCtx("1.0.5"), nil, "bd and linked beads library versions match"},
		{"confirmed mismatch", "1.0.5", validCtx("1.0.4"), nil, "bd version differs from linked beads library version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			if got := c.checkVersionCompat(tt.ctx, tt.err); got.Summary != tt.want {
				t.Fatalf("summary = %q, want %q", got.Summary, tt.want)
			}
		})
	}
}

// TestPreflightEligibleOnReplacedBeadsModuleWithReadableBDContext is the live
// shape from ga-40qh1: a rig scope IS a git repo, so `bd context` succeeds and
// every check can actually run. Before the fix the version compare was the only
// FAIL, and it fired for a question it could not ask — dropping a healthy scope
// to BdStore, which at the time answered listIncludesCompleteDependencies() with
// a hardcoded false, so its complete-ready cache reads were permanently declined
// (ga-tgpfm).
func TestPreflightEligibleOnReplacedBeadsModuleWithReadableBDContext(t *testing.T) {
	scope := "/city/rigs/gascity"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                   fs,
		Provider:             "bd",
		BeadsLibraryVersion:  "0.0.0-20260810084121-1aa7bf160786",
		BeadsLibraryReplaced: true,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	// Eligibility here is earned by every check passing, not by the
	// unreachable-bd-context upgrade — bd context was readable.
	if result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = true, want false when bd context is readable")
	}
	if result.Fallback != "" {
		t.Errorf("Fallback = %q, want empty for an eligible scope", result.Fallback)
	}
}

// TestPreflightBlocksOnRealVersionSkew is the other direction: with no replace
// and two comparable releases that disagree, the check must still block the
// native store. Widening "unknown" must not widen "compatible".
func TestPreflightBlocksOnRealVersionSkew(t *testing.T) {
	scope := "/city/rigs/gascity"
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
		BeadsLibraryVersion: "1.2.0",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckFail)
	if result.Fallback != PreflightFallbackBdStore {
		t.Errorf("Fallback = %q, want %q", result.Fallback, PreflightFallbackBdStore)
	}
}

// TestCheckVersionCompatSemverCompatibleNewerBD verifies that a bd release
// newer than the linked beads library — but semver-compatible with it (same
// major version, not older) — passes instead of failing an exact-string
// compare. This is the common Homebrew case: gascity's go.mod pins one beads
// release, but the `gascity` formula's unversioned `depends_on "beads"`
// installs whatever is current, which drifts ahead over time
// (gastownhall/gascity#5164). A differing major version, or an older bd, is
// still not assumed compatible.
func TestCheckVersionCompatSemverCompatibleNewerBD(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 50}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"newer patch, same major.minor — pass", "1.1.0", validCtx("1.1.2"), PreflightCheckPass},
		{"newer minor, same major — pass", "1.1.0", validCtx("1.2.0"), PreflightCheckPass},
		{"newer major — still fails, majors are not semver-compatible", "1.1.0", validCtx("2.0.0"), PreflightCheckFail},
		{"older patch — still fails", "1.1.2", validCtx("1.1.0"), PreflightCheckFail},
		{"older major — still fails", "2.0.0", validCtx("1.9.9"), PreflightCheckFail},
		{"v-prefixed newer patch — pass", "v1.1.0", validCtx("v1.1.2"), PreflightCheckPass},
		{"non-semver bd version — falls back to exact match, fails", "1.1.0", validCtx("not-a-version"), PreflightCheckFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}

// TestCheckVersionCompatSamePrereleaseSeries pins the second widening, added
// during the beads v1.3.0-rc.2 pin bump. gascity's own anchors are not split --
// go.mod and deps.env BD_VERSION are both rc.2 -- so this does not gate the
// shipped pairing. It gates the general case: an OLDER bd against a NEWER
// library, which newerSemverCompatibleBD refuses by design.
//
// For the pair this was written for that refusal is wrong: rc.1 and rc.2 embed
// a byte-identical internal/storage/schema (LatestVersion() == 66 on both), so
// there is no skew to catch -- a checked property of that pair, not a law of
// RC series (see samePrereleaseSeries). Where such a pairing does occur, the
// check would FAIL, the verdict would go BLOCKED, and every scope would
// silently fall off the native Dolt store onto the fork-per-op BdStore -- the
// exact degradation #5164 was fixed to prevent.
//
// The widening stays narrow, and the negative cases below are the point: a
// different release's prerelease, and an RC against its own final release, must
// still fail.
func TestCheckVersionCompatSamePrereleaseSeries(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 66}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"older RC against newer RC of the same release — pass", "v1.3.0-rc.2", validCtx("1.3.0-rc.1"), PreflightCheckPass},
		{"newer RC against older RC of the same release — pass", "v1.3.0-rc.1", validCtx("1.3.0-rc.2"), PreflightCheckPass},
		{"RC against the final release it precedes — still fails", "v1.3.0", validCtx("1.3.0-rc.1"), PreflightCheckFail},
		{"prereleases of different releases — still fails", "v1.4.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckFail},
		{"prereleases across majors — still fails", "v2.0.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}

// TestPreflightEligibleOnSemverCompatibleNewerBD is the live shape from
// gastownhall/gascity#5164: Homebrew's unversioned beads dependency installs
// a newer bd release than gc's pinned go.mod version. Before this fix the
// version compare was the only FAIL, dropping a healthy scope to the slow
// per-op BdStore fallback (fork-per-op, degrading order-firing-current and
// fork-rate) for a difference that was never actually incompatible.
func TestPreflightEligibleOnSemverCompatibleNewerBD(t *testing.T) {
	scope := "/city/rigs/gascity"
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
		BeadsLibraryVersion: "1.1.0",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.2", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	if result.Fallback != "" {
		t.Errorf("Fallback = %q, want empty for an eligible scope", result.Fallback)
	}
}

// TestLinkedBeadsLibraryFromBuildInfo covers the build-info reader that feeds
// checkVersionCompat in production. A replace directive — of either form — means
// the recorded version does not describe the code that is actually linked.
func TestLinkedBeadsLibraryFromBuildInfo(t *testing.T) {
	dep := func(version string, replace *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{Deps: []*debug.Module{
			{Path: "github.com/spf13/cobra", Version: "v1.10.2"},
			{Path: beadsModulePath, Version: version, Replace: replace},
		}}
	}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want beadsLibrary
	}{
		{"nil build info", nil, beadsLibrary{}},
		{"beads is not linked", &debug.BuildInfo{Deps: []*debug.Module{{Path: "github.com/spf13/cobra", Version: "v1.10.2"}}}, beadsLibrary{}},
		{"plain require", dep("v1.1.0", nil), beadsLibrary{Version: "v1.1.0"}},
		{
			"module replace records the replacement's version",
			dep("v1.1.1-0.20260805093327-bf97b73749ac", &debug.Module{Path: "github.com/gascity/bd-enterprise", Version: "v0.0.0-20260810084121-1aa7bf160786"}),
			beadsLibrary{Version: "v0.0.0-20260810084121-1aa7bf160786", Replaced: true},
		},
		{
			"local-path replace records no version",
			dep("v1.1.0", &debug.Module{Path: "../beads"}),
			beadsLibrary{Replaced: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linkedBeadsLibraryFrom(tt.info); got != tt.want {
				t.Fatalf("linkedBeadsLibraryFrom() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestComparableReleaseVersion pins the classifier that decides whether the two
// reported versions name the same kind of thing. Only a released semver tag can
// be compared to bd's self-reported release.
func TestComparableReleaseVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"", false},
		{"(devel)", false},
		{"devel", false},
		{"unknown", false},
		{"1.1.0", true},
		{"v1.1.0", true},
		{"1.1.0-rc.1", true},
		{"0.0.0-20260810084121-1aa7bf160786", false},
		{"1.1.1-0.20260805093327-bf97b73749ac", false},
		{"1.2.0-pre.0.20260805093327-bf97b73749ac", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := comparableReleaseVersion(tt.version); got != tt.want {
				t.Fatalf("comparableReleaseVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

// TestCheckVersionCompatPseudoVersionLockstep covers the case ga-40qh1's
// widening left uncovered and the 2026-09-08 srvcity outage walked through: gc
// and bd BOTH built from untagged commits, so both report pseudo-versions.
//
// ga-40qh1 was right that a pseudo-version cannot be compared to a bd RELEASE —
// the two strings name different kinds of thing. It is comparable to another
// pseudo-version, because both name a commit in the same repository, and that
// comparison is the only evidence available in gc's normal configuration: gc
// pins beads at an untagged commit whenever it tracks beads ahead of its last
// tag.
//
// The stakes are why this must FAIL rather than warn. Opening the native store
// runs MigrateUp, so a gc built from a newer beads commit migrates every store
// it touches past what an older bd can read — irreversibly, on first contact.
// A Fail routes the factory to openBdFallback, so the migration never runs.
func TestCheckVersionCompatPseudoVersionLockstep(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 50}
	}
	const (
		gcPin  = "1.1.1-0.20260805093327-bf97b73749ac" // gc's go.mod pin, 2026-09-08
		bdOld  = "0.0.0-20260713082743-98b4b79e6a4d"   // the bd that went blind
		bdSame = "0.0.0-20260805093327-bf97b73749ac"   // same commit, different stamp
	)
	tests := []struct {
		name       string
		libVersion string
		replaced   bool
		ctx        PreflightBDContext
		want       PreflightCheckState
		wantRev    string // substring the summary must name, when non-empty
	}{
		{
			name:       "different commits — the live outage — fails",
			libVersion: gcPin,
			ctx:        validCtx(bdOld),
			want:       PreflightCheckFail,
			wantRev:    "98b4b79e6a4d",
		},
		{
			// The version STAMPS differ (bd stamps its own 0.0.0-date-sha while
			// gc records the module's v1.1.1-0.date-sha) but the commit is the
			// same, which is the only thing that matters. A string compare would
			// call this drift; it is lockstep.
			name:       "same commit under different version stamps — passes",
			libVersion: gcPin,
			ctx:        validCtx(bdSame),
			want:       PreflightCheckPass,
			wantRev:    "bf97b73749ac",
		},
		{
			// A replaced module's revision belongs to the replacement, not to
			// beads. Comparing it to bd's revision would assert agreement between
			// commits in two different repositories, so this stays unconfirmable
			// — the ga-40qh1 enterprise-fork case, preserved.
			name:       "replaced module with a pseudo-version stays unconfirmable",
			libVersion: "0.0.0-20260810084121-1aa7bf160786",
			replaced:   true,
			ctx:        validCtx(bdOld),
			want:       PreflightCheckPass,
		},
		{
			// Only one side names a commit; equality of the underlying source is
			// not establishable, so the pre-existing "unknown, pass" holds.
			name:       "pseudo library vs bd release stays unconfirmable",
			libVersion: gcPin,
			ctx:        validCtx("1.1.0"),
			want:       PreflightCheckPass,
		},
		{
			// The mirror direction is NOT touched by this change and is already
			// conservative: a released library pin is comparable, so the existing
			// string compare runs, and a bd pseudo-version cannot be shown to be a
			// newer release of it (major 0 vs 1) — so it fails to BdStore. Pinned
			// here so the lockstep compare is never mistaken for having widened it.
			name:       "release library vs bd pseudo-version keeps failing",
			libVersion: "1.1.0",
			ctx:        validCtx(bdOld),
			want:       PreflightCheckFail,
		},
		{
			// "(devel)" names no commit at all. gc and bd built from one tree both
			// report it, and so would two unrelated trees; nothing to compare.
			name:       "source build stays unconfirmable",
			libVersion: "(devel)",
			ctx:        validCtx(bdOld),
			want:       PreflightCheckPass,
		},
		{
			// A v prefix on either side is cosmetic and must not change the answer.
			name:       "v-prefixed pseudo-versions on the same commit pass",
			libVersion: "v" + gcPin,
			ctx:        validCtx("v" + bdSame),
			want:       PreflightCheckPass,
			wantRev:    "bf97b73749ac",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion, BeadsLibraryReplaced: tt.replaced}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.ID != PreflightCheckVersionCompat {
				t.Fatalf("ID = %q, want %q", got.ID, PreflightCheckVersionCompat)
			}
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
			if tt.wantRev != "" && !strings.Contains(got.Summary, tt.wantRev) {
				t.Errorf("summary = %q, want it to name commit %q", got.Summary, tt.wantRev)
			}
		})
	}
}

// TestPseudoVersionRev pins the parse the lockstep compare rests on. "Not
// comparable" and "different" must stay distinct answers: only the second is
// evidence of drift, and conflating them either blinds the check or takes
// healthy scopes offline.
func TestPseudoVersionRev(t *testing.T) {
	tests := []struct {
		in      string
		wantRev string
		wantOK  bool
	}{
		{"1.1.1-0.20260805093327-bf97b73749ac", "bf97b73749ac", true},
		{"v1.1.1-0.20260805093327-bf97b73749ac", "bf97b73749ac", true},
		{"0.0.0-20260713082743-98b4b79e6a4d", "98b4b79e6a4d", true},
		{"1.1.0", "", false},   // a release names no commit
		{"v1.1.0", "", false},  // ditto
		{"(devel)", "", false}, // a source build names no commit
		{"", "", false},
		{"not-a-version", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			rev, ok := pseudoVersionRev(tt.in)
			if ok != tt.wantOK || rev != tt.wantRev {
				t.Fatalf("pseudoVersionRev(%q) = (%q, %v), want (%q, %v)", tt.in, rev, ok, tt.wantRev, tt.wantOK)
			}
		})
	}
}
