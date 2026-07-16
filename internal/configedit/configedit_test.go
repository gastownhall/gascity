package configedit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type failRenameFS struct {
	fsys.OSFS
	target string
	failed bool
}

func (f *failRenameFS) Rename(oldpath, newpath string) error {
	if !f.failed && pathutil.SamePath(newpath, f.target) {
		f.failed = true
		return errors.New("injected rename failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

type failRemoveFakeFS struct {
	*fsys.Fake
	target string
}

func (f *failRemoveFakeFS) Remove(name string) error {
	if filepath.Clean(name) == filepath.Clean(f.target) {
		return errors.New("permission denied")
	}
	return f.Fake.Remove(name)
}

// minimalCity returns a minimal valid city.toml with one agent.
func minimalCity() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
`
}

// cityWithRig returns a city.toml with one agent and one rig.
func cityWithRig() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
}

func withTestProviderCatalog(content string) string {
	additions := []struct {
		name string
		body string
	}{
		{name: "claude", body: `base = "builtin:claude"`},
		{name: "codex", body: `base = "builtin:codex"`},
		{name: "gemini", body: `base = "builtin:gemini"`},
		{name: "legacy", body: `command = "legacy"`},
	}
	for _, addition := range additions {
		if strings.Contains(content, "[providers."+addition.name+"]") {
			continue
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "\n[providers." + addition.name + "]\n" + addition.body + "\n"
	}
	return content
}

func writeTOML(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(withTestProviderCatalog(content)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	return cfg
}

func readEffectiveTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg := readTOML(t, path)
	if _, err := config.ApplySiteBindings(fsys.OSFS{}, filepath.Dir(path), cfg); err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	return cfg
}

// readExpandedTOML loads the city config with full pack expansion via
// LoadWithIncludes. Use this when a test needs to observe the merged
// state of pack-discovered or convention-discovered agents (e.g. that
// suspended state set in agents/<name>/agent.toml propagates back into
// the expanded config). Tests that only need the raw city.toml should
// use readTOML; tests verifying site-binding rig paths should use
// readEffectiveTOML.
func readExpandedTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reloading expanded config: %v", err)
	}
	return cfg
}

func readSiteBinding(t *testing.T, dir string) *config.SiteBinding {
	t.Helper()
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	return binding
}

func TestEdit_SetsAgentSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetAgentSuspended(cfg, "mayor", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			if !a.Suspended {
				t.Error("expected mayor to be suspended")
			}
			return
		}
	}
	t.Error("mayor not found after edit")
}

// TestDo_SerializesConcurrentCalls proves Editor.Do runs its callbacks under
// the same mutex as Edit, so a config-write surface that runs outside the
// load→mutate→write shape (pack import add/remove) never overlaps another
// mutation of the same city. If Do did not lock, the concurrent callbacks would
// observe more than one in-flight at once.
func TestDo_SerializesConcurrentCalls(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	var inFlight, overlaps, ran int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ed.Do(func() error {
				if atomic.AddInt32(&inFlight, 1) != 1 {
					atomic.StoreInt32(&overlaps, 1)
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&ran, 1)
				atomic.AddInt32(&inFlight, -1)
				return nil
			})
		}()
	}
	wg.Wait()

	if overlaps != 0 {
		t.Fatal("Editor.Do allowed concurrent callbacks to overlap; the lock did not serialize")
	}
	if ran != 32 {
		t.Fatalf("ran = %d, want 32", ran)
	}
}

// TestDo_PropagatesResult confirms Do surfaces the callback's error unchanged.
func TestDo_PropagatesResult(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	sentinel := errors.New("boom")
	if err := ed.Do(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Do error = %v, want %v", err, sentinel)
	}
	if err := ed.Do(func() error { return nil }); err != nil {
		t.Fatalf("Do(nil) = %v, want nil", err)
	}
}

func TestEdit_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		// Add an agent with an invalid name to trigger validation failure.
		cfg.Agents = append(cfg.Agents, config.Agent{Name: ""})
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

func TestEdit_ValidatesRigsAgainstEffectiveHQPrefix(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
provider = "claude"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "big-lane"
path = "/tmp/my-rig"
`)
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, dir, "bright-lights", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(_ *config.City) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), `rig "big-lane": prefix "bl" collides with HQ`) {
		t.Fatalf("Edit error = %v, want HQ prefix collision", err)
	}
}

