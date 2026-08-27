package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func reloadPoolIntPtr(n int) *int { return &n }

func TestFormatResolvedMaxActiveSessions(t *testing.T) {
	if got := formatResolvedMaxActiveSessions(nil); got != "unlimited" {
		t.Fatalf("nil = %q, want unlimited", got)
	}
	if got := formatResolvedMaxActiveSessions(reloadPoolIntPtr(-1)); got != "unlimited" {
		t.Fatalf("-1 = %q, want unlimited", got)
	}
	if got := formatResolvedMaxActiveSessions(reloadPoolIntPtr(0)); got != "0" {
		t.Fatalf("0 = %q, want 0", got)
	}
	if got := formatResolvedMaxActiveSessions(reloadPoolIntPtr(8)); got != "8" {
		t.Fatalf("8 = %q, want 8", got)
	}
}

func TestPoolMaxActiveSessionChanges_Table(t *testing.T) {
	t.Parallel()

	agent := func(name, dir string, maxActive *int) config.Agent {
		return config.Agent{Name: name, Dir: dir, MaxActiveSessions: maxActive}
	}

	tests := []struct {
		name string
		old  *config.City
		new  *config.City
		want []string // formatted change lines; empty means no changes
	}{
		{
			name: "bound increase reported",
			old: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(6))},
			},
			new: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(8))},
			},
			want: []string{"pool rig-a/executor: max_active_sessions 6 → 8"},
		},
		{
			name: "bound decrease reported",
			old: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(4))},
			},
			new: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(2))},
			},
			want: []string{"pool rig-a/executor: max_active_sessions 4 → 2"},
		},
		{
			name: "no pool changes",
			old: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(6))},
			},
			new: &config.City{
				Agents: []config.Agent{agent("executor", "rig-a", reloadPoolIntPtr(6))},
			},
			want: nil,
		},
		{
			name: "default resolution via workspace max",
			old: &config.City{
				Workspace: config.Workspace{MaxActiveSessions: reloadPoolIntPtr(2)},
				Agents:    []config.Agent{agent("worker", "", nil)},
			},
			new: &config.City{
				Workspace: config.Workspace{MaxActiveSessions: reloadPoolIntPtr(5)},
				Agents:    []config.Agent{agent("worker", "", nil)},
			},
			want: []string{"pool worker: max_active_sessions 2 → 5"},
		},
		{
			name: "default resolution via rig max",
			old: &config.City{
				Rigs:   []config.Rig{{Name: "oss", MaxActiveSessions: reloadPoolIntPtr(3)}},
				Agents: []config.Agent{agent("worker-pool", "oss", nil)},
			},
			new: &config.City{
				Rigs:   []config.Rig{{Name: "oss", MaxActiveSessions: reloadPoolIntPtr(7)}},
				Agents: []config.Agent{agent("worker-pool", "oss", nil)},
			},
			want: []string{"pool oss/worker-pool: max_active_sessions 3 → 7"},
		},
		{
			name: "explicit agent max wins over workspace change",
			old: &config.City{
				Workspace: config.Workspace{MaxActiveSessions: reloadPoolIntPtr(2)},
				Agents:    []config.Agent{agent("worker", "", reloadPoolIntPtr(4))},
			},
			new: &config.City{
				Workspace: config.Workspace{MaxActiveSessions: reloadPoolIntPtr(9)},
				Agents:    []config.Agent{agent("worker", "", reloadPoolIntPtr(4))},
			},
			want: nil,
		},
		{
			name: "nil to unlimited equivalent skips",
			old: &config.City{
				Agents: []config.Agent{agent("worker", "", nil)},
			},
			new: &config.City{
				Agents: []config.Agent{agent("worker", "", reloadPoolIntPtr(-1))},
			},
			want: nil,
		},
		{
			name: "added agent does not produce pool line",
			old: &config.City{
				Agents: []config.Agent{agent("a", "", reloadPoolIntPtr(1))},
			},
			new: &config.City{
				Agents: []config.Agent{
					agent("a", "", reloadPoolIntPtr(1)),
					agent("b", "", reloadPoolIntPtr(3)),
				},
			},
			want: nil,
		},
		{
			name: "multiple pools sorted by name",
			old: &config.City{
				Agents: []config.Agent{
					agent("planner", "rig-a", reloadPoolIntPtr(2)),
					agent("executor", "rig-a", reloadPoolIntPtr(6)),
				},
			},
			new: &config.City{
				Agents: []config.Agent{
					agent("planner", "rig-a", reloadPoolIntPtr(3)),
					agent("executor", "rig-a", reloadPoolIntPtr(8)),
				},
			},
			want: []string{
				"pool rig-a/executor: max_active_sessions 6 → 8",
				"pool rig-a/planner: max_active_sessions 2 → 3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := poolMaxActiveSessionChanges(tt.old, tt.new)
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("changes = %+v, want none", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(changes) = %d, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, line := range tt.want {
				if gotLine := formatPoolMaxBoundChangeLine(got[i]); gotLine != line {
					t.Fatalf("change[%d] = %q, want %q", i, gotLine, line)
				}
			}
		})
	}
}

