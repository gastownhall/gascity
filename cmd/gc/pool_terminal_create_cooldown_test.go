package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const terminalCooldownCollisionReason = "work_dir_collision"

func terminalCooldownAgent(name, workDir string) config.Agent {
	return config.Agent{
		Name:              name,
		WorkDir:           workDir,
		StartCommand:      "true",
		ScaleCheck:        "printf 1",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(3),
	}
}

func terminalCooldownCity(agent config.Agent) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "cooldown-city"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents:    []config.Agent{agent},
	}
}

func terminalCooldownConfiguredAgent(t *testing.T, name, workDir, cooldown string) config.Agent {
	t.Helper()
	agent := terminalCooldownAgent(name, workDir)
	if _, err := toml.Decode(fmt.Sprintf("terminal_create_cooldown = %q", cooldown), &agent); err != nil {
		t.Fatalf("decode terminal_create_cooldown: %v", err)
	}
	return agent
}

func terminalCooldownSessionRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{
		Type:          sessionBeadType,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		t.Fatalf("list session rows: %v", err)
	}
	return rows
}

func terminalCooldownNewRows(before, after []beads.Bead) []beads.Bead {
	seen := make(map[string]bool, len(before))
	for _, row := range before {
		seen[row.ID] = true
	}
	var created []beads.Bead
	for _, row := range after {
		if !seen[row.ID] {
			created = append(created, row)
		}
	}
	return created
}

func terminalCooldownBuild(
	t *testing.T,
	cityPath string,
	now time.Time,
	cfg *config.City,
	store beads.Store,
) []beads.Bead {
	t.Helper()
	before := terminalCooldownSessionRows(t, store)
	buildDesiredState(cfg.EffectiveCityName(), cityPath, now.UTC(), cfg, runtime.NewFake(), store, io.Discard)
	after := terminalCooldownSessionRows(t, store)
	return terminalCooldownNewRows(before, after)
}

func terminalCooldownSeedHistory(
	t *testing.T,
	store beads.Store,
	name string,
	template string,
	origin string,
	workerDir string,
	legacyWorkDir string,
	reason string,
	failedAt *time.Time,
	closed bool,
) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		"session_name":   name,
		"agent_name":     name,
		"template":       template,
		"state":          string(sessionpkg.StateAsleep),
		"sleep_reason":   string(sessionpkg.SleepReasonProviderTerminalError),
		"session_origin": origin,
		"worker_dir":     workerDir,
		"work_dir":       legacyWorkDir,
	}
	if origin == "ephemeral" {
		metadata["pool_managed"] = "true"
	}
	if reason != "" {
		metadata[sessionProviderTerminalErrorMetadataKey] = reason
	}
	if failedAt != nil {
		metadata[sessionProviderTerminalErrorAtKey] = failedAt.UTC().Format(time.RFC3339)
	}
	row, err := store.Create(beads.Bead{
		Title:    name,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "template:" + template},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("seed terminal-create history: %v", err)
	}
	if closed && !closeFailedCreateBead(sessionFrontDoor(store), row.ID, failedAtOrNow(failedAt), io.Discard) {
		t.Fatalf("close terminal-create history %s", row.ID)
	}
	row, err = store.Get(row.ID)
	if err != nil {
		t.Fatalf("reload terminal-create history %s: %v", row.ID, err)
	}
	return row
}

func failedAtOrNow(at *time.Time) time.Time {
	if at == nil {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	}
	return at.UTC()
}

func terminalCooldownFailAttempt(
	t *testing.T,
	store beads.Store,
	row beads.Bead,
	workDir string,
	clk clock.Clock,
) {
	t.Helper()
	front := sessionFrontDoor(store)
	info, err := front.Get(row.ID)
	if err != nil {
		t.Fatalf("load fresh session %s: %v", row.ID, err)
	}
	info, err = front.ApplyPatchInfo(info, sessionpkg.MetadataPatch{
		"worker_dir": workDir,
		"work_dir":   workDir,
	})
	if err != nil {
		t.Fatalf("stamp fresh session work_dir %s: %v", row.ID, err)
	}
	if _, err := markProviderTerminalError(info, front, clk, terminalCooldownCollisionReason); err != nil {
		t.Fatalf("mark terminal provider error on %s: %v", row.ID, err)
	}
	if !closeFailedCreateBead(front, row.ID, clk.Now().UTC(), io.Discard) {
		t.Fatalf("close failed-create session %s", row.ID)
	}
}