func TestEditExpanded_ValidatesRigsAgainstEffectiveHQPrefix(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
provider = "claude"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "big-lane"
path = "/tmp/my-rig"
`)
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, dir, "bright-lights", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.EditExpanded(func(_, _ *config.City) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), `rig "big-lane": prefix "bl" collides with HQ`) {
		t.Fatalf("EditExpanded error = %v, want HQ prefix collision", err)
	}
}

func TestSetAgentSuspended_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetAgentSuspended(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSetRigSuspendedOnStart(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetRigSuspendedOnStart(cfg, "my-rig", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			if !r.SuspendedOnStart {
				t.Error("expected my-rig to have suspended_on_start = true")
			}
			if r.Suspended {
				t.Error("legacy suspended field must not be set by SetRigSuspendedOnStart")
			}
			return
		}
	}
	t.Error("my-rig not found after edit")
}

func TestSetRigSuspendedOnStart_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetRigSuspendedOnStart(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}

func TestAgentOrigin_Inline(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "mayor")
	if origin != configedit.OriginInline {
		t.Errorf("got %v, want OriginInline", origin)
	}
}

// TestLoadRaw_MatchesGateBasis verifies Editor.LoadRaw returns the same raw
// (pre-expansion, site-bound) config the mutation gate uses. The read path's
// provenance must be computed from this exact basis so pack_derived agrees
// with the ErrPackDerived/409 gate (Editor.UpdateAgent → AgentOrigin).
func TestLoadRaw_MatchesGateBasis(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	raw, err := ed.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if raw == nil {
		t.Fatal("LoadRaw returned nil config")
	}
	// minimalCity declares "mayor" inline. AgentOrigin computed against the
	// LoadRaw basis must agree it is inline (not pack-derived), which is the
	// exact decision the 409 gate makes.
	if got := configedit.AgentOrigin(raw, raw, "mayor"); got != configedit.OriginInline {
		t.Errorf("AgentOrigin(LoadRaw) = %v, want OriginInline", got)
	}
	if len(raw.Agents) != 1 || raw.Agents[0].Name != "mayor" {
		t.Errorf("LoadRaw agents = %+v, want single inline mayor", raw.Agents)
	}
}

func TestAgentOrigin_Derived(t *testing.T) {
	raw := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	expanded := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "polecat", Dir: "my-rig"},
		},
	}
	origin := configedit.AgentOrigin(raw, expanded, "my-rig/polecat")
	if origin != configedit.OriginDerived {
		t.Errorf("got %v, want OriginDerived", origin)
	}
}

func TestAgentOrigin_NotFound(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "nonexistent")
	if origin != configedit.OriginNotFound {
		t.Errorf("got %v, want OriginNotFound", origin)
	}
}

func TestRigOrigin(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "my-rig"}},
	}
	if configedit.RigOrigin(cfg, "my-rig") != configedit.OriginInline {
		t.Error("expected OriginInline for existing rig")
	}
	if configedit.RigOrigin(cfg, "nope") != configedit.OriginNotFound {
		t.Error("expected OriginNotFound for missing rig")
	}
}

func TestAddOrUpdateAgentPatch_New(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		suspended := true
		p.Suspended = &suspended
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(cfg.Patches.Agents))
	}
	p := cfg.Patches.Agents[0]
	if p.Dir != "my-rig" || p.Name != "polecat" {
		t.Errorf("patch target = %s/%s, want my-rig/polecat", p.Dir, p.Name)
	}
	if p.Suspended == nil || !*p.Suspended {
		t.Error("expected suspended=true in patch")
	}
}

func TestAddOrUpdateAgentPatch_Existing(t *testing.T) {
	suspended := false
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Dir: "my-rig", Name: "polecat", Suspended: &suspended},
			},
		},
	}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch (updated), got %d", len(cfg.Patches.Agents))
	}
	if cfg.Patches.Agents[0].Suspended == nil || !*cfg.Patches.Agents[0].Suspended {
		t.Error("expected suspended=true after update")
	}
}

func TestAddOrUpdateRigPatch(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateRigPatch(cfg, "my-rig", func(p *config.RigPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Rigs) != 1 {
		t.Fatalf("expected 1 rig patch, got %d", len(cfg.Patches.Rigs))
	}
	if cfg.Patches.Rigs[0].Name != "my-rig" {
		t.Errorf("patch target = %s, want my-rig", cfg.Patches.Rigs[0].Name)
	}
}

func TestEdit_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Successful edit should leave no temp files.
	err := ed.Edit(func(cfg *config.City) error {
		cfg.Agents[0].Suspended = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "city.toml" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSuspendAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("mayor"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Agents[0].Suspended {
		t.Error("expected mayor to be suspended")
	}
}

func TestResumeAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeAgent("mayor"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Suspended {
		t.Error("expected mayor to not be suspended")
	}
}

func TestSuspendAgent_LocalDiscovered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should not gain agent patch:\n%s", raw)
	}
	agentToml := string(mustReadFile(t, filepath.Join(agentDir, "agent.toml")))
	if !strings.Contains(agentToml, "suspended = true") {
		t.Fatalf("agent.toml = %q, want suspended = true", agentToml)
	}

	cfg := readExpandedTOML(t, path)
	if !findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should be suspended in expanded config")
	}
}

func TestResumeAgent_LocalDiscovered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("provider = \"codex\"\nsuspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should not gain agent patch:\n%s", raw)
	}
	agentToml := string(mustReadFile(t, filepath.Join(agentDir, "agent.toml")))
	if !strings.Contains(agentToml, "provider = \"codex\"") {
		t.Fatalf("agent.toml = %q, want provider preserved", agentToml)
	}
	if strings.Contains(agentToml, "suspended") {
		t.Fatalf("agent.toml = %q, want suspended cleared", agentToml)
	}

	cfg := readExpandedTOML(t, path)
	worker := findAgent(t, cfg, "worker")
	if worker.Suspended {
		t.Fatal("worker should not be suspended in expanded config")
	}
	if worker.Provider != "codex" {
		t.Fatalf("worker.Provider = %q, want codex", worker.Provider)
	}
}

// setupSymlinkedConventionAgent builds a schema-2 city with a convention
// "worker" whose agents/worker/agent.toml is a symlink into a separate
// checked-in location. It returns the city.toml path, the agent.toml link
// path, and the resolved checked-in target path.
func setupSymlinkedConventionAgent(t *testing.T, agentTomlBody string) (cityTOML, link, target string) {
	t.Helper()
	dir := t.TempDir()
	cityTOML = writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkedIn := filepath.Join(dir, "checked-in")
	if err := os.MkdirAll(checkedIn, 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(checkedIn, "worker.agent.toml")
	if err := os.WriteFile(target, []byte(agentTomlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(agentDir, "agent.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return cityTOML, link, target
}

func assertStillSymlink(t *testing.T, link string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q symlink was replaced by a regular file", link)
	}
}

func TestSuspendAgent_LocalDiscovered_SymlinkedAgentTomlWritesThroughLink(t *testing.T) {
	cityTOML, link, target := setupSymlinkedConventionAgent(t, "provider = \"codex\"\n")

	ed := configedit.NewEditor(fsys.OSFS{}, cityTOML)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	assertStillSymlink(t, link)
	got := string(mustReadFile(t, target))
	if !strings.Contains(got, "suspended = true") {
		t.Fatalf("checked-in target = %q, want suspended = true written through the link", got)
	}
	if !strings.Contains(got, `provider = "codex"`) {
		t.Fatalf("checked-in target = %q, want provider preserved", got)
	}
}

func TestResumeAgent_LocalDiscovered_SymlinkedAgentTomlEmptyClearsTarget(t *testing.T) {
	// Only the suspended flag is durable, so resume empties the config and the
	// resolved checked-in target is cleared. The operator's link is left in
	// place (edits act on the target, not the link).
	cityTOML, link, target := setupSymlinkedConventionAgent(t, "suspended = true\n")

	ed := configedit.NewEditor(fsys.OSFS{}, cityTOML)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("checked-in target should be cleared on empty resume, stat err = %v", err)
	}
	assertStillSymlink(t, link)
	cfg := readExpandedTOML(t, cityTOML)
	if findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should not be suspended after resume")
	}
}

func TestUpdateAgent_LocalDiscovered_SymlinkedAgentTomlWritesThroughLink(t *testing.T) {
	cityTOML, link, target := setupSymlinkedConventionAgent(t, "provider = \"codex\"\n")

	ed := configedit.NewEditor(fsys.OSFS{}, cityTOML)
	if err := ed.UpdateAgent("worker", configedit.AgentUpdate{Provider: "claude"}); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	assertStillSymlink(t, link)
	got := string(mustReadFile(t, target))
	if !strings.Contains(got, `provider = "claude"`) {
		t.Fatalf("checked-in target = %q, want provider updated through the link", got)
	}
}

func TestWriteLocalDiscoveredAgentConfig_SymlinkedAgentTomlWritesThroughLink(t *testing.T) {
	cityTOML, link, target := setupSymlinkedConventionAgent(t, "provider = \"codex\"\n")
	cityRoot := filepath.Dir(cityTOML)

	agent := config.Agent{
		Name:     "worker",
		Provider: "claude",
		Scope:    "city",
	}
	if err := configedit.WriteLocalDiscoveredAgentConfig(fsys.OSFS{}, cityRoot, agent); err != nil {
		t.Fatalf("WriteLocalDiscoveredAgentConfig: %v", err)
	}

	assertStillSymlink(t, link)
	got := string(mustReadFile(t, target))
	if !strings.Contains(got, `provider = "claude"`) {
		t.Fatalf("checked-in target = %q, want provider written through the link", got)
	}
}

// TestSuspendAgent_PackDeclaredAgentUsesPatch ensures that an [[agent]]
// explicitly declared in the city's pack.toml is suspended via
// [[patches.agent]] in city.toml — not via agents/<name>/agent.toml,
// which would be silently shadowed by the pack.toml declaration during
// composition. Regression for the SourceDir == cityRoot heuristic that
// also matched pack-declared agents.
func TestSuspendAgent_PackDeclaredAgentUsesPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "worker"
provider = "claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A conventional prompt template at the discovery location must NOT
	// trigger the agent.toml write path when an [[agent]] entry exists.
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(agentDir, "agent.toml")); err == nil {
		t.Fatalf("agent.toml must not be created for pack-declared agent")
	}

	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should gain agent patch:\n%s", raw)
	}

	cfg := readExpandedTOML(t, path)
	if !findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should be suspended in expanded config")
	}
}

// TestResumeAgent_StripsLegacyPatchSuspended covers the migration case
// where a city.toml has a stale [[patches.agent]] suspended override
// from older code. Resuming a convention-discovered agent must strip
// that patch override so it doesn't continue to shadow agent.toml.
func TestResumeAgent_StripsLegacyPatchSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = true
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("suspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("legacy patch should be stripped:\n%s", raw)
	}

	cfg := readExpandedTOML(t, path)
	if findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should not be suspended in expanded config after resume")
	}
}

// TestSuspendAgent_StripsLegacyPatchSuspendedKeepsOtherFields ensures
// that an existing patch with overrides beyond Suspended keeps the
// non-Suspended fields intact when the Suspended override is stripped.
func TestSuspendAgent_StripsLegacyPatchSuspendedKeepsOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = false
provider = "codex"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, `provider = "codex"`) {
		t.Fatalf("non-Suspended patch fields should be preserved:\n%s", raw)
	}
	if strings.Contains(raw, "suspended =") {
		t.Fatalf("Suspended override should be removed from patch:\n%s", raw)
	}
}

// TestStripAgentPatchSuspended_OnlyMatchingIdentity unit-tests the
// patch-stripping helper directly. Iteration-2 fix: callers must thread
// the resolved (Dir, Name) qualified identity so a same-bare-name patch
// targeting a different rig is never accidentally cleared.
func TestStripAgentPatchSuspended_OnlyMatchingIdentity(t *testing.T) {
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Dir: "rigA", Name: "worker", Suspended: boolPtrTest(true)},
				{Dir: "rigB", Name: "worker", Suspended: boolPtrTest(true)},
				{Dir: "", Name: "worker", Suspended: boolPtrTest(true)},
			},
		},
	}
	// Strip city-scoped (dir="") only.
	if !configedit.StripAgentPatchSuspended(cfg, "worker") {
		t.Fatal("StripAgentPatchSuspended should report a change")
	}
	if got := len(cfg.Patches.Agents); got != 2 {
		t.Fatalf("Patches.Agents len = %d, want 2; got %#v", got, cfg.Patches.Agents)
	}
	for _, p := range cfg.Patches.Agents {
		if p.Dir == "" {
			t.Errorf("city-scoped patch should be removed; remaining: %#v", p)
		}
	}

	// Strip rigA-scoped via qualified identity.
	if !configedit.StripAgentPatchSuspended(cfg, "rigA/worker") {
		t.Fatal("StripAgentPatchSuspended should report a change for rigA")
	}
	if got := len(cfg.Patches.Agents); got != 1 || cfg.Patches.Agents[0].Dir != "rigB" {
		t.Fatalf("after stripping rigA, expected only rigB patch, got %#v", cfg.Patches.Agents)
	}

	// Stripping a non-matching identity is a no-op.
	if configedit.StripAgentPatchSuspended(cfg, "rigC/worker") {
		t.Fatal("StripAgentPatchSuspended should be a no-op for non-matching identity")
	}
}

func boolPtrTest(b bool) *bool { return &b }

// TestLocalDiscoveredAgent_RejectsRigScopedAgentWithCityPromptPath
// guards against the iteration-3 Major finding (Gemini): a rig-scoped
// agent whose prompt_template happens to point at the city's
// <cityRoot>/agents/<name>/ template must NOT be classified as local
// discovered. Writing agent.toml for it would corrupt the city agent's
// durable state instead of producing the correct [[patches.agent]].
func TestLocalDiscoveredAgent_RejectsRigScopedAgentWithCityPromptPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigAgent := config.Agent{
		Dir:            "myrig",
		Name:           "worker",
		PromptTemplate: filepath.Join(dir, "agents", "worker", "prompt.template.md"),
	}
	local, err := configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, rigAgent)
	if err != nil {
		t.Fatalf("LocalDiscoveredAgent rig-scoped: %v", err)
	}
	if local {
		t.Fatal("rig-scoped agent must not be classified as local-discovered even when prompt_template points at the city's agents/<name>/ tree")
	}

	cityAgent := config.Agent{
		Dir:            "",
		Name:           "worker",
		PromptTemplate: filepath.Join(dir, "agents", "worker", "prompt.template.md"),
	}
	local, err = configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, cityAgent)
	if err != nil {
		t.Fatalf("LocalDiscoveredAgent city-scoped: %v", err)
	}
	if !local {
		t.Fatal("city-scoped scaffolded agent should be classified as local-discovered")
	}
}

func TestLocalDiscoveredAgent_AgentTOMLDefinesCityScopedConventionContract(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityAgent := config.Agent{
		Name:           "worker",
		PromptTemplate: filepath.Join(dir, "custom", "worker.md"),
	}
	local, err := configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, cityAgent)
	if err != nil {
		t.Fatalf("LocalDiscoveredAgent without agent.toml: %v", err)
	}
	if local {
		t.Fatal("custom prompt path without agent.toml should not be classified as local-discovered")
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("provider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err = configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, cityAgent)
	if err != nil {
		t.Fatalf("LocalDiscoveredAgent with agent.toml: %v", err)
	}
	if !local {
		t.Fatal("agents/<name>/agent.toml should classify a city-scoped agent as local-discovered even with a custom prompt path")
	}
}

func TestLocalDiscoveredAgent_StatErrorIsNotPositiveEvidence(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city"] = true
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/worker"] = true
	fs.Files["/city/pack.toml"] = []byte("[pack]\nname = \"test-city\"\nschema = 2\n")
	fs.Errors["/city/agents/worker/agent.toml"] = errors.New("injected stat failure")

	agent := config.Agent{
		Name:           "worker",
		PromptTemplate: "/city/custom/worker.md",
	}
	local, err := configedit.LocalDiscoveredAgent(fs, "/city", agent)
	if err != nil {
		t.Fatalf("LocalDiscoveredAgent: %v", err)
	}
	if local {
		t.Fatal("agent.toml stat errors must not classify an agent as local-discovered")
	}
}

func TestLocalDiscoveredAgent_PackTOMLReadErrorSurfacesError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city"] = true
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/worker"] = true
	fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"claude\"\n")
	fs.Errors["/city/pack.toml"] = os.ErrPermission

	agent := config.Agent{Name: "worker"}
	ok, err := configedit.LocalDiscoveredAgent(fs, "/city", agent)
	if err == nil {
		t.Fatal("LocalDiscoveredAgent error = nil, want pack.toml read error")
	}
	if ok {
		t.Fatal("LocalDiscoveredAgent classified agent as local-discovered despite pack.toml read error")
	}
	if !strings.Contains(err.Error(), "reading /city/pack.toml") {
		t.Fatalf("LocalDiscoveredAgent error = %v, want pack.toml read context", err)
	}
}

func TestLocalDiscoveredAgent_MalformedPackTOMLSurfacesError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city"] = true
	fs.Dirs["/city/agents"] = true
	fs.Dirs["/city/agents/worker"] = true
	fs.Files["/city/pack.toml"] = []byte("[[agent]\nname = \"worker\"\n")
	fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"claude\"\n")

	agent := config.Agent{Name: "worker"}
	ok, err := configedit.LocalDiscoveredAgent(fs, "/city", agent)
	if err == nil {
		t.Fatal("LocalDiscoveredAgent error = nil, want pack.toml parse error")
	}
	if ok {
		t.Fatal("LocalDiscoveredAgent classified agent as local-discovered despite malformed pack.toml")
	}
	if !strings.Contains(err.Error(), "parsing /city/pack.toml") {
		t.Fatalf("LocalDiscoveredAgent error = %v, want pack.toml parse context", err)
	}
}

func TestSuspendAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSuspendRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}

	// Suspension is recorded in the runtime state file, not city.toml.
	st, err := suspensionstate.Load(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("Load suspension state: %v", err)
	}
	if !suspensionstate.IsRigSuspended(st, "my-rig") {
		t.Error("expected my-rig to be suspended in runtime state")
	}
	cfg := readTOML(t, path)
	if cfg.Rigs[0].Suspended {
		t.Error("expected city.toml to NOT have suspended=true (legacy field is deprecated)")
	}
	if cfg.Rigs[0].SuspendedOnStart {
		t.Error("expected city.toml to NOT have suspended_on_start=true (runtime state owns transient suspend)")
	}
}

func TestSuspendRig_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendRig("nonexistent"); err == nil {
		t.Fatal("expected ErrNotFound for nonexistent rig")
	}
}

func TestResumeRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
suspended_on_start = true
`
	path := writeTOML(t, dir, city)
	want := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, dir, "my-rig", &want); err != nil {
		t.Fatalf("pre-suspend: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeRig("my-rig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}

	// city.toml must NOT be edited; the SuspendedOnStart flag is the
	// committable default and resume records the override in runtime state.
	cfg := readTOML(t, path)
	if !cfg.Rigs[0].SuspendedOnStart {
		t.Error("expected city.toml suspended_on_start to remain set after resume")
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("Load suspension state: %v", err)
	}
	if v, ok := suspensionstate.ExplicitRig(st, "my-rig"); !ok || v {
		t.Errorf("expected explicit resume in runtime state, got (%v, %v)", v, ok)
	}
	// Effective state must be not-suspended (explicit resume wins).
	if suspensionstate.EffectiveRigSuspended(st, "my-rig", cfg.Rigs[0].SuspendedOnStart) {
		t.Error("explicit resume in runtime state must beat suspended_on_start=true")
	}
}

