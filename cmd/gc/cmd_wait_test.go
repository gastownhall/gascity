package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type waitErrorStore struct {
	*beads.MemStore
}

type waitNudgeMetadataFailStore struct {
	*beads.MemStore
}

func (s waitNudgeMetadataFailStore) SetMetadata(id, key, value string) error {
	if key == "nudge_id" {
		return errors.New("set nudge id failed")
	}
	return s.MemStore.SetMetadata(id, key, value)
}

var (
	waitTestRealBDPathOnce sync.Once
	waitTestRealBDCached   string
	waitTestRealBDErr      error

	managedBdWaitTemplateOnce sync.Once
	managedBdWaitTemplatePath string
	managedBdWaitTemplateErr  error
)

func waitTestEnv(overrides map[string]string) []string {
	env := map[string]string{}
	for _, entry := range sanitizedBaseEnv() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	for key, value := range overrides {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func waitTestRealBDPath(t *testing.T) string {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a managed bd lifecycle city; run without -short or via integration packages")
	waitTestRealBDPathOnce.Do(func() {
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if strings.TrimSpace(dir) == "" {
				continue
			}
			candidate := filepath.Join(dir, "bd")
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() {
				continue
			}
			cmd := exec.Command(candidate, "init", "--help")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), `unknown subcommand "init"`) {
				waitTestRealBDCached = candidate
				return
			}
		}
		waitTestRealBDErr = errors.New("bd with init not installed")
	})
	if waitTestRealBDErr != nil {
		t.Skip(waitTestRealBDErr.Error())
	}
	return waitTestRealBDCached
}

func writeWaitTestDoltIdentity(homeDir string) error {
	if err := os.MkdirAll(filepath.Join(homeDir, ".dolt"), 0o755); err != nil {
		return err
	}
	doltConfig := `{"user.name":"gc-test","user.email":"gc-test@example.com"}`
	return os.WriteFile(filepath.Join(homeDir, ".dolt", "config_global.json"), []byte(doltConfig), 0o644)
}

func writeManagedBdWaitTestCityScaffold(cityPath string) (string, error) {
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		return "", err
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		return "", err
	}
	return rigPath, nil
}

func managedBdWaitTestTemplate(t *testing.T, bdPath, doltPath string) string {
	t.Helper()
	managedBdWaitTemplateOnce.Do(func() {
		cityPath, err := os.MkdirTemp("/tmp", "gc-bd-template-city-")
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("MkdirTemp(template city): %w", err)
			return
		}
		rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("write template scaffold: %w", err)
			return
		}
		if err := MaterializeBuiltinPacks(cityPath); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("MaterializeBuiltinPacks(template): %w", err)
			return
		}
		script := gcBeadsBdScriptPath(cityPath)
		homeDir, err := os.MkdirTemp("/tmp", "gc-bd-template-home-")
		if err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("MkdirTemp(template home): %w", err)
			return
		}
		if err := writeWaitTestDoltIdentity(homeDir); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("write template dolt identity: %w", err)
			return
		}
		env := waitTestEnv(map[string]string{
			"GC_BEADS":       "bd",
			"GC_DOLT":        "",
			"GC_BIN":         currentGCBinaryForTests(t),
			"GC_CITY":        cityPath,
			"GC_CITY_PATH":   cityPath,
			"HOME":           homeDir,
			"DOLT_ROOT_PATH": homeDir,
			"PATH":           strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)),
		})
		runScript := func(args ...string) error {
			cmd := exec.Command(script, args...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, out)
			}
			return nil
		}
		if err := runScript("start"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		if err := runScript("init", cityPath, "gc", "hq"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		if err := runScript("init", rigPath, "fe", "fe"); err != nil {
			managedBdWaitTemplateErr = err
			return
		}
		stopCmd := exec.Command(script, "stop")
		stopCmd.Env = env
		if out, err := stopCmd.CombinedOutput(); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("stop template city: %w\n%s", err, out)
			return
		}
		if err := clearManagedDoltRuntimeState(cityPath); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("clear published dolt runtime state: %w", err)
			return
		}
		if err := removeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath)); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("remove provider dolt runtime state: %w", err)
			return
		}
		if err := os.RemoveAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")); err != nil {
			managedBdWaitTemplateErr = fmt.Errorf("remove template runtime pack state: %w", err)
			return
		}
		removeDoltPortFile(cityPath)
		removeDoltPortFile(rigPath)
		managedBdWaitTemplatePath = cityPath
	})
	if managedBdWaitTemplateErr != nil {
		t.Fatal(managedBdWaitTemplateErr)
	}
	return managedBdWaitTemplatePath
}

