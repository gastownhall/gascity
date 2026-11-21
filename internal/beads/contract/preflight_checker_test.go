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
	// bd context is not consulted once metadata_backend has blocked: the
	// bd-context checks report that, as a WARN, instead of a cross-check.
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
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
		SchemaLatestVersion: 1,
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			// The schema signal comes straight from the database over SQL, so
			// it is unaffected by bd context's unreachability.
			return PreflightSchemaCursors{Main: 1}, true, nil
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
	assertCheckState(t, result, PreflightCheckBDVersionHint, PreflightCheckWarn)
	// The database schema check does not depend on bd context at all, so it
	// independently PASSes even though bd context is unreachable.
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
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
	assertCheckState(t, result, PreflightCheckBDVersionHint, PreflightCheckWarn)
	// No DatabaseSchemaCursors is configured here, so the schema check WARNs
	// too (for an unrelated reason — the reader is simply not wired up) and
	// alone still degrades the verdict, same as before.
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

// TestPreflightWarnsWhenScopeHasNoAuthoritativeSchemaCursors pins the
// version_compat verdict for a scope whose cursor reader has no authoritative
// database to read (it reports no cursors and no error, as it does for a scope
// with no canonical dolt target) and that does not defer to native open: WARN
// "could not be confirmed", degrading the scope — neither a PASS that would
// skip the schema gate nor a FAIL that would block on a signal never read.
func TestPreflightWarnsWhenScopeHasNoAuthoritativeSchemaCursors(t *testing.T) {
	scope := "/city"
	checker := testPreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local")
	checker.DatabaseSchemaCursors = func(string) (PreflightSchemaCursors, bool, error) {
		return PreflightSchemaCursors{}, false, nil
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckWarn)
	for _, check := range result.Checks {
		if check.ID == PreflightCheckVersionCompat && check.Summary != "database schema could not be confirmed" {
			t.Fatalf("version_compat summary = %q, want %q", check.Summary, "database schema could not be confirmed")
		}
	}
}

