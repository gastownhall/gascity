package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type partialListPoolProvider struct {
	*runtime.Fake
	listErr   error
	listNames []string
}

func (p *partialListPoolProvider) ListRunning(prefix string) ([]string, error) {
	names := p.listNames
	if names == nil {
		names, _ = p.Fake.ListRunning(prefix)
	}
	return names, p.listErr
}

func TestEvaluatePoolSuccess(t *testing.T) {
	pool := scaleParams{Min: 0, Max: 10, Check: "echo 5"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "5", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Fatalf("evaluatePool: %v", err)
	}
	if got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}

func TestEvaluatePoolClampToMax(t *testing.T) {
	pool := scaleParams{Min: 0, Max: 10, Check: "echo 20"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "20", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Fatalf("evaluatePool: %v", err)
	}
	if got != 10 {
		t.Errorf("got %d, want 10 (max)", got)
	}
}

func TestEvaluatePoolClampToMin(t *testing.T) {
	pool := scaleParams{Min: 2, Max: 10, Check: "echo 0"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "0", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Fatalf("evaluatePool: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (min)", got)
	}
}

func TestEvaluatePoolRunnerError(t *testing.T) {
	pool := scaleParams{Min: 2, Max: 10, Check: "fail"}
	runner := func(_, _ string, _ map[string]string) (string, error) {
		return "", fmt.Errorf("command failed")
	}

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (min on error)", got)
	}
}

