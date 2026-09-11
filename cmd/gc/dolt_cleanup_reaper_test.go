package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractConfigPath_SpaceSeparated(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--config", "/tmp/TestFoo123/config.yaml"}
	got := extractConfigPath(argv)
	want := "/tmp/TestFoo123/config.yaml"
	if got != want {
		t.Errorf("extractConfigPath() = %q, want %q", got, want)
	}
}

func TestExtractConfigPath_EqualsForm(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--config=/tmp/TestFoo/config.yaml"}
	got := extractConfigPath(argv)
	want := "/tmp/TestFoo/config.yaml"
	if got != want {
		t.Errorf("extractConfigPath() = %q, want %q", got, want)
	}
}

func TestExtractConfigPath_Missing(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--port", "3307"}
	got := extractConfigPath(argv)
	if got != "" {
		t.Errorf("extractConfigPath() = %q, want empty", got)
	}
}

func TestExtractConfigPath_FlagAtEnd(t *testing.T) {
	// --config with no value should return empty (malformed cmdline).
	argv := []string{"dolt", "sql-server", "--config"}
	got := extractConfigPath(argv)
	if got != "" {
		t.Errorf("extractConfigPath() = %q, want empty for trailing --config", got)
	}
}

func TestExtractDataDirPath_SpaceSeparated(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--data-dir", "/tmp/TestFoo123/dolt"}
	got := extractDataDirPath(argv)
	want := "/tmp/TestFoo123/dolt"
	if got != want {
		t.Errorf("extractDataDirPath() = %q, want %q", got, want)
	}
}

func TestExtractDataDirPath_EqualsForm(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--data-dir=/tmp/TestFoo/dolt"}
	got := extractDataDirPath(argv)
	want := "/tmp/TestFoo/dolt"
	if got != want {
		t.Errorf("extractDataDirPath() = %q, want %q", got, want)
	}
}

func TestExtractDataDirPath_Missing(t *testing.T) {
	argv := []string{"dolt", "sql-server", "--config", "/tmp/TestFoo/config.yaml"}
	got := extractDataDirPath(argv)
	if got != "" {
		t.Errorf("extractDataDirPath() = %q, want empty", got)
	}
}

func TestIsTestConfigPath_TmpTestPrefix(t *testing.T) {
	if !isTestConfigPath("/tmp/TestOrchestrator123/config.yaml", "/home/u", "") {
		t.Error("expected /tmp/Test* to be a test path")
	}
}

func TestIsTestConfigPath_CmdGCTestPrefix(t *testing.T) {
	if !isTestConfigPath("/tmp/gctest-123/TestCase/001/.gc/runtime/packs/dolt/dolt-config.yaml", "/home/u", "") {
		t.Error("expected /tmp/gctest-* to be a test path")
	}
}

func TestIsTestConfigPath_HomeGotmpTestPrefix(t *testing.T) {
	if !isTestConfigPath("/home/u/.gotmp/TestFuzz/config.yaml", "/home/u", "") {
		t.Error("expected $HOME/.gotmp/Test* to be a test path")
	}
}

func TestIsTestConfigPath_ProcessTempDirTestPrefix(t *testing.T) {
	if !isTestConfigPath("/var/tmp/go-test/TestRepro/config.yaml", "/home/u", "/var/tmp/go-test") {
		t.Error("expected os.TempDir()/Test* to be a test path")
	}
}

func TestIsTestConfigPath_KnownGCTestPrefix(t *testing.T) {
	if !isTestConfigPath("/data/tmp/gc-state-mutation-builtin-123/.gc/runtime/packs/dolt/dolt-config.yaml", "/home/u", "/data/tmp") {
		t.Error("expected known gc-* test prefix under os.TempDir() to be a test path")
	}
}

func TestIsTestConfigPath_IntegrationTempPrefixes(t *testing.T) {
	cases := []string{
		"/tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml",
		"/tmp/gc-int-env-123/.gc/runtime/packs/dolt/dolt-config.yaml",
	}
	for _, p := range cases {
		if !isTestConfigPath(p, "/home/u", "") {
			t.Errorf("isTestConfigPath(%q) = false, want true", p)
		}
	}
}