func (s waitErrorStore) ListByLabel(label string, limit int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	if label == waitBeadLabel {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.ListByLabel(label, limit)
}

func (s waitErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == waitBeadLabel {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.List(query)
}

func TestPrepareWaitWakeState_MarksDepsReady(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if updated.Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func TestPrepareWaitWakeState_FailsMissingDependencyWait(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"wait_hold":          "true",
			"sleep_reason":       "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          "gc-missing",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet unexpectedly contains session %s", sessionBead.ID)
	}

	updatedWait, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updatedWait.Metadata["state"]; got != waitStateFailed {
		t.Fatalf("wait state = %q, want %q", got, waitStateFailed)
	}
	if updatedWait.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updatedWait.Status)
	}
	if updatedWait.Metadata["failed_at"] == "" {
		t.Fatal("failed_at was not recorded")
	}
	if updatedWait.Metadata["last_error"] == "" {
		t.Fatal("last_error was not recorded")
	}

	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updatedSession.Metadata["wait_hold"] != "" {
		t.Fatalf("wait_hold = %q, want cleared", updatedSession.Metadata["wait_hold"])
	}
	if updatedSession.Metadata["sleep_reason"] != "" {
		t.Fatalf("sleep_reason = %q, want cleared", updatedSession.Metadata["sleep_reason"])
	}
}