func TestEvaluatePoolNonInteger(t *testing.T) {
	pool := scaleParams{Min: 1, Max: 10, Check: "echo abc"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "abc", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err == nil {
		t.Fatal("expected error for non-integer output")
	}
	if got != 1 {
		t.Errorf("got %d, want 1 (min on error)", got)
	}
}

func TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork(t *testing.T) {
	skipSlowCmdGCTest(t, "uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage")
	bdPath, err := findPreferredBinary("bd", "/home/ubuntu/.local/bin/bd")
	if err != nil {
		t.Skip("bd not installed")
	}
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	t.Setenv("PATH", filepath.Dir(bdPath)+":"+filepath.Dir(jqPath)+":"+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	runExternal(t, dir, bdPath, "init", "-p", "ct", "--skip-hooks", "-q")

	agent := &config.Agent{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(3),
	}

	got, err := evaluatePool("worker", scaleParamsFor(agent), dir, nil, shellScaleCheck)
	if err != nil {
		t.Fatalf("evaluatePool without routed work: %v", err)
	}
	if got != 0 {
		t.Fatalf("evaluatePool without routed work = %d, want 0", got)
	}

	runExternal(t, dir, bdPath, "create", "--json", "queued worker job", "-t", "task",
		"--metadata", `{"gc.routed_to":"worker"}`)

	got, err = evaluatePool("worker", scaleParamsFor(agent), dir, nil, shellScaleCheck)
	if err != nil {
		t.Fatalf("evaluatePool with routed work: %v", err)
	}
	if got != 1 {
		t.Fatalf("evaluatePool with routed work = %d, want 1", got)
	}
}

func TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork(t *testing.T) {
	skipSlowCmdGCTest(t, "uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage")
	bdPath, err := findPreferredBinary("bd", "/home/ubuntu/.local/bin/bd")
	if err != nil {
		t.Skip("bd not installed")
	}
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed")
	}
	t.Setenv("PATH", filepath.Dir(bdPath)+":"+filepath.Dir(jqPath)+":"+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	runExternal(t, dir, bdPath, "init", "-p", "ct", "--skip-hooks", "-q")

	raw := runExternalOutput(t, dir, bdPath, "create", "--json", "active worker job", "-t", "task",
		"--metadata", `{"gc.routed_to":"worker"}`)
	if idx := bytes.IndexByte(raw, '{'); idx >= 0 {
		raw = raw[idx:]
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("parse create output: %v\n%s", err, raw)
	}
	if created.ID == "" {
		t.Fatalf("create output missing id: %s", raw)
	}
	runExternal(t, dir, bdPath, "update", created.ID, "--status", "in_progress")

	agent := &config.Agent{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(3),
	}
	got, err := evaluatePool("worker", scaleParamsFor(agent), dir, nil, shellScaleCheck)
	if err != nil {
		t.Fatalf("evaluatePool with routed in-progress work: %v", err)
	}
	if got != 0 {
		t.Fatalf("evaluatePool with routed in-progress work = %d, want 0", got)
	}
}

func TestEvaluatePoolNewDemandDoesNotApplyMinOrMax(t *testing.T) {
	sp := scaleParams{Min: 2, Max: 3, Check: "ignored"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "5\n", nil }

	got, err := evaluatePoolNewDemand("worker", sp, "", nil, runner)
	if err != nil {
		t.Fatalf("evaluatePoolNewDemand: %v", err)
	}
	if got != 5 {
		t.Fatalf("evaluatePoolNewDemand = %d, want raw new demand 5", got)
	}
}

func TestEvaluatePoolNewDemandErrorFallsBackToZero(t *testing.T) {
	sp := scaleParams{Min: 2, Max: 3, Check: "ignored"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "not-a-number\n", nil }

	got, err := evaluatePoolNewDemand("worker", sp, "", nil, runner)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got != 0 {
		t.Fatalf("evaluatePoolNewDemand error fallback = %d, want 0", got)
	}
}

func TestFindPreferredBinary_SkipsTestscriptShim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	shimDir := filepath.Join(root, "testscript-main123", "bin")
	realDir := filepath.Join(root, "real-bin")
	for _, dir := range []string{shimDir, realDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	shimPath := filepath.Join(shimDir, "bd")
	realPath := filepath.Join(realDir, "bd")
	for _, candidate := range []string{shimPath, realPath} {
		if err := os.WriteFile(candidate, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", candidate, err)
		}
	}
	t.Setenv("PATH", strings.Join([]string{shimDir, realDir}, string(os.PathListSeparator)))

	got, err := findPreferredBinary("bd")
	if err != nil {
		t.Fatalf("findPreferredBinary: %v", err)
	}
	if got != realPath {
		t.Fatalf("findPreferredBinary = %q, want %q", got, realPath)
	}
}

func TestIsMultiSessionCfgAgent_NamepoolMaxOneIsStillPool(t *testing.T) {
	a := &config.Agent{
		Name:              "polecat",
		MaxActiveSessions: intPtr(1),
		Namepool:          "namepools/mad-max.txt",
		NamepoolNames:     []string{"furiosa"},
	}

	if !a.SupportsInstanceExpansion() {
		t.Fatal("expected namepool-backed max=1 agent to support instance expansion")
	}
}

func TestEvaluatePoolWhitespace(t *testing.T) {
	pool := scaleParams{Min: 0, Max: 10, Check: "echo 3"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return " 3\n", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Fatalf("evaluatePool: %v", err)
	}
	if got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

// Regression: empty check output must be an error, not silent success.
func TestEvaluatePoolEmptyOutput(t *testing.T) {
	pool := scaleParams{Min: 2, Max: 10, Check: "true"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (min on error)", got)
	}
}

// Regression: whitespace-only output should also be treated as empty.
func TestEvaluatePoolWhitespaceOnly(t *testing.T) {
	pool := scaleParams{Min: 1, Max: 10, Check: "echo"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "  \n", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err == nil {
		t.Fatal("expected error for whitespace-only output")
	}
	if got != 1 {
		t.Errorf("got %d, want 1 (min on error)", got)
	}
}

func TestEvaluatePoolUnlimitedNoClamp(t *testing.T) {
	pool := scaleParams{Min: 0, Max: -1, Check: "echo 100"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "100", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// With max=-1 (unlimited), the value should not be clamped.
	if got != 100 {
		t.Errorf("got %d, want 100 (no upper clamp for unlimited)", got)
	}
}

func TestEvaluatePoolUnlimitedClampsToMin(t *testing.T) {
	pool := scaleParams{Min: 2, Max: -1, Check: "echo 0"}
	runner := func(_, _ string, _ map[string]string) (string, error) { return "0", nil }

	got, err := evaluatePool("worker", pool, "", nil, runner)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (clamped to min)", got)
	}
}

func TestDiscoverPoolInstancesBounded(t *testing.T) {
	sp := runtime.NewFake()
	pool := scaleParams{Min: 0, Max: 3}
	instances := discoverPoolInstances("worker", "myrig", pool, nil, "city", "", sp)
	if len(instances) != 3 {
		t.Fatalf("len = %d, want 3", len(instances))
	}
	want := []string{"myrig/worker-1", "myrig/worker-2", "myrig/worker-3"}
	for i, got := range instances {
		if got != want[i] {
			t.Errorf("instances[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestDiscoverPoolInstancesBoundedWithNamepool(t *testing.T) {
	sp := runtime.NewFake()
	a := &config.Agent{
		Name:              "worker",
		Dir:               "myrig",
		MaxActiveSessions: intPtr(3),
		NamepoolNames:     []string{"furiosa", "nux", "slit"},
	}
	pool := scaleParams{Min: 0, Max: 3}
	instances := discoverPoolInstances("worker", "myrig", pool, a, "city", "", sp)
	if len(instances) != 3 {
		t.Fatalf("len = %d, want 3", len(instances))
	}
	want := []string{"myrig/furiosa", "myrig/nux", "myrig/slit"}
	for i, got := range instances {
		if got != want[i] {
			t.Errorf("instances[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestDiscoverPoolInstancesUnlimited(t *testing.T) {
	sp := runtime.NewFake()
	// Start some instances that look like pool members.
	_ = sp.Start(context.Background(), "myrig--worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "myrig--worker-3", runtime.Config{})
	// Start a non-matching session.
	_ = sp.Start(context.Background(), "myrig--refinery", runtime.Config{})

	pool := scaleParams{Min: 0, Max: -1}
	instances := discoverPoolInstances("worker", "myrig", pool, nil, "city", "", sp)
	if len(instances) != 2 {
		t.Fatalf("len = %d, want 2 (instances: %v)", len(instances), instances)
	}
}

func TestDiscoverPoolInstancesUnlimitedFailsClosedOnPartialResults(t *testing.T) {
	sp := &partialListPoolProvider{
		Fake:    runtime.NewFake(),
		listErr: &runtime.PartialListError{Err: errors.New("remote backend down")},
	}
	_ = sp.Start(context.Background(), "myrig--worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "myrig--worker-3", runtime.Config{})

	pool := scaleParams{Min: 0, Max: -1}
	instances := discoverPoolInstances("worker", "myrig", pool, nil, "city", "", sp)
	if len(instances) != 0 {
		t.Fatalf("len = %d, want fail-closed empty result on partial list (instances: %v)", len(instances), instances)
	}
}

func TestCountRunningPoolInstancesUnlimited(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "worker-3", runtime.Config{})

	count := countRunningPoolInstances("worker", "", scaleParams{Min: 0, Max: -1}, nil, "city", "", sp)
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestCountRunningPoolInstancesUsesPartialListResults(t *testing.T) {
	sp := &partialListPoolProvider{
		Fake:    runtime.NewFake(),
		listErr: &runtime.PartialListError{Err: errors.New("remote backend down")},
	}
	_ = sp.Start(context.Background(), "worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "worker-3", runtime.Config{})

	count := countRunningPoolInstances("worker", "", scaleParams{Min: 0, Max: 3}, nil, "city", "", sp)
	if count != 2 {
		t.Errorf("count = %d, want 2 from per-session fallback", count)
	}
}

// ---------------------------------------------------------------------------
// poolInstanceName tests
// ---------------------------------------------------------------------------

func TestPoolInstanceName_ThemedName(t *testing.T) {
	a := &config.Agent{
		Name:          "polecat",
		NamepoolNames: []string{"furiosa", "nux", "slit"},
	}
	if got := poolInstanceName("polecat", 1, a); got != "furiosa" {
		t.Errorf("slot 1: got %q, want %q", got, "furiosa")
	}
	if got := poolInstanceName("polecat", 2, a); got != "nux" {
		t.Errorf("slot 2: got %q, want %q", got, "nux")
	}
	if got := poolInstanceName("polecat", 3, a); got != "slit" {
		t.Errorf("slot 3: got %q, want %q", got, "slit")
	}
}

func TestPoolInstanceName_OverflowFallback(t *testing.T) {
	a := &config.Agent{
		Name:          "polecat",
		NamepoolNames: []string{"furiosa", "nux"},
	}
	if got := poolInstanceName("polecat", 3, a); got != "polecat-3" {
		t.Errorf("slot 3 (overflow): got %q, want %q", got, "polecat-3")
	}
}

func TestPoolInstanceName_EmptyNamepool(t *testing.T) {
	if got := poolInstanceName("polecat", 1, nil); got != "polecat-1" {
		t.Errorf("slot 1 (no namepool): got %q, want %q", got, "polecat-1")
	}
}

// ---------------------------------------------------------------------------
// poolInstanceIdentity tests
// ---------------------------------------------------------------------------

func TestPoolInstanceIdentity_NilAgent(t *testing.T) {
	instance, qualified := poolInstanceIdentity(nil, 1, io.Discard)
	if instance != "" || qualified != "" {
		t.Errorf("nil agent: got (%q, %q), want (\"\", \"\")", instance, qualified)
	}
}

func TestPoolInstanceIdentity_NonExpansionAgent_WarnsAndReturnsBase(t *testing.T) {
	a := &config.Agent{
		Name:              "refinery",
		Dir:               "rig",
		MaxActiveSessions: intPtr(1),
	}
	var stderr bytes.Buffer
	instance, qualified := poolInstanceIdentity(a, 1, &stderr)
	if instance != "refinery" {
		t.Errorf("instance = %q, want %q (non-expansion uses base name)", instance, "refinery")
	}
	if qualified != "rig/refinery" {
		t.Errorf("qualified = %q, want %q (non-expansion uses base qualified name)", qualified, "rig/refinery")
	}
	if !strings.Contains(stderr.String(), "does not support instance expansion") {
		t.Errorf("expected warning about instance expansion, got: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "max_active_sessions=1") {
		t.Errorf("expected warning to include max_active_sessions, got: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"refinery"-1`) {
		t.Errorf("expected warning to mention phantom name, got: %q", stderr.String())
	}
}

func TestPoolInstanceIdentity_NonExpansionAgent_NilStderrNoPanic(t *testing.T) {
	a := &config.Agent{
		Name:              "refinery",
		MaxActiveSessions: intPtr(1),
	}
	instance, qualified := poolInstanceIdentity(a, 1, nil)
	if instance != "refinery" {
		t.Errorf("instance = %q, want %q", instance, "refinery")
	}
	if qualified != "refinery" {
		t.Errorf("qualified = %q, want %q", qualified, "refinery")
	}
}

func TestPoolInstanceIdentity_NonExpansionAgent_ZeroSlotSuppressesWarning(t *testing.T) {
	a := &config.Agent{
		Name:              "refinery",
		MaxActiveSessions: intPtr(1),
	}
	var stderr bytes.Buffer
	instance, qualified := poolInstanceIdentity(a, 0, &stderr)
	if instance != "refinery" {
		t.Errorf("instance = %q, want %q", instance, "refinery")
	}
	if qualified != "refinery" {
		t.Errorf("qualified = %q, want %q", qualified, "refinery")
	}
	if stderr.String() != "" {
		t.Errorf("slot=0 should suppress warning, got: %q", stderr.String())
	}
}

func TestPoolInstanceIdentity_ExpansionAgent_ReturnsSlotName(t *testing.T) {
	a := &config.Agent{
		Name:              "claude",
		Dir:               "rig",
		MaxActiveSessions: intPtr(3),
	}
	var stderr bytes.Buffer
	instance, qualified := poolInstanceIdentity(a, 2, &stderr)
	if instance != "claude-2" {
		t.Errorf("instance = %q, want %q", instance, "claude-2")
	}
	if qualified != "rig/claude-2" {
		t.Errorf("qualified = %q, want %q", qualified, "rig/claude-2")
	}
	if stderr.String() != "" {
		t.Errorf("expansion agent should not warn, got: %q", stderr.String())
	}
}

func TestPoolInstanceIdentity_ExpansionAgent_NamepoolThemedName(t *testing.T) {
	a := &config.Agent{
		Name:          "polecat",
		Dir:           "rig",
		NamepoolNames: []string{"furiosa", "nux"},
	}
	instance, qualified := poolInstanceIdentity(a, 1, io.Discard)
	if instance != "furiosa" {
		t.Errorf("instance = %q, want %q (namepool slot 1)", instance, "furiosa")
	}
	if qualified != "rig/furiosa" {
		t.Errorf("qualified = %q, want %q", qualified, "rig/furiosa")
	}
}

// ---------------------------------------------------------------------------
// formatMaxSessions tests
// ---------------------------------------------------------------------------

func TestFormatMaxSessions_NilAgent(t *testing.T) {
	if got := formatMaxSessions(nil); got != "<nil>" {
		t.Errorf("nil agent: got %q, want %q", got, "<nil>")
	}
}

func TestFormatMaxSessions_UnlimitedWhenNil(t *testing.T) {
	a := &config.Agent{Name: "claude"}
	if got := formatMaxSessions(a); got != "unlimited" {
		t.Errorf("nil max: got %q, want %q", got, "unlimited")
	}
}

func TestFormatMaxSessions_ReturnsInt(t *testing.T) {
	a := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(5)}
	if got := formatMaxSessions(a); got != "5" {
		t.Errorf("max=5: got %q, want %q", got, "5")
	}
}

func TestFormatMaxSessions_ZeroValue(t *testing.T) {
	a := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(0)}
	if got := formatMaxSessions(a); got != "0" {
		t.Errorf("max=0: got %q, want %q", got, "0")
	}
}

// ---------------------------------------------------------------------------
// Session setup template expansion tests
// ---------------------------------------------------------------------------

func TestExpandSessionSetup_Basic(t *testing.T) {
	ctx := SessionSetupContext{
		Session:  "mayor",
		Agent:    "mayor",
		Rig:      "",
		CityRoot: "/home/user/city",
		CityName: "bright-lights",
		WorkDir:  "/home/user/city",
	}
	cmds := []string{
		"tmux set-option -t {{.Session}} status-style 'bg=blue'",
		"tmux set-option -t {{.Session}} status-left ' {{.Agent}} '",
	}
	got := expandSessionSetup(cmds, ctx)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != "tmux set-option -t mayor status-style 'bg=blue'" {
		t.Errorf("cmd[0] = %q", got[0])
	}
	if got[1] != "tmux set-option -t mayor status-left ' mayor '" {
		t.Errorf("cmd[1] = %q", got[1])
	}
}

func TestExpandSessionSetup_AllVariables(t *testing.T) {
	ctx := SessionSetupContext{
		Session:   "hw--polecat",
		Agent:     "hw/polecat",
		AgentBase: "polecat",
		Rig:       "hello-world",
		RigRoot:   "/repos/hello-world",
		CityRoot:  "/city",
		CityName:  "bl",
		WorkDir:   "/city/.gc/worktrees/polecat",
	}
	cmds := []string{
		"echo {{.Session}} {{.Agent}} {{.AgentBase}} {{.Rig}} {{.RigRoot}} {{.CityRoot}} {{.CityName}} {{.WorkDir}}",
	}
	got := expandSessionSetup(cmds, ctx)
	want := "echo hw--polecat hw/polecat polecat hello-world /repos/hello-world /city bl /city/.gc/worktrees/polecat"
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

func TestExpandSessionSetup_InvalidTemplate(t *testing.T) {
	ctx := SessionSetupContext{Session: "test"}
	cmds := []string{
		"tmux {{.Session}}",    // valid
		"tmux {{.BadSyntax",    // invalid template
		"tmux {{.Session}} ok", // valid
	}
	got := expandSessionSetup(cmds, ctx)
	if got[0] != "tmux test" {
		t.Errorf("cmd[0] = %q, want expanded", got[0])
	}
	// Invalid template → raw command preserved.
	if got[1] != "tmux {{.BadSyntax" {
		t.Errorf("cmd[1] = %q, want raw (fallback)", got[1])
	}
	if got[2] != "tmux test ok" {
		t.Errorf("cmd[2] = %q, want expanded", got[2])
	}
}

func TestExpandSessionSetup_Nil(t *testing.T) {
	got := expandSessionSetup(nil, SessionSetupContext{})
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestExpandSessionSetup_Empty(t *testing.T) {
	got := expandSessionSetup([]string{}, SessionSetupContext{})
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestResolveSetupScript_Relative(t *testing.T) {
	got := resolveSetupScript("scripts/setup.sh", "/home/user/city/packs/gastown", "/home/user/city")
	if got != "/home/user/city/packs/gastown/scripts/setup.sh" {
		t.Errorf("got %q, want absolute path", got)
	}
}

func TestResolveSetupScript_DoubleSlashUsesCityRoot(t *testing.T) {
	got := resolveSetupScript("//scripts/setup.sh", "/home/user/city/packs/gastown", "/home/user/city")
	if got != "/home/user/city/scripts/setup.sh" {
		t.Errorf("got %q, want city-root path", got)
	}
}

func TestResolveSetupScript_LegacyCityRelativeStillWorks(t *testing.T) {
	got := resolveSetupScript("packs/gastown/scripts/setup.sh", "/home/user/city/packs/gastown", "/home/user/city")
	if got != "/home/user/city/packs/gastown/scripts/setup.sh" {
		t.Errorf("got %q, want legacy city-root-relative path to remain supported", got)
	}
}

func TestResolveSetupScript_LegacySharedCityRelativeFallback(t *testing.T) {
	cityPath := t.TempDir()
	sourceDir := filepath.Join(cityPath, "packs", "feature")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityScript := filepath.Join(cityPath, "packs", "shared", "scripts", "setup.sh")
	if err := os.MkdirAll(filepath.Dir(cityScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cityScript, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveSetupScript("packs/shared/scripts/setup.sh", sourceDir, cityPath)
	if got != cityScript {
		t.Errorf("got %q, want legacy shared city-root-relative path to remain supported", got)
	}
}

func TestResolveSetupScript_Absolute(t *testing.T) {
	got := resolveSetupScript("/usr/local/bin/setup.sh", "/home/user/city/packs/gastown", "/home/user/city")
	if got != "/usr/local/bin/setup.sh" {
		t.Errorf("got %q, want unchanged absolute path", got)
	}
}

func TestResolveSetupScript_Empty(t *testing.T) {
	got := resolveSetupScript("", "/home/user/city/packs/gastown", "/home/user/city")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExpandSessionSetup_ConfigDir(t *testing.T) {
	ctx := SessionSetupContext{
		Session:   "mayor",
		Agent:     "mayor",
		CityRoot:  "/home/user/city",
		CityName:  "bright-lights",
		WorkDir:   "/home/user/city",
		ConfigDir: "/home/user/city/packs/gastown",
	}
	cmds := []string{
		"{{.ConfigDir}}/assets/scripts/status-line.sh {{.Agent}}",
	}
	got := expandSessionSetup(cmds, ctx)
	want := "/home/user/city/packs/gastown/assets/scripts/status-line.sh mayor"
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

func TestCountRunningPoolInstancesUsesListRunning(t *testing.T) {
	sp := runtime.NewFake()
	// Start 3 out of 5 pool instances.
	_ = sp.Start(context.Background(), "worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "worker-3", runtime.Config{})
	_ = sp.Start(context.Background(), "worker-5", runtime.Config{})

	count := countRunningPoolInstances("worker", "", scaleParams{Min: 0, Max: 5}, nil, "city", "", sp)
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestCountRunningPoolInstancesWithDir(t *testing.T) {
	sp := runtime.NewFake()
	// Rig-scoped pool: dir/name pattern.
	_ = sp.Start(context.Background(), "myrig--worker-1", runtime.Config{})
	_ = sp.Start(context.Background(), "myrig--worker-2", runtime.Config{})

	count := countRunningPoolInstances("worker", "myrig", scaleParams{Min: 0, Max: 3}, nil, "city", "", sp)
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestCountRunningPoolInstancesNoneRunning(t *testing.T) {
	sp := runtime.NewFake()
	count := countRunningPoolInstances("worker", "", scaleParams{Min: 0, Max: 10}, nil, "city", "", sp)
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// TestDeepCopyAgentCoversAllFields verifies that deepCopyAgent copies every
// field from config.Agent. Uses reflection to detect fields added to Agent
// but not handled in the deep-copy, preventing silent data loss for pool
// instances.
func TestDeepCopyAgentCoversAllFields(t *testing.T) {
	trueVal := true
	intVal := 42
	src := config.Agent{
		Name:                         "original",
		Description:                  "test agent description",
		Dir:                          "original-dir",
		WorkDir:                      ".gc/agents/original",
		Scope:                        "city",
		Suspended:                    true,
		PreStart:                     []string{"pre-cmd"},
		PromptTemplate:               "prompts/test.md",
		Nudge:                        "nudge text",
		Session:                      "acp",
		Provider:                     "claude",
		StartCommand:                 "claude --dangerously",
		Args:                         []string{"--arg1"},
		PromptMode:                   "flag",
		PromptFlag:                   "--prompt",
		ReadyDelayMs:                 &intVal,
		ReadyPromptPrefix:            "ready>",
		ProcessNames:                 []string{"claude"},
		EmitsPermissionWarning:       &trueVal,
		Env:                          map[string]string{"K": "V"},
		MaxActiveSessions:            intPtr(5),
		MinActiveSessions:            intPtr(1),
		ScaleCheck:                   "echo 3",
		WorkQuery:                    "bd ready",
		SlingQuery:                   "bd update {}",
		IdleTimeout:                  "15m",
		MaxSessionAge:                "5h",
		MaxSessionAgeJitter:          "15m",
		SleepAfterIdle:               "30s",
		SleepAfterIdleSource:         "agent",
		InstallAgentHooks:            []string{"claude"},
		SkillsDir:                    "/skills",
		MCPDir:                       "/mcp",
		HooksInstalled:               &trueVal,
		InjectAssignedSkills:         &trueVal,
		SessionSetup:                 []string{"setup-cmd"},
		SessionSetupScript:           "scripts/setup.sh",
		SessionLive:                  []string{"live-cmd"},
		OverlayDir:                   "overlays/test",
		SourceDir:                    "/src",
		DefaultSlingFormula:          strPtr("mol-work"),
		InheritedDefaultSlingFormula: strPtr("mol-pack"),
		InjectFragments:              []string{"frag1"},
		AppendFragments:              []string{"agent-footer"},
		InheritedAppendFragments:     []string{"pack-footer"},
		Attach:                       &trueVal,
		Fallback:                     true,
		PoolName:                     "template/name",
		ResumeCommand:                "claude --resume {{.SessionKey}} --dangerously",
		DependsOn:                    []string{"other-agent"},
		WakeMode:                     "fresh",
		Implicit:                     true,
		DrainTimeout:                 "10m",
		OnBoot:                       "echo boot",
		OnDeath:                      "echo death",
		Namepool:                     "names.txt",
		NamepoolNames:                []string{"alpha", "bravo"},
		OptionDefaults:               map[string]string{"effort": "max"},
		BindingName:                  "gastown",
		PackName:                     "gastown",
	}

	// Tombstone fields (deprecated in v0.15.1, removed in v0.16) are not
	// deep-copied; they are accepted by the TOML parser but not propagated
	// through the runtime. The deep-copy contract deliberately drops them.
	//
	// The unexported `source` (ga-tpfc) and `layout` (ga-9ogb) fields
	// are also intentionally dropped: they are config-package-internal
	// provenance enums that describe the agent's discovery origin. Pool
	// instances are derived objects, not discovery sites, so leaving
	// them at the zero value is semantically correct and the deep-copy
	// in cmd/gc cannot reach across the package boundary to set an
	// unexported field anyway.
	tombstones := map[string]bool{
		"Skills":       true,
		"MCP":          true,
		"SharedSkills": true,
		"SharedMCP":    true,
		"source":       true,
		"layout":       true,
	}

	// Verify every non-tombstone Agent field is set (non-zero) in the test data.
	sv := reflect.ValueOf(src)
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		fname := st.Field(i).Name
		if tombstones[fname] {
			continue
		}
		if sv.Field(i).IsZero() {
			t.Fatalf("Agent field %q is zero in test data — add it to the test source", fname)
		}
	}

	dst := deepCopyAgent(&src, "copy-name", "copy-dir")

	// Name and Dir should be the overridden values.
	if dst.Name != "copy-name" {
		t.Errorf("Name = %q, want %q", dst.Name, "copy-name")
	}
	if dst.Dir != "copy-dir" {
		t.Errorf("Dir = %q, want %q", dst.Dir, "copy-dir")
	}

	// All other non-tombstone fields should match the source.
	dv := reflect.ValueOf(dst)
	for i := 0; i < st.NumField(); i++ {
		fname := st.Field(i).Name
		if fname == "Name" || fname == "Dir" {
			continue // Intentionally overridden.
		}
		if tombstones[fname] {
			continue
		}
		if dv.Field(i).IsZero() {
			t.Errorf("deepCopyAgent did not copy field %q", fname)
		}
	}

	// Verify deep independence: mutating src slices/maps should not affect dst.
	src.PreStart[0] = "MUTATED"
	src.Env["K"] = "MUTATED"
	src.SessionSetup[0] = "MUTATED"
	src.Args[0] = "MUTATED"
	src.ProcessNames[0] = "MUTATED"
	src.InjectFragments[0] = "MUTATED"
	src.AppendFragments[0] = "MUTATED"
	src.InheritedAppendFragments[0] = "MUTATED"
	src.InstallAgentHooks[0] = "MUTATED"
	newMin := 999
	src.MinActiveSessions = &newMin

	if dst.PreStart[0] == "MUTATED" {
		t.Error("PreStart is not a deep copy")
	}
	if dst.Env["K"] == "MUTATED" {
		t.Error("Env is not a deep copy")
	}
	if dst.SessionSetup[0] == "MUTATED" {
		t.Error("SessionSetup is not a deep copy")
	}
	if dst.Args[0] == "MUTATED" {
		t.Error("Args is not a deep copy")
	}
	if dst.ProcessNames[0] == "MUTATED" {
		t.Error("ProcessNames is not a deep copy")
	}
	if dst.InjectFragments[0] == "MUTATED" {
		t.Error("InjectFragments is not a deep copy")
	}
	if dst.AppendFragments[0] == "MUTATED" {
		t.Error("AppendFragments is not a deep copy")
	}
	if dst.InheritedAppendFragments[0] == "MUTATED" {
		t.Error("InheritedAppendFragments is not a deep copy")
	}
	if dst.InstallAgentHooks[0] == "MUTATED" {
		t.Error("InstallAgentHooks is not a deep copy")
	}
	if dst.MinActiveSessions != nil && *dst.MinActiveSessions == 999 {
		t.Error("MinActiveSessions is not a deep copy")
	}
}

func TestDeepCopyAgentSetsPoolName(t *testing.T) {
	src := &config.Agent{
		Name:              "dog",
		Dir:               "hello-world",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}
	dst := deepCopyAgent(src, "dog-1", "hello-world")
	if dst.PoolName != "hello-world/dog" {
		t.Errorf("PoolName = %q, want %q", dst.PoolName, "hello-world/dog")
	}
}

func TestRunPoolOnBoot(t *testing.T) {
	var ran []string
	runner := func(cmd, _ string, _ map[string]string) (string, error) {
		ran = append(ran, cmd)
		return "", nil
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), OnBoot: "bd update --unclaim"},
			{Name: "cat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), OnBoot: "bd update --unclaim"},
		},
	}

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, t.TempDir(), runner, &stderr)

	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want 2 (one per pool agent)", len(ran))
	}
	// Both should contain unclaim logic.
	for i, cmd := range ran {
		if !strings.Contains(cmd, "--unclaim") {
			t.Errorf("ran[%d] = %q, want --unclaim", i, cmd)
		}
	}
}

func TestRunPoolOnBootError(t *testing.T) {
	runner := func(_, _ string, _ map[string]string) (string, error) {
		return "", fmt.Errorf("bd not found")
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), OnBoot: "bd update --unclaim"},
		},
	}

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, t.TempDir(), runner, &stderr)

	// Error should be logged, not fatal.
	if !strings.Contains(stderr.String(), "on_boot dog") {
		t.Errorf("stderr = %q, want on_boot error logged", stderr.String())
	}
}

func TestRunPoolOnBootUsesRigRootForRigScopedPools(t *testing.T) {
	var dirs []string
	runner := func(_ string, dir string, _ map[string]string) (string, error) {
		dirs = append(dirs, dir)
		return "", nil
	}
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo-rig")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: rigRoot}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "demo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), OnBoot: "bd update --unclaim"},
		},
	}

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, cityPath, runner, &stderr)

	if len(dirs) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(dirs))
	}
	if dirs[0] != rigRoot {
		t.Fatalf("on_boot dir = %q, want %q", dirs[0], rigRoot)
	}
}

func TestRunPoolOnBootUsesCanonicalRigEnv(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	cfg.Agents[0].MinActiveSessions = intPtr(0)
	cfg.Agents[0].MaxActiveSessions = intPtr(2)

	var gotDir string
	var gotPort string
	var gotPassword string
	var gotBeadsDir string
	runner := func(_ string, dir string, env map[string]string) (string, error) {
		gotDir = dir
		gotPort = env["GC_DOLT_PORT"]
		gotPassword = env["GC_DOLT_PASSWORD"]
		gotBeadsDir = env["BEADS_DIR"]
		return "", nil
	}

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, cityPath, runner, &stderr)

	if gotDir != rigDir {
		t.Fatalf("on_boot dir = %q, want %q", gotDir, rigDir)
	}
	wantPort := currentManagedDoltPort(cityPath)
	if gotPort != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", gotPort, wantPort)
	}
	if gotPassword != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", gotPassword, "city-secret")
	}
	if gotBeadsDir != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", gotBeadsDir, filepath.Join(rigDir, ".beads"))
	}
}

func TestRunPoolOnBootExpandsTemplateCommands(t *testing.T) {
	var ran []string
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigRoot := filepath.Join(cityPath, "frontend")
	runner := func(cmd, _ string, _ map[string]string) (string, error) {
		ran = append(ran, cmd)
		return "", nil
	}

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "frontend", Path: rigRoot}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "frontend",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
				OnBoot:            "echo {{.CityName}} {{.Rig}} {{.AgentBase}}",
			},
		},
	}

	runPoolOnBoot(cfg, cityPath, runner, io.Discard)

	if len(ran) != 1 {
		t.Fatalf("ran %d commands, want 1", len(ran))
	}
	if ran[0] != "echo demo-city frontend worker" {
		t.Fatalf("on_boot command = %q, want %q", ran[0], "echo demo-city frontend worker")
	}
}