func TestIsTestConfigPath_NotTest(t *testing.T) {
	cases := []string{
		"/tmp/be-s9d-bench-dolt/config.yaml", // benchmark
		"/var/lib/dolt/config.yaml",          // production-ish
		"/tmp/random/config.yaml",            // tmp but not Test prefix
		"/home/u/.gotmp/other/config.yaml",   // gotmp but not Test prefix
		"/var/tmp/go-test/Other/config.yaml", // temp root but not Test prefix
		"",                                   // missing
	}
	for _, p := range cases {
		if isTestConfigPath(p, "/home/u", "/var/tmp/go-test") {
			t.Errorf("isTestConfigPath(%q) = true, want false", p)
		}
	}
}

func TestClassifyDoltProcess_ProtectedByRigPort(t *testing.T) {
	p := DoltProcInfo{
		PID:   1234,
		Argv:  []string{"dolt", "sql-server", "--config", "/tmp/TestFoo/config.yaml"},
		Ports: []int{28231},
	}
	got := classifyDoltProcess(p, map[int]string{28231: "beads"}, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect", got.Action)
	}
	if got.Reason == "" || !strings.Contains(got.Reason, "rig") || !strings.Contains(got.Reason, "beads") {
		t.Errorf("Reason = %q, want rig+beads reference", got.Reason)
	}
}

func TestClassifyDoltProcess_OrphanByTestPath(t *testing.T) {
	p := DoltProcInfo{
		PID:   2222,
		Argv:  []string{"dolt", "sql-server", "--config", "/tmp/TestMailRouter9182/config.yaml"},
		Ports: []int{},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "reap" {
		t.Errorf("Action = %q, want reap", got.Action)
	}
	if got.ConfigPath != "/tmp/TestMailRouter9182/config.yaml" {
		t.Errorf("ConfigPath = %q", got.ConfigPath)
	}
}

func TestClassifyDoltProcess_ReapsIntegrationTempRoots(t *testing.T) {
	cases := []string{
		"/tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml",
		"/tmp/gc-int-env-123/.gc/runtime/packs/dolt/dolt-config.yaml",
	}
	for _, cfg := range cases {
		p := DoltProcInfo{
			PID:   2224,
			Argv:  []string{"dolt", "sql-server", "--config", cfg},
			Ports: []int{},
		}
		got := classifyDoltProcess(p, nil, "/home/u", "", nil)
		if got.Action != "reap" {
			t.Errorf("classifyDoltProcess(%q).Action = %q, want reap", cfg, got.Action)
		}
		if got.ConfigPath != cfg {
			t.Errorf("classifyDoltProcess(%q).ConfigPath = %q, want %q", cfg, got.ConfigPath, cfg)
		}
	}
}

func TestClassifyDoltProcess_ProtectsActiveTestRoot(t *testing.T) {
	p := DoltProcInfo{
		PID:   2223,
		Argv:  []string{"dolt", "sql-server", "--config", "/tmp/TestPersonalWorkFormulaCompileAndRun123/001/city/.gc/runtime/packs/dolt/dolt-config.yaml"},
		Ports: []int{},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", []string{"/tmp/TestPersonalWorkFormulaCompileAndRun123"})

	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect", got.Action)
	}
	if !strings.Contains(got.Reason, "active test root") {
		t.Errorf("Reason = %q, want active-test-root reason", got.Reason)
	}
}

func TestClassifyDoltProcess_ProtectedByPathNotOnAllowlist(t *testing.T) {
	// Active benchmark — config path doesn't match /tmp/Test*.
	p := DoltProcInfo{
		PID:   3333,
		Argv:  []string{"dolt", "sql-server", "--config", "/tmp/be-s9d-bench-dolt/config.yaml"},
		Ports: []int{33400},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect", got.Action)
	}
	if !strings.Contains(got.Reason, "allowlist") {
		t.Errorf("Reason = %q, want mention of allowlist", got.Reason)
	}
	// Reason should echo the actual config path so operators can see it.
	if !strings.Contains(got.Reason, "/tmp/be-s9d-bench-dolt") {
		t.Errorf("Reason = %q, want config path echoed (architect Open Q 0)", got.Reason)
	}
}

func TestClassifyDoltProcess_ProtectsRealManagedConfig(t *testing.T) {
	cfg := "/home/u/projects/foo/.gc/runtime/packs/dolt/dolt-config.yaml"
	p := DoltProcInfo{
		PID:   3334,
		Argv:  []string{"dolt", "sql-server", "--config", cfg},
		Ports: []int{},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)
	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect", got.Action)
	}
	if !strings.Contains(got.Reason, "allowlist") || !strings.Contains(got.Reason, cfg) {
		t.Errorf("Reason = %q, want allowlist reason containing config path", got.Reason)
	}
}

