package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

const teamMCPJSON = `{"mcpServers":{"team-db":{"command":"team-db-mcp"}}}` + "\n"

// A city-level MCP server reaches every rig. A rig repository that commits
// its own .mcp.json keeps it: gc does not adopt a file that holds servers it
// does not manage, and says which agents start without gc's servers there.
func TestRunStage1MCPProjectionLeavesAFileWithTheUsersOwnServersAlone(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rigPath, ".mcp.json"))
	if err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the rig's own .mcp.json changed: %q (err=%v)", data, err)
	}
	for _, rel := range []string{".gc/mcp-adopted", ".gc/mcp-managed", ".gc/mcp-locks", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(rigPath, rel)); !os.IsNotExist(err) {
			t.Fatalf("refused adoption wrote %s in the rig (stat err = %v)", rel, err)
		}
	}
	if _, ok := mcpServersInFile(t, filepath.Join(cityPath, ".mcp.json"))["notes"]; !ok {
		t.Fatal("the city root was not projected")
	}
	want := `gc: warning: agent "frontend/coder" starts without gc's MCP server "notes": not adopting ` + filepath.Join(rigPath, ".mcp.json") +
		`: gc does not manage it yet, and adopting it would drop MCP server "team-db"; to let gc manage it, ` +
		"define the servers you still need in gc's MCP catalog (an mcp/ directory) and remove those entries from the file\n"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing %q:\n%s", want, stderr.String())
	}

	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("second runStage1MCPProjection: %v", err)
	}
	if strings.Contains(stderr.String(), "not adopting") {
		t.Fatalf("an unchanged refusal warned again:\n%s", stderr.String())
	}

	// Once the catalog defines the team server the way the file does, the
	// file holds nothing gc does not manage, and gc adopts it.
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "team-db.toml"), `
name = "team-db"
command = "team-db-mcp"
`)
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("third runStage1MCPProjection: %v", err)
	}
	servers := mcpServersInFile(t, filepath.Join(rigPath, ".mcp.json"))
	if _, ok := servers["notes"]; !ok {
		t.Fatalf("rig .mcp.json not adopted once its servers are in the catalog: %v", servers)
	}
	if _, ok := servers["team-db"]; !ok {
		t.Fatalf("adopted rig .mcp.json lost team-db: %v", servers)
	}
}

// A refusal is reported once, and again when it comes back: after the file's
// content changes, and after it cleared (gc adopted the file, then the user
// handed it back). Rewriting the same bytes, as gc's overlay staging does on
// every tick, is not a change.
func TestRunStage1MCPProjectionWarnsAgainWhenARefusalComesBack(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Agents:     []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
	}
	target := filepath.Join(cityPath, ".mcp.json")
	pass := func(label string, wantWarning bool) {
		t.Helper()
		var stderr bytes.Buffer
		if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
			t.Fatalf("%s: runStage1MCPProjection: %v", label, err)
		}
		if got := strings.Contains(stderr.String(), "not adopting "+target); got != wantWarning {
			t.Fatalf("%s: warned = %v, want %v:\n%s", label, got, wantWarning, stderr.String())
		}
	}
	pass("first pass", true)
	pass("unchanged", false)

	later := time.Now().Add(time.Hour)
	if err := os.WriteFile(target, []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, later, later); err != nil {
		t.Fatal(err)
	}
	pass("same bytes rewritten", false)

	// The team changes its own server: the same refusal, other content.
	if err := os.WriteFile(target, []byte(`{"mcpServers":{"team-db":{"command":"team-db-mcp","args":["--v2"]}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pass("file edited", true)
	pass("unchanged again", false)
	if err := os.WriteFile(target, []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	pass("edit reverted", true)

	// The catalog takes the team server over; gc adopts the file.
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "team-db.toml"), `
name = "team-db"
command = "team-db-mcp"
`)
	pass("adopted", false)
	// The user hands the file back: catalog entry gone, marker deleted, the
	// team's file restored.
	if err := os.Remove(filepath.Join(cityPath, "mcp", "team-db.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cityPath, ".gc", "mcp-managed", "claude.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	pass("handed back", true)
}

// Agents may share a stage-2 work dir when they project the same servers.
// Each is named in a refusal message of its own, and neither message evicts
// the other, so each is reported once, however many ticks resolve them.
func TestStage2MCPRefusalWarningsStayBoundedForAgentsSharingAWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := stage2RefusalTestParams(t, cityPath, &stderr)
	workDir := filepath.Join(cityPath, ".gc", "worktrees", "shared")
	writeTeamGeminiSettings(t, workDir)
	agents := []*config.Agent{
		{Name: "worker", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/shared"},
		{Name: "helper", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/shared"},
	}
	for tick := 0; tick < 4; tick++ {
		for _, agent := range agents {
			if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
				t.Fatalf("tick %d: resolveTemplate(%s): %v", tick, agent.Name, err)
			}
		}
	}
	for _, name := range []string{"worker", "helper"} {
		want := `gc: warning: agent "` + name + `" starts without gc's MCP server "notes": not adopting `
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Fatalf("agent %s warned %d times over 4 ticks, want once:\n%s", name, got, stderr.String())
		}
	}
}