func TestResumeRig_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeRig("nonexistent"); err == nil {
		t.Fatal("expected ErrNotFound for nonexistent rig")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func findAgent(t *testing.T, cfg *config.City, name string) config.Agent { //nolint:unparam // helper kept generic for future tests
	t.Helper()
	for _, a := range cfg.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q not found in %#v", name, cfg.Agents)
	return config.Agent{}
}

func TestSuspendCity(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}

	// city.toml must remain untouched — suspension lives in runtime state.
	cfg := readTOML(t, path)
	if cfg.Workspace.Suspended {
		t.Error("expected city.toml workspace.suspended to remain unset")
	}
	if cfg.Workspace.SuspendedOnStart {
		t.Error("expected city.toml workspace.suspended_on_start to remain unset")
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if !suspensionstate.IsCitySuspended(st) {
		t.Error("expected city to be suspended in runtime state")
	}
}

func TestResumeCity(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"
suspended_on_start = true
`
	path := writeTOML(t, dir, city)
	want := true
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, dir, &want); err != nil {
		t.Fatalf("pre-suspend: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}

	// city.toml suspended_on_start stays as committed default; explicit
	// resume sticks in runtime state and wins at read time.
	cfg := readTOML(t, path)
	if !cfg.Workspace.SuspendedOnStart {
		t.Error("expected workspace.suspended_on_start to remain set after resume")
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if v, ok := suspensionstate.ExplicitCity(st); !ok || v {
		t.Errorf("expected explicit resume in runtime state, got (%v, %v)", v, ok)
	}
	if suspensionstate.EffectiveCitySuspended(st, cfg.Workspace.SuspendedOnStart) {
		t.Error("explicit resume in runtime state must beat workspace.suspended_on_start=true")
	}
}

func TestCreateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "agents", "coder", "agent.toml")); err != nil {
		t.Fatalf("agent.toml stat: %v", err)
	}
	cfg := readExpandedTOML(t, path)
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "coder" && a.Provider == "claude" {
			found = true
		}
	}
	if !found {
		t.Error("agent 'coder' not found after create")
	}
}

func TestCreateAgentSchema2RollsBackFreshConventionScaffoldWhenAgentTOMLWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "coder")
	ed := configedit.NewEditor(&failRenameFS{target: filepath.Join(agentDir, "agent.toml")}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "city"})
	if err == nil {
		t.Fatal("CreateAgent succeeded, want injected agent.toml write failure")
	}
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("agent dir stat err = %v, want fresh scaffold removed", statErr)
	}
	for _, agent := range readExpandedTOML(t, path).Agents {
		if agent.Name == "coder" {
			t.Fatalf("expanded agents include ghost coder after failed create: %+v", agent)
		}
	}
}

func TestWriteLocalDiscoveredAgentConfigWritesConventionFields(t *testing.T) {
	fs := fsys.NewFake()

	err := configedit.WriteLocalDiscoveredAgentConfig(fs, "/city", config.Agent{
		Name:        "coder",
		Description: "Writes code.",
		Provider:    "claude",
		Scope:       "city",
		Suspended:   true,
	})
	if err != nil {
		t.Fatalf("WriteLocalDiscoveredAgentConfig: %v", err)
	}

	got := string(fs.Files["/city/agents/coder/agent.toml"])
	for _, want := range []string{
		`description = "Writes code."`,
		`provider = "claude"`,
		`scope = "city"`,
		`suspended = true`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("agent.toml = %q, want %q", got, want)
		}
	}
}

func TestLocalDiscoveredAgentDirRejectsEscapes(t *testing.T) {
	got, err := configedit.LocalDiscoveredAgentDir("/city", "coder")
	if err != nil {
		t.Fatalf("LocalDiscoveredAgentDir valid name: %v", err)
	}
	if got != filepath.Join("/city", "agents", "coder") {
		t.Fatalf("LocalDiscoveredAgentDir = %q, want /city/agents/coder", got)
	}

	if _, err := configedit.LocalDiscoveredAgentDir("/city", "../escape"); !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("LocalDiscoveredAgentDir escape error = %v, want ErrValidation", err)
	}
}

func TestCreateAgentSchema2RejectsInvalidNameBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "..", Provider: "claude"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agent.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("escaped agent.toml stat err = %v, want not exist", statErr)
	}
}

func TestCreateAgentSchema2RejectsInvalidScopeBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "global"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agents", "coder", "agent.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("agent.toml stat err = %v, want not exist", statErr)
	}
}

func TestCreateAgentSchema2RejectsRigScopedConventionAgentBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Dir: "rig-a", Provider: "claude", Scope: "city"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agents", "coder", "agent.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("agent.toml stat err = %v, want not exist", statErr)
	}
}

func TestCreateAgentSchema2RejectsRigScopeConventionAgentBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "rig"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agents", "coder", "agent.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("agent.toml stat err = %v, want not exist", statErr)
	}
}

func TestCreateAgentLegacyNoPackAppendsInline(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "coder" || cfg.Agents[0].Provider != "claude" {
		t.Fatalf("agents = %+v, want inline legacy coder", cfg.Agents)
	}
}

func TestCreateAgent_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "mayor", Provider: "claude"})
	if err == nil {
		t.Error("expected error for duplicate agent")
	}
}

func TestUpdateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("mayor", configedit.AgentUpdate{Provider: "gemini"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Provider != "gemini" {
		t.Errorf("provider = %q, want %q", cfg.Agents[0].Provider, "gemini")
	}
}

func TestUpdateAgent_PreservesSuspended(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// PATCH provider only — suspended must NOT be reset.
	err := ed.UpdateAgent("mayor", configedit.AgentUpdate{Provider: "gemini"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Provider != "gemini" {
		t.Errorf("provider = %q, want %q", cfg.Agents[0].Provider, "gemini")
	}
	if !cfg.Agents[0].Suspended {
		t.Error("suspended was reset to false — zero-value bug")
	}
}

func TestUpdateAgentSchema2LocalConventionAgentWritesAgentTOML(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := ed.UpdateAgent("coder", configedit.AgentUpdate{
		Provider:  "gemini",
		Scope:     "city",
		Suspended: boolPtrTest(true),
	}); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	raw := readTOML(t, path)
	if len(raw.Agents) != 0 {
		t.Fatalf("raw city.toml agents = %+v, want schema-2 convention agent outside city.toml", raw.Agents)
	}
	data := string(mustReadFile(t, filepath.Join(dir, "agents", "coder", "agent.toml")))
	for _, want := range []string{
		`provider = "gemini"`,
		`scope = "city"`,
		`suspended = true`,
	} {
		if !strings.Contains(data, want) {
			t.Fatalf("agent.toml = %q, want %s", data, want)
		}
	}
	agent := findAgent(t, readExpandedTOML(t, path), "coder")
	if agent.Provider != "gemini" || agent.Scope != "city" || !agent.Suspended {
		t.Fatalf("expanded agent = %+v, want updated provider/scope/suspended", agent)
	}
}

func TestUpdateAgentSchema2RejectsRigScopeConventionAgentBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentTomlPath := filepath.Join(dir, "agents", "coder", "agent.toml")
	before := string(mustReadFile(t, agentTomlPath))

	err := ed.UpdateAgent("coder", configedit.AgentUpdate{Scope: "rig"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("UpdateAgent error = %v, want ErrValidation", err)
	}
	after := string(mustReadFile(t, agentTomlPath))
	if after != before {
		t.Fatalf("agent.toml changed after rejected rig scope:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestUpdateAgentSchema2LocalConventionRollsBackAgentTOMLWhenCityWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
name = "coder"
provider = "legacy"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "coder")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the coder.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentTomlPath := filepath.Join(agentDir, "agent.toml")
	if err := os.WriteFile(agentTomlPath, []byte("provider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(&failRenameFS{target: path}, path)

	err := ed.UpdateAgent("coder", configedit.AgentUpdate{Provider: "gemini"})
	if err == nil {
		t.Fatal("UpdateAgent succeeded, want injected city write failure")
	}
	agentToml := string(mustReadFile(t, agentTomlPath))
	if agentToml != "provider = \"claude\"\n" {
		t.Fatalf("agent.toml = %q, want original after rollback", agentToml)
	}
	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, `provider = "legacy"`) {
		t.Fatalf("city.toml patch was stripped despite failed write:\n%s", raw)
	}
}

func TestUpdateAgentSchema2LocalConventionValidatesRawBeforeWritingAgentTOML(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]

[[rigs]]
name = "frontend"

[[patches.agent]]
name = "coder"
provider = "legacy"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "coder")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the coder.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentTomlPath := filepath.Join(agentDir, "agent.toml")
	if err := os.WriteFile(agentTomlPath, []byte("provider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("coder", configedit.AgentUpdate{Provider: "gemini"})
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("UpdateAgent error = %v, want ErrValidation", err)
	}
	agentToml := string(mustReadFile(t, agentTomlPath))
	if strings.Contains(agentToml, "gemini") || !strings.Contains(agentToml, `provider = "claude"`) {
		t.Fatalf("agent.toml = %q, want original provider preserved after validation failure", agentToml)
	}
}

func TestUpdateAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("nonexistent", configedit.AgentUpdate{Provider: "claude"})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestDeleteAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("mayor"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			t.Error("agent 'mayor' still exists after delete")
		}
	}
}

func TestDeleteAgentSchema2LocalConventionAgentRemovesAgentTOML(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := ed.DeleteAgent("coder"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "agents", "coder", "agent.toml")); !os.IsNotExist(err) {
		t.Fatalf("agent.toml stat err = %v, want removed file", err)
	}
	raw := readTOML(t, path)
	if len(raw.Agents) != 0 {
		t.Fatalf("raw city.toml agents = %+v, want schema-2 convention agent outside city.toml", raw.Agents)
	}
	for _, agent := range readExpandedTOML(t, path).Agents {
		if agent.Name == "coder" {
			t.Fatalf("expanded agents still include deleted coder: %+v", agent)
		}
	}
}

func TestDeleteAgentSchema2PromptBackedConventionAgentRemovesScaffold(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "coder")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the coder.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("coder"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("agent scaffold stat err = %v, want removed directory", err)
	}
	for _, agent := range readExpandedTOML(t, path).Agents {
		if agent.Name == "coder" {
			t.Fatalf("expanded agents still include deleted coder: %+v", agent)
		}
	}
}

func TestDeleteAgentSchema2LocalConventionRollsBackScaffoldWhenCityWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
name = "coder"
provider = "legacy"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "coder")
	if err := os.MkdirAll(filepath.Join(agentDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, data := range map[string]string{
		"agent.toml":         "provider = \"claude\"\n",
		"prompt.template.md": "You are the coder.\n",
		"skills/local.md":    "skill notes\n",
	} {
		if err := os.WriteFile(filepath.Join(agentDir, rel), []byte(data), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	ed := configedit.NewEditor(&failRenameFS{target: path}, path)

	err := ed.DeleteAgent("coder")
	if err == nil {
		t.Fatal("DeleteAgent succeeded, want injected city write failure")
	}
	for rel, want := range map[string]string{
		"agent.toml":         "provider = \"claude\"\n",
		"prompt.template.md": "You are the coder.\n",
		"skills/local.md":    "skill notes\n",
	} {
		got := string(mustReadFile(t, filepath.Join(agentDir, rel)))
		if got != want {
			t.Fatalf("%s = %q, want restored %q", rel, got, want)
		}
	}
	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, `provider = "legacy"`) {
		t.Fatalf("city.toml patch was removed despite failed write:\n%s", raw)
	}
}

func TestDeleteAgentSchema2LocalConventionRollsBackScaffoldWhenRemoveFails(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(withTestProviderCatalog(`[workspace]

[[patches.agent]]
name = "coder"
provider = "legacy"
`))
	fs.Files["/city/pack.toml"] = []byte("[pack]\nname = \"test-city\"\nschema = 2\n")
	if err := fs.MkdirAll("/city/agents/coder", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/coder/agent.toml"] = []byte("suspended = true\n")
	fs.Files["/city/agents/coder/prompt.template.md"] = []byte("You are the coder.\n")
	fs.Files["/city/agents/coder/zz-blocked"] = []byte("keep me\n")
	ed := configedit.NewEditor(&failRemoveFakeFS{
		Fake:   fs,
		target: "/city/agents/coder/zz-blocked",
	}, "/city/city.toml")

	err := ed.DeleteAgent("coder")
	if err == nil {
		t.Fatal("DeleteAgent succeeded, want removal error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("DeleteAgent error = %v, want permission denied", err)
	}
	if _, ok := fs.Files["/city/agents/coder/zz-blocked"]; !ok {
		t.Fatal("blocked file was removed despite injected error")
	}
	for path, want := range map[string]string{
		"/city/agents/coder/agent.toml":         "suspended = true\n",
		"/city/agents/coder/prompt.template.md": "You are the coder.\n",
	} {
		if got, ok := fs.Files[path]; !ok || string(got) != want {
			t.Fatalf("%s = %q, want restored %q", path, got, want)
		}
	}
	if raw := string(fs.Files["/city/city.toml"]); !strings.Contains(raw, `provider = "legacy"`) {
		t.Fatalf("city.toml patch was removed despite failed local mutation:\n%s", raw)
	}
}

func TestDeleteAgentSchema2LocalConventionWithoutPatchRollsBackScaffoldWhenRemoveFails(t *testing.T) {
	fs := fsys.NewFake()
	initialCity := withTestProviderCatalog("[workspace]\n")
	fs.Files["/city/city.toml"] = []byte(initialCity)
	fs.Files["/city/pack.toml"] = []byte("[pack]\nname = \"test-city\"\nschema = 2\n")
	if err := fs.MkdirAll("/city/agents/coder", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/coder/agent.toml"] = []byte("provider = \"claude\"\n")
	fs.Files["/city/agents/coder/prompt.template.md"] = []byte("You are the coder.\n")
	fs.Files["/city/agents/coder/zz-blocked"] = []byte("keep me\n")
	ed := configedit.NewEditor(&failRemoveFakeFS{
		Fake:   fs,
		target: "/city/agents/coder/zz-blocked",
	}, "/city/city.toml")

	err := ed.DeleteAgent("coder")
	if err == nil {
		t.Fatal("DeleteAgent succeeded, want removal error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("DeleteAgent error = %v, want permission denied", err)
	}
	for path, want := range map[string]string{
		"/city/agents/coder/agent.toml":         "provider = \"claude\"\n",
		"/city/agents/coder/prompt.template.md": "You are the coder.\n",
		"/city/agents/coder/zz-blocked":         "keep me\n",
	} {
		if got, ok := fs.Files[path]; !ok || string(got) != want {
			t.Fatalf("%s = %q, want restored %q", path, got, want)
		}
	}
	if got := string(fs.Files["/city/city.toml"]); got != initialCity {
		t.Fatalf("city.toml = %q, want unchanged", got)
	}
}

func TestWriteLocalDiscoveredAgentConfigRejectsUnsupportedRichFields(t *testing.T) {
	fs := fsys.NewFake()

	err := configedit.WriteLocalDiscoveredAgentConfig(fs, "/city", config.Agent{
		Name:           "worker",
		Provider:       "claude",
		Scope:          "city",
		PromptTemplate: "custom.md",
		Args:           []string{"--danger"},
	})

	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("WriteLocalDiscoveredAgentConfig error = %v, want ErrValidation", err)
	}
	for _, want := range []string{"PromptTemplate", "Args"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want unsupported field %s", err, want)
		}
	}
	if len(fs.Files) != 0 {
		t.Fatalf("files were written despite validation failure: %+v", fs.Files)
	}
}

func TestWriteLocalDiscoveredAgentConfigRejectsUnsafeScaffoldPaths(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*fsys.Fake)
		wantError string
	}{
		{
			name: "agents root symlink",
			setup: func(fs *fsys.Fake) {
				fs.Dirs["/outside/agents"] = true
				fs.Symlinks["/city/agents"] = "/outside/agents"
			},
			wantError: "not a symlink",
		},
		{
			name: "agents root file",
			setup: func(fs *fsys.Fake) {
				fs.Files["/city/agents"] = []byte("not a directory")
			},
			wantError: "must be a directory",
		},
		{
			name: "agent dir symlink",
			setup: func(fs *fsys.Fake) {
				fs.Dirs["/city/agents"] = true
				fs.Dirs["/outside/worker"] = true
				fs.Symlinks["/city/agents/worker"] = "/outside/worker"
			},
			wantError: "not a symlink",
		},
		{
			name: "agent dir file",
			setup: func(fs *fsys.Fake) {
				fs.Dirs["/city/agents"] = true
				fs.Files["/city/agents/worker"] = []byte("not a directory")
			},
			wantError: "must be a directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			tc.setup(fs)

			err := configedit.WriteLocalDiscoveredAgentConfig(fs, "/city", config.Agent{Name: "worker", Provider: "claude"})
			if !errors.Is(err, configedit.ErrValidation) {
				t.Fatalf("WriteLocalDiscoveredAgentConfig error = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("WriteLocalDiscoveredAgentConfig error = %v, want %q", err, tc.wantError)
			}
			for _, path := range []string{
				"/city/agents/worker/agent.toml",
				"/outside/agents/worker/agent.toml",
				"/outside/worker/agent.toml",
			} {
				if _, ok := fs.Files[path]; ok {
					t.Fatalf("%s was written through rejected scaffold path", path)
				}
			}
			for _, call := range fs.Calls {
				if call.Method == "WriteFile" || call.Method == "Rename" {
					t.Fatalf("unexpected write call after scaffold path rejection: %+v", call)
				}
			}
		})
	}
}

func TestDeleteAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestUpdateRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateRig("my-rig", configedit.RigUpdate{Path: "/tmp/updated"})
	if err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	raw := readTOML(t, path)
	if raw.Rigs[0].Path != "" {
		t.Errorf("raw path = %q, want empty city.toml binding", raw.Rigs[0].Path)
	}
	cfg := readEffectiveTOML(t, path)
	if cfg.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("effective path = %q, want %q", cfg.Rigs[0].Path, "/tmp/updated")
	}
	binding := readSiteBinding(t, dir)
	if len(binding.Rigs) != 1 || binding.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("site binding = %+v, want updated path", binding.Rigs)
	}
}

func TestUpdateRigPreservesOrphanSiteBinding(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[agent]]
name = "test-agent"
provider = "claude"

[[rigs]]
name = "frontend"
path = "/tmp/frontend"
`)
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SiteBindingPath(dir), []byte(`[[rig]]
name = "frontend"
path = "/site/frontend"

[[rig]]
name = "archived"
path = "/site/archived"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.UpdateRig("frontend", configedit.RigUpdate{Path: "/site/updated"}); err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	binding := readSiteBinding(t, dir)
	if len(binding.Rigs) != 2 {
		t.Fatalf("site binding rigs = %+v, want frontend and archived", binding.Rigs)
	}
	got := map[string]string{}
	for _, rig := range binding.Rigs {
		got[rig.Name] = rig.Path
	}
	if got["frontend"] != "/site/updated" {
		t.Fatalf("frontend binding = %q, want updated path", got["frontend"])
	}
	if got["archived"] != "/site/archived" {
		t.Fatalf("archived binding = %q, want orphan preserved", got["archived"])
	}
}

func TestUpdateRig_PreservesSuspended(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// PATCH path only — suspended must NOT be reset.
	err := ed.UpdateRig("my-rig", configedit.RigUpdate{Path: "/tmp/updated"})
	if err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	raw := readTOML(t, path)
	if raw.Rigs[0].Path != "" {
		t.Errorf("raw path = %q, want empty city.toml binding", raw.Rigs[0].Path)
	}
	cfg := readEffectiveTOML(t, path)
	if cfg.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("effective path = %q, want %q", cfg.Rigs[0].Path, "/tmp/updated")
	}
	if !cfg.Rigs[0].Suspended {
		t.Error("suspended was reset to false — zero-value bug")
	}
}

func TestDeleteRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "polecat"
dir = "my-rig"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("my-rig"); err != nil {
		t.Fatalf("DeleteRig: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			t.Error("rig 'my-rig' still exists after delete")
		}
	}
	// Rig-scoped agents should also be removed.
	for _, a := range cfg.Agents {
		if a.Dir == "my-rig" {
			t.Errorf("rig-scoped agent %q still exists after rig delete", a.QualifiedName())
		}
	}
	// City-scoped agent should remain.
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			found = true
		}
	}
	if !found {
		t.Error("city-scoped agent 'mayor' was incorrectly removed")
	}
}

func TestDeleteRigRemovesDeletedSiteBindingAndPreservesOrphan(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "city-agent"
provider = "claude"

[[agent]]
name = "rig-agent"
dir = "frontend"
provider = "claude"

[[rigs]]
name = "frontend"
path = "/tmp/frontend"
`
	path := writeTOML(t, dir, city)
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SiteBindingPath(dir), []byte(`[[rig]]
name = "frontend"
path = "/site/frontend"

[[rig]]
name = "archived"
path = "/site/archived"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("frontend"); err != nil {
		t.Fatalf("DeleteRig: %v", err)
	}

	raw := readTOML(t, path)
	for _, rig := range raw.Rigs {
		if rig.Name == "frontend" {
			t.Fatalf("deleted rig %q still exists in city.toml", rig.Name)
		}
	}
	for _, agent := range raw.Agents {
		if agent.Dir == "frontend" {
			t.Fatalf("rig-scoped agent %q still exists in city.toml", agent.QualifiedName())
		}
	}

	binding := readSiteBinding(t, dir)
	got := map[string]string{}
	for _, rig := range binding.Rigs {
		got[rig.Name] = rig.Path
	}
	if _, ok := got["frontend"]; ok {
		t.Fatalf("deleted rig site binding was preserved: %+v", binding.Rigs)
	}
	if got["archived"] != "/site/archived" {
		t.Fatalf("orphan binding = %q, want preserved path %q", got["archived"], "/site/archived")
	}
}

func TestDeleteRig_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("nonexistent"); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}

// cityWithProvider returns a city.toml with a custom provider.
func cityWithProvider() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[providers.custom]
display_name = "Custom Agent"
command = "custom-cli"
`
}

func TestCreateProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	spec := config.ProviderSpec{
		DisplayName: "My Provider",
		Command:     "my-provider-cli",
		Args:        []string{"--flag"},
	}
	if err := ed.CreateProvider("myprov", spec); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got, ok := cfg.Providers["myprov"]
	if !ok {
		t.Fatal("provider 'myprov' not found after create")
	}
	if got.Command != "my-provider-cli" {
		t.Errorf("command = %q, want %q", got.Command, "my-provider-cli")
	}
	if got.DisplayName != "My Provider" {
		t.Errorf("display_name = %q, want %q", got.DisplayName, "My Provider")
	}
}

// TestCreateProvider_BaseOnlyNoCommand verifies the relaxed validation:
// a provider with only `base` set is valid — the chain walk inherits
// the command from the ancestor.
func TestCreateProvider_BaseOnlyNoCommand(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	base := "builtin:codex"
	spec := config.ProviderSpec{Base: &base}
	if err := ed.CreateProvider("codex-max", spec); err != nil {
		t.Fatalf("CreateProvider with base and no command: %v", err)
	}

	cfg := readTOML(t, path)
	got, ok := cfg.Providers["codex-max"]
	if !ok {
		t.Fatal("provider 'codex-max' not found after create")
	}
	if got.Base == nil {
		t.Fatal("Base pointer is nil after round-trip")
	}
	if *got.Base != "builtin:codex" {
		t.Errorf("*Base = %q, want builtin:codex", *got.Base)
	}
	if got.Command != "" {
		t.Errorf("Command = %q, want empty (inherited)", got.Command)
	}
}

// TestCreateProvider_NoBaseNoCommandRejected ensures that a provider
// that declares neither command nor base is still rejected by
// validateProviders.
func TestCreateProvider_NoBaseNoCommandRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("nothing", config.ProviderSpec{})
	if err == nil {
		t.Fatal("expected error for provider without command or base")
	}
}