func TestClassifyDoltProcess_ProtectedWhenConfigMissing(t *testing.T) {
	p := DoltProcInfo{
		PID:   4444,
		Argv:  []string{"dolt", "sql-server"},
		Ports: []int{},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect", got.Action)
	}
	if !strings.Contains(got.Reason, "config") {
		t.Errorf("Reason = %q, want config-path-related reason", got.Reason)
	}
}

func TestClassifyDoltProcess_RigPortBeatsConfigPath(t *testing.T) {
	// Even if the cmdline says /tmp/Test*, a rig-port match always protects.
	p := DoltProcInfo{
		PID:   5555,
		Argv:  []string{"dolt", "sql-server", "--config", "/tmp/TestSomething/config.yaml"},
		Ports: []int{28231},
	}
	got := classifyDoltProcess(p, map[int]string{28231: "beads"}, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Errorf("Action = %q, want protect (rig port wins)", got.Action)
	}
}

func TestClassifyDoltProcess_DeletedScopeSignals(t *testing.T) {
	// Deleted-scope reap signals for dolt servers (ga-10wmzh): a process whose
	// cwd readlink ends in " (deleted)" is an unambiguous zombie even without
	// --config, and that deleted-cwd signal is what authorizes reaping. A
	// --config path that has vanished is NOT a standalone reap signal — on its
	// own (live or unknown cwd) it protects, because a lone missing-config
	// observation is not proof the scope was removed; it only reaps when the
	// deleted-cwd signal corroborates it. Both deleted-cwd and active-test-root
	// protection are evaluated before the no-config / not-on-allowlist branches.
	// Unknown state must always protect.
	cases := []struct {
		name           string
		proc           DoltProcInfo
		rigPorts       map[int]string
		activeRoots    []string
		wantAction     string
		wantConfigPath string
		wantReasonSub  string
	}{
		{
			name: "deleted cwd without config reaps",
			proc: DoltProcInfo{
				PID:      6001,
				Argv:     []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33401"},
				CWDState: procPathStateDeleted,
			},
			wantAction:    "reap",
			wantReasonSub: "deleted",
		},
		{
			name: "deleted cwd with non-allowlist config reaps and echoes config",
			proc: DoltProcInfo{
				PID:      6002,
				Argv:     []string{"dolt", "sql-server", "--config", "/data/clones/pr-123/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState: procPathStateDeleted,
			},
			wantAction:     "reap",
			wantConfigPath: "/data/clones/pr-123/.gc/runtime/packs/dolt/dolt-config.yaml",
			wantReasonSub:  "deleted",
		},
		{
			name: "missing config with live cwd protects (needs deleted-cwd corroboration)",
			proc: DoltProcInfo{
				PID:             6003,
				Argv:            []string{"dolt", "sql-server", "--config", "/data/worktrees/gone/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState:        procPathStateLive,
				ConfigPathState: procPathStateDeleted,
			},
			wantAction:    "protect",
			wantReasonSub: "missing",
		},
		{
			name: "missing config with unknown cwd protects and does not claim cwd live",
			proc: DoltProcInfo{
				PID:             6012,
				Argv:            []string{"dolt", "sql-server", "--config", "/data/worktrees/gone/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState:        procPathStateUnknown,
				ConfigPathState: procPathStateDeleted,
			},
			wantAction:    "protect",
			wantReasonSub: "could not be determined",
		},
		{
			name: "missing config with deleted cwd reaps (corroborated) and echoes config",
			proc: DoltProcInfo{
				PID:             6010,
				Argv:            []string{"dolt", "sql-server", "--config", "/data/worktrees/gone/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState:        procPathStateDeleted,
				ConfigPathState: procPathStateDeleted,
			},
			wantAction:     "reap",
			wantConfigPath: "/data/worktrees/gone/.gc/runtime/packs/dolt/dolt-config.yaml",
			wantReasonSub:  "deleted",
		},
		{
			name: "missing config on allowlist test path still reaps (ownership signal)",
			proc: DoltProcInfo{
				PID:             6011,
				Argv:            []string{"dolt", "sql-server", "--config", "/tmp/TestLeaked123/001/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState:        procPathStateLive,
				ConfigPathState: procPathStateDeleted,
			},
			wantAction:     "reap",
			wantConfigPath: "/tmp/TestLeaked123/001/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
		{
			name: "live cwd with existing non-allowlist config protects",
			proc: DoltProcInfo{
				PID:             6004,
				Argv:            []string{"dolt", "sql-server", "--config", "/var/lib/external-app/dolt-config.yaml"},
				CWDState:        procPathStateLive,
				ConfigPathState: procPathStateLive,
			},
			wantAction:    "protect",
			wantReasonSub: "allowlist",
		},
		{
			name: "unknown cwd and unknown config state protects",
			proc: DoltProcInfo{
				PID:  6005,
				Argv: []string{"dolt", "sql-server", "--config", "/var/lib/external-app/dolt-config.yaml"},
			},
			wantAction:    "protect",
			wantReasonSub: "allowlist",
		},
		{
			name: "unknown cwd without config protects",
			proc: DoltProcInfo{
				PID:  6006,
				Argv: []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33402"},
			},
			wantAction:    "protect",
			wantReasonSub: "no --config",
		},
		{
			name: "live cwd without config protects",
			proc: DoltProcInfo{
				PID:      6007,
				Argv:     []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33403"},
				CWDState: procPathStateLive,
			},
			wantAction:    "protect",
			wantReasonSub: "no --config",
		},
		{
			name: "rig port match beats deleted cwd",
			proc: DoltProcInfo{
				PID:      6008,
				Argv:     []string{"dolt", "sql-server"},
				Ports:    []int{28231},
				CWDState: procPathStateDeleted,
			},
			rigPorts:      map[int]string{28231: "beads"},
			wantAction:    "protect",
			wantReasonSub: "rig",
		},
		{
			name: "active test root beats missing config",
			proc: DoltProcInfo{
				PID:             6009,
				Argv:            []string{"dolt", "sql-server", "--config", "/tmp/TestActive123/001/.gc/runtime/packs/dolt/dolt-config.yaml"},
				CWDState:        procPathStateLive,
				ConfigPathState: procPathStateDeleted,
			},
			activeRoots:   []string{"/tmp/TestActive123"},
			wantAction:    "protect",
			wantReasonSub: "active test root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDoltProcess(tc.proc, tc.rigPorts, "/home/u", "", tc.activeRoots)
			if got.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q (reason: %q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.Action == "reap" && got.ConfigPath != tc.wantConfigPath {
				t.Errorf("ConfigPath = %q, want %q", got.ConfigPath, tc.wantConfigPath)
			}
			if tc.wantReasonSub != "" && !strings.Contains(got.Reason, tc.wantReasonSub) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, tc.wantReasonSub)
			}
		})
	}
}

func TestPlanReap_CarriesDeletedScopeReason(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 7001, Argv: []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33410"}, CWDState: procPathStateDeleted},
	}
	plan := planOrphanReap(procs, nil, "/home/u", "", nil)
	if len(plan.Reap) != 1 {
		t.Fatalf("Reap len = %d, want 1 (protected: %+v)", len(plan.Reap), plan.Protected)
	}
	if plan.Reap[0].Reason == "" {
		t.Errorf("ReapTarget.Reason empty; want deleted-cwd explanation for the report")
	}
}

func TestPlanReap_BuildsOrphanAndProtectedLists(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 1138290, Ports: []int{28231}, Argv: []string{"dolt", "sql-server"}},
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}},
		{PID: 1319499, Ports: []int{33400}, Argv: []string{"dolt", "sql-server", "--config", "/tmp/be-s9d-bench-dolt/config.yaml"}},
		{PID: 1281099, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestB/config.yaml"}},
		{PID: 1281100, Argv: []string{"dolt", "sql-server", "--config", "/data/tmp/gc-state-runtime-builtin-1/.gc/runtime/packs/dolt/dolt-config.yaml"}},
		{PID: 1281101, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestActive/001/city/.gc/runtime/packs/dolt/dolt-config.yaml"}},
	}
	rigPorts := map[int]string{28231: "beads"}

	plan := planOrphanReap(procs, rigPorts, "/home/u", "/data/tmp", []string{"/tmp/TestActive"})

	wantReap := []int{1281044, 1281099, 1281100}
	gotReap := make([]int, 0, len(plan.Reap))
	for _, target := range plan.Reap {
		gotReap = append(gotReap, target.PID)
	}
	if !reflect.DeepEqual(gotReap, wantReap) {
		t.Errorf("Reap PIDs = %v, want %v", gotReap, wantReap)
	}

	wantProtected := []int{1138290, 1319499, 1281101}
	gotProtected := make([]int, 0, len(plan.Protected))
	for _, e := range plan.Protected {
		gotProtected = append(gotProtected, e.PID)
	}
	if !reflect.DeepEqual(gotProtected, wantProtected) {
		t.Errorf("Protected PIDs = %v, want %v", gotProtected, wantProtected)
	}
}

// TestClassifyDoltProcess_BareServerReapsWhenDataDirOnAllowlist covers
// ga-ntbpyb.2 acceptance criterion 1's confirmed regression exemplar:
// examples/gastown's TestReaperWorkflowRootCleanupRealDoltSemantics launches
// `dolt sql-server --data-dir <t.TempDir()>/dolt` with no --config at all.
// Before this change, classifyDoltProcess's rule 4 protected every bare
// server unconditionally; a --data-dir match against the allowlist is now an
// ownership signal in its own right, mirroring the existing --config
// allowlist rule.
func TestClassifyDoltProcess_BareServerReapsWhenDataDirOnAllowlist(t *testing.T) {
	dataDir := "/tmp/TestReaperWorkflowRootCleanupRealDoltSemantics42/002/dolt"
	p := DoltProcInfo{
		PID:  6001,
		Argv: []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33420", "--data-dir", dataDir, "--loglevel", "warning"},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap", got.Action)
	}
	if got.DataDir != dataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, dataDir)
	}
	if got.ConfigPath != "" {
		t.Errorf("ConfigPath = %q, want empty (no --config on this cmdline)", got.ConfigPath)
	}
}