func TestPoolMaxBoundDrainWarning(t *testing.T) {
	t.Parallel()
	c := poolMaxBoundChange{
		Name: "rig-a/executor",
		Old:  reloadPoolIntPtr(4),
		New:  reloadPoolIntPtr(2),
	}
	if got := poolMaxBoundDrainWarning(c, 2); got != "" {
		t.Fatalf("active==new bound: warning = %q, want empty", got)
	}
	if got := poolMaxBoundDrainWarning(c, 1); got != "" {
		t.Fatalf("active<new bound: warning = %q, want empty", got)
	}
	got := poolMaxBoundDrainWarning(c, 3)
	if !strings.Contains(got, "rig-a/executor") {
		t.Fatalf("warning missing pool name: %q", got)
	}
	if !strings.Contains(got, "4 → 2") {
		t.Fatalf("warning missing bounds: %q", got)
	}
	if !strings.Contains(got, "3 active") {
		t.Fatalf("warning missing active count: %q", got)
	}
	if !strings.Contains(got, "normal reconcile") || !strings.Contains(got, "not replaced") {
		t.Fatalf("warning missing attrition semantics: %q", got)
	}
	// Unlimited new bound never warns.
	c.New = nil
	if got := poolMaxBoundDrainWarning(c, 99); got != "" {
		t.Fatalf("unlimited new: warning = %q, want empty", got)
	}
}

func TestPoolMaxBoundDrainWarningIgnoresIncreases(t *testing.T) {
	t.Parallel()
	// Bound went UP but active sits above the new bound: not a drain.
	up := poolMaxBoundChange{Name: "rig-a/executor", Old: reloadPoolIntPtr(2), New: reloadPoolIntPtr(4)}
	if got := poolMaxBoundDrainWarning(up, 6); got != "" {
		t.Fatalf("increase with active above new bound: warning = %q, want empty", got)
	}
	// Unlimited → finite is still a tightening and must warn.
	tighten := poolMaxBoundChange{Name: "rig-a/planner", Old: nil, New: reloadPoolIntPtr(2)}
	if got := poolMaxBoundDrainWarning(tighten, 5); got == "" {
		t.Fatal("unlimited→finite with active above new bound: want warning")
	}
}

func TestAppendPoolMaxBoundFeedbackMixedChanges(t *testing.T) {
	t.Parallel()
	changes := []poolMaxBoundChange{
		{Name: "rig-a/executor", Old: reloadPoolIntPtr(2), New: reloadPoolIntPtr(4)},
		{Name: "rig-a/planner", Old: reloadPoolIntPtr(4), New: reloadPoolIntPtr(2)},
	}
	active := map[string]int{"rig-a/executor": 6, "rig-a/planner": 3}
	msg, warnings := appendPoolMaxBoundFeedback("Config reloaded", nil, changes, active)
	if !strings.Contains(msg, "pool rig-a/executor: max_active_sessions 2 → 4") {
		t.Fatalf("message missing increase line: %q", msg)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "rig-a/planner") {
		t.Fatalf("warnings = %v, want exactly one planner drain warning", warnings)
	}
}

func TestAppendPoolMaxBoundFeedback(t *testing.T) {
	t.Parallel()
	base := "Config reloaded: 1 agents, 0 rigs (rev abc)"
	changes := []poolMaxBoundChange{
		{Name: "rig-a/executor", Old: reloadPoolIntPtr(6), New: reloadPoolIntPtr(8)},
		{Name: "rig-a/planner", Old: reloadPoolIntPtr(4), New: reloadPoolIntPtr(2)},
	}
	active := map[string]int{"rig-a/planner": 3}
	msg, warnings := appendPoolMaxBoundFeedback(base, nil, changes, active)
	if !strings.HasPrefix(msg, base+"\n") {
		t.Fatalf("message prefix = %q", msg)
	}
	if !strings.Contains(msg, "pool rig-a/executor: max_active_sessions 6 → 8") {
		t.Fatalf("message missing increase line: %q", msg)
	}
	if !strings.Contains(msg, "pool rig-a/planner: max_active_sessions 4 → 2") {
		t.Fatalf("message missing decrease line: %q", msg)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "rig-a/planner") {
		t.Fatalf("warnings = %v, want one planner drain warning", warnings)
	}

	// No changes: message and warnings unchanged.
	msg2, warnings2 := appendPoolMaxBoundFeedback(base, []string{"keep"}, nil, nil)
	if msg2 != base {
		t.Fatalf("no-change message = %q, want %q", msg2, base)
	}
	if len(warnings2) != 1 || warnings2[0] != "keep" {
		t.Fatalf("no-change warnings = %v", warnings2)
	}
}