// A pack overlay that ships a provider file the hook installer does not own
// (here .gemini/settings.json) is merged into the work dir's file on every
// tick, rewriting the same bytes once merged. gc's own rewrite is not a
// change: the refusal is reported once.
func TestStage2MCPRefusalWarningsIgnoreGCsOwnRewrites(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := stage2RefusalTestParams(t, cityPath, &stderr)
	overlayDir := filepath.Join(cityPath, "packs", "ui", "overlay")
	if err := os.MkdirAll(filepath.Join(overlayDir, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, ".gemini", "settings.json"), []byte(`{"ui":{"hideBanner":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	params.packOverlayDirs = []string{overlayDir}
	workDir := filepath.Join(cityPath, ".gc", "worktrees", "worker-1")
	writeTeamGeminiSettings(t, workDir)
	target := filepath.Join(workDir, ".gemini", "settings.json")
	agent := &config.Agent{Name: "worker", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/worker-1"}
	old := time.Unix(1_000_000, 0)
	for tick := 0; tick < 4; tick++ {
		// Backdate the file, so the tick's rewrite shows in its mtime.
		if err := os.Chtimes(target, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil); err != nil {
			t.Fatalf("tick %d: resolveTemplatePrepared: %v", tick, err)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Equal(old) {
			t.Fatalf("tick %d: overlay staging did not rewrite %s; the test no longer covers gc's own rewrite", tick, target)
		}
	}
	want := `gc: warning: agent "worker" starts without gc's MCP server "notes": not adopting ` + target
	if got := strings.Count(stderr.String(), want); got != 1 {
		t.Fatalf("warned %d times over 4 ticks of overlay rewrites, want once:\n%s", got, stderr.String())
	}
}

// returnsPromptlyCmd runs fn and fails the test if it has not returned within
// five seconds. A reader stuck opening fifo is released by opening fifo for
// writing, so a regression fails the test instead of hanging it.
func returnsPromptlyCmd(t *testing.T, fifo string, fn func()) {
	t.Helper()
	const limit = 5 * time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(limit):
	}
	// Release each blocked reader in turn: a non-blocking writer open lets
	// one waiting open return, and its read then sees end of file.
	release := time.NewTicker(20 * time.Millisecond)
	defer release.Stop()
	deadline := time.After(30 * time.Second)
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		select {
		case <-done:
			t.Fatalf("blocked on the named pipe %s for more than %s (released by opening it for writing)", fifo, limit)
		case <-deadline:
			t.Fatalf("still blocked on the named pipe %s", fifo)
		case <-release.C:
		}
	}
}

// noReadFS passes every call to the real filesystem but never reads the
// guarded paths: it records the attempt and fails it, so a regression fails
// the test instead of reading /dev/zero or a huge file.
type noReadFS struct {
	fsys.OSFS
	guarded map[string]bool
	mu      sync.Mutex
	reads   []string
}

func (g *noReadFS) ReadFile(name string) ([]byte, error) {
	if g.guarded[filepath.Clean(name)] {
		g.mu.Lock()
		g.reads = append(g.reads, name)
		g.mu.Unlock()
		return nil, errors.New("guarded path read: " + name)
	}
	return g.OSFS.ReadFile(name)
}