// TestPoolTerminalCreateCooldownBoundsRepeatedFailedCreates proves the whole
// failure cycle: the first demanded ephemeral session is materialized, reaches
// durable terminal failed-create state, and then nineteen same-signature ticks
// inside the default window cannot grow the ledger. The fake clock keeps the
// test independent of reconciler cadence and wall-clock scheduling.
func TestPoolTerminalCreateCooldownBoundsRepeatedFailedCreates(t *testing.T) {
	cityPath := t.TempDir()
	workDir := filepath.Join(cityPath, "shared-worker-dir")
	cfg := terminalCooldownCity(terminalCooldownAgent("worker", workDir))
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}

	for tick := 0; tick < 20; tick++ {
		created := terminalCooldownBuild(t, cityPath, clk.Now(), cfg, store)
		if tick == 0 && len(created) != 1 {
			t.Fatalf("first tick created %d session beads, want 1 so the terminal-failure precondition is exercised", len(created))
		}
		if len(created) > 1 {
			t.Fatalf("tick %d created %d session beads, want at most 1", tick, len(created))
		}
		for _, row := range created {
			terminalCooldownFailAttempt(t, store, row, workDir, clk)
		}
		if tick == 0 {
			got, err := store.Get(created[0].ID)
			if err != nil {
				t.Fatalf("reload first failed attempt: %v", err)
			}
			if got.Status != "closed" || got.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
				t.Fatalf("first attempt = status %q state %q, want closed terminal failed-create", got.Status, got.Metadata["state"])
			}
			if got.Metadata[sessionProviderTerminalErrorMetadataKey] != terminalCooldownCollisionReason {
				t.Fatalf("first attempt terminal reason = %q, want %q",
					got.Metadata[sessionProviderTerminalErrorMetadataKey], terminalCooldownCollisionReason)
			}
			if got.Metadata[sessionProviderTerminalErrorAtKey] != clk.Now().UTC().Format(time.RFC3339) {
				t.Fatalf("first attempt terminal timestamp = %q, want fake-clock time %q",
					got.Metadata[sessionProviderTerminalErrorAtKey], clk.Now().UTC().Format(time.RFC3339))
			}
		}
		clk.Advance(10 * time.Second)
	}

	if rows := terminalCooldownSessionRows(t, store); len(rows) != 1 {
		t.Fatalf("20 same-signature ticks inside the cooldown created %d session beads, want 1", len(rows))
	}
}

// TestPoolTerminalCreateCooldownReadsOpenAndClosedHistory guards the persistent
// source of truth. In particular, the closed case prevents an implementation
// from consulting only the reconciler's open-session snapshot.
func TestPoolTerminalCreateCooldownReadsOpenAndClosedHistory(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)

	tests := []struct {
		name string
		seed func(t *testing.T, store beads.Store, template, workDir string)
	}{
		{
			name: "open_recent_terminal_failure",
			seed: func(t *testing.T, store beads.Store, template, workDir string) {
				terminalCooldownSeedHistory(t, store, "open-history", template, "ephemeral",
					workDir, workDir, terminalCooldownCollisionReason, &recent, false)
			},
		},
		{
			name: "closed_recent_terminal_failure_survives_reconciler_restart",
			seed: func(t *testing.T, store beads.Store, template, workDir string) {
				terminalCooldownSeedHistory(t, store, "closed-history", template, "ephemeral",
					workDir, workDir, terminalCooldownCollisionReason, &recent, true)
			},
		},
		{
			name: "most_recent_failure_timestamp_wins_over_store_order",
			seed: func(t *testing.T, store beads.Store, template, workDir string) {
				old := now.Add(-10 * time.Minute)
				// Create the recent row first and the expired row second. A
				// store-order or oldest-timestamp implementation retries;
				// the metadata timestamp maximum remains inside cooldown.
				terminalCooldownSeedHistory(t, store, "recent-first", template, "ephemeral",
					workDir, workDir, terminalCooldownCollisionReason, &recent, true)
				terminalCooldownSeedHistory(t, store, "old-second", template, "ephemeral",
					workDir, workDir, terminalCooldownCollisionReason, &old, true)
			},
		},
		{
			name: "canonical_worker_dir_precedes_legacy_work_dir",
			seed: func(t *testing.T, store beads.Store, template, workDir string) {
				terminalCooldownSeedHistory(t, store, "canonical-history", template, "ephemeral",
					workDir, filepath.Join(filepath.Dir(workDir), "legacy-decoy"),
					terminalCooldownCollisionReason, &recent, true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			workDir := filepath.Join(cityPath, "shared-worker-dir")
			template := "worker"
			store := beads.NewMemStore()
			tt.seed(t, store, template, workDir)

			created := terminalCooldownBuild(
				t, cityPath, now, terminalCooldownCity(terminalCooldownAgent(template, workDir)), store,
			)
			if len(created) != 0 {
				t.Fatalf("recent matching terminal history created %d additional session beads, want 0", len(created))
			}
		})
	}
}

