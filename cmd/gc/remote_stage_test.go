package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

const remoteStageSplitSource = `[workspace]
name = "template"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

// newRemoteStageSource is a controller city directory with a split
// [storage], as initCityInPod copies it into a pod.
func newRemoteStageSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "controller-city")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "city.toml"), []byte(remoteStageSplitSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pack.toml"), []byte("[pack]\nname = \"template\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScopeProjectID(t, src, "prj-controller")
	return src
}

func setInitRemoteStage(t *testing.T, controller string) {
	t.Helper()
	prev := initRemoteStageFlag
	t.Cleanup(func() { initRemoteStageFlag = prev })
	initRemoteStageFlag = controller
}

// TestRemoteStagePodInitThenInfraCommandRefusesAndCreatesNoBinding is the
// pod simulation of S5 §10.7: `gc init --from … --remote-stage` into a temp
// dir, then an infrastructure-class write. It must refuse, naming the
// controller's city, and must not create a pod-local .gc/store (genesis)
// that the pod would then serve mail, nudges and sessions from.
func TestRemoteStagePodInitThenInfraCommandRefusesAndCreatesNoBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	src := newRemoteStageSource(t)
	pod := filepath.Join(t.TempDir(), "workspace")
	setInitRemoteStage(t, "/city")

	var stdout, stderr bytes.Buffer
	if code := doInitFromDirWithOptionsInternal(src, pod, "", &stdout, &stderr, true, true, hostedDoltInitOptions{}); code != 0 {
		t.Fatalf("gc init --from --remote-stage = %d; stderr=%s", code, stderr.String())
	}
	stage, ok, err := readRemoteStage(pod)
	if err != nil || !ok {
		t.Fatalf("the pod has no remote-stage record: ok=%v err=%v", ok, err)
	}
	if stage.ControllerPath != "/city" || stage.ControllerCity != "template" {
		t.Errorf("remote-stage record = %+v, want the controller's path and its city name (not the pod directory's)", stage)
	}
	if raw, _ := os.ReadFile(remoteStagePath(pod)); strings.Contains(string(raw), src) {
		t.Errorf("the record names the staging copy %s: %s", src, raw)
	}

	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(pod, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loading the staged config: %v", err)
	}
	var gateErr bytes.Buffer
	routes, err := storageBootGate(pod, cfg, "gc mail send", nil, &gateErr)
	if err == nil {
		_ = routes.close()
		t.Fatal("an infrastructure-class command on a remote stage was served")
	}
	if !strings.Contains(err.Error(), "remote stage of city") || !strings.Contains(err.Error(), "/city") {
		t.Errorf("refusal %q does not say this is a remote stage of the controller's city", err)
	}
	if _, statErr := os.Stat(filepath.Join(pod, ".gc", "store")); !os.IsNotExist(statErr) {
		t.Fatalf("the remote stage created its own binding (.gc/store): %v", statErr)
	}
}

// TestRemoteStageLeavesWorkOnTheWorkStore: the refusal is for the
// infrastructure classes only. Work ids still resolve to the work store.
func TestRemoteStageLeavesWorkOnTheWorkStore(t *testing.T) {
	city := t.TempDir()
	if err := writeRemoteStage(city, remoteStage{ControllerCity: "ctl", ControllerPath: "/city"}); err != nil {
		t.Fatal(err)
	}
	cfg := infraSplitConfig(filepath.Join(city, ".gc", "store"))
	_, err := storageBootGate(city, cfg, "gc bd", nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("remote stage served the split")
	}
	routes := refusingStorageRoutes("infra", err)
	if got := resolveClassStore(routes, nil, cfg, city, config.BeadClassWork, nil); got != nil {
		t.Errorf("work resolved to %T on a remote stage, want the work store", got)
	}
}

// TestRemoteStageWithoutASplitIsUntouched: an unsplit city keeps every
// class on its work store, so a remote stage of it has nothing to refuse.
func TestRemoteStageWithoutASplitIsUntouched(t *testing.T) {
	city := t.TempDir()
	if err := writeRemoteStage(city, remoteStage{ControllerCity: "ctl", ControllerPath: "/city"}); err != nil {
		t.Fatal(err)
	}
	routes, err := storageBootGate(city, &config.City{}, "gc bd", nil, &bytes.Buffer{})
	if err != nil || routes != nil {
		t.Fatalf("an unsplit remote stage = (%v, %v), want the identity path", routes, err)
	}
}

// TestRemoteStageUnreadableRecordRefuses: a record that exists and cannot be
// read is still evidence this is a stage, so it refuses as one.
func TestRemoteStageUnreadableRecordRefuses(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteStagePath(city), []byte("not = [toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := remoteStageStorageRefusal(city); err == nil {
		t.Fatal("an unreadable remote-stage record was taken as absent")
	}
}

// TestInitRemoteStageRequiresFromAndNoStart: a remote stage is never a city
// that starts, and only --from stages one.
func TestInitRemoteStageRequiresFromAndNoStart(t *testing.T) {
	for _, args := range [][]string{
		{"--remote-stage", "/city", "--no-start", t.TempDir()},
		{"--remote-stage", "/city", "--from", t.TempDir(), t.TempDir()},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newInitCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err == nil {
			t.Errorf("gc init %v succeeded", args)
		}
		if !strings.Contains(stderr.String(), "--remote-stage") {
			t.Errorf("gc init %v: stderr %q does not name --remote-stage", args, stderr.String())
		}
	}
}

// TestRemoteStageMarkCommandMarksAnSSHHostsCity: an existing city directory
// (an SSH host's) is marked with gc storage remote-stage mark, after which its
// infrastructure classes refuse; unmark removes the record.
func TestRemoteStageMarkCommandMarksAnSSHHostsCity(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(remoteStageSplitSource), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", city)
	t.Chdir(city)
	run := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		cmd := newStorageCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		err := cmd.Execute()
		return stdout.String() + stderr.String(), err
	}
	if out, err := run("remote-stage", "mark"); err == nil {
		t.Fatalf("mark without --controller succeeded: %s", out)
	}
	if out, err := run("remote-stage", "mark", "--controller", "/srv/city"); err != nil {
		t.Fatalf("mark: %v %s", err, out)
	}
	stage, ok, err := readRemoteStage(city)
	if err != nil || !ok || stage.ControllerPath != "/srv/city" || stage.ControllerCity != "template" {
		t.Fatalf("record = %+v ok=%v err=%v", stage, ok, err)
	}
	cfg := infraSplitConfig(filepath.Join(city, ".gc", "store"))
	if _, err := storageBootGate(city, cfg, "gc mail send", nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "remote stage of city template") {
		t.Fatalf("a marked SSH host's infra command = %v, want the remote-stage refusal", err)
	}
	if out, err := run("remote-stage", "unmark"); err != nil {
		t.Fatalf("unmark: %v %s", err, out)
	}
	if _, ok, _ := readRemoteStage(city); ok {
		t.Fatal("unmark left the record")
	}
}

// TestStorageVerbsRefuseOnARemoteStage (mutant M33): a writing storage verb
// on a remote stage refuses before it takes any lock or opens any store.
func TestStorageVerbsRefuseOnARemoteStage(t *testing.T) {
	request, _ := exclusionCity(t)
	staged, _ := exclusionCity(t)
	if code, stderr := migrateFor(t, staged); code != 0 {
		t.Fatalf("migrating the city recover-stranded runs on = %d %s", code, stderr)
	}
	for _, city := range []string{request.CityPath, staged.CityPath} {
		if err := writeRemoteStage(city, remoteStage{ControllerCity: "ctl", ControllerPath: "/city"}); err != nil {
			t.Fatal(err)
		}
	}
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "remote stage of city ctl") {
		t.Fatalf("migrate on a remote stage = %d %q", code, stderr)
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, request.Cfg), "a remote stage migrates nothing")
	var out, errOut bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), staged, strandedRecoveryOptions{}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "remote stage of city ctl") {
		t.Fatalf("recover-stranded on a remote stage = %d %q", code, errOut.String())
	}
}

// TestRemoteStageEnvRefusesBeforeAnyRecordExists closes the prebaked window
// (S5-A round 2): a pod's processes carry the remote-stage environment from
// the container's start, so a gc command that runs before the on-disk record
// is written still refuses the infrastructure classes and creates no binding.
func TestRemoteStageEnvRefusesBeforeAnyRecordExists(t *testing.T) {
	city := t.TempDir()
	t.Setenv(remoteStageControllerEnv, "/city")
	t.Setenv(remoteStageCityEnv, "bright-lights")
	if _, err := os.Stat(remoteStagePath(city)); !os.IsNotExist(err) {
		t.Fatalf("precondition: a record exists: %v", err)
	}
	cfg := infraSplitConfig(filepath.Join(city, ".gc", "store"))
	_, err := storageBootGate(city, cfg, "gc mail send", nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "remote stage of city bright-lights (/city)") {
		t.Fatalf("an infra command in a pod before its record = %v, want the remote-stage refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(city, ".gc", "store")); !os.IsNotExist(statErr) {
		t.Fatalf("the pod created a local binding before its record existed: %v", statErr)
	}
}