func TestComputePoolDeathHandlers(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // not a pool (no MinActiveSessions/ScaleCheck)
			{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), OnDeath: "echo death"},
			{Name: "cat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), OnDeath: "echo death"}, // max=1 POOL (MinActiveSessions set) — keeps cat-1 per the disambiguator
		},
	}

	handlers := computePoolDeathHandlers(cfg, "test", t.TempDir(), runtime.NewFake(), nil)

	// dog has max=3, so 3 handlers (dog-1, dog-2, dog-3).
	// cat is a max=1 POOL agent (MinActiveSessions set), so cat-1 keeps the
	// `-N` suffix per SupportsInstanceExpansion's disambiguator — 1 handler.
	// mayor is not a pool (neither MinActiveSessions nor ScaleCheck set).
	if len(handlers) != 4 {
		t.Fatalf("len(handlers) = %d, want 4", len(handlers))
	}

	// dog-1, dog-2, dog-3 handlers
	for i := 1; i <= 3; i++ {
		sn := fmt.Sprintf("dog-%d", i)
		info, ok := handlers[sn]
		if !ok {
			t.Errorf("missing handler for %s (have keys: %v)", sn, handlerKeys(handlers))
			continue
		}
		if !strings.Contains(info.Command, "echo death") {
			t.Errorf("handler[%s].Command = %q, want configured on_death command", sn, info.Command)
		}
	}

	// cat-1 handler (max=1 pool keeps the suffix)
	if info, ok := handlers["cat-1"]; !ok {
		t.Errorf("missing handler for cat-1 (have keys: %v)", handlerKeys(handlers))
	} else if !strings.Contains(info.Command, "echo death") {
		t.Errorf("handler[cat-1].Command = %q, want configured on_death command", info.Command)
	}
}

