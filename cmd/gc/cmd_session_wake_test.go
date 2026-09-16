package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestSessionWake_StateTransitionsAndMetadata(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name            string
		metadata        map[string]string
		wantState       string
		wantSleepReason string
		wantPending     string
		wantWakeRequest string
	}{
		{
			name: "suspended requests start",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "suspended",
				"held_until":   future,
				"sleep_reason": "user-hold",
			},
			wantState:       "asleep",
			wantSleepReason: "",
			wantPending:     "",
			wantWakeRequest: "explicit",
		},
		{
			name: "drained requests start",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "drained",
				"sleep_reason": "drained",
			},
			wantState:       "asleep",
			wantSleepReason: "",
			wantPending:     "",
			wantWakeRequest: "explicit",
		},
		{
			name: "creating clears quarantine but stays creating",
			metadata: map[string]string{
				"template":          "worker",
				"state":             "creating",
				"quarantined_until": future,
				"sleep_reason":      "quarantine",
				"wake_attempts":     "5",
			},
			wantState:       "creating",
			wantSleepReason: "",
			wantPending:     "",
			wantWakeRequest: "explicit",
		},
		{
			name: "active stays active",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "active",
				"sleep_reason": "idle",
			},
			wantState:       "active",
			wantSleepReason: "idle",
			wantPending:     "",
			wantWakeRequest: "explicit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: tt.metadata,
			})
			if err != nil {
				t.Fatalf("store.Create(): %v", err)
			}

			if _, err := session.NewStore(beads.SessionStore{Store: store}).WakeSession(b.ID, time.Now(), session.WakeOpts{}); err != nil {
				t.Fatalf("WakeSession: %v", err)
			}

			updated, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", b.ID, err)
			}
			if got := updated.Metadata["state"]; got != tt.wantState {
				t.Fatalf("state = %q, want %q", got, tt.wantState)
			}
			if got := updated.Metadata["sleep_reason"]; got != tt.wantSleepReason {
				t.Fatalf("sleep_reason = %q, want %q", got, tt.wantSleepReason)
			}
			if got := updated.Metadata["pending_create_claim"]; got != tt.wantPending {
				t.Fatalf("pending_create_claim = %q, want %q", got, tt.wantPending)
			}
			if got := updated.Metadata["wake_request"]; got != tt.wantWakeRequest {
				t.Fatalf("wake_request = %q, want %q", got, tt.wantWakeRequest)
			}
			if got := updated.Metadata["held_until"]; got != "" {
				t.Fatalf("held_until = %q, want empty", got)
			}
			if got := updated.Metadata["quarantined_until"]; got != "" {
				t.Fatalf("quarantined_until = %q, want empty", got)
			}
			if got := updated.Metadata["wait_hold"]; got != "" {
				t.Fatalf("wait_hold = %q, want empty", got)
			}
			if got := updated.Metadata["sleep_intent"]; got != "" {
				t.Fatalf("sleep_intent = %q, want empty", got)
			}
			if got := updated.Metadata["wake_attempts"]; got != "0" {
				t.Fatalf("wake_attempts = %q, want 0", got)
			}
			if got := updated.Metadata["churn_count"]; got != "0" {
				t.Fatalf("churn_count = %q, want 0", got)
			}
		})
	}
}