// A provider file that is a symlink is refused without reading what it
// points at: a repository can commit .mcp.json -> /dev/zero, which never
// ends, or a link to a named pipe, which blocks. Stage 1 returns at once and
// warns once.
func TestRunStage1MCPProjectionNeverReadsThroughASymlinkedTarget(t *testing.T) {
	cityPath := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	target := filepath.Join(cityPath, ".mcp.json")
	if err := os.Symlink(fifo, target); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Agents:     []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
	}
	var stderr bytes.Buffer
	for tick := 0; tick < 3; tick++ {
		var err error
		returnsPromptlyCmd(t, fifo, func() { err = runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr) })
		if err != nil {
			t.Fatalf("tick %d: runStage1MCPProjection: %v", tick, err)
		}
	}
	if got := strings.Count(stderr.String(), target+" is a symlink"); got != 1 {
		t.Fatalf("symlink refusal warned %d times over 3 ticks, want once:\n%s", got, stderr.String())
	}
}

// A symlink refusal does not depend on what the link points at. When only
// the file behind the link changes, the warning is not repeated, since gc
// does not read through the link to notice.
func TestRunStage1MCPProjectionSymlinkRefusalIgnoresTheLinkedFilesContent(t *testing.T) {
	cityPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cityPath, ".mcp.json")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Agents:     []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
	}
	var stderr bytes.Buffer
	for tick := 0; tick < 4; tick++ {
		if err := os.WriteFile(outside, []byte(strings.Repeat(" ", tick)+teamMCPJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
			t.Fatalf("tick %d: runStage1MCPProjection: %v", tick, err)
		}
	}
	if got := strings.Count(stderr.String(), target+" is a symlink"); got != 1 {
		t.Fatalf("symlink refusal warned %d times while only the linked file changed, want once:\n%s", got, stderr.String())
	}
}

// Stage 2 the same: resolveTemplate, run for every agent on every tick,
// neither reads through a work dir's symlinked .mcp.json (here to /dev/zero,
// behind a guard that fails instead of reading it) nor reads an oversized
// one, and warns once each.
func TestResolveTemplateNeverReadsThroughASymlinkOrAnOversizedTarget(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	linked := filepath.Join(cityPath, ".gc", "worktrees", "linked", ".mcp.json")
	oversized := filepath.Join(cityPath, ".gc", "worktrees", "oversized", ".mcp.json")
	for _, path := range []string{linked, oversized} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/dev/zero", linked); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	guard := &noReadFS{guarded: map[string]bool{linked: true, oversized: true, "/dev/zero": true}}
	cityCfg := &config.City{
		Workspace:  config.Workspace{Provider: "claude"},
		Providers:  map[string]config.ProviderSpec{"claude": {Command: "echo", PromptMode: "none"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
	}
	var stderr bytes.Buffer
	params := &agentBuildParams{
		city:            cityCfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cityCfg.Workspace,
		providers:       cityCfg.Providers,
		lookPath:        stubLookPath,
		fs:              guard,
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          &stderr,
		sessionProvider: "tmux",
	}
	agents := []*config.Agent{
		{Name: "linked", Scope: "city", Provider: "claude", WorkDir: ".gc/worktrees/linked"},
		{Name: "oversized", Scope: "city", Provider: "claude", WorkDir: ".gc/worktrees/oversized"},
	}
	for tick := 0; tick < 3; tick++ {
		for _, agent := range agents {
			if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
				t.Fatalf("tick %d: resolveTemplate(%s): %v", tick, agent.Name, err)
			}
		}
	}
	if len(guard.reads) != 0 {
		t.Fatalf("gc read through a symlink or an oversized file: %v", guard.reads)
	}
	for _, want := range []string{linked + " is a symlink", "67108864 bytes, more than the 4194304 gc reads"} {
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Fatalf("%q warned %d times over 3 ticks, want once:\n%s", want, got, stderr.String())
		}
	}
}