func TestCreateProvider_RejectsInvalidLegacyBuiltinOptionDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("codex-fast", config.ProviderSpec{
		Command: "codex",
		OptionDefaults: map[string]string{
			"permission_mode": "typo",
		},
	})
	if err == nil {
		t.Fatal("expected invalid option_defaults to be rejected")
	}
	if !strings.Contains(err.Error(), `option_defaults key "permission_mode"`) {
		t.Fatalf("error = %v, want option_defaults validation detail", err)
	}
}

func TestCreateProvider_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("custom", config.ProviderSpec{Command: "other"})
	if err == nil {
		t.Error("expected error for duplicate provider")
	}
}

func TestUpdateProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	newCmd := "updated-cli"
	newACPCmd := "updated-cli-acp"
	newName := "Updated Agent"
	err := ed.UpdateProvider("custom", configedit.ProviderUpdate{
		Command:     &newCmd,
		ACPCommand:  &newACPCmd,
		ACPArgs:     []string{"rpc", "--stdio"},
		DisplayName: &newName,
	})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["custom"]
	if got.Command != "updated-cli" {
		t.Errorf("command = %q, want %q", got.Command, "updated-cli")
	}
	if got.ACPCommand != "updated-cli-acp" {
		t.Errorf("acp_command = %q, want %q", got.ACPCommand, "updated-cli-acp")
	}
	if len(got.ACPArgs) != 2 || got.ACPArgs[0] != "rpc" || got.ACPArgs[1] != "--stdio" {
		t.Errorf("acp_args = %#v, want [rpc --stdio]", got.ACPArgs)
	}
	if got.DisplayName != "Updated Agent" {
		t.Errorf("display_name = %q, want %q", got.DisplayName, "Updated Agent")
	}
}