func TestComputePoolDeathHandlersUsesRigRootForRigScopedPools(t *testing.T) {
	rigRoot := filepath.Join(t.TempDir(), "demo-rig")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "demo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), OnDeath: "echo death"},
		},
	}

	handlers := computePoolDeathHandlers(cfg, "test", t.TempDir(), runtime.NewFake(), nil)
	if len(handlers) != 2 {
		t.Fatalf("len(handlers) = %d, want 2", len(handlers))
	}
	for sessionName, info := range handlers {
		if info.Dir != rigRoot {
			t.Fatalf("handler[%s].Dir = %q, want %q", sessionName, info.Dir, rigRoot)
		}
	}
}

func TestComputePoolDeathHandlersExpandsTemplateCommands(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigRoot := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "frontend", Path: rigRoot}},
		Agents: []config.Agent{
			{
				Name:              "worker",
				Dir:               "frontend",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
				OnDeath:           "echo {{.CityName}} {{.Rig}} {{.AgentBase}}",
			},
		},
	}

	handlers := computePoolDeathHandlers(cfg, "demo-city", cityPath, runtime.NewFake(), nil)
	if len(handlers) != 2 {
		t.Fatalf("len(handlers) = %d, want 2", len(handlers))
	}
	for sessionName, info := range handlers {
		if !strings.Contains(info.Command, "echo demo-city frontend worker-") {
			t.Fatalf("handler[%s].Command = %q, want expanded on_death template", sessionName, info.Command)
		}
	}
}