// mcpTargetStamp hashes a regular file of at most the cap; anything else it
// stamps by type or size, unread.
func TestMCPTargetStampReadsOnlyRegularFilesUpToTheCap(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(regular, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if stamp := mcpTargetStamp(fsys.OSFS{}, regular); !strings.HasPrefix(stamp, "sha256:") {
		t.Fatalf("stamp(regular) = %q", stamp)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	var stamp string
	returnsPromptlyCmd(t, fifo, func() { stamp = mcpTargetStamp(fsys.OSFS{}, fifo) })
	if !strings.HasPrefix(stamp, "type:p") {
		t.Fatalf("stamp(named pipe) = %q", stamp)
	}
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	guard := &noReadFS{guarded: map[string]bool{big: true}}
	if stamp := mcpTargetStamp(guard, big); stamp != "size:1073741824" || len(guard.reads) != 0 {
		t.Fatalf("stamp(1 GiB) = %q, reads %v; want a size stamp, unread", stamp, guard.reads)
	}
}

// stage2RefusalTestParams returns build params for a gemini city whose
// catalog has one server, writing its warnings to stderr.
func stage2RefusalTestParams(t *testing.T, cityPath string, stderr *bytes.Buffer) *agentBuildParams {
	t.Helper()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cityCfg := &config.City{
		Workspace:  config.Workspace{Provider: "gemini"},
		Providers:  map[string]config.ProviderSpec{"gemini": {Command: "echo", PromptMode: "none"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
	}
	return &agentBuildParams{
		city:            cityCfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cityCfg.Workspace,
		providers:       cityCfg.Providers,
		lookPath:        stubLookPath,
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          stderr,
		sessionProvider: "tmux",
	}
}

// writeTeamGeminiSettings writes a work dir's own .gemini/settings.json
// holding a server of the team's, which gc does not manage.
func writeTeamGeminiSettings(t *testing.T, workDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workDir, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	team := `{"theme":"dark","mcpServers":{"team-db":{"command":"team-db-mcp"}}}`
	if err := os.WriteFile(filepath.Join(workDir, ".gemini", "settings.json"), []byte(team), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gc's .gitignore entries cover the files it projects and no others: at a
// root where it leaves the Claude file alone but projects Gemini's, only
// .gemini/settings.json is ignored. The dry-run plan says the same.
func TestStage1MCPGitignoreCoversOnlyTheFilesGCProjects(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "gemini"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
			{Name: "analyst", Dir: "frontend", Provider: "gemini"},
		},
	}

	lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	plan := strings.Join(lines, "\n")
	if want := "create " + filepath.Join(rigPath, ".gitignore") + ": add /.gemini/settings.json\n"; !strings.Contains(plan+"\n", want) {
		t.Fatalf("plan missing %q:\n%s", want, plan)
	}
	if want := "create " + filepath.Join(cityPath, ".gitignore") + ": add /.mcp.json\n"; !strings.Contains(plan+"\n", want) {
		t.Fatalf("plan missing %q:\n%s", want, plan)
	}

	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	for root, want := range map[string]string{
		rigPath:  "# Gas City\n/.gemini/settings.json\n",
		cityPath: "# Gas City\n/.mcp.json\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		if err != nil || string(got) != want {
			t.Fatalf("%s/.gitignore = %q (err %v), want %q", root, got, err, want)
		}
	}
	if lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil || strings.Contains(strings.Join(lines, "\n"), ".gitignore") {
		t.Fatalf("plan after the pass = %q (err %v), want no .gitignore change", lines, err)
	}
}

// gc's entries are anchored to the root they ignore (/.mcp.json), so a
// .mcp.json deeper in the repository is not ignored. An earlier gc wrote them
// unanchored; such an entry matches the file too and counts as present, so
// an existing .gitignore does not change.
func TestStage1MCPGitignoreKeepsAnEarlierGCsEntries(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "gemini"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "analyst", Scope: "city", Provider: "gemini"},
		},
	}
	earlier := "node_modules/\n\n# Gas City\n.mcp.json\n/.gemini/settings.json\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".gitignore"), []byte(earlier), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	if plan := strings.Join(lines, "\n"); strings.Contains(plan, ".gitignore") {
		t.Fatalf("plan changes .gitignore though it has both entries:\n%s", plan)
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(cityPath, ".gitignore")); err != nil || string(got) != earlier {
		t.Fatalf(".gitignore = %q (err %v), want it unchanged: %q", got, err, earlier)
	}
}