func TestUpdateProvider_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "x"
	err := ed.UpdateProvider("nonexistent", configedit.ProviderUpdate{Command: &cmd})
	if err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

func TestUpdateProvider_PreservesUnchangedFields(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Only update command — display_name should be preserved.
	newCmd := "updated-cli"
	err := ed.UpdateProvider("custom", configedit.ProviderUpdate{Command: &newCmd})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["custom"]
	if got.Command != "updated-cli" {
		t.Errorf("command = %q, want %q", got.Command, "updated-cli")
	}
	if got.DisplayName != "Custom Agent" {
		t.Errorf("display_name was lost: %q", got.DisplayName)
	}
}

// cityWithModelProvider returns a city.toml with a custom provider whose
// options_schema declares model + permission_mode, so option_defaults for
// those keys pass schema validation.
func cityWithModelProvider() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "custom"

[providers.custom]
command = "custom-cli"

[[providers.custom.options_schema]]
key = "model"
label = "Model"
type = "select"
default = "x"

  [[providers.custom.options_schema.choices]]
  value = "x"
  label = "X"
  flag_args = ["--model", "x"]

  [[providers.custom.options_schema.choices]]
  value = "y"
  label = "Y"
  flag_args = ["--model", "y"]

[[providers.custom.options_schema]]
key = "permission_mode"
label = "Permission Mode"
type = "select"
default = "plan"

  [[providers.custom.options_schema.choices]]
  value = "plan"
  label = "Plan"
  flag_args = ["--permission-mode", "plan"]

  [[providers.custom.options_schema.choices]]
  value = "unrestricted"
  label = "Unrestricted"
  flag_args = ["--dangerously-skip-permissions"]