func TestClassifyDoltProcess_BareServerProtectsWhenDataDirNotOnAllowlist(t *testing.T) {
	dataDir := "/var/lib/prod-dolt-store"
	p := DoltProcInfo{
		PID:  6002,
		Argv: []string{"dolt", "sql-server", "--data-dir", dataDir},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect (data-dir not test-owned)", got.Action)
	}
	if got.DataDir != "" {
		t.Errorf("DataDir = %q, want empty when protecting", got.DataDir)
	}
	if !strings.Contains(got.Reason, dataDir) || !strings.Contains(got.Reason, "allowlist") {
		t.Errorf("Reason = %q, want allowlist reason containing data-dir path", got.Reason)
	}
}

func TestClassifyDoltProcess_BareServerNoConfigNoDataDirPreservesOriginalReason(t *testing.T) {
	// Byte-for-byte preservation check: a truly bare server (neither --config
	// nor --data-dir) must still hit the exact original protect message, so
	// any caller matching on this literal string is unaffected by the
	// data-dir-driven rule 4 extension.
	p := DoltProcInfo{
		PID:  6003,
		Argv: []string{"dolt", "sql-server", "-H", "127.0.0.1"},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	want := "no --config path detected; refusing to kill an unidentified dolt server"
	if got.Action != "protect" || got.Reason != want {
		t.Errorf("got {Action: %q, Reason: %q}, want {protect, %q}", got.Action, got.Reason, want)
	}
	if got.DataDir != "" {
		t.Errorf("DataDir = %q, want empty", got.DataDir)
	}
}

func TestClassifyDoltProcess_ConfigAllowlistReapAlsoPopulatesDataDir(t *testing.T) {
	cfg := "/tmp/TestA/config.yaml"
	dataDir := "/tmp/TestA/dolt"
	p := DoltProcInfo{
		PID:  6004,
		Argv: []string{"dolt", "sql-server", "--config", cfg, "--data-dir", dataDir},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap", got.Action)
	}
	if got.ConfigPath != cfg {
		t.Errorf("ConfigPath = %q, want %q", got.ConfigPath, cfg)
	}
	if got.DataDir != dataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, dataDir)
	}
}