func TestPrepareWaitWakeState_FinalizesFromNudge(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(waitBead)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id":           nudgeID,
			"state":              "injected",
			"commit_boundary":    "provider-nudge-return",
			"terminal_reason":    "",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if readyWaitSet[sessionBead.ID] {
		t.Fatalf("session %s should not remain in ready set after terminal nudge", sessionBead.ID)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateClosed {
		t.Fatalf("wait state = %q, want %q", got, waitStateClosed)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestDepsWaitReady_IgnoresEmptyDependencyEntries(t *testing.T) {
	store := beads.NewMemStore()
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	ready := depsWaitReady(store, beads.Bead{
		Metadata: map[string]string{
			"dep_ids":  dep.ID + ", ,",
			"dep_mode": "all",
		},
	})
	if !ready {
		t.Fatal("depsWaitReady = false, want true with only one real closed dependency")
	}
}

func TestNextWaitDeliveryAttempt_IncrementsAfterTerminalNudge(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel},
		Metadata: map[string]string{
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(wait)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	next, err := nextWaitDeliveryAttempt(store, wait)
	if err != nil {
		t.Fatalf("nextWaitDeliveryAttempt: %v", err)
	}
	if next != "2" {
		t.Fatalf("nextWaitDeliveryAttempt = %q, want 2", next)
	}
}

func TestRetryClosedWait_CreatesReplacement(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "2",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	nudgeID := waitNudgeID(wait)
	nudge, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Title:  "nudge:" + nudgeID,
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := store.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.ID == wait.ID {
		t.Fatal("retryClosedWait reused original wait ID")
	}
	if retried.Type != waitBeadType {
		t.Fatalf("retried type = %q, want %q", retried.Type, waitBeadType)
	}
	if retried.Metadata["state"] != waitStateReady {
		t.Fatalf("retried state = %q, want %q", retried.Metadata["state"], waitStateReady)
	}
	if retried.Metadata["delivery_attempt"] != "2" {
		t.Fatalf("retried attempt = %q, want 2", retried.Metadata["delivery_attempt"])
	}
	if retried.Metadata["registered_epoch"] != "2" {
		t.Fatalf("retried registered_epoch = %q, want 2", retried.Metadata["registered_epoch"])
	}
	if retried.Metadata["retried_from_wait"] != wait.ID {
		t.Fatalf("retried_from_wait = %q, want %q", retried.Metadata["retried_from_wait"], wait.ID)
	}
	if retried.Status == "closed" {
		t.Fatalf("retried wait status = %q, want open", retried.Status)
	}
}

func TestRetryClosedWait_DropsInternalMetadata(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id":         "gc-session",
			"session_name":       "worker",
			"kind":               "deps",
			"state":              waitStateFailed,
			"dep_ids":            "gc-1",
			"dep_mode":           "all",
			"registered_epoch":   "1",
			"delivery_attempt":   "1",
			"created_by_session": "gc-origin",
			"nudge_id":           "wait-gc-1-1-1",
			"last_error":         "boom",
			"synced_at":          "2026-03-16T10:00:00Z",
			"future_internal":    "should-not-carry",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.Metadata["dep_ids"] != "gc-1" {
		t.Fatalf("dep_ids = %q, want gc-1", retried.Metadata["dep_ids"])
	}
	if retried.Metadata["created_by_session"] != "gc-origin" {
		t.Fatalf("created_by_session = %q, want gc-origin", retried.Metadata["created_by_session"])
	}
	if retried.Metadata["nudge_id"] != "" {
		t.Fatalf("nudge_id = %q, want cleared", retried.Metadata["nudge_id"])
	}
	if retried.Metadata["last_error"] != "" {
		t.Fatalf("last_error = %q, want cleared", retried.Metadata["last_error"])
	}
	if retried.Metadata["synced_at"] != "" {
		t.Fatalf("synced_at = %q, want omitted", retried.Metadata["synced_at"])
	}
	if retried.Metadata["future_internal"] != "" {
		t.Fatalf("future_internal = %q, want omitted", retried.Metadata["future_internal"])
	}
}

func TestRetryClosedWait_PreservesNonDepsMetadata(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Title:       "wait:worker",
		Description: "Retry me.",
		Labels:      []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id":       "gc-session",
			"session_name":     "worker",
			"kind":             "probe",
			"state":            waitStateFailed,
			"registered_epoch": "1",
			"delivery_attempt": "1",
			"probe_name":       "github-pr-approval",
			"probe_target":     "owner/repo#123",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	if err := store.Close(wait.ID); err != nil {
		t.Fatalf("close wait bead: %v", err)
	}

	retried, err := retryClosedWait(store, wait, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("retryClosedWait: %v", err)
	}
	if retried.Metadata["kind"] != "probe" {
		t.Fatalf("kind = %q, want probe", retried.Metadata["kind"])
	}
	if retried.Metadata["probe_name"] != "github-pr-approval" {
		t.Fatalf("probe_name = %q, want github-pr-approval", retried.Metadata["probe_name"])
	}
	if retried.Metadata["probe_target"] != "owner/repo#123" {
		t.Fatalf("probe_target = %q, want owner/repo#123", retried.Metadata["probe_target"])
	}
}

func TestDispatchReadyWaitNudges_EnqueuesDeterministicNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + sessionBead.ID},
		Description: "Continue after review closes.",
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now().UTC())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	wantID := waitNudgeID(waitBead)
	if pending[0].ID != wantID {
		t.Fatalf("queued nudge id = %q, want %q", pending[0].ID, wantID)
	}
	if pending[0].SessionID != sessionBead.ID {
		t.Fatalf("queued nudge session_id = %q, want %q", pending[0].SessionID, sessionBead.ID)
	}
	if pending[0].Reference == nil || pending[0].Reference.ID != waitBead.ID {
		t.Fatalf("queued nudge reference = %#v, want wait bead %s", pending[0].Reference, waitBead.ID)
	}
	if pending[0].BeadID == "" {
		t.Fatal("queued nudge bead_id is empty")
	}
	refreshedStore, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt(refresh): %v", err)
	}
	if _, err := refreshedStore.Get(pending[0].BeadID); err != nil {
		t.Fatalf("refreshedStore.Get(%s): %v", pending[0].BeadID, err)
	}
}