// A target that fails fails the pass, but the files gc projected before it
// stay ignored: .gitignore is written on the way out.
func TestRunStage1MCPProjectionIgnoresWhatItProjectedWhenALaterTargetFails(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	// gc manages the rig's .mcp.json, which has since become a symlink:
	// projecting there fails, after the city root (which sorts first).
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rigPath, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".gc", "mcp-managed", "claude.json"), []byte(`{"managed_by":"gc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
		},
	}
	err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "symlinked path") {
		t.Fatalf("runStage1MCPProjection = %v, want the rig target's symlink error", err)
	}
	if _, ok := mcpServersInFile(t, filepath.Join(cityPath, ".mcp.json"))["notes"]; !ok {
		t.Fatal("the city root was not projected before the failure")
	}
	if got, err := os.ReadFile(filepath.Join(cityPath, ".gitignore")); err != nil || string(got) != "# Gas City\n/.mcp.json\n" {
		t.Fatalf("city .gitignore = %q (err %v), want the projected .mcp.json ignored", got, err)
	}
}

// A project may link one provider's MCP file to another's. With no MCP
// servers in scope, gc has nothing to write there and must not fail the
// city's start over the link; with servers in scope, it leaves the link
// alone and warns.
func TestRunStage1MCPProjectionLeavesASymlinkedProviderFileAlone(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".mcp.json"), filepath.Join(cityPath, ".cursor", "mcp.json")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Session:   config.SessionConfig{Provider: "tmux"},
		Providers: builtinProviderAliasesForTest("cursor"),
		Agents:    []config.Agent{{Name: "editor", Scope: "city", Provider: "cursor"}},
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection with no MCP servers in scope: %v", err)
	}

	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg.PackMCPDir = filepath.Join(cityPath, "mcp")
	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection with a server in scope: %v", err)
	}
	if want := filepath.Join(cityPath, ".cursor", "mcp.json") + " is a symlink"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing %q:\n%s", want, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(cityPath, ".mcp.json")); err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the linked .mcp.json changed: %q (err=%v)", data, err)
	}
}

// The stage-2 pre-start leaves a work dir's own MCP config alone the same
// way, and lets the session start.
func TestInternalProjectMCPLeavesTheWorkDirsOwnConfigAlone(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.claude]
base = "builtin:claude"
command = "echo"
prompt_mode = "none"
`, `
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"internal", "project-mcp", "--agent", "mayor", "--workdir", workdir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the session starts): stderr=%q", code, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(workdir, ".mcp.json")); err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the work dir's own .mcp.json changed: %q (err=%v)", data, err)
	}
	want := `gc internal project-mcp: warning: agent "mayor" starts without gc's MCP server "notes": not adopting ` + filepath.Join(workdir, ".mcp.json")
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("refused adoption wrote .gitignore in the work dir (stat err = %v)", err)
	}
}

// A runtime discards a successful pre-start's output, so resolveTemplate,
// which wires the stage-2 pre-start, also says when the work dir's file will
// be left alone: once per process, where the controller logs.
func TestResolveTemplateWarnsWhenAStage2TargetIsNotAdopted(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	workDir := filepath.Join(cityPath, ".gc", "worktrees", "worker-1")
	team := `{"theme":"dark","mcpServers":{"team-db":{"command":"team-db-mcp"}}}`
	if err := os.MkdirAll(filepath.Join(workDir, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".gemini", "settings.json"), []byte(team), 0o644); err != nil {
		t.Fatal(err)
	}
	cityCfg := &config.City{
		Workspace:  config.Workspace{Provider: "gemini"},
		Providers:  map[string]config.ProviderSpec{"gemini": {Command: "echo", PromptMode: "none"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
	}
	var stderr bytes.Buffer
	params := &agentBuildParams{
		city:            cityCfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cityCfg.Workspace,
		providers:       cityCfg.Providers,
		lookPath:        stubLookPath,
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          &stderr,
		sessionProvider: "tmux",
	}
	agent := &config.Agent{Name: "worker", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/worker-1"}
	for range 2 {
		if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
	}
	want := `gc: warning: agent "worker" starts without gc's MCP server "notes": not adopting ` +
		filepath.Join(workDir, ".gemini", "settings.json") + `: gc does not manage it yet, and adopting it would drop MCP server "team-db"`
	if got := strings.Count(stderr.String(), want); got != 1 {
		t.Fatalf("warning %q appeared %d times, want once:\n%s", want, got, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, ".gemini", "settings.json")); err != nil || string(data) != team {
		t.Fatalf("resolveTemplate changed the work dir's settings: %q (err %v)", data, err)
	}

	// The work dir goes away with its session; a later session checks it out
	// again, file and all. The refusal is back, so the warning is too.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
		t.Fatalf("resolveTemplate without the work dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".gemini", "settings.json"), []byte(team), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := strings.Count(stderr.String(), want); got != 2 {
		t.Fatalf("warning %q appeared %d times, want twice once the refusal came back:\n%s", want, got, stderr.String())
	}
}

func TestMCPAdoptionDoctorCheck(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "gemini"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "analyst", Scope: "city", Provider: "gemini"},
			// Stage-2 agents: one whose work dir holds a file of its own,
			// one whose work dir does not exist yet.
			{Name: "worker", Scope: "city", Provider: "claude", WorkDir: "workspaces/worker"},
			{Name: "fresh", Scope: "city", Provider: "claude", WorkDir: "workspaces/fresh"},
		},
	}
	check := newMCPAdoptionDoctorCheck(cityPath, cfg, stubLookPath)
	if result := check.Run(&doctor.CheckContext{CityPath: cityPath}); result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s), want OK with nothing to adopt", result.Status, result.Message)
	}

	// The city's .mcp.json holds a server of the user's own: gc leaves it.
	if err := os.WriteFile(filepath.Join(cityPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// So does the worker's work dir.
	workerDir := filepath.Join(cityPath, "workspaces", "worker")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// The gemini settings were adopted by an earlier gc that replaced the
	// user's server; its definition survives only in the snapshot.
	snapshotDir := filepath.Join(cityPath, ".gc", "mcp-adopted", "gemini")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "20260101T000000Z.json"), []byte(`{"mcpServers":{"wiki":{"command":"wiki-mcp"},"notes":{"command":"old"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cityPath, "workspaces", "fresh")); err != nil {
		t.Fatal(err)
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`agent "mayor" starts without gc's MCP server "notes": not adopting ` + filepath.Join(cityPath, ".mcp.json"),
		`agent "worker" starts without gc's MCP server "notes": not adopting ` + filepath.Join(workerDir, ".mcp.json"),
		filepath.Join(cityPath, ".gemini", "settings.json") + ` (gemini): gc adopted this file and replaced MCP servers it held that gc does not project ("wiki")`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, `"notes")`) {
		t.Fatalf("a server gc projects is reported as dropped:\n%s", details)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "workspaces", "fresh")); !os.IsNotExist(err) {
		t.Fatalf("the doctor check created a work dir (stat err = %v)", err)
	}
}