// TestClassifyDoltProcess_ConfigAllowlistReapDataDirNotOnAllowlistStaysEmpty
// is the key safety-gate test: DataDir is never trusted merely because the
// process as a whole was reaped via its --config allowlist match. The
// --data-dir value must independently pass the same allowlist, or the
// process is still reaped but no directory is removed.
func TestClassifyDoltProcess_ConfigAllowlistReapDataDirNotOnAllowlistStaysEmpty(t *testing.T) {
	cfg := "/tmp/TestA/config.yaml"
	unrelatedDataDir := "/var/lib/some-other-store"
	p := DoltProcInfo{
		PID:  6005,
		Argv: []string{"dolt", "sql-server", "--config", cfg, "--data-dir", unrelatedDataDir},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap (config allowlist match)", got.Action)
	}
	if got.DataDir != "" {
		t.Errorf("DataDir = %q, want empty: a non-allowlisted --data-dir must never be trusted for removal, even when the process is reaped on other grounds", got.DataDir)
	}
}

func TestClassifyDoltProcess_DeletedCwdReapAlsoPopulatesDataDir(t *testing.T) {
	dataDir := "/tmp/TestZombie/dolt"
	p := DoltProcInfo{
		PID:      6006,
		Argv:     []string{"dolt", "sql-server", "--data-dir", dataDir},
		CWDState: procPathStateDeleted,
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap (deleted cwd)", got.Action)
	}
	if got.DataDir != dataDir {
		t.Errorf("DataDir = %q, want %q", got.DataDir, dataDir)
	}
}