func TestDispatchReadyWaitNudges_StartsCodexPoller(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "worker" || sessionName != "worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestDispatchReadyWaitNudges_PropagatesNudgeIDMetadataFailure(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := waitNudgeMetadataFailStore{MemStore: beads.NewMemStore()}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "setting wait nudge_id") {
		t.Fatalf("dispatchReadyWaitNudges error = %v, want nudge_id failure", err)
	}
}

func TestDispatchReadyWaitNudges_PropagatesPollerFailure(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"provider":           "codex",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error {
		return errors.New("poller failed")
	}
	t.Cleanup(func() { startNudgePoller = prev })

	err = dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "starting wait nudge poller") {
		t.Fatalf("dispatchReadyWaitNudges error = %v, want poller failure", err)
	}
}

func TestWithdrawQueuedWaitNudges_RemovesQueuedNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudgeWithOptions("worker", "Wait satisfied.", "wait", time.Now().Add(-time.Minute), queuedNudgeOptions{
		ID:        "wait-gc-1-1-1",
		Reference: &nudgeReference{Kind: "bead", ID: "gc-1"},
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	if err := withdrawQueuedWaitNudges(dir, []string{item.ID}); err != nil {
		t.Fatalf("withdrawQueuedWaitNudges: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	nudge, ok, err := findAnyQueuedNudgeBead(store, item.ID)
	if err != nil {
		t.Fatalf("findAnyQueuedNudgeBead: %v", err)
	}
	if !ok {
		t.Fatal("findAnyQueuedNudgeBead returned not found")
	}
	if nudge.Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", nudge.Status)
	}
	if nudge.Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("terminal_reason = %q, want wait-canceled", nudge.Metadata["terminal_reason"])
	}
}

func TestCancelWaitsForSession(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      waitStatePending,
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	if err := cancelWaitsForSession(store, sessionBead.ID); err != nil {
		t.Fatalf("cancelWaitsForSession: %v", err)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("wait status = %q, want closed", updated.Status)
	}
}

func TestLoadSessionWaitBeads_IncludesLegacyWaitType(t *testing.T) {
	store := beads.NewMemStore()
	sessionID := "gc-session"
	if _, err := store.Create(beads.Bead{
		Type:   sessionpkg.LegacyWaitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionID},
		Metadata: map[string]string{
			"session_id": sessionID,
			"state":      waitStatePending,
		},
	}); err != nil {
		t.Fatalf("create legacy wait bead: %v", err)
	}

	waits, err := loadSessionWaitBeads(store, sessionID)
	if err != nil {
		t.Fatalf("loadSessionWaitBeads: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("loadSessionWaitBeads returned %d waits, want 1", len(waits))
	}
	if waits[0].Type != sessionpkg.LegacyWaitBeadType {
		t.Fatalf("wait type = %q, want legacy %q", waits[0].Type, sessionpkg.LegacyWaitBeadType)
	}
}

func TestClearSessionWaitHoldIfIdle_PropagatesWaitLoadError(t *testing.T) {
	store := waitErrorStore{MemStore: beads.NewMemStore()}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"wait_hold":    "true",
			"sleep_intent": "wait-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	if err := clearSessionWaitHoldIfIdle(store, sessionBead.ID); err == nil {
		t.Fatal("expected clearSessionWaitHoldIfIdle to return load error")
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if updated.Metadata["wait_hold"] != "true" {
		t.Fatalf("wait_hold = %q, want true", updated.Metadata["wait_hold"])
	}
	if updated.Metadata["sleep_intent"] != "wait-hold" {
		t.Fatalf("sleep_intent = %q, want wait-hold", updated.Metadata["sleep_intent"])
	}
}

func TestCmdSessionWait_DoesNotMaterializeTemplateTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
	})

	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	dep, err := store.Create(beads.Bead{Title: "dep"})
	if err != nil {
		t.Fatalf("create dep bead: %v", err)
	}
	if err := store.Close(dep.ID); err != nil {
		t.Fatalf("close dep bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionWait([]string{"worker"}, []string{dep.ID}, false, "block", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionWait() = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(sessions))
	}
}