func TestDoSessionWake_PokesManagedControllerAfterStateChange(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "managed wake session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-managed",
			"template":     "worker",
			"state":        "suspended",
			"held_until":   "2026-07-16T01:00:00Z",
			"sleep_reason": "user-hold",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var calls []string
	deps := sessionWakeDeps{
		store:        store,
		cityPath:     "/city",
		cityResolved: true,
		now: func() time.Time {
			return time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
		},
		withdrawQueuedWaitNudges: func(cityPath string, nudgeIDs []string) error {
			if cityPath != "/city" {
				t.Fatalf("withdraw cityPath = %q, want /city", cityPath)
			}
			if len(nudgeIDs) != 0 {
				t.Fatalf("withdraw nudge IDs = %v, want none", nudgeIDs)
			}
			calls = append(calls, "withdraw")
			return nil
		},
		cityUsesManagedReconciler: func(cityPath string) bool {
			if cityPath != "/city" {
				t.Fatalf("managed-reconciler cityPath = %q, want /city", cityPath)
			}
			calls = append(calls, "managed")
			return true
		},
		pokeController: func(cityPath string) error {
			if cityPath != "/city" {
				t.Fatalf("poke cityPath = %q, want /city", cityPath)
			}
			updated, getErr := store.Get(sessionBead.ID)
			if getErr != nil {
				t.Fatalf("store.Get(%s) during poke: %v", sessionBead.ID, getErr)
			}
			if got := updated.Metadata["state"]; got != "asleep" {
				t.Fatalf("state during poke = %q, want asleep", got)
			}
			if got := updated.Metadata["wake_requested_at"]; got != "2026-07-16T00:00:00Z" {
				t.Fatalf("wake_requested_at during poke = %q, want injected time", got)
			}
			calls = append(calls, "poke")
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := doSessionWake(sessionBead.ID, &stdout, &stderr, false, deps); code != 0 {
		t.Fatalf("doSessionWake() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(calls, ","); got != "withdraw,managed,poke" {
		t.Fatalf("effect calls = %q, want withdraw,managed,poke", got)
	}
	if got := stdout.String(); !strings.Contains(got, "wake requested") {
		t.Fatalf("stdout = %q, want wake requested", got)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionBead.ID, err)
	}
	if got := updated.Metadata["state"]; got != "asleep" {
		t.Fatalf("state = %q, want asleep", got)
	}
	if got := updated.Metadata["held_until"]; got != "" {
		t.Fatalf("held_until = %q, want empty", got)
	}
	if got := updated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("sleep_reason = %q, want empty", got)
	}
}

func TestDoSessionWake_DoesNotPokeWithoutManagedController(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
			"state":    "suspended",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	poked := false
	deps := sessionWakeDeps{
		store:        store,
		cityPath:     "/city",
		cityResolved: true,
		now: func() time.Time {
			return time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
		},
		withdrawQueuedWaitNudges: func(string, []string) error {
			return nil
		},
		cityUsesManagedReconciler: func(string) bool {
			return false
		},
		pokeController: func(string) error {
			poked = true
			return nil
		},
	}

	if code := doSessionWake(sessionBead.ID, &bytes.Buffer{}, &bytes.Buffer{}, false, deps); code != 0 {
		t.Fatalf("doSessionWake() = %d, want 0", code)
	}
	if poked {
		t.Fatal("pokeController called for a city without a managed reconciler")
	}
}

func TestDoSessionWake_PokeFailureWarnsWithoutFailingWake(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
			"state":    "suspended",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	deps := sessionWakeDeps{
		store:        store,
		cityPath:     "/city",
		cityResolved: true,
		now: func() time.Time {
			return time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
		},
		withdrawQueuedWaitNudges: func(string, []string) error {
			return nil
		},
		cityUsesManagedReconciler: func(string) bool {
			return true
		},
		pokeController: func(string) error {
			return errors.New("dial failed")
		},
	}

	var stderr bytes.Buffer
	if code := doSessionWake(sessionBead.ID, &bytes.Buffer{}, &stderr, false, deps); code != 0 {
		t.Fatalf("doSessionWake() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "warning: poke failed: dial failed") {
		t.Fatalf("stderr = %q, want poke failure warning", got)
	}
	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionBead.ID, err)
	}
	if got := updated.Metadata["state"]; got != "asleep" {
		t.Fatalf("state = %q, want asleep", got)
	}
}