func TestPlanReap_CarriesDataDirForBareServerWithAllowlistedDataDir(t *testing.T) {
	dataDir := "/tmp/TestReaperWorkflowRootCleanupRealDoltSemantics42/002/dolt"
	procs := []DoltProcInfo{
		{PID: 7002, Argv: []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33421", "--data-dir", dataDir}},
	}
	plan := planOrphanReap(procs, nil, "/home/u", "", nil)

	if len(plan.Reap) != 1 {
		t.Fatalf("Reap len = %d, want 1 (protected: %+v)", len(plan.Reap), plan.Protected)
	}
	if plan.Reap[0].DataDir != dataDir {
		t.Errorf("ReapTarget.DataDir = %q, want %q", plan.Reap[0].DataDir, dataDir)
	}
}

// TestContainerDoltServerIsClassified covers ga-sm1cvj: a bare dolt server
// (no --config) running inside a container is currently misclassified with
// the generic "no --config path detected" protect reason, which is
// misleading — killing the PID on the host does nothing to a
// container-managed process, and the reason gives the operator no hint that
// the runtime CLI (not SIGKILL) is the correct remediation.
func TestContainerDoltServerIsClassified(t *testing.T) {
	p := DoltProcInfo{
		PID:              6020,
		Argv:             []string{"dolt", "sql-server", "-H", "127.0.0.1"},
		ContainerRuntime: "podman",
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)
	if got.Action != "protect" || !strings.Contains(got.Reason, "container") {
		t.Errorf("got {Action: %q, Reason: %q}, want protect with a container-aware reason", got.Action, got.Reason)
	}
}

// TestGotmpdirTestConfigIsOnAllowlist covers ga-sm1cvj: the fleet's go shim
// pins GOTMPDIR=/var/tmp/gotmp (see AGENTS.md Build Cache Conventions), so
// `go test` scratch dirs used by dolt integration tests live there — not
// under os.TempDir() (tempDir here is "/tmp", not "/var/tmp/gotmp").
//
// An allowlist hit is an *ownership* signal: it is what lets classify step 6
// reap an already-orphaned test server whose scope is gone. It is not what
// keeps a running test alive — a live test is protected earlier, by
// classify's active-test-root check (step 2), which is fed by
// activeTestRootFromPath. Both halves must know the same roots; see
// TestClassifyDoltProcess_ProtectsLiveTestUnderFleetGotmpRoot.
func TestGotmpdirTestConfigIsOnAllowlist(t *testing.T) {
	cfg := "/var/tmp/gotmp/TestAdoptPRFormulaCompileAndRun1071459171/001/review-formula-test/.gc/runtime/packs/dolt/dolt-config.yaml"
	if !isTestConfigPath(cfg, "/home/jaword", "/tmp") {
		t.Fatalf("isTestConfigPath(%q) = false, want true", cfg)
	}
}

// TestClassifyDoltProcess_ProtectsLiveTestUnderFleetGotmpRoot is the paired
// guard for the allowlist above (ga-sm1cvj): widening isTestConfigPath to the
// fleet GOTMPDIR root widens what gets reaped, so activeTestRootFromPath must
// recognize the same root or a *running* test's own dolt server — cwd live,
// config live — flips from protect to reap, taking its --data-dir with it.
func TestClassifyDoltProcess_ProtectsLiveTestUnderFleetGotmpRoot(t *testing.T) {
	const testRoot = "/var/tmp/gotmp/TestAdoptPRFormulaCompileAndRun1071459171"
	cfg := testRoot + "/001/review-formula-test/.gc/runtime/packs/dolt/dolt-config.yaml"

	// The live test process itself is what advertises the root, exactly as
	// discoverActiveTestRootsFromPS would observe it in another process's argv.
	root, ok := activeTestRootFromPath(cfg, "/home/u", "/tmp")
	if !ok {
		t.Fatalf("activeTestRootFromPath(%q) = _, false; want the enclosing test root", cfg)
	}
	if root != testRoot {
		t.Fatalf("activeTestRootFromPath(%q) = %q, want %q", cfg, root, testRoot)
	}

	p := DoltProcInfo{
		PID:             6070,
		Argv:            []string{"dolt", "sql-server", "--config", cfg, "--data-dir", testRoot + "/001/review-formula-test/dolt"},
		CWDState:        procPathStateLive,
		ConfigPathState: procPathStateLive,
	}
	got := classifyDoltProcess(p, nil, "/home/u", "/tmp", []string{root})
	if got.Action != "protect" {
		t.Fatalf("classifyDoltProcess = {Action: %q, Reason: %q, DataDir: %q}, want protect", got.Action, got.Reason, got.DataDir)
	}
	if got.DataDir != "" {
		t.Errorf("protect classification carries DataDir %q; a protect must never hand the reaper a RemoveAll target", got.DataDir)
	}
}

// TestActiveTestRootFromPath_GotmpdirEnvRoot covers the $GOTMPDIR half of the
// pairing: a host whose Go temp root differs from the AGENTS.md-pinned
// /var/tmp/gotmp is still protectable, and a sibling that does not start with
// "Test" is still not mistaken for a test root.
func TestActiveTestRootFromPath_GotmpdirEnvRoot(t *testing.T) {
	gotmp := "/var/tmp/host-gotmp"
	t.Setenv("GOTMPDIR", gotmp)

	root, ok := activeTestRootFromPath(gotmp+"/TestDoltThing12345/001/x/dolt-config.yaml", "/home/u", "/tmp")
	if !ok || root != gotmp+"/TestDoltThing12345" {
		t.Errorf("activeTestRootFromPath(direct Test* child) = (%q, %v), want (%q, true)", root, ok, gotmp+"/TestDoltThing12345")
	}

	if root, ok := activeTestRootFromPath(gotmp+"/buildcache/001/x/dolt-config.yaml", "/home/u", "/tmp"); ok {
		t.Errorf("activeTestRootFromPath(non-Test sibling) = (%q, true), want ok=false", root)
	}
}

func TestContainerRuntimeFromCgroup(t *testing.T) {
	tests := []struct {
		name   string
		cgroup string
		want   string
	}{
		{
			name:   "cgroup v2 docker scope",
			cgroup: "0::/system.slice/docker-3f1a2b4c5d6e.scope\n",
			want:   "docker",
		},
		{
			name:   "cgroup v2 rootless podman scope",
			cgroup: "0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-9a8b7c6d5e4f.scope\n",
			want:   "podman",
		},
		{
			name: "cgroup v1 cgroupfs driver on a later controller line",
			cgroup: "12:pids:/\n" +
				"11:memory:/docker/3f1a2b4c5d6e\n" +
				"1:name=systemd:/docker/3f1a2b4c5d6e\n",
			want: "docker",
		},
		{
			name:   "plain host process",
			cgroup: "0::/user.slice/user-1000.slice/session-3.scope\n",
			want:   "",
		},
		{
			name:   "empty cgroup file",
			cgroup: "",
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerRuntimeFromCgroup([]byte(tt.cgroup)); got != tt.want {
				t.Errorf("containerRuntimeFromCgroup(%q) = %q, want %q", tt.cgroup, got, tt.want)
			}
		})
	}
}