func TestCmdSessionWait_AllowsRigDependencyBeads(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)

	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	sessionBead, err := cityStore.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := rigStore.Create(beads.Bead{Title: "rig dep"})
	if err != nil {
		t.Fatalf("create rig dep bead: %v", err)
	}
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close rig dep bead: %v", err)
	}
	if got := beadPrefix(dep.ID); got != "fe" {
		t.Fatalf("rig dep prefix = %q, want %q", got, "fe")
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionWait([]string{sessionBead.ID}, []string{dep.ID}, false, "block", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionWait() = %d, want success; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	cityStore, err = openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	waits, err := cityStore.ListByLabel("session:"+sessionBead.ID, 0)
	if err != nil {
		t.Fatalf("ListByLabel(wait): %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("wait count = %d, want 1", len(waits))
	}
	if got := waits[0].Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if waits[0].Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func TestPrepareWaitWakeState_ResolvesRigDependencyBeads(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)

	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	sessionBead, err := cityStore.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	dep, err := rigStore.Create(beads.Bead{Title: "rig dep"})
	if err != nil {
		t.Fatalf("create rig dep bead: %v", err)
	}
	wait, err := cityStore.Create(beads.Bead{
		Title:  "wait:worker session",
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	if err := rigStore.Close(dep.ID); err != nil {
		t.Fatalf("close rig dep bead: %v", err)
	}
	if got := beadPrefix(dep.ID); got != "fe" {
		t.Fatalf("rig dep prefix = %q, want %q", got, "fe")
	}
	cityStore, err = openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}

	readyWaitSet, err := prepareWaitWakeStateForCity(cityPath, cityStore, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeStateForCity: %v", err)
	}
	if !readyWaitSet[sessionBead.ID] {
		t.Fatalf("readyWaitSet missing session %s", sessionBead.ID)
	}
	updatedWait, err := cityStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updatedWait.Metadata["state"]; got != waitStateReady {
		t.Fatalf("wait state = %q, want %q", got, waitStateReady)
	}
	if updatedWait.Metadata["ready_at"] == "" {
		t.Fatal("ready_at was not recorded")
	}
}

func setupFreshManagedBdWaitTestCity(t *testing.T) (string, string) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")

	homeDir := filepath.Join(shortSocketTempDir(t, "gc-bd-home-"), "home")
	if err := writeWaitTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("writeWaitTestDoltIdentity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return currentGCBinaryForTests(t) }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
	})

	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
	if err != nil {
		t.Fatalf("writeManagedBdWaitTestCityScaffold: %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityPath)
	})
	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(rig): %v", err)
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}
	return cityPath, rigPath
}