func TestPoolTerminalCreateCooldownBecomesEligibleAtExpiry(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	workDir := filepath.Join(cityPath, "shared-worker-dir")
	store := beads.NewMemStore()
	failedAt := now.Add(-5 * time.Minute)
	terminalCooldownSeedHistory(t, store, "expired-history", "worker", "ephemeral",
		workDir, workDir, terminalCooldownCollisionReason, &failedAt, true)

	created := terminalCooldownBuild(
		t, cityPath, now, terminalCooldownCity(terminalCooldownAgent("worker", workDir)), store,
	)
	if len(created) != 1 {
		t.Fatalf("demand at the exact 5m expiry boundary created %d session beads, want 1", len(created))
	}
}

func TestPoolTerminalCreateCooldownHonorsConfiguredPolicy(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		cooldown   string
		age        time.Duration
		wantCreate int
	}{
		{name: "custom_window_inside", cooldown: "90s", age: 89 * time.Second, wantCreate: 0},
		{name: "custom_window_exact_boundary", cooldown: "90s", age: 90 * time.Second, wantCreate: 1},
		{name: "invalid_value_uses_5m_default", cooldown: "not-a-duration", age: 4 * time.Minute, wantCreate: 0},
		{name: "explicit_zero_disables_throttle", cooldown: "0s", age: 0, wantCreate: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			workDir := filepath.Join(cityPath, "shared-worker-dir")
			store := beads.NewMemStore()
			failedAt := now.Add(-tt.age)
			terminalCooldownSeedHistory(t, store, "policy-history", "worker", "ephemeral",
				workDir, workDir, terminalCooldownCollisionReason, &failedAt, true)
			agent := terminalCooldownConfiguredAgent(t, "worker", workDir, tt.cooldown)

			created := terminalCooldownBuild(t, cityPath, now, terminalCooldownCity(agent), store)
			if len(created) != tt.wantCreate {
				t.Fatalf("terminal_create_cooldown=%q age=%s created %d session beads, want %d",
					tt.cooldown, tt.age, len(created), tt.wantCreate)
			}
		})
	}
}

func TestAgentTerminalCreateCooldownConfigKeyIsRecognized(t *testing.T) {
	var agent config.Agent
	md, err := toml.Decode(`name = "worker"
terminal_create_cooldown = "45s"
`, &agent)
	if err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	for _, key := range md.Undecoded() {
		if key.String() == "terminal_create_cooldown" {
			t.Fatal("terminal_create_cooldown is undecoded; the agent config surface does not recognize the cooldown policy")
		}
	}
}