`
}

// TestCreateProvider_OptionDefaults verifies a create with an option_defaults
// map (e.g. model) round-trips to the provider's TOML.
func TestCreateProvider_OptionDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	spec := config.ProviderSpec{
		Command: "custom-cli",
		OptionsSchema: []config.ProviderOption{{
			Key:   "model",
			Label: "Model",
			Type:  "select",
			Choices: []config.OptionChoice{
				{Value: "x", Label: "X", FlagArgs: []string{"--model", "x"}},
				{Value: "y", Label: "Y", FlagArgs: []string{"--model", "y"}},
			},
		}},
		OptionDefaults: map[string]string{"model": "x"},
	}
	if err := ed.CreateProvider("myprov", spec); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["myprov"]
	if got.OptionDefaults["model"] != "x" {
		t.Errorf("OptionDefaults[model] = %q, want %q", got.OptionDefaults["model"], "x")
	}
}

// TestUpdateProvider_OptionDefaultsMergeNotReplace verifies that updating
// option_defaults merges keys: a model-only edit changes model while leaving
// a pre-existing unrelated option-default key untouched.
func TestUpdateProvider_OptionDefaultsMergeNotReplace(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithModelProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Seed a provider with two option defaults.
	if err := ed.UpdateProvider("custom", configedit.ProviderUpdate{
		OptionDefaults: map[string]string{"model": "x", "permission_mode": "unrestricted"},
	}); err != nil {
		t.Fatalf("seed UpdateProvider: %v", err)
	}

	// Edit only model; permission_mode must survive.
	if err := ed.UpdateProvider("custom", configedit.ProviderUpdate{
		OptionDefaults: map[string]string{"model": "y"},
	}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["custom"]
	if got.OptionDefaults["model"] != "y" {
		t.Errorf("OptionDefaults[model] = %q, want %q", got.OptionDefaults["model"], "y")
	}
	if got.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("OptionDefaults[permission_mode] = %q, want %q (merge, not replace)",
			got.OptionDefaults["permission_mode"], "unrestricted")
	}
}

func TestDeleteProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteProvider("custom"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	cfg := readTOML(t, path)
	if _, ok := cfg.Providers["custom"]; ok {
		t.Error("provider 'custom' still exists after delete")
	}
}

func TestDeleteProvider_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteProvider("nonexistent"); err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

// --- Patch resource tests ---

func TestSetAgentPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	err := ed.SetAgentPatch(config.AgentPatch{
		Dir: "rig1", Name: "worker", Suspended: &suspended,
	})
	if err != nil {
		t.Fatalf("SetAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(cfg.Patches.Agents))
	}
	if cfg.Patches.Agents[0].Name != "worker" {
		t.Errorf("name = %q, want %q", cfg.Patches.Agents[0].Name, "worker")
	}
}

func TestSetAgentPatch_Replaces(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Set initial patch.
	suspended := true
	_ = ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})

	// Replace with different values.
	suspended = false
	err := ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})
	if err != nil {
		t.Fatalf("SetAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1 (should replace, not append)", len(cfg.Patches.Agents))
	}
}

func TestDeleteAgentPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	_ = ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})

	if err := ed.DeleteAgentPatch("rig1/worker"); err != nil {
		t.Fatalf("DeleteAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 0 {
		t.Error("patches.agent should be empty after delete")
	}
}

func TestDeleteAgentPatch_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgentPatch("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent patch")
	}
}

func TestSetRigPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	err := ed.SetRigPatch(config.RigPatch{Name: "myrig", Suspended: &suspended})
	if err != nil {
		t.Fatalf("SetRigPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Rigs) != 1 {
		t.Fatalf("patches.rigs count = %d, want 1", len(cfg.Patches.Rigs))
	}
}

func TestDeleteRigPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	_ = ed.SetRigPatch(config.RigPatch{Name: "myrig", Suspended: &suspended})

	if err := ed.DeleteRigPatch("myrig"); err != nil {
		t.Fatalf("DeleteRigPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Rigs) != 0 {
		t.Error("patches.rigs should be empty after delete")
	}
}

func TestSetProviderPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "my-claude"
	err := ed.SetProviderPatch(config.ProviderPatch{Name: "claude", Command: &cmd})
	if err != nil {
		t.Fatalf("SetProviderPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Providers) != 1 {
		t.Fatalf("patches.providers count = %d, want 1", len(cfg.Patches.Providers))
	}
}

func TestDeleteProviderPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "my-claude"
	_ = ed.SetProviderPatch(config.ProviderPatch{Name: "claude", Command: &cmd})

	if err := ed.DeleteProviderPatch("claude"); err != nil {
		t.Fatalf("DeleteProviderPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Providers) != 0 {
		t.Error("patches.providers should be empty after delete")
	}
}

func TestSetOrderOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := false
	trigger := "cooldown"
	err := ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
		Trigger: &trigger,
	})
	if err != nil {
		t.Fatalf("SetOrderOverride: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(raw); got != "" && strings.Contains(got, "gate =") {
		t.Fatalf("city.toml still contains legacy gate key:\n%s", got)
	}
	if !strings.Contains(string(raw), `trigger = "cooldown"`) {
		t.Fatalf("city.toml missing canonical trigger key:\n%s", string(raw))
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Name != "health-check" {
		t.Errorf("override name = %q, want %q", ov.Name, "health-check")
	}
	if ov.Enabled == nil || *ov.Enabled {
		t.Error("expected enabled=false")
	}
	if ov.Trigger == nil || *ov.Trigger != "cooldown" {
		t.Fatalf("override trigger = %#v, want cooldown", ov.Trigger)
	}
}

func TestSetOrderOverride_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	disabled := false
	trigger := "cooldown"
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &disabled,
		Trigger: &trigger,
	})

	enabled := true
	err := ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("SetOrderOverride (update): %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Enabled == nil || !*ov.Enabled {
		t.Error("expected enabled=true after update")
	}
	if ov.Trigger != nil {
		t.Fatalf("expected trigger to be replaced away, got %#v", ov.Trigger)
	}
}

func TestMergeOrderOverridePreservesExistingTriggerOnPartialUpdate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	disabled := false
	trigger := "cooldown"
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &disabled,
		Trigger: &trigger,
	})

	enabled := true
	err := ed.MergeOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("MergeOrderOverride (partial update): %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Enabled == nil || !*ov.Enabled {
		t.Fatal("expected enabled=true after partial update")
	}
	if ov.Trigger == nil || *ov.Trigger != "cooldown" {
		t.Fatalf("trigger = %#v, want cooldown", ov.Trigger)
	}
}

func TestMergeOrderOverrideMergesEnv(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	_ = ed.SetOrderOverride(config.OrderOverride{
		Name: "health-check",
		Env:  map[string]string{"KEEP": "source", "OVERRIDE": "source"},
	})

	err := ed.MergeOrderOverride(config.OrderOverride{
		Name: "health-check",
		Env:  map[string]string{"OVERRIDE": "city", "ADD": "city"},
	})
	if err != nil {
		t.Fatalf("MergeOrderOverride: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	env := cfg.Orders.Overrides[0].Env
	if env["KEEP"] != "source" || env["OVERRIDE"] != "city" || env["ADD"] != "city" {
		t.Fatalf("Env = %+v, want merged env", env)
	}
}

func TestDeleteOrderOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := false
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})

	if err := ed.DeleteOrderOverride("health-check", ""); err != nil {
		t.Fatalf("DeleteOrderOverride: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 0 {
		t.Error("overrides should be empty after delete")
	}
}

func TestDeleteOrderOverride_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.DeleteOrderOverride("nonexistent", "")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent override")
	}
}

func TestMergeOrderOverrideMergesIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	tru := true
	if err := ed.SetOrderOverride(config.OrderOverride{Name: "unrouted-feeder", Idempotent: &tru}); err != nil {
		t.Fatalf("SetOrderOverride: %v", err)
	}

	// A partial merge that does not mention idempotent must PRESERVE it.
	trig := "cooldown"
	if err := ed.MergeOrderOverride(config.OrderOverride{Name: "unrouted-feeder", Trigger: &trig}); err != nil {
		t.Fatalf("MergeOrderOverride: %v", err)
	}
	cfg := readTOML(t, path)
	if got := cfg.Orders.Overrides[0].Idempotent; got == nil || !*got {
		t.Fatalf("idempotent should be preserved through a partial merge, got %v", got)
	}

	// An explicit idempotent=false must be APPLIED through the merge.
	fls := false
	if err := ed.MergeOrderOverride(config.OrderOverride{Name: "unrouted-feeder", Idempotent: &fls}); err != nil {
		t.Fatalf("MergeOrderOverride: %v", err)
	}
	cfg = readTOML(t, path)
	if got := cfg.Orders.Overrides[0].Idempotent; got == nil || *got {
		t.Fatalf("idempotent=false should be applied through merge, got %v", got)
	}
}

func TestMergeOrderOverrideNormalizesLegacyGateToTriggerOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity()+`
[orders]

[[orders.overrides]]
name = "health-check"
gate = "cooldown"
`)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := true
	err := ed.MergeOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("MergeOrderOverride: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "gate =") {
		t.Fatalf("city.toml still contains legacy gate key:\n%s", got)
	}
	if !strings.Contains(got, `trigger = "cooldown"`) {
		t.Fatalf("city.toml missing canonical trigger after enabled-only update:\n%s", got)
	}
}