// TestPreflightDefersIdentityToNativeOpenForExternalEndpoint covers hosted
// beads-gateway endpoints: the direct project_id and schema-cursor SQL probes
// (managedDoltOpenDatabase) connect as root over plaintext and cannot
// authenticate the EIA-as-username + TLS gateway, so neither ever confirms
// anything. For an external endpoint the authoritative database _project_id
// and schema are verified by beadslib at native-open time (verifyProjectIdentity
// over the authenticated connection; CheckForwardDrift and the shared/remote
// migrate gate on open), so both the identity check and the schema check defer
// to that gate and keep the scope native-eligible instead of degrading to the
// shell BdStore — without claiming a control-plane confirmation neither probe
// can make.
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
	// ...the schema cursor probe dials the same way and fails the same way:
	// the direct SQL schema-cursor reader is just as unable to authenticate a
	// hosted gateway as the identity probe is, so a fake that leaves it
	// healthy (as the shared test helper's default does) could never have
	// caught a preflight that forgot to defer checkSchemaCompat the same way
	// it defers checkIdentityMatch.
	checker.DatabaseSchemaCursors = func(string) (PreflightSchemaCursors, bool, error) {
		return PreflightSchemaCursors{}, false, errors.New("dial hosted gateway: access denied")
	}
	// ...and the scope resolves to an external endpoint, so both identity and
	// schema are deferred to beadslib's native-open verification rather than
	// degraded: beads refuses forward schema drift unconditionally at open and
	// gates a behind schema's migration on shared/remote stores there too.
	checker.DeferIdentityToNativeOpen = func(string) bool { return true }

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
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
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4"}, nil
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
		SchemaLatestVersion:        1,
		SchemaLatestIgnoredVersion: 1,
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 1, Ignored: 1, IgnoredChecked: true}, true, nil
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
		PreflightCheckBDVersionHint,
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
// version — does not take the native store offline. checkBDVersionHint is
// informational only: a bd/library semver difference is evidence the two
// strings differ, never that the database is unopenable (that is
// checkSchemaCompat's job).
func TestCheckVersionCompatSourceBuild(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion}
	}
	tests := []struct {
		name       string
		libVersion string
		replaced   bool
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"source build reports (devel) — unconfirmable, pass", "(devel)", false, validCtx("1.0.5"), PreflightCheckPass},
		{"confirmed version mismatch warns", "1.0.5", false, validCtx("1.0.4"), PreflightCheckWarn},
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
		// disagrees only warns, never fails — this check is informational.
		{"unreplaced release skew warns", "1.1.1", false, validCtx("1.1.0"), PreflightCheckWarn},
		{"unreplaced exact match still passes", "1.1.0", false, validCtx("1.1.0"), PreflightCheckPass},
		{"v-prefixed unreplaced release skew warns", "v1.2.0", false, validCtx("v1.1.0"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion, BeadsLibraryReplaced: tt.replaced}
			got := c.checkBDVersionHint(tt.ctx, nil)
			if got.ID != PreflightCheckBDVersionHint {
				t.Fatalf("ID = %q, want %q", got.ID, PreflightCheckBDVersionHint)
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
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		err        error
		want       string
	}{
		{"unreachable bd context", "1.0.5", PreflightBDContext{}, errors.New("not a git repository"), "bd context is unreachable; cannot confirm bd/beads version compatibility"},
		{"missing bd version", "1.0.5", validCtx(""), nil, "bd/beads version compatibility could not be confirmed"},
		{"source build", "(devel)", validCtx("1.0.5"), nil, "linked beads library version unconfirmed (source build)"},
		{"matching releases", "1.0.5", validCtx("1.0.5"), nil, "bd and linked beads library versions match"},
		{"confirmed mismatch", "1.0.5", validCtx("1.0.4"), nil, "bd version differs from the linked beads library version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			if got := c.checkBDVersionHint(tt.ctx, tt.err); got.Summary != tt.want {
				t.Fatalf("summary = %q, want %q", got.Summary, tt.want)
			}
		})
	}
}

// TestCheckSchemaCompatSummariesAreStable pins the wording of
// checkSchemaCompat's own outcomes, the eligibility-deciding version_compat
// check.
func TestCheckSchemaCompatSummariesAreStable(t *testing.T) {
	tests := []struct {
		name    string
		checker PreflightChecker
		want    string
	}{
		{
			"reader not configured",
			PreflightChecker{},
			"database schema cursor reader is not configured",
		},
		{
			"reader errors",
			PreflightChecker{DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
				return PreflightSchemaCursors{}, false, errors.New("dial dolt")
			}},
			"database schema could not be confirmed",
		},
		{
			"equal — passes",
			PreflightChecker{SchemaLatestVersion: 66, DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
				return PreflightSchemaCursors{Main: 66}, true, nil
			}},
			"database schema is compatible with the linked beads library",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.checker.checkSchemaCompat("/city"); got.Summary != tt.want {
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
		SchemaLatestVersion:  1,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 1}, true, nil
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

// TestPreflightEligibleOnSemverSkewWithCompatibleSchema is the other
// direction: with no replace and two comparable releases whose version
// strings disagree, but whose schema IS compatible (the database's real,
// SQL-read schema cursor is within what the linked library can open), native
// storage stays fully ELIGIBLE — a semver mismatch is informational only
// (PreflightCheckBDVersionHint WARN) and never degrades the verdict on its
// own. A bd reporting "1.1.0" against a gc linked to beads v1.3.1 has
// version strings that will never match, but its schema (66, at the linked
// library's ceiling) is fine: eligibility is decided by the schema result
// alone, and semver skew never blocks or degrades it.
func TestPreflightEligibleOnSemverSkewWithCompatibleSchema(t *testing.T) {
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
		SchemaLatestVersion: 66,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 66}, true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckBDVersionHint, PreflightCheckWarn)
	if result.Fallback != "" {
		t.Errorf("Fallback = %q, want empty for an eligible scope", result.Fallback)
	}
}