func TestPoolBoundChangesNeedActiveCounts(t *testing.T) {
	t.Parallel()
	if poolBoundChangesNeedActiveCounts(nil) {
		t.Fatal("nil changes should not need counts")
	}
	if poolBoundChangesNeedActiveCounts([]poolMaxBoundChange{
		{Name: "a", Old: reloadPoolIntPtr(2), New: reloadPoolIntPtr(8)},
	}) {
		t.Fatal("increase-only should not need counts")
	}
	if !poolBoundChangesNeedActiveCounts([]poolMaxBoundChange{
		{Name: "a", Old: reloadPoolIntPtr(8), New: reloadPoolIntPtr(2)},
	}) {
		t.Fatal("finite decrease should need counts")
	}
	if !poolBoundChangesNeedActiveCounts([]poolMaxBoundChange{
		{Name: "a", Old: nil, New: reloadPoolIntPtr(2)},
	}) {
		t.Fatal("unlimited→finite should need counts")
	}
	if poolBoundChangesNeedActiveCounts([]poolMaxBoundChange{
		{Name: "a", Old: reloadPoolIntPtr(2), New: nil},
	}) {
		t.Fatal("finite→unlimited should not need counts")
	}
}

// TestCountActiveSessionsByTemplate_NoStore returns empty when the sessions
// bead store is unavailable — no provider-list heuristics.
func TestCountActiveSessionsByTemplate_NoStore(t *testing.T) {
	t.Parallel()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "planner", MaxActiveSessions: reloadPoolIntPtr(4)},
		},
	}
	sp := runtime.NewFake()
	// Provider sessions must not be attributed without a bead store.
	for _, name := range []string{"planner-1", "planner-2", "gc-test-city-planner"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: t.TempDir(),
		CityName: "test-city",
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	counts := cr.countActiveSessionsByTemplate(cfg)
	if len(counts) != 0 {
		t.Fatalf("without bead store, counts = %v, want empty", counts)
	}
}

// TestCountActiveSessionsByTemplate_BeadStore counts open session beads by template.
func TestCountActiveSessionsByTemplate_BeadStore(t *testing.T) {
	t.Parallel()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "planner", MaxActiveSessions: reloadPoolIntPtr(4)},
			{Name: "executor", MaxActiveSessions: reloadPoolIntPtr(6)},
		},
	}
	store := beads.NewMemStore()
	for i, name := range []string{"planner-1", "planner-2", "executor-1"} {
		agent := "planner"
		if strings.HasPrefix(name, "executor") {
			agent = "executor"
		}
		if _, err := sessionFrontDoor(store).CreateSession(session.CreateSpec{
			Title:     agent,
			AgentName: agent,
			Metadata: map[string]string{
				"session_name": name,
				"template":     agent,
			},
		}); err != nil {
			t.Fatalf("create session %d (%s): %v", i, name, err)
		}
	}
	// Closed session must not count.
	closedID, err := sessionFrontDoor(store).CreateSession(session.CreateSpec{
		Title:     "planner",
		AgentName: "planner",
		Metadata: map[string]string{
			"session_name": "planner-closed",
			"template":     "planner",
		},
	})
	if err != nil {
		t.Fatalf("create closed session: %v", err)
	}
	if err := store.Close(closedID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: t.TempDir(),
		CityName: "test-city",
		Cfg:      cfg,
		SP:       runtime.NewFake(),
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(runtime.NewFake()),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cr.standaloneCityStore = store

	counts := cr.countActiveSessionsByTemplate(cfg)
	if got := counts["planner"]; got != 2 {
		t.Fatalf("counts[planner] = %d, want 2; full=%v", got, counts)
	}
	if got := counts["executor"]; got != 1 {
		t.Fatalf("counts[executor] = %d, want 1; full=%v", got, counts)
	}
}