func TestPoolTerminalCreateCooldownDoesNotSuppressDistinctOrIneligibleHistory(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)

	tests := []struct {
		name         string
		historyAgent string
		demandAgent  string
		historyDir   func(cityPath string) string
		demandDir    func(cityPath string) string
		legacyDir    func(cityPath string) string
		origin       string
		reason       string
		at           *time.Time
	}{
		{
			name:         "changed_agent_identity",
			historyAgent: "worker-a", demandAgent: "worker-b",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "ephemeral",
			reason: terminalCooldownCollisionReason, at: &recent,
		},
		{
			name:         "changed_resolved_worker_dir",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: oldTerminalCooldownDir, demandDir: newTerminalCooldownDir,
			// Deliberately make legacy work_dir equal the new demand. Correct
			// canonical worker_dir precedence must still treat the signatures
			// as distinct and allow the repaired config immediately.
			legacyDir: newTerminalCooldownDir, origin: "ephemeral",
			reason: terminalCooldownCollisionReason, at: &recent,
		},
		{
			name:         "manual_origin_terminal_history",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "manual",
			reason: terminalCooldownCollisionReason, at: &recent,
		},
		{
			name:         "named_origin_terminal_history",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "named",
			reason: terminalCooldownCollisionReason, at: &recent,
		},
		{
			name:         "non_terminal_failure_has_no_reason",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "ephemeral",
			reason: "", at: &recent,
		},
		{
			name:         "terminal_failure_without_timestamp_fails_open",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "ephemeral",
			reason: terminalCooldownCollisionReason, at: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			terminalCooldownSeedHistory(
				t,
				store,
				"unmatched-history",
				tt.historyAgent,
				tt.origin,
				tt.historyDir(cityPath),
				tt.legacyDir(cityPath),
				tt.reason,
				tt.at,
				true,
			)
			cfg := terminalCooldownCity(terminalCooldownAgent(tt.demandAgent, tt.demandDir(cityPath)))

			created := terminalCooldownBuild(t, cityPath, now, cfg, store)
			if len(created) != 1 {
				t.Fatalf("eligible distinct demand created %d session beads, want 1", len(created))
			}
		})
	}
}

func sameTerminalCooldownDir(cityPath string) string {
	return filepath.Join(cityPath, "shared-worker-dir")
}

func oldTerminalCooldownDir(cityPath string) string {
	return filepath.Join(cityPath, "old-worker-dir")
}

func newTerminalCooldownDir(cityPath string) string {
	return filepath.Join(cityPath, "new-worker-dir")
}

func TestPoolTerminalCreateCooldownDoesNotInterceptManualOrNamedSessions(t *testing.T) {
	t.Run("manual_preferred_session_returns_before_history_lookup", func(t *testing.T) {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		cityPath := t.TempDir()
		workDir := sameTerminalCooldownDir(cityPath)
		base := beads.NewMemStore()
		row := terminalCooldownSeedHistory(t, base, "manual-session", "worker", "manual",
			workDir, workDir, terminalCooldownCollisionReason, &now, false)
		if err := base.SetMetadataBatch(row.ID, map[string]string{
			"manual_session": "true",
			"pool_managed":   "",
		}); err != nil {
			t.Fatalf("mark manual session: %v", err)
		}
		preferred, err := sessionFrontDoor(base).Get(row.ID)
		if err != nil {
			t.Fatalf("load manual session: %v", err)
		}
		fault := &terminalCooldownHistoryErrorStore{
			Store: base,
			err:   errors.New("manual path must not query terminal history"),
		}
		agent := terminalCooldownAgent("worker", workDir)
		bp := &agentBuildParams{
			city:                   terminalCooldownCity(agent),
			cityName:               "cooldown-city",
			cityPath:               cityPath,
			agents:                 []config.Agent{agent},
			beadStore:              fault,
			sessionBeads:           newSessionBeadSnapshotFromInfos([]sessionpkg.Info{preferred}),
			beaconTime:             now,
			providerHealthSnapshot: &providerHealthSnapshot{},
		}

		got, _, plan, err := selectOrPlanPoolSessionBead(
			bp, &agent, "worker", &preferred, SessionRequest{}, now, map[string]bool{}, map[int]bool{},
		)
		if err != nil {
			t.Fatalf("manual session selection: %v", err)
		}
		if plan != nil || got.ID != row.ID {
			t.Fatalf("manual selection = info %q plan %#v, want existing session %q with no fresh plan", got.ID, plan, row.ID)
		}
		if reads := fault.closedHistoryReads.Load(); reads != 0 {
			t.Fatalf("manual preferred path performed %d closed-history reads, want 0", reads)
		}
	})

	t.Run("configured_named_session_remains_desired", func(t *testing.T) {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		cityPath := t.TempDir()
		workDir := sameTerminalCooldownDir(cityPath)
		store := beads.NewMemStore()
		agent := terminalCooldownAgent("worker", workDir)
		agent.ScaleCheck = "printf 0"
		agent.MaxActiveSessions = intPtr(1)
		cfg := terminalCooldownCity(agent)
		cfg.NamedSessions = []config.NamedSession{{Name: "primary", Template: "worker", Mode: "always"}}
		sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "primary")
		row := terminalCooldownSeedHistory(t, store, sessionName, "worker", "named",
			workDir, workDir, terminalCooldownCollisionReason, &now, false)
		if err := store.SetMetadataBatch(row.ID, map[string]string{
			"alias":                      "primary",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "primary",
			namedSessionModeMetadata:     "always",
		}); err != nil {
			t.Fatalf("mark configured named session: %v", err)
		}

		var stderr bytes.Buffer
		result := buildDesiredState(cfg.EffectiveCityName(), cityPath, now, cfg, runtime.NewFake(), store, &stderr)
		if _, ok := result.State[sessionName]; !ok {
			t.Fatalf("named session %q missing from desired state after terminal history; state=%v stderr=%s",
				sessionName, result.State, stderr.String())
		}
		if rows := terminalCooldownSessionRows(t, store); len(rows) != 1 {
			t.Fatalf("named-session path materialized %d session beads, want the existing named session only", len(rows))
		}
	})
}