// TestDoSessionWake_StuckInFlightAgeGate pins the age-gate on the
// hasRunnableTemplate/stuck-in-flight CLI arm: a session that is still
// healthily mid-create (fresh pending_create_started_at) must wake
// successfully, and only a create that has sat past
// staleCreatingStateTimeout should be rejected. Before the age gate, ANY
// creating/start-pending session with a runnable template failed wake with
// exit 1, even one a provider had just legitimately started.
//
// Every subtest — including the rejecting ones — also asserts that queued wait
// nudges are withdrawn. WakeSession cancels the waits and commits the batch
// before doSessionWake ever sees the result, so a reject arm that returns
// before the cleanup block strands those nudges in the queue with nothing left
// to withdraw them.
func TestDoSessionWake_StuckInFlightAgeGate(t *testing.T) {
	freshStart := time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	leasedStart := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	staleStart := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	tests := []struct {
		name      string
		state     string
		claim     bool
		startedAt string
		wantCode  int
	}{
		{name: "fresh creating wakes normally", state: "creating", startedAt: freshStart, wantCode: 0},
		{name: "fresh start-pending wakes normally", state: "start-pending", startedAt: freshStart, wantCode: 0},
		{name: "stale creating rejects wake", state: "creating", startedAt: staleStart, wantCode: 1},
		{name: "stale start-pending rejects wake", state: "start-pending", startedAt: staleStart, wantCode: 1},
		// The window the sweep protects but a staleness-only gate would
		// reject: past staleCreatingStateTimeout (1m) yet still inside the
		// never-started lease (pendingCreateNeverStartedTimeout, 10m), with
		// no last_woke_at. The sweep skips this bead; the CLI must agree.
		{name: "leased never-started create wakes normally", state: "creating", claim: true, startedAt: leasedStart, wantCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			metadata := map[string]string{
				"template":                  "worker",
				"state":                     tt.state,
				"pending_create_started_at": tt.startedAt,
				"last_woke_at":              "",
			}
			if tt.claim {
				metadata["pending_create_claim"] = "true"
			}
			b, err := store.Create(beads.Bead{
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("store.Create(): %v", err)
			}

			withdrawCalled := false
			deps := sessionWakeDeps{
				store:        store,
				cityPath:     "/city",
				cityResolved: true,
				now:          time.Now,
				withdrawQueuedWaitNudges: func(string, []string) error {
					withdrawCalled = true
					return nil
				},
				cityUsesManagedReconciler: func(string) bool { return false },
			}

			var stdout, stderr bytes.Buffer
			code := doSessionWake(b.ID, &stdout, &stderr, false, deps)
			if code != tt.wantCode {
				t.Fatalf("doSessionWake() = %d, want %d; stderr=%s", code, tt.wantCode, stderr.String())
			}
			if !withdrawCalled {
				t.Fatalf("withdrawQueuedWaitNudges was not called (exit %d): WakeSession already canceled the waits, so their nudges are stranded", code)
			}
			if strings.Contains(stderr.String(), "no live runtime") {
				t.Fatalf("stderr = %q, message must state what was checked, not the old 'no live runtime' wording", stderr.String())
			}
			if tt.wantCode == 1 {
				want := `has been in state "` + tt.state + `" since`
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
				}
				updated, err := store.Get(b.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", b.ID, err)
				}
				if got := updated.Metadata["wake_request"]; got != "explicit" {
					t.Fatalf("wake_request = %q, want %q: the reject arm reports that wake cannot complete now, it does not roll the recorded wake back", got, "explicit")
				}
			}
		})
	}
}

