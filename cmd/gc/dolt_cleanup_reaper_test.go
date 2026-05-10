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

func TestIsTestConfigPath_TmpTestPrefix(t *testing.T) {
	if !isTestConfigPath("/tmp/TestOrchestrator123/config.yaml", "/home/u", "") {
		t.Error("expected /tmp/Test* to be a test path")
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