func TestReloadConfigTracedReportsPoolMaxBoundChanges(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeReloadPoolBoundConfig(t, tomlPath, 6, 4)

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	// Three open planner session beads so a decrease to 2 produces a drain warning.
	// Counts come only from the sessions bead store (no provider-list heuristics).
	store := beads.NewMemStore()
	for i, name := range []string{"planner-1", "planner-2", "planner-3"} {
		if _, err := sessionFrontDoor(store).CreateSession(session.CreateSpec{
			Title:     "planner",
			AgentName: "planner",
			Metadata: map[string]string{
				"session_name": name,
				"template":     "planner",
			},
		}); err != nil {
			t.Fatalf("create session %d (%s): %v", i, name, err)
		}
	}
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	cr.standaloneCityStore = store

	// (a) increase + (b) decrease with active above new bound
	writeReloadPoolBoundConfig(t, tomlPath, 8, 2)
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q, want applied; error=%q warnings=%v stderr=%q",
			reply.Outcome, reply.Error, reply.Warnings, stderr.String())
	}
	if !strings.Contains(reply.Message, "pool executor: max_active_sessions 6 → 8") {
		t.Fatalf("message missing increase: %q", reply.Message)
	}
	if !strings.Contains(reply.Message, "pool planner: max_active_sessions 4 → 2") {
		t.Fatalf("message missing decrease: %q", reply.Message)
	}
	if !warningsContain(reply.Warnings, "pool planner: max_active_sessions 4 → 2") {
		t.Fatalf("warnings missing drain notice: %v", reply.Warnings)
	}
	if !warningsContain(reply.Warnings, "3 active") {
		t.Fatalf("warnings missing active count: %v", reply.Warnings)
	}
	// Increase must not produce a drain warning.
	for _, w := range reply.Warnings {
		if strings.Contains(w, "executor") && strings.Contains(w, "drain") {
			t.Fatalf("unexpected drain warning for increase: %q", w)
		}
	}
}

func TestReloadConfigTracedNoPoolLinesWhenBoundsUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeReloadPoolBoundConfig(t, tomlPath, 6, 4)

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	// Change only an unrelated field so revision changes but pool bounds do not.
	writeReloadPoolBoundConfigWithExtra(t, tomlPath, 6, 4, "1s")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q, want applied; error=%q", reply.Outcome, reply.Error)
	}
	if strings.Contains(reply.Message, "max_active_sessions") {
		t.Fatalf("message should omit pool lines when bounds unchanged: %q", reply.Message)
	}
	for _, w := range reply.Warnings {
		if strings.Contains(w, "max_active_sessions") {
			t.Fatalf("warnings should omit pool lines: %v", reply.Warnings)
		}
	}
}

func TestReloadConfigTracedDefaultResolutionWorkspaceMax(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeReloadWorkspaceMaxConfig(t, tomlPath, 2)

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	writeReloadWorkspaceMaxConfig(t, tomlPath, 5)
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q, want applied; error=%q", reply.Outcome, reply.Error)
	}
	if !strings.Contains(reply.Message, "pool worker: max_active_sessions 2 → 5") {
		t.Fatalf("message missing default-resolution change: %q", reply.Message)
	}
}

func writeReloadPoolBoundConfig(t *testing.T, tomlPath string, executorMax, plannerMax int) {
	t.Helper()
	writeReloadPoolBoundConfigWithExtra(t, tomlPath, executorMax, plannerMax, "")
}

func writeReloadPoolBoundConfigWithExtra(t *testing.T, tomlPath string, executorMax, plannerMax int, shutdownTimeout string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test-city\"\n\n")
	b.WriteString("[beads]\nprovider = \"file\"\n\n")
	b.WriteString("[session]\nprovider = \"fake\"\n\n")
	if shutdownTimeout != "" {
		b.WriteString("[daemon]\nshutdown_timeout = \"")
		b.WriteString(shutdownTimeout)
		b.WriteString("\"\n\n")
	}
	b.WriteString("[[agent]]\nname = \"executor\"\nmax_active_sessions = ")
	b.WriteString(strconv.Itoa(executorMax))
	b.WriteString("\n\n[[agent]]\nname = \"planner\"\nmax_active_sessions = ")
	b.WriteString(strconv.Itoa(plannerMax))
	b.WriteString("\n")
	if err := os.WriteFile(tomlPath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeReloadWorkspaceMaxConfig(t *testing.T, tomlPath string, workspaceMax int) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	data := "[workspace]\nname = \"test-city\"\nmax_active_sessions = " + strconv.Itoa(workspaceMax) +
		"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n" +
		"[[agent]]\nname = \"worker\"\n"
	if err := os.WriteFile(tomlPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
