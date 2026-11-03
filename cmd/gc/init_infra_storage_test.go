package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestFreshGasCityScaffoldRecordsNativeInfrastructureBeforeRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	fs := fsys.NewFake()
	city := "/new-gascity"
	var stdout, stderr bytes.Buffer
	if code := doInit(fs, city, wizardConfig{configName: "gascity", provider: "codex"}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("scaffold = %d: %s", code, stderr.String())
	}
	witness := filepath.Join(city, ".gc", "init-infrastructure.json")
	if _, err := fs.ReadFile(witness); err != nil {
		t.Fatalf("a fresh default city must record native infrastructure intent before the API can register/start it: %v", err)
	}
}

func nativeInfrastructureFixture(t *testing.T) (string, []byte, initInfrastructurePlan) {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("GC_HOME", t.TempDir())
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("# operator-owned comment\n[workspace]\nprovider = \"codex\"\n[beads]\nprovider = \"bd\"\n")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	pack := newInitPackConfig("native-fixture")
	factory := config.GascityCityWithProviders("native-fixture", "codex", []string{"codex"})
	pack.Imports = factory.Imports
	pack.Providers = factory.Providers
	addBuiltinImportsToInitPack(&pack, "bd")
	if _, err := writeInitPackTomlOpts(fsys.OSFS{}, city, pack, false); err != nil {
		t.Fatal(err)
	}
	if err := recordFreshInitInfrastructure(fsys.OSFS{}, city, true); err != nil {
		t.Fatal(err)
	}
	plan, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if err != nil || plan == nil {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	return city, raw, *plan
}

func nativeInfrastructureFakeOps(t *testing.T, plan initInfrastructurePlan) initInfrastructureOps {
	t.Helper()
	return initInfrastructureOps{
		EnsureIdentity: func(context.Context, string, initInfrastructurePlan) error { return nil },
		Run: func(_ context.Context, root string, env []string, args ...string) error {
			for _, e := range env {
				key, value, _ := strings.Cut(e, "=")
				if strings.HasPrefix(key, "BEADS_") && (key != "BEADS_DIR" || value != filepath.Join(root, ".beads")) {
					t.Errorf("ambient Beads selector escaped: %s", key)
				}
				if strings.HasPrefix(key, "BD_") || (strings.HasPrefix(key, "GC_") && key != execenv.UsageMetricsDisableEnv) || strings.HasPrefix(key, "DOLT_") {
					t.Errorf("ambient selector escaped: %s", key)
				}
			}
			if args[0] == "bd" && args[1] == "init" {
				joined := strings.Join(args, " ")
				for _, want := range []string{"--server", "--shared-server=false", "--database " + plan.Database, "-p gcg", "--skip-hooks", "--skip-agents"} {
					if !strings.Contains(joined, want) {
						t.Errorf("missing %s from %v", want, args)
					}
				}
				if strings.Contains(joined, "--server-host") || strings.Contains(joined, "--server-port") {
					t.Error("pinned a foreign endpoint")
				}
				if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o700); err != nil {
					return err
				}
				md := initInfrastructureMetadata{Backend: "dolt", Mode: "server", Database: plan.Database, ProjectID: "owned-project"}
				b, _ := json.Marshal(md)
				return os.WriteFile(filepath.Join(root, ".beads", "metadata.json"), b, 0o600)
			}
			return nil
		},
		Verify: func(context.Context, string, initInfrastructurePlan) error { return nil },
	}
}

