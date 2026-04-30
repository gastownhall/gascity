package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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

type waitGetSpyStore struct {
	beads.Store
	getIDs []string
}

func (s waitNudgeMetadataFailStore) SetMetadata(id, key, value string) error {
	if key == "nudge_id" {
		return errors.New("set nudge id failed")
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *waitGetSpyStore) Get(id string) (beads.Bead, error) {
	s.getIDs = append(s.getIDs, id)
	return s.Store.Get(id)
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
	skipSlowCmdGCTest(t, "requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage")
	waitTestRealBDPathOnce.Do(func() {
		candidate, err := findPreferredBinary("bd")
		if err != nil {
			waitTestRealBDErr = errors.New("bd with init not installed")
			return
		}
		cmd := exec.Command(candidate, "init", "--help")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), `unknown subcommand "init"`) {
			waitTestRealBDCached = candidate
			return
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

func TestPrepareWaitWakeState_SkipsMissingOpenSessionWithoutBackingGet(t *testing.T) {
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if len(readyWaitSet) != 0 {
		t.Fatalf("readyWaitSet = %#v, want empty for non-open session", readyWaitSet)
	}
	for _, id := range store.getIDs {
		if id == sessionBead.ID {
			t.Fatalf("prepare used Get for non-open session %s; getIDs=%v", sessionBead.ID, store.getIDs)
		}
	}
}

func TestPrepareWaitWakeState_CancelsStaleEpochWaitForClosedSession(t *testing.T) {
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"continuation_epoch": "2",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	readyWaitSet, err := prepareWaitWakeState(store, time.Now().UTC())
	if err != nil {
		t.Fatalf("prepareWaitWakeState: %v", err)
	}
	if len(readyWaitSet) != 0 {
		t.Fatalf("readyWaitSet = %#v, want empty after stale wait cancellation", readyWaitSet)
	}
	updated, err := store.Get(waitBead.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if got := updated.Metadata["state"]; got != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", got, waitStateCanceled)
	}
	if got := updated.Metadata["last_error"]; got != "continuation-stale" {
		t.Fatalf("last_error = %q, want continuation-stale", got)
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

func TestDispatchReadyWaitNudges_UsesOpenSessionSnapshotInsteadOfWorkerRunningCheck(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"template":           "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
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
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	for _, id := range store.getIDs {
		if id == sessionBead.ID {
			t.Fatalf("dispatch used Get for session %s instead of the open-session snapshot; getIDs=%v", sessionBead.ID, store.getIDs)
		}
	}
	for _, call := range sp.Calls {
		switch call.Method {
		case "IsRunning", "ProcessAlive", "IsAttached", "GetLastActivity", "GetMeta":
			t.Fatalf("dispatch should trust cached session state, saw provider call %#v", call)
		}
	}
}

func TestDispatchReadyWaitNudges_SkipsClosedSessionWithoutBackingGet(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	base := beads.NewMemStore()
	store := &waitGetSpyStore{Store: base}
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"agent_name":         "worker",
			"template":           "worker",
			"continuation_epoch": "1",
			"state":              string(sessionpkg.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(sessionBead.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id":       sessionBead.ID,
			"session_name":     "worker",
			"kind":             "deps",
			"state":            waitStateReady,
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	}); err != nil {
		t.Fatalf("create wait bead: %v", err)
	}
	sp := runtime.NewFake()

	if err := dispatchReadyWaitNudges(dir, store, sp, time.Now().UTC()); err != nil {
		t.Fatalf("dispatchReadyWaitNudges: %v", err)
	}
	for _, id := range store.getIDs {
		if id == sessionBead.ID {
			t.Fatalf("dispatch used Get for closed session %s; getIDs=%v", sessionBead.ID, store.getIDs)
		}
	}
	if len(sp.Calls) != 0 {
		t.Fatalf("dispatch should not query provider for a session absent from the open-session snapshot, calls=%#v", sp.Calls)
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
	skipSlowCmdGCTest(t, "requires a managed bd/dolt lifecycle city; run make test-cmd-gc-process for full coverage")
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