// TestCheckVersionCompatSemverCompatibleNewerBD verifies that a bd release
// newer than the linked beads library — but semver-compatible with it (same
// major version, not older) — PASSes instead of just warning on an
// exact-string compare. This is the common Homebrew case: gascity's go.mod
// pins one beads release, but the `gascity` formula's unversioned
// `depends_on "beads"` installs whatever is current, which drifts ahead over
// time (gastownhall/gascity#5164). A differing major version, or an older bd,
// is not assumed compatible by this widening — but it still only WARNs
// rather than FAILs, and that WARN is purely informational
// (checkBDVersionHint never fails, and its WARN never degrades the verdict —
// see preflightVerdictForChecks); the real eligibility gate is
// checkSchemaCompat's direct SQL read.
func TestCheckVersionCompatSemverCompatibleNewerBD(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"newer patch, same major.minor — pass", "1.1.0", validCtx("1.1.2"), PreflightCheckPass},
		{"newer minor, same major — pass", "1.1.0", validCtx("1.2.0"), PreflightCheckPass},
		{"newer major — not widened, warns (schema compatible)", "1.1.0", validCtx("2.0.0"), PreflightCheckWarn},
		{"older patch — not widened, warns (schema compatible)", "1.1.2", validCtx("1.1.0"), PreflightCheckWarn},
		{"older major — not widened, warns (schema compatible)", "2.0.0", validCtx("1.9.9"), PreflightCheckWarn},
		{"v-prefixed newer patch — pass", "v1.1.0", validCtx("v1.1.2"), PreflightCheckPass},
		{"non-semver bd version — falls back to exact match, warns", "1.1.0", validCtx("not-a-version"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkBDVersionHint(tt.ctx, nil)
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
// different release's prerelease, and an RC against its own final release, do
// not FAIL (a semver difference alone never does) but still do not qualify
// for THIS widening's PASS — they fall through to the general
// schema-compatible WARN.
func TestCheckVersionCompatSamePrereleaseSeries(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"older RC against newer RC of the same release — pass", "v1.3.0-rc.2", validCtx("1.3.0-rc.1"), PreflightCheckPass},
		{"newer RC against older RC of the same release — pass", "v1.3.0-rc.1", validCtx("1.3.0-rc.2"), PreflightCheckPass},
		{"RC against the final release it precedes — not widened, warns", "v1.3.0", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
		{"prereleases of different releases — not widened, warns", "v1.4.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
		{"prereleases across majors — not widened, warns", "v2.0.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkBDVersionHint(tt.ctx, nil)
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
		SchemaLatestVersion: 1,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.2"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 1}, true, nil
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

// TestCheckSchemaCompatGate is the table test for the
// authoritative schema-compatibility signal: checkSchemaCompat's verdict on
// the database schema cursors read directly over SQL
// (internal/beads/proxyendpoint.ReadCursorReportOverConn in production),
// never bd context's JSON envelope version. Ahead of the linked beads
// library's ceiling always FAILs (the linked library cannot open the
// database at all). Behind the ceiling FAILs unless the operator has opted
// in (beads.allow_schema_behind_migrate / BD_ALLOW_REMOTE_MIGRATE), because
// opening would let the linked library migrate the city's database forward.
// At the ceiling always PASSes. The ignored lane is gated the same way, but
// only when the reader reports it was actually checked (CursorReality).
func TestCheckSchemaCompatGate(t *testing.T) {
	tests := []struct {
		name           string
		cursors        PreflightSchemaCursors
		latestMain     int
		latestIgnored  int
		allowBehind    bool
		want           PreflightCheckState
		wantSummaryHas string
	}{
		{"main behind ceiling without opt-in — fails", PreflightSchemaCursors{Main: 65}, 66, 0, false, PreflightCheckFail, "behind"},
		{"main behind ceiling with opt-in — pass", PreflightSchemaCursors{Main: 65}, 66, 0, true, PreflightCheckPass, ""},
		{"main at ceiling — pass", PreflightSchemaCursors{Main: 66}, 66, 0, false, PreflightCheckPass, ""},
		{"main ahead of ceiling — fails even with opt-in", PreflightSchemaCursors{Main: 67}, 66, 0, true, PreflightCheckFail, "ahead"},
		{"ignored lane behind without opt-in — fails", PreflightSchemaCursors{Main: 66, Ignored: 20, IgnoredChecked: true}, 66, 26, false, PreflightCheckFail, "ignored-lane schema"},
		{"ignored lane behind with opt-in — pass", PreflightSchemaCursors{Main: 66, Ignored: 20, IgnoredChecked: true}, 66, 26, true, PreflightCheckPass, ""},
		{"ignored lane ahead — fails regardless of opt-in", PreflightSchemaCursors{Main: 66, Ignored: 30, IgnoredChecked: true}, 66, 26, true, PreflightCheckFail, "ignored-lane schema"},
		{"ignored lane not checked — main decides alone", PreflightSchemaCursors{Main: 66, Ignored: 999, IgnoredChecked: false}, 66, 26, false, PreflightCheckPass, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{
				SchemaLatestVersion:        tt.latestMain,
				SchemaLatestIgnoredVersion: tt.latestIgnored,
				AllowSchemaBehindMigrate:   func(string) bool { return tt.allowBehind },
				DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
					return tt.cursors, true, nil
				},
			}
			got := c.checkSchemaCompat("/city")
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
			if tt.wantSummaryHas != "" && !strings.Contains(got.Summary, tt.wantSummaryHas) {
				t.Fatalf("summary = %q, want it to contain %q", got.Summary, tt.wantSummaryHas)
			}
			if got.State == PreflightCheckFail && got.Details.Expected == "" {
				t.Errorf("Details.Expected is empty on a FAIL; must name the ceiling that was violated")
			}
		})
	}
}

// TestCheckSchemaCompatNeverReadsBDContextEnvelopeSchemaVersion guards the
// schema signal's source: `bd context --json`'s top-level "schema_version"
// field is bd's JSON envelope format version (cmd/bd/output.go's
// JSONSchemaVersion constant, currently stamped at 1 on every response),
// never the database's migration cursor. This test
// parses a real bd-context-shaped envelope and proves checkSchemaCompat's
// verdict is driven only by DatabaseSchemaCursors: even though the
// envelope's schema_version (1) is wildly different from the database's
// real schema (66, which matches the linked library's ceiling here),
// checkSchemaCompat PASSes. PreflightBDContext itself carries no schema
// field to misread in the first place (checkSchemaCompat's signature does
// not even take a PreflightBDContext) — this test guards the behavior, not
// just the type shape.
func TestCheckSchemaCompatNeverReadsBDContextEnvelopeSchemaVersion(t *testing.T) {
	const bdContextJSON = `{"schema_version":1,"backend":"dolt","dolt_mode":"server","bd_version":"1.3.1","project_id":"gc-local"}`
	var raw struct {
		SchemaVersion int    `json:"schema_version"`
		Backend       string `json:"backend"`
		DoltMode      string `json:"dolt_mode"`
		BDVersion     string `json:"bd_version"`
	}
	if err := json.Unmarshal([]byte(bdContextJSON), &raw); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if raw.SchemaVersion != 1 {
		t.Fatalf("fixture's envelope schema_version = %d, want 1 (JSONSchemaVersion)", raw.SchemaVersion)
	}
	// PreflightBDContext is built from only the fields it actually has —
	// there is no SchemaVersion field left to (mis)populate from
	// raw.SchemaVersion, and checkSchemaCompat never sees this value at all.
	_ = PreflightBDContext{Backend: raw.Backend, DoltMode: raw.DoltMode, BDVersion: raw.BDVersion}

	c := PreflightChecker{
		BeadsLibraryVersion: "1.3.1",
		SchemaLatestVersion: 66,
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 66}, true, nil
		},
	}
	got := c.checkSchemaCompat("/city")
	if got.State != PreflightCheckPass {
		t.Fatalf("checkSchemaCompat state = %q, want %q — the envelope's schema_version (1) must never be mistaken for the database schema (66)", got.State, PreflightCheckPass)
	}
}

// TestPreflightBlocksOnSchemaBehindWithoutOptIn is the Check()-level
// end-to-end shape of the schema-behind gate: a city whose database schema
// (65) is behind what the linked beads library can open (66). Opening it
// would let the linked library migrate the database forward, which requires
// an explicit opt-in (beads.allow_schema_behind_migrate) — without it, this
// BLOCKS and stays on BdStore, naming both remedies, even though bd's own
// semver string ("1.1.0") differs from the linked library's ("1.3.1") in
// exactly the way TestPreflightEligibleOnSemverSkewWithCompatibleSchema
// shows is, on its own, never blocking: eligibility here is decided by the
// schema result, not semver.
func TestPreflightBlocksOnSchemaBehindWithoutOptIn(t *testing.T) {
	scope := "/city/rigs/legacy"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "legacy",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.3.1",
		SchemaLatestVersion: 66,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 65}, true, nil
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
	const wantOptInSubstring = "beads.allow_schema_behind_migrate"
	if !strings.Contains(result.FallbackReason, wantOptInSubstring) {
		t.Errorf("FallbackReason = %q, want it to name the opt-in %s", result.FallbackReason, wantOptInSubstring)
	}
	const wantMigrateSubstring = "`bd migrate schema`"
	if !strings.Contains(result.FallbackReason, wantMigrateSubstring) {
		t.Errorf("FallbackReason = %q, want it to name the migration remedy %s", result.FallbackReason, wantMigrateSubstring)
	}
}