// gc mcp list shows the servers an agent's config would carry. When gc leaves
// that file alone, it says so, and why, in text and JSON.
func TestMcpListReportsAFileGCLeavesAlone(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[beads]
provider = "file"

[session]
provider = "tmux"

[providers.gemini]
command = "echo"
prompt_mode = "none"
`, `
provider = "gemini"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	writeCatalogFile(t, cityDir, ".gemini/settings.json", `{"mcpServers":{"team-db":{"command":"team-db-mcp"},"notes":{"command":"team-notes"}}}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list exited %d: %s", code, stderr.String())
	}
	want := "Not delivered: gc leaves " + filepath.Join(cityDir, ".gemini", "settings.json") +
		` as it is, so the agent starts without these servers: gc does not manage it yet, and adopting it would drop MCP server "team-db" and its own definition of MCP server "notes"; to let gc manage it,`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("mcp list output missing %q:\n%s", want, stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"mcp", "list", "--agent", "mayor", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list --json exited %d: %s", code, stderr.String())
	}
	contentOpValidate(t, "schemas/mcp/list/result.schema.json", stdout.String())
	var got projectedMCPJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	notAdopted := got.Projection.NotAdopted
	if notAdopted == nil || strings.Join(notAdopted.UnmanagedServers, ",") != "team-db" ||
		strings.Join(notAdopted.RedefinedServers, ",") != "notes" || notAdopted.Remedy == "" {
		t.Fatalf("projection.not_adopted = %+v", notAdopted)
	}
}

// A pool or worktree session works in a checkout of its own. When that
// checkout commits an MCP file of its own, gc mcp list --session says gc
// leaves it alone.
func TestMcpListSessionReportsAWorkDirFileGCLeavesAlone(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
command = "echo"
prompt_mode = "none"
`, `
provider = "claude"
scope = "city"
max_active_sessions = 1
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionDir := filepath.Join(cityDir, "worktrees", "mayor-2")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "mayor session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":      "mayor",
			"session_name":  "s-mayor-2",
			"agent_name":    "mayor-2",
			"provider":      "claude",
			"provider_kind": "claude",
			"work_dir":      sessionDir,
			"state":         "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"mcp", "list", "--session", bead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list --session exited %d: %s", code, stderr.String())
	}
	want := "Not delivered: gc leaves " + filepath.Join(sessionDir, ".mcp.json") +
		` as it is, so the agent starts without these servers: gc does not manage it yet, and adopting it would drop MCP server "team-db"`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("mcp list --session output missing %q:\n%s", want, stdout.String())
	}
}

// mcpServersInFile returns the mcpServers object of a Claude-shaped MCP file.
func mcpServersInFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, data)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	return servers
}
