package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const terminalCooldownCollisionReason = "work_dir_collision"

// terminalCooldownProcessStart is the frozen beacon time a long-running
// controller carries: hours older than every failure and decision time the
// tests below use, as in production.
var terminalCooldownProcessStart = time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)

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

// terminalCooldownBuild runs one desired-state build the way the controller
// does: the beacon is the frozen process-start time and the pool decision time
// is the tick's wall clock.
func terminalCooldownBuild(
	t *testing.T,
	cityPath string,
	decision time.Time,
	cfg *config.City,
	store beads.Store,
	stderr io.Writer,
) []beads.Bead {
	t.Helper()
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	before := terminalCooldownSessionRows(t, store)
	buildDesiredStateWithSessionBeadsAt(
		cfg.EffectiveCityName(), cityPath, terminalCooldownProcessStart, decision.UTC(),
		cfg, runtime.NewFake(), store, nil, snapshot, nil, stderr,
	)
	return terminalCooldownNewRows(before, terminalCooldownSessionRows(t, store))
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
	if closed && !closeFailedCreateBead(sessionFrontDoor(store), sessionInfoFromBead(row), failedAtOrNow(failedAt), io.Discard) {
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
	info, err = markProviderTerminalError(info, front, clk, terminalCooldownCollisionReason)
	if err != nil {
		t.Fatalf("mark terminal provider error on %s: %v", row.ID, err)
	}
	if !closeFailedCreateBead(front, info, clk.Now().UTC(), io.Discard) {
		t.Fatalf("close failed-create session %s", row.ID)
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

// TestPoolTerminalCreateCooldownBoundsRepeatedFailedCreates proves the whole
// failure cycle on a production-shaped clock: the beacon is frozen hours
// before the fake wall clock, which alone drives the decision time. The first
// demanded ephemeral session reaches durable terminal failed-create state;
// every later same-signature tick inside the window must not grow the ledger,
// and the tick after the window must retry.
func TestPoolTerminalCreateCooldownBoundsRepeatedFailedCreates(t *testing.T) {
	cityPath := t.TempDir()
	workDir := sameTerminalCooldownDir(cityPath)
	cfg := terminalCooldownCity(terminalCooldownAgent("worker", workDir))
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}

	created := terminalCooldownBuild(t, cityPath, clk.Now(), cfg, store, io.Discard)
	if len(created) != 1 {
		t.Fatalf("first tick created %d session beads, want 1 so the terminal-failure precondition is exercised", len(created))
	}
	terminalCooldownFailAttempt(t, store, created[0], workDir, clk)
	failedAt := clk.Now()

	for clk.Now().Sub(failedAt) < poolTerminalCreateCooldown {
		clk.Advance(10 * time.Second)
		if clk.Now().Sub(failedAt) >= poolTerminalCreateCooldown {
			break
		}
		if got := terminalCooldownBuild(t, cityPath, clk.Now(), cfg, store, io.Discard); len(got) != 0 {
			t.Fatalf("tick %s after the failure created %d session beads inside the cooldown, want 0",
				clk.Now().Sub(failedAt), len(got))
		}
	}
	if rows := terminalCooldownSessionRows(t, store); len(rows) != 1 {
		t.Fatalf("ticks inside the cooldown left %d session beads, want 1", len(rows))
	}

	if got := terminalCooldownBuild(t, cityPath, clk.Now(), cfg, store, io.Discard); len(got) != 1 {
		t.Fatalf("tick at the %s expiry created %d session beads, want 1 retry", poolTerminalCreateCooldown, len(got))
	}
}

// TestPoolTerminalCreateCooldownExpiresOnDecisionTimeNotBeacon is the
// regression for the frozen-beacon defect: the controller's beacon time is its
// process start, so a gate measured against it never expires. The failure here
// is older than the window by the decision clock but newer than the beacon.
func TestPoolTerminalCreateCooldownExpiresOnDecisionTimeNotBeacon(t *testing.T) {
	decision := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		age        time.Duration
		wantCreate int
	}{
		{name: "just_inside_window", age: poolTerminalCreateCooldown - time.Second, wantCreate: 0},
		{name: "exact_expiry_boundary", age: poolTerminalCreateCooldown, wantCreate: 1},
		{name: "hours_past_expiry", age: 2 * time.Hour, wantCreate: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			workDir := sameTerminalCooldownDir(cityPath)
			store := beads.NewMemStore()
			failedAt := decision.Add(-tt.age)
			if !failedAt.After(terminalCooldownProcessStart) {
				t.Fatalf("fixture invalid: failure %s must be newer than the frozen beacon %s", failedAt, terminalCooldownProcessStart)
			}
			terminalCooldownSeedHistory(t, store, "history", "worker", "ephemeral",
				workDir, workDir, terminalCooldownCollisionReason, &failedAt, true)

			got := terminalCooldownBuild(t, cityPath, decision, terminalCooldownCity(terminalCooldownAgent("worker", workDir)), store, io.Discard)
			if len(got) != tt.wantCreate {
				t.Fatalf("failure %s old created %d session beads, want %d", tt.age, len(got), tt.wantCreate)
			}
		})
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
			workDir := sameTerminalCooldownDir(cityPath)
			store := beads.NewMemStore()
			tt.seed(t, store, "worker", workDir)

			created := terminalCooldownBuild(t, cityPath, now, terminalCooldownCity(terminalCooldownAgent("worker", workDir)), store, io.Discard)
			if len(created) != 0 {
				t.Fatalf("recent matching terminal history created %d additional session beads, want 0", len(created))
			}
		})
	}
}