func TestNativeInfrastructureFailedProvisionResumesSameIdentity(t *testing.T) {
	city, raw, plan := nativeInfrastructureFixture(t)
	ops := nativeInfrastructureFakeOps(t, plan)
	run := ops.Run
	failed := false
	ops.Run = func(ctx context.Context, root string, env []string, args ...string) error {
		if args[0] == "bd" && args[1] == "init" && !failed {
			failed = true
			return errors.New("bounded init failure")
		}
		return run(ctx, root, env, args...)
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil {
		t.Fatal("failed init reported ready")
	}
	got, _ := os.ReadFile(filepath.Join(city, "city.toml"))
	if !bytes.Equal(got, raw) {
		t.Fatal("failed init changed config")
	}
	// A separate continuation resolves only durable filesystem intent.
	if err := resumeInitInfrastructureWithOps(context.Background(), city, nativeInfrastructureFakeOps(t, plan)); err != nil {
		t.Fatal(err)
	}
	recovered, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if err != nil || !recovered.Ready || recovered.Database != plan.Database || recovered.Owner != plan.Owner {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage == nil || cfg.Storage.Classes.Sessions != "infra" || cfg.Storage.Classes.Work != "work" || cfg.Storage.Bindings["infra"].Provider != "beads-workspace" {
		t.Fatalf("wrong storage: %+v", cfg.Storage)
	}
	if !slices.Equal(cfg.Ops.Authoritative, []string{"message", "interrupt", "respond"}) {
		t.Fatalf("authoritative=%v", cfg.Ops.Authoritative)
	}
	got, _ = os.ReadFile(filepath.Join(city, "city.toml"))
	if !bytes.HasPrefix(got, raw) {
		t.Fatal("lost original config/comments")
	}
	scope := filepath.Join(city, ".gc", "storage", "infra")
	roots, err := providerOwnedLifecycleScopeRoots(city, "stop")
	if err != nil || !slices.Contains(roots, scope) {
		t.Fatalf("owned workspace absent from stop scopes: %v %v", roots, err)
	}
}

func TestNativeInfrastructureProbeFailureAndIdentityChangeRefuse(t *testing.T) {
	city, raw, plan := nativeInfrastructureFixture(t)
	ops := nativeInfrastructureFakeOps(t, plan)
	ops.Verify = func(context.Context, string, initInfrastructurePlan) error {
		return errors.New("guarded batch unavailable")
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil {
		t.Fatal("failed probe reported ready")
	}
	stored, _ := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if stored.ProjectID != "owned-project" || stored.Ready {
		t.Fatalf("identity not retained: %+v", stored)
	}
	path := filepath.Join(city, ".gc", "storage", "infra", ".beads", "metadata.json")
	md := initInfrastructureMetadata{Backend: "dolt", Mode: "server", Database: plan.Database, ProjectID: "another-project"}
	b, _ := json.Marshal(md)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, nativeInfrastructureFakeOps(t, plan)); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("foreign project not refused: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(city, "city.toml"))
	if !bytes.Equal(got, raw) {
		t.Fatal("failed probe changed config")
	}
}

func TestNativeInfrastructureRefusesUserConfigChanges(t *testing.T) {
	city, raw, plan := nativeInfrastructureFixture(t)
	ops := nativeInfrastructureFakeOps(t, plan)
	changed := append(append([]byte(nil), raw...), []byte("\n# user edit during probe\n")...)
	ops.Verify = func(context.Context, string, initInfrastructurePlan) error {
		return os.WriteFile(filepath.Join(city, "city.toml"), changed, 0o600)
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("user change not refused: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(city, "city.toml"))
	if !bytes.Equal(got, changed) {
		t.Fatal("overwrote user edit")
	}
}

func TestNativeInfrastructureRecoversLostConfigCommitAck(t *testing.T) {
	city, _, plan := nativeInfrastructureFixture(t)
	ops := nativeInfrastructureFakeOps(t, plan)
	ops.Commit = func(path string, data []byte, mode os.FileMode) error {
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, data, mode); err != nil {
			return err
		}
		return errors.New("lost config acknowledgment")
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil {
		t.Fatal("lost acknowledgment reported success")
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, nativeInfrastructureFakeOps(t, plan)); err != nil {
		t.Fatal(err)
	}
	stored, _ := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if !stored.Ready || stored.Database != plan.Database {
		t.Fatalf("did not recover original intent: %+v", stored)
	}
}

func TestNativeInfrastructureExplicitStorageOpsAndExistingDataStayUntouched(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	for _, suffix := range []string{"\n[ops]\nauthoritative=[]\n", "\n[storage.classes]\nwork=\"work\"\ngraph=\"work\"\nsessions=\"work\"\nmessaging=\"work\"\norders=\"work\"\nnudges=\"work\"\n"} {
		fs := fsys.NewFake()
		city := "/authored"
		if err := fs.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
			t.Fatal(err)
		}
		raw := []byte("[workspace]\nprovider=\"codex\"\n" + suffix)
		if err := fs.WriteFile(filepath.Join(city, "city.toml"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := recordFreshInitInfrastructure(fs, city, true); err != nil {
			t.Fatal(err)
		}
		if plan, _ := readInitInfrastructurePlan(fs, city); plan != nil {
			t.Fatalf("explicit configuration was replaced: %s", suffix)
		}
	}
	fs := fsys.NewFake()
	city := "/existing"
	if err := fs.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if freshInitInfrastructureCandidate(fs, city, false) {
		t.Fatal("existing beads may not auto-split")
	}
	if freshInitInfrastructureCandidate(fs, "/new", true) {
		t.Fatal("adopt/preserve may not auto-split")
	}
}

func TestNativeInfrastructureHTTPScaffoldDefersAndResumesBeforeBootstrap(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("GC_HOME", t.TempDir())
	city := filepath.Join(t.TempDir(), "api-native-city")
	oldOps := initInfrastructureDefaultOps
	oldReload := reloadSupervisorNoWaitHook
	t.Cleanup(func() { initInfrastructureDefaultOps = oldOps; reloadSupervisorNoWaitHook = oldReload })
	reloads := 0
	reloadSupervisorNoWaitHook = func() error {
		reloads++
		plan, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
		if err != nil || plan == nil || plan.Ready {
			t.Fatalf("registration did not preserve a pending intent: %+v %v", plan, err)
		}
		initInfrastructureDefaultOps = nativeInfrastructureFakeOps(t, *plan)
		cfg, _, err := loadSupervisorCityConfig(city)
		if err != nil {
			return err
		}
		if cfg.Storage == nil || cfg.Storage.Classes.Graph != "infra" {
			t.Errorf("bootstrap loaded old work-only config: %+v", cfg.Storage)
		}
		return nil
	}
	svc := mustNewCityInitService(t)
	mux := api.NewSupervisorMux(fakeDashResolver{}, svc, false, "test", "test", time.Time{}).WithAnyHostAllowed()
	body, _ := json.Marshal(map[string]string{"dir": city, "provider": "codex", "template": "gascity"})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v0/city", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	req.Header.Set("Idempotency-Key", "native-infra-http")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("HTTP create=%d %s", rr.Code, rr.Body.String())
	}
	if reloads != 1 {
		t.Fatalf("deferred bootstrap reloads=%d", reloads)
	}
	plan, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if err != nil || plan == nil || !plan.Ready {
		t.Fatalf("HTTP scaffold left unready infrastructure: %+v %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(city, ".beads", "metadata.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap wrote work data before infrastructure ready: %v", err)
	}
}

func TestNativeInfrastructureDryRunDoesNotProvisionPendingCity(t *testing.T) {
	city, raw, plan := nativeInfrastructureFixture(t)
	oldDry := dryRunMode
	oldOps := initInfrastructureDefaultOps
	t.Cleanup(func() { dryRunMode = oldDry; initInfrastructureDefaultOps = oldOps })
	dryRunMode = true
	ops := nativeInfrastructureFakeOps(t, plan)
	ops.Run = func(context.Context, string, []string, ...string) error {
		t.Fatal("dry run launched infrastructure")
		return nil
	}
	initInfrastructureDefaultOps = ops
	if _, _, err := loadStartCityConfig(city); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(city, "city.toml"))
	if !bytes.Equal(got, raw) {
		t.Fatal("dry run changed config")
	}
}