func TestComputePoolDeathHandlersLogsTemplateExpansionWarning(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{
				Name:              "worker",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
				OnDeath:           "echo {{.Rig",
			},
		},
	}

	var stderr bytes.Buffer
	handlers := computePoolDeathHandlers(cfg, "demo-city", t.TempDir(), runtime.NewFake(), &stderr)
	if len(handlers) != 2 {
		t.Fatalf("len(handlers) = %d, want 2", len(handlers))
	}
	for sessionName, info := range handlers {
		if info.Command != "echo {{.Rig" {
			t.Fatalf("handler[%s].Command = %q, want raw command fallback", sessionName, info.Command)
		}
	}
	if !strings.Contains(stderr.String(), "on_death") {
		t.Fatalf("stderr missing field name: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "echo {{.Rig") {
		t.Fatalf("stderr should redact raw template, got %q", stderr.String())
	}
}

func TestComputePoolDeathHandlersUsesCanonicalRigEnv(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})
	writeScopePassword(t, rigDir, "rig-secret")
	cfg.Workspace.Name = "test"
	cfg.Agents[0].Name = "polecat"
	cfg.Agents[0].MinActiveSessions = intPtr(0)
	cfg.Agents[0].MaxActiveSessions = intPtr(2)

	handlers := computePoolDeathHandlers(cfg, "test", cityPath, runtime.NewFake(), nil)
	if len(handlers) != 2 {
		t.Fatalf("len(handlers) = %d, want 2", len(handlers))
	}
	for sessionName, info := range handlers {
		if info.Env["GC_DOLT_PORT"] != "3308" {
			t.Fatalf("handler[%s].Env[GC_DOLT_PORT] = %q, want %q", sessionName, info.Env["GC_DOLT_PORT"], "3308")
		}
		if info.Env["GC_DOLT_USER"] != "rig-user" {
			t.Fatalf("handler[%s].Env[GC_DOLT_USER] = %q, want %q", sessionName, info.Env["GC_DOLT_USER"], "rig-user")
		}
		if info.Env["GC_DOLT_PASSWORD"] != "rig-secret" {
			t.Fatalf("handler[%s].Env[GC_DOLT_PASSWORD] = %q, want %q", sessionName, info.Env["GC_DOLT_PASSWORD"], "rig-secret")
		}
	}
}