// TestDoSessionWake_NoRunnableTemplateAgeGate pins the age-gate on the
// no-runnable-template CLI arm, the mirror of TestDoSessionWake_StuckInFlightAgeGate.
// When config has no agent matching the session's template, a session still
// healthily mid-create must be left alone: force-clearing its
// pending_create_claim/pending_create_started_at lease would yank the lease out
// from under a create a provider had just legitimately started. Only a create
// whose pending-create lease has expired AND which has sat past
// staleCreatingStateTimeout is healed to asleep — the same two-condition gate,
// in the same order, that the sweep applies in city_runtime.go. Either way the
// command succeeds — unlike the stuck-in-flight arm, there is no runnable
// template to report a failure against.
func TestDoSessionWake_NoRunnableTemplateAgeGate(t *testing.T) {
	freshStart := time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	leasedStart := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	staleStart := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	tests := []struct {
		name      string
		state     string
		startedAt string
		wantState string
		wantHeal  bool
	}{
		{name: "fresh creating keeps its lease", state: "creating", startedAt: freshStart, wantState: "creating", wantHeal: false},
		{name: "fresh start-pending keeps its lease", state: "start-pending", startedAt: freshStart, wantState: "start-pending", wantHeal: false},
		// Mirror of the stuck-in-flight arm's leased case: stale by the
		// 1-minute bound, still leased by the 10-minute never-started bound.
		// The sweep protects this bead, so the mutating CLI arm must not
		// clear the lease out from under it.
		{name: "leased never-started create keeps its lease", state: "creating", startedAt: leasedStart, wantState: "creating", wantHeal: false},
		{name: "stale creating heals to asleep", state: "creating", startedAt: staleStart, wantState: "asleep", wantHeal: true},
		{name: "stale start-pending heals to asleep", state: "start-pending", startedAt: staleStart, wantState: "asleep", wantHeal: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Type:   session.BeadType,
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"template":                  "worker",
					"state":                     tt.state,
					"pending_create_claim":      "true",
					"pending_create_started_at": tt.startedAt,
					"last_woke_at":              "",
				},
			})
			if err != nil {
				t.Fatalf("store.Create(): %v", err)
			}

			// An agent that deliberately does NOT match template "worker",
			// so sessionWakeHasRunnableTemplateInfo reports false. A nil cfg
			// would report true and route to the stuck-in-flight arm instead.
			cfg := &config.City{Agents: []config.Agent{{Name: "other"}}}
			if sessionWakeHasRunnableTemplateInfo(session.Info{Template: "worker"}, cfg) {
				t.Fatal("test setup: template \"worker\" must not be runnable under this config")
			}
			deps := sessionWakeDeps{store: store, cfg: cfg, cityPath: "/city", now: time.Now}

			var stdout, stderr bytes.Buffer
			if code := doSessionWake(b.ID, &stdout, &stderr, false, deps); code != 0 {
				t.Fatalf("doSessionWake() = %d, want 0; stderr=%s", code, stderr.String())
			}

			updated, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", b.ID, err)
			}
			if got := updated.Metadata["state"]; got != tt.wantState {
				t.Fatalf("state = %q, want %q", got, tt.wantState)
			}
			if tt.wantHeal {
				if got := updated.Metadata["pending_create_claim"]; got != "" {
					t.Fatalf("pending_create_claim = %q, want cleared", got)
				}
				if got := updated.Metadata["pending_create_started_at"]; got != "" {
					t.Fatalf("pending_create_started_at = %q, want cleared", got)
				}
				return
			}
			if got := updated.Metadata["pending_create_started_at"]; got != tt.startedAt {
				t.Fatalf("pending_create_started_at = %q, want %q (healthy in-flight create must keep its lease)", got, tt.startedAt)
			}
		})
	}
}

// TestDoSessionWake_SuspendedRigRejectsWake pins the fix for gastownhall/gascity#5785:
// waking a session whose owning rig is suspended must fail loudly with a
// rig-resume hint instead of printing the generic "wake requested" success
// line, since no reconciler will ever act on the wake. The non-suspended
// case is the regression guard -- the same session/agent/rig shape must
// still wake normally when the rig isn't suspended.
func TestDoSessionWake_SuspendedRigRejectsWake(t *testing.T) {
	tests := []struct {
		name             string
		rigSuspended     bool
		wantCode         int
		wantStderrSubstr string
	}{
		{
			name:             "suspended rig rejects wake",
			rigSuspended:     true,
			wantCode:         1,
			wantStderrSubstr: `rig "frontend" is suspended`,
		},
		{
			name:         "non-suspended rig wakes normally",
			rigSuspended: false,
			wantCode:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			sessionBead, err := store.Create(beads.Bead{
				Title:  "rig-scoped wake session",
				Type:   session.BeadType,
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "s-gc-frontend-worker",
					"template":     "frontend/worker",
					"state":        "suspended",
					"held_until":   future,
					"sleep_reason": "user-hold",
				},
			})
			if err != nil {
				t.Fatalf("store.Create(session bead): %v", err)
			}

			cfg := &config.City{
				Agents: []config.Agent{{Name: "worker", Dir: "frontend"}},
				Rigs:   []config.Rig{{Name: "frontend", SuspendedOnStart: tt.rigSuspended}},
			}
			deps := sessionWakeDeps{
				store:        store,
				cfg:          cfg,
				cityPath:     "/city",
				cityResolved: true,
				now:          time.Now,
				withdrawQueuedWaitNudges: func(string, []string) error {
					return nil
				},
				cityUsesManagedReconciler: func(string) bool {
					return false
				},
				pokeController: func(string) error {
					return nil
				},
			}

			var stdout, stderr bytes.Buffer
			code := doSessionWake(sessionBead.ID, &stdout, &stderr, false, deps)
			if code != tt.wantCode {
				t.Fatalf("doSessionWake() = %d, want %d; stdout=%s stderr=%s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if tt.wantStderrSubstr != "" {
				if got := stderr.String(); !strings.Contains(got, tt.wantStderrSubstr) {
					t.Fatalf("stderr = %q, want substring %q", got, tt.wantStderrSubstr)
				}
				if got := stderr.String(); !strings.Contains(got, "gc rig resume frontend") {
					t.Fatalf("stderr = %q, want rig-resume hint", got)
				}
				if got := stdout.String(); strings.Contains(got, "wake requested") {
					t.Fatalf("stdout = %q, must not report success for a dropped wake", got)
				}
				return
			}
			if got := stdout.String(); !strings.Contains(got, "wake requested") {
				t.Fatalf("stdout = %q, want wake requested", got)
			}
		})
	}
}