// terminalCooldownHistoryErrorStore fails only closed ephemeral-history reads:
// a closed read filtered on session_origin. Ordinary open snapshots, fresh
// creates, and the single-key closed identity lookups a create runs to check
// session-name availability still use the embedded store so the test can
// distinguish fail-open-with-a-visible-error from either silent swallowing or
// fail-closed suppression.
type terminalCooldownHistoryErrorStore struct {
	beads.Store
	err                error
	closedHistoryReads atomic.Int64
}

func (s *terminalCooldownHistoryErrorStore) historyError() error {
	s.closedHistoryReads.Add(1)
	return s.err
}

func terminalCooldownIsHistoryScan(includeClosed bool, metadata map[string]string) bool {
	return includeClosed && metadata["session_origin"] != ""
}

func (s *terminalCooldownHistoryErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if terminalCooldownIsHistoryScan(query.IncludeClosed, query.Metadata) {
		return nil, s.historyError()
	}
	return s.Store.List(query)
}

func (s *terminalCooldownHistoryErrorStore) ListOpen(status ...string) ([]beads.Bead, error) {
	if len(status) > 0 && status[0] == "closed" {
		return nil, s.historyError()
	}
	return s.Store.ListOpen(status...)
}

func (s *terminalCooldownHistoryErrorStore) ListByLabel(
	label string,
	limit int,
	opts ...beads.QueryOpt,
) ([]beads.Bead, error) {
	if beads.HasOpt(opts, beads.IncludeClosed) {
		return nil, s.historyError()
	}
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *terminalCooldownHistoryErrorStore) ListByMetadata(
	filters map[string]string,
	limit int,
	opts ...beads.QueryOpt,
) ([]beads.Bead, error) {
	if terminalCooldownIsHistoryScan(beads.HasOpt(opts, beads.IncludeClosed), filters) {
		return nil, s.historyError()
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func TestPoolTerminalCreateCooldownHistoryQueryFailureFailsOpenAndIsVisible(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)
	cityPath := t.TempDir()
	workDir := sameTerminalCooldownDir(cityPath)
	base := beads.NewMemStore()
	terminalCooldownSeedHistory(t, base, "closed-history", "worker", "ephemeral",
		workDir, workDir, terminalCooldownCollisionReason, &recent, true)
	sentinel := errors.New("terminal create history unavailable")
	store := &terminalCooldownHistoryErrorStore{Store: base, err: sentinel}
	cfg := terminalCooldownCity(terminalCooldownAgent("worker", workDir))

	var stderr bytes.Buffer
	buildDesiredState(cfg.EffectiveCityName(), cityPath, now, cfg, runtime.NewFake(), store, &stderr)

	if rows := terminalCooldownSessionRows(t, base); len(rows) != 2 {
		t.Fatalf("history query failure left %d session beads, want prior failure + one fail-open create", len(rows))
	}
	if reads := store.closedHistoryReads.Load(); reads == 0 {
		t.Fatal("build performed no closed-history read, so the injected query failure was not exercised")
	}
	if !strings.Contains(stderr.String(), sentinel.Error()) {
		t.Fatalf("stderr %q does not surface terminal-history query error %q", stderr.String(), sentinel)
	}
}