func setupManagedBdWaitTestCity(t *testing.T) (string, string) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")

	homeDir := filepath.Join(shortSocketTempDir(t, "gc-bd-home-"), "home")
	if err := writeWaitTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("writeWaitTestDoltIdentity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return currentGCBinaryForTests(t) }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
	})

	templatePath := managedBdWaitTestTemplate(t, bdPath, doltPath)
	cityPath := shortSocketTempDir(t, "gc-bd-city-")
	if err := overlay.CopyDir(templatePath, cityPath, io.Discard); err != nil {
		t.Fatalf("overlay.CopyDir(template city): %v", err)
	}
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.Chmod(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("Chmod(city .beads): %v", err)
	}
	if err := os.Chmod(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatalf("Chmod(rig .beads): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_DOLT_STATE_FILE", poisonStateFile)
	scriptEnv := sanitizedBaseEnv(
		"GC_CITY="+cityPath,
		"GC_CITY_PATH="+cityPath,
	)
	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = scriptEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_, _ = cmd.CombinedOutput()
	})

	runScript("start")
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("start leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}
	return cityPath, rigPath
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc wait list` and `gc wait inspect`
// (ADR 0001, ga-h6w, ga-2fr). Each row exercises one branch of routeWaitList
// / routeWaitInspect. The matrix is enforced by scripts/check-routed-test-rows.sh:
//
//   api-happy-path       API returns 200 with items         route=api, exit 0
//   api-cache-not-live   API returns 503 cache_not_live     fallback, exit 0
//   api-500-fallback     API returns generic 500            fallback (conn-refused), exit 0
//   api-404-error        API returns 404                    no fallback, exit 1
//   controller-down      apiClient returns nil (no env)     fallback (controller-down), exit 0
//   escape-hatch         GC_NO_API truthy                   fallback (escape-hatch), exit 0
//
// Wait beads are located via the existing beads endpoint using the
// sessionpkg.WaitBeadLabel contract — no new server surface exists for waits.
// ---------------------------------------------------------------------------

type waitMatrixHandler func(t *testing.T) http.Handler

// okWaitListHandler returns a 200 with one gc:wait-labeled gate bead, mirroring
// what the supervisor would emit for GET /v0/city/{name}/beads?label=gc:wait.
func okWaitListHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/beads") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"id":         "ga-wait-1",
					"title":      "wait:worker",
					"issue_type": sessionpkg.WaitBeadType,
					"status":     "open",
					"labels":     []string{sessionpkg.WaitBeadLabel, "session:ga-sess-1"},
					"metadata": map[string]string{
						"session_id": "ga-sess-1",
						"state":      waitStatePending,
						"kind":       "deps",
					},
					"description": "wait note",
				},
			},
			"total": 1,
		})
	})
}

// okWaitInspectHandler returns a 200 for a single wait bead, mirroring GET
// /v0/city/{name}/bead/{id}.
func okWaitInspectHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/bead/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "ga-wait-1",
			"title":      "wait:worker",
			"issue_type": sessionpkg.WaitBeadType,
			"status":     "open",
			"labels":     []string{sessionpkg.WaitBeadLabel, "session:ga-sess-1"},
			"metadata": map[string]string{
				"session_id":       "ga-sess-1",
				"state":            waitStatePending,
				"kind":             "deps",
				"dep_ids":          "gc-1",
				"dep_mode":         "all",
				"registered_epoch": "1",
				"delivery_attempt": "1",
			},
			"description": "wait note",
		})
	})
}

func waitProblemHandler(status int, detail string) waitMatrixHandler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// writeWaitTestCity prepares a file-provider city for fallback path tests.
// Mirrors writeBeadsTestCity but tagged for wait tests; kept separate so either
// file can evolve its city.toml independently.
func writeWaitTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	return cityPath
}

func TestRouteWaitList_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      waitMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okWaitListHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "ga-wait-1",
		},
		{
			name:       "api-cache-not-live",
			handler:    waitProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
			wantStdout: "WAIT",
		},
		{
			name:       "api-500-fallback",
			handler:    waitProblemHandler(http.StatusInternalServerError, "internal: explode"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
			wantStdout: "WAIT",
		},
		{
			name:       "api-404-error",
			handler:    waitProblemHandler(http.StatusNotFound, "not_found: city missing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
			wantStdout:   "WAIT",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
			wantStdout:   "WAIT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeWaitTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeWaitList(cityPath, c, tc.nilReason, "", "", &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

func TestRouteWaitInspect_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      waitMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okWaitInspectHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "ga-wait-1",
		},
		{
			name:       "api-cache-not-live",
			handler:    waitProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
			wantStderr: "not found",
		},
		{
			name:       "api-500-fallback",
			handler:    waitProblemHandler(http.StatusInternalServerError, "explode"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
			wantStderr: "not found",
		},
		{
			name:       "api-404-error",
			handler:    waitProblemHandler(http.StatusNotFound, "not_found: bead missing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
			wantStderr:   "not found",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
			wantStderr:   "not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeWaitTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeWaitInspect(cityPath, c, tc.nilReason, "ga-missing", &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteWaitList_PassesWaitBeadLabelConstant locks in the architect's §5.1
// guardrail: the CLI must pass sessionpkg.WaitBeadLabel through to
// ListBeadsOpts.Label. Renaming the constant or inlining "gc:wait" on either
// side breaks the locator contract without a loud test.
func TestRouteWaitList_PassesWaitBeadLabelConstant(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeWaitTestCity(t)

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("label")
		w.Header().Set("X-GC-Cache-Age-S", "0")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeWaitList(cityPath, c, "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if gotQuery != sessionpkg.WaitBeadLabel {
		t.Errorf("API label query = %q, want %q", gotQuery, sessionpkg.WaitBeadLabel)
	}
}

// TestRouteWaitList_StaleBannerOver30s confirms the >30 s cache-age banner
// contract (parity with gc beads list API path).
func TestRouteWaitList_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeWaitTestCity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeWaitList(cityPath, c, "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

// TestRenderWaitListFromAPI_FiltersNonWaitBeads guards the architect's §5.4
// guardrail: a non-wait bead labeled gc:wait must not leak through to the
// rendered output. IsWaitBead is the type guard that enforces it.
func TestRenderWaitListFromAPI_FiltersNonWaitBeads(t *testing.T) {
	cr := api.CachedRead[[]beads.Bead]{
		Body: []beads.Bead{
			{
				ID:       "ga-wait-keep",
				Type:     sessionpkg.WaitBeadType,
				Status:   "open",
				Labels:   []string{sessionpkg.WaitBeadLabel},
				Metadata: map[string]string{"state": waitStatePending},
			},
			{
				ID:       "ga-task-drop",
				Type:     "task",
				Status:   "open",
				Labels:   []string{sessionpkg.WaitBeadLabel},
				Metadata: map[string]string{},
			},
			{
				ID:       "ga-closed-drop",
				Type:     sessionpkg.WaitBeadType,
				Status:   "closed",
				Labels:   []string{sessionpkg.WaitBeadLabel},
				Metadata: map[string]string{},
			},
			{
				ID:       "ga-legacy-keep",
				Type:     sessionpkg.LegacyWaitBeadType,
				Status:   "open",
				Labels:   []string{sessionpkg.WaitBeadLabel},
				Metadata: map[string]string{"state": waitStatePending},
			},
		},
		AgeSeconds: 1,
	}

	var stdout bytes.Buffer
	if code := renderWaitListFromAPI(cr, "", "", &stdout); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "ga-wait-keep") {
		t.Errorf("expected wait-typed bead to render:\n%s", out)
	}
	if !strings.Contains(out, "ga-legacy-keep") {
		t.Errorf("expected legacy wait-typed bead to render:\n%s", out)
	}
	if strings.Contains(out, "ga-task-drop") {
		t.Errorf("task-typed bead with gc:wait label leaked into output:\n%s", out)
	}
	if strings.Contains(out, "ga-closed-drop") {
		t.Errorf("closed wait leaked into default (--all=false) output:\n%s", out)
	}
}

// TestRenderWaitInspectFromAPI_RejectsNonWait verifies the §5.4 guardrail on
// the inspect path: GET /bead/{id} can return any bead ID, so IsWaitBead must
// still gate the API path.
func TestRenderWaitInspectFromAPI_RejectsNonWait(t *testing.T) {
	cr := api.CachedRead[beads.Bead]{
		Body: beads.Bead{
			ID:       "ga-task",
			Type:     "task",
			Status:   "open",
			Labels:   []string{"something-else"},
			Metadata: map[string]string{},
		},
	}

	var stdout, stderr bytes.Buffer
	code := renderWaitInspectFromAPI(cr, "ga-task", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "is not a wait") {
		t.Errorf("stderr missing 'is not a wait':\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on non-wait rejection, got:\n%s", stdout.String())
	}
}