// TestPoolTerminalCreateCooldownLogsSuppression pins the operator-visible
// line: a suppressed create must say so, once per suppressed request.
func TestPoolTerminalCreateCooldownLogsSuppression(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	workDir := sameTerminalCooldownDir(cityPath)
	store := beads.NewMemStore()
	recent := now.Add(-30 * time.Second)
	terminalCooldownSeedHistory(t, store, "history", "worker", "ephemeral",
		workDir, workDir, terminalCooldownCollisionReason, &recent, true)

	var stderr bytes.Buffer
	terminalCooldownBuild(t, cityPath, now, terminalCooldownCity(terminalCooldownAgent("worker", workDir)), store, &stderr)

	if !strings.Contains(stderr.String(), "terminal cooldown active") ||
		!strings.Contains(stderr.String(), "fresh create blocked") {
		t.Fatalf("stderr %q does not record the terminal-cooldown suppression", stderr.String())
	}
}

func TestPoolTerminalCreateCooldownDoesNotSuppressDistinctOrIneligibleHistory(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)
	// A timestamp beyond the allowed skew cannot be a real past failure; it
	// must not pin a create out indefinitely.
	future := now.Add(poolTerminalCreateClockSkew + time.Hour)

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
		{
			name:         "terminal_failure_timestamp_beyond_skew_fails_open",
			historyAgent: "worker", demandAgent: "worker",
			historyDir: sameTerminalCooldownDir, demandDir: sameTerminalCooldownDir,
			legacyDir: sameTerminalCooldownDir, origin: "ephemeral",
			reason: terminalCooldownCollisionReason, at: &future,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			terminalCooldownSeedHistory(
				t, store, "unmatched-history", tt.historyAgent, tt.origin,
				tt.historyDir(cityPath), tt.legacyDir(cityPath), tt.reason, tt.at, true,
			)
			cfg := terminalCooldownCity(terminalCooldownAgent(tt.demandAgent, tt.demandDir(cityPath)))

			created := terminalCooldownBuild(t, cityPath, now, cfg, store, io.Discard)
			if len(created) != 1 {
				t.Fatalf("eligible distinct demand created %d session beads, want 1", len(created))
			}
		})
	}
}

// TestPoolTerminalCreateCooldownToleratesSmallClockSkew keeps the skew bound
// from turning a slightly-ahead writer clock into a fail-open.
func TestPoolTerminalCreateCooldownToleratesSmallClockSkew(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	workDir := sameTerminalCooldownDir(cityPath)
	store := beads.NewMemStore()
	slightlyAhead := now.Add(poolTerminalCreateClockSkew / 2)
	terminalCooldownSeedHistory(t, store, "history", "worker", "ephemeral",
		workDir, workDir, terminalCooldownCollisionReason, &slightlyAhead, true)

	created := terminalCooldownBuild(t, cityPath, now, terminalCooldownCity(terminalCooldownAgent("worker", workDir)), store, io.Discard)
	if len(created) != 0 {
		t.Fatalf("failure stamped %s ahead of the decision clock created %d session beads, want 0 (within skew)",
			poolTerminalCreateClockSkew/2, len(created))
	}
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
			beaconTime:             terminalCooldownProcessStart,
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
		snapshot, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatalf("load session snapshot: %v", err)
		}
		result := buildDesiredStateWithSessionBeadsAt(
			cfg.EffectiveCityName(), cityPath, terminalCooldownProcessStart, now,
			cfg, runtime.NewFake(), store, nil, snapshot, nil, &stderr,
		)
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
	terminalCooldownBuild(t, cityPath, now, cfg, store, &stderr)

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