// TestPreflightEligibleOnSchemaBehindWithOptIn is the same city with the
// opt-in set: the schema gate PASSes (opening will migrate the database
// forward, which the operator has explicitly allowed), and the scope is
// fully ELIGIBLE — the semver difference remains a non-degrading,
// informational WARN.
func TestPreflightEligibleOnSchemaBehindWithOptIn(t *testing.T) {
	scope := "/city/rigs/legacy"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "legacy",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.3.1",
		SchemaLatestVersion: 66,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 65}, true, nil
		},
		AllowSchemaBehindMigrate: func(string) bool { return true },
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckBDVersionHint, PreflightCheckWarn)
	if result.Fallback != "" {
		t.Errorf("Fallback = %q, want empty for an eligible scope", result.Fallback)
	}
}

// TestPreflightBlocksOnSchemaAheadOfLinkedLibrary is the genuine-danger case
// that must still BLOCK unconditionally: the database's schema is ahead of
// what the linked beads library can open at all, so opening it would fail
// outright (schema skew). No semver widening and no behind-schema opt-in
// rescues this; it is a hard, typed FAIL naming both schema numbers.
func TestPreflightBlocksOnSchemaAheadOfLinkedLibrary(t *testing.T) {
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
		BeadsLibraryVersion: "1.3.1",
		SchemaLatestVersion: 66,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.3.1"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
		DatabaseSchemaCursors: func(string) (PreflightSchemaCursors, bool, error) {
			return PreflightSchemaCursors{Main: 67}, true, nil
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
	if !strings.Contains(result.FallbackReason, "schema") {
		t.Errorf("FallbackReason = %q, want it to name the schema skew", result.FallbackReason)
	}
}

// TestLinkedBeadsLibraryFromBuildInfo covers the build-info reader that feeds
// checkBDVersionHint in production. A replace directive — of either form — means
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