// This is the single real CLI/config/file-store/controller-socket composition
// proof for session wake. Lower-level wake behavior belongs in doSessionWake
// unit tests; managed-Dolt consistency has its own provider boundary owner.
func TestCmdSessionWake_PokesManagedControllerAndRequestsSuspendedStart(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-wake-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":        "suspended",
		"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"sleep_reason": "user-hold",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	commands := make(chan string, 2)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for range 2 {
			conn, err := lis.Accept()
			if err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			cmd := string(buf[:n])
			commands <- cmd
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{"mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionWake() = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 2)
	deadline := time.After(testutil.GoroutineRaceTimeout)
	for len(gotCommands) < 2 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				t.Fatalf("controller commands = %v, want ping plus poke", gotCommands)
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller commands, got %v", gotCommands)
		}
	}
	wantCommands := []string{"ping\n", "poke\n"}
	for i, want := range wantCommands {
		if gotCommands[i] != want {
			t.Fatalf("controller command %d = %q, want %q", i, gotCommands[i], want)
		}
	}

	freshStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	updated, err := freshStore.Get(sessionID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionID, err)
	}
	if got := updated.Metadata["state"]; got != "asleep" {
		t.Fatalf("state = %q, want asleep", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want empty", got)
	}
	if got := updated.Metadata["wake_request"]; got != "explicit" {
		t.Fatalf("wake_request = %q, want explicit", got)
	}
	if got := updated.Metadata["held_until"]; got != "" {
		t.Fatalf("held_until = %q, want empty", got)
	}
	if got := updated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("sleep_reason = %q, want empty", got)
	}
}

func TestCmdSessionWake_RejectsArchivedHistoricalSessionID(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	rootDir, err := os.MkdirTemp("", "gcw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	cityDir := filepath.Join(rootDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityDir, err)
	}
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "false",
		"alias":               "",
		"session_name":        "",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{sessionID}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionWake() = %d, want rejection; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	// Pin the CLI wake-conflict artifact: the fused WakeSession returns a
	// WakeConflictError the CLI renders as "session <id> is <state>".
	if want := "gc session wake: session " + sessionID + " is archived"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr missing %q:\n%s", want, stderr.String())
	}
}

func TestCmdSessionWake_RequestsStartForContinuityEligibleArchivedSessionID(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	rootDir, err := os.MkdirTemp("", "gcw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	cityDir := filepath.Join(rootDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityDir, err)
	}
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "true",
		"archived_at":         time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{sessionID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionWake() = %d, want success; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "wake requested") {
		t.Fatalf("stdout = %q, want wake requested message", stdout.String())
	}

	freshStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	updated, err := freshStore.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sessionID, err)
	}
	if got := updated.Metadata["state"]; got != "archived" {
		t.Fatalf("state = %q, want archived", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want empty", got)
	}
	if got := updated.Metadata["wake_request"]; got != "explicit" {
		t.Fatalf("wake_request = %q, want explicit", got)
	}
}