// TestPoolTerminalCreateCooldownWorkDirResolveFailureFailsOpenAndIsVisible
// covers the other fail-open branch: when the prospective work dir cannot be
// resolved the gate allows the create, but says so rather than going quiet.
func TestPoolTerminalCreateCooldownWorkDirResolveFailureFailsOpenAndIsVisible(t *testing.T) {
	cityPath := t.TempDir()
	agent := terminalCooldownAgent("worker", "{{.NoSuchField}}")
	var stderr bytes.Buffer
	fault := &terminalCooldownHistoryErrorStore{Store: beads.NewMemStore(), err: errors.New("must not be read")}
	bp := &agentBuildParams{
		city:      terminalCooldownCity(agent),
		cityName:  "cooldown-city",
		cityPath:  cityPath,
		agents:    []config.Agent{agent},
		beadStore: fault,
		stderr:    &stderr,
	}

	if poolTerminalCreateCooldownActive(bp, &agent, "worker", "worker", time.Now()) {
		t.Fatal("gate suppressed a create it could not resolve a work dir for, want fail-open")
	}
	if reads := fault.closedHistoryReads.Load(); reads != 0 {
		t.Fatalf("gate read history %d times without a resolved work dir, want 0", reads)
	}
	if !strings.Contains(stderr.String(), "fail-open") || !strings.Contains(stderr.String(), "work dir") {
		t.Fatalf("stderr %q does not surface the work-dir resolve failure", stderr.String())
	}
}

// TestPoolTerminalCreateCooldownWorkDirResolutionDoesNotCreateDirectories
// holds the gate to the gc-r9fx dry-run purity contract (see
// TestPoolTriggerWorkDirDoesNotCreateDirectories): evaluating a gate is a
// read, so it must resolve the work dir without creating it.
func TestPoolTerminalCreateCooldownWorkDirResolutionDoesNotCreateDirectories(t *testing.T) {
	cityPath := t.TempDir()
	agent := config.Agent{Name: "claude", Dir: "agents/claude", MaxActiveSessions: intPtr(3)}
	bp := &agentBuildParams{
		cityName:  "testcity",
		cityPath:  cityPath,
		beadStore: beads.NewMemStore(),
		stderr:    io.Discard,
	}

	poolTerminalCreateCooldownActive(bp, &agent, "claude", "claude", time.Now())

	if _, err := os.Stat(filepath.Join(cityPath, "agents")); !os.IsNotExist(err) {
		t.Fatalf("gate evaluation created directories under the city path (stat err = %v)", err)
	}
}

// TestPoolTerminalCreateCooldownPlanOnlyReadsNothing: a plan-only build makes
// no store reads, so the gate consults no history.
func TestPoolTerminalCreateCooldownPlanOnlyReadsNothing(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	workDir := sameTerminalCooldownDir(cityPath)
	base := beads.NewMemStore()
	recent := now.Add(-30 * time.Second)
	terminalCooldownSeedHistory(t, base, "history", "worker", "ephemeral",
		workDir, workDir, terminalCooldownCollisionReason, &recent, true)
	fault := &terminalCooldownHistoryErrorStore{Store: base, err: errors.New("plan-only must not read history")}
	agent := terminalCooldownAgent("worker", workDir)
	bp := &agentBuildParams{
		city: terminalCooldownCity(agent), cityName: "cooldown-city", cityPath: cityPath,
		beadStore: fault, stderr: io.Discard, planOnly: true,
	}

	if poolTerminalCreateCooldownActive(bp, &agent, "worker", "worker", now) {
		t.Fatal("plan-only build was throttled by history it must not read")
	}
	if reads := fault.closedHistoryReads.Load(); reads != 0 {
		t.Fatalf("plan-only gate performed %d closed-history reads, want 0", reads)
	}
}