func handlerKeys(m map[string]poolDeathInfo) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// BUG: PR #207 — shellScaleCheck runs `sh -c <command>` without injecting
// BEADS_DOLT_SERVER_PORT (or any rig-scoped environment variables) into the
// subprocess environment. For rig-scoped agents whose scale_check commands
// query bd (beads via Dolt), the subprocess cannot connect to the managed
// Dolt instance because the port is not propagated.
//
// This test demonstrates that shellScaleCheck does not set any environment
// variables — it relies entirely on the parent process environment. A
// rig-scoped agent's scale_check needs BEADS_DOLT_SERVER_PORT injected so bd can
// find the managed Dolt server, but shellScaleCheck has no mechanism for this.
func TestShellScaleCheck_NoBEADS_DOLT_SERVER_PORT_Injection(t *testing.T) {
	// shellScaleCheck runs the command via `sh -c`. Verify that the command
	// environment does NOT contain BEADS_DOLT_SERVER_PORT by having the command
	// print the variable.
	//
	// Clear any inherited value first so we can detect injection (or lack thereof).
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")

	out, err := shellScaleCheck("echo ${BEADS_DOLT_SERVER_PORT:-unset}", "", nil)
	if err != nil {
		t.Fatalf("shellScaleCheck: %v", err)
	}
	trimmed := strings.TrimSpace(out)

	// The output should be "unset" because shellScaleCheck does not inject
	// BEADS_DOLT_SERVER_PORT into the subprocess environment.
	if trimmed != "unset" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q in subprocess, want %q (should not be set)", trimmed, "unset")
	}

	// Note: BEADS_DOLT_SERVER_PORT injection happens at the evaluatePendingPools
	// level (PR #207), not in shellScaleCheck itself. See
	// TestBuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent
	// for the integration test.
}

func runExternal(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	runExternalOutput(t, dir, name, args...)
}

func runExternalOutput(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if filepath.Base(name) == "bd" {
		cmd.Env = append(cmd.Env, "BEADS_DIR="+filepath.Join(dir, ".beads"))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func findPreferredBinary(name string, preferred ...string) (string, error) {
	seen := make(map[string]struct{})
	var candidates []string
	for _, candidate := range preferred {
		if candidate == "" {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		candidates = append(candidates,
			filepath.Join(homeDir, ".local", "bin", name),
			filepath.Join(homeDir, "bin", name),
		)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(dir, name))
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || isTestscriptShim(candidate) {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", exec.ErrNotFound
}

func isTestscriptShim(path string) bool {
	clean := filepath.Clean(path)
	return strings.Contains(clean, string(filepath.Separator)+"testscript-")
}
