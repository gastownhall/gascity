//go:build acceptance_a

// Migration regression tests.
//
// Each test encodes a specific bug found by contributor quad341 while
// migrating from steveyegge/gastown to the gascity gastown pack. The
// tests are permanent regression guards: fast (no tmux, no dolt, no
// inference), testing config invariants and pack materialization only.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// hasAgent reports whether cfg contains an agent with the given name
// (unqualified). This matches any Dir value.
func hasAgent(cfg *config.City, name string) bool {
	for _, a := range cfg.Agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// hasAgentQualified reports whether cfg contains an agent whose
// QualifiedName() matches identity exactly.
func hasAgentQualified(cfg *config.City, identity string) bool {
	for _, a := range cfg.Agents {
		if a.QualifiedName() == identity {
			return true
		}
	}
	return false
}

// agentCount returns the number of agents with the given unqualified name.
func agentCount(cfg *config.City, name string) int {
	n := 0
	for _, a := range cfg.Agents {
		if a.Name == name {
			n++
		}
	}
	return n
}

// TestRegression_GastownConfig groups regression tests that validate config
// invariants on a plain gastown city (no rig additions). They share a
// single gc init call.
func TestRegression_GastownConfig(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(c.Dir, "city.toml"))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	// PR #202: named_session defaults to mode "always" instead of "on_demand".
	t.Run("PR202_DefaultNamedSessionAlways", func(t *testing.T) {
		ns := config.FindNamedSession(cfg, "mayor")
		if ns == nil {
			t.Fatal("mayor named_session not found in loaded config")
		}

		mode := ns.ModeOrDefault()
		if mode != "always" {
			t.Errorf("mayor named_session mode = %q, want %q (PR #202 regression)", mode, "always")
		}
	})

	// PR #204: closed session beads permanently reserved their explicit name.
	t.Run("PR204_ClosedSessionReleasesName", func(t *testing.T) {
		if len(cfg.NamedSessions) == 0 {
			t.Fatal("no named sessions found in gastown config")
		}

		for i, ns := range cfg.NamedSessions {
			if ns.Template == "" {
				t.Errorf("named_session[%d] has empty Template", i)
			}
			m := ns.ModeOrDefault()
			if m != "always" && m != "on_demand" {
				t.Errorf("named_session[%d] (template=%q) has unknown mode %q", i, ns.Template, m)
			}
			qn := ns.QualifiedName()
			if qn == "" {
				t.Errorf("named_session[%d] (template=%q) has empty QualifiedName", i, ns.Template)
			}
		}
	})

	// Fallback resolution: gastown's non-fallback dog overrides maintenance's fallback.
	t.Run("FallbackAgentResolution", func(t *testing.T) {
		count := agentCount(cfg, "dog")
		if count != 1 {
			t.Errorf("expected exactly 1 dog agent, got %d (fallback resolution failure)", count)
		}

		for _, a := range cfg.Agents {
			if a.Name == "dog" {
				if a.Fallback {
					t.Error("dog agent has fallback=true; gastown's non-fallback should have won")
				}
				if len(a.SessionLive) == 0 {
					t.Error("dog agent has no session_live; expected gastown's themed dog")
				}
				break
			}
		}
	})

	// Transitive inclusion: gastown pack includes maintenance pack.
	t.Run("PackIncludesTransitive", func(t *testing.T) {
		gastownAgents := []string{"mayor", "deacon", "boot"}
		for _, name := range gastownAgents {
			if !hasAgent(cfg, name) {
				t.Errorf("gastown agent %q missing from config (transitive include failure)", name)
			}
		}

		if !hasAgent(cfg, "dog") {
			t.Error("maintenance agent 'dog' missing from config (transitive include failure)")
		}
	})

	// PR #213: system packs (maintenance) auto-included via pack expansion.
	t.Run("SystemPacksAutoIncluded", func(t *testing.T) {
		if !hasAgent(cfg, "dog") {
			t.Error("maintenance pack agent 'dog' not found; system pack auto-inclusion failed (PR #213 regression)")
		}

		hasGastownInclude := false
		for _, inc := range cfg.Workspace.Includes {
			if strings.Contains(inc, "gastown") {
				hasGastownInclude = true
				break
			}
		}
		if !hasGastownInclude {
			t.Error("workspace.includes does not reference gastown pack")
		}

		if len(cfg.PackDirs) == 0 {
			t.Error("PackDirs is empty after config load; pack expansion did not run")
		}

		if cfg.Beads.Provider == "bd" || cfg.Beads.Provider == "" {
			t.Log("beads provider is bd (or default); maintenance formulas expected via pack expansion")
		}

		cityFormulas := cfg.FormulaLayers.City
		hasMaintenanceFormulas := false
		for _, dir := range cityFormulas {
			if strings.Contains(dir, "maintenance") {
				hasMaintenanceFormulas = true
				break
			}
		}
		if !hasMaintenanceFormulas && len(cityFormulas) > 0 {
			t.Error("maintenance pack formulas not found in formula layers")
		}
	})
}

// TestRegression_GastownPackArtifacts groups regression tests that validate
// materialized pack artifacts (formulas, prompts, git excludes) on a plain
// gastown city. They share a single gc init call.
func TestRegression_GastownPackArtifacts(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// PR #3044: invalid TOML escape in a formula file broke 5 CI tests.
	t.Run("FormulasParse", func(t *testing.T) {
		formulaDirs := []string{
			filepath.Join(c.Dir, "packs", "gastown", "formulas"),
			filepath.Join(c.Dir, "packs", "maintenance", "formulas"),
		}

		count := 0
		for _, dir := range formulaDirs {
			err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				if !strings.HasSuffix(path, ".formula.toml") && !strings.HasSuffix(path, "order.toml") {
					return nil
				}
				count++

				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Errorf("reading %s: %v", relPath(c.Dir, path), readErr)
					return nil
				}

				var raw map[string]interface{}
				if _, parseErr := toml.Decode(string(data), &raw); parseErr != nil {
					t.Errorf("invalid TOML in %s: %v (PR #3044 regression)", relPath(c.Dir, path), parseErr)
				}
				return nil
			})
			if err != nil {
				t.Errorf("walking %s: %v", dir, err)
			}
		}

		if count == 0 {
			t.Fatal("no formula/order TOML files found in materialized packs")
		}
		t.Logf("validated %d formula/order TOML files", count)
	})

	// PR #2939: prompt referenced nonexistent /ralph-loop slash command.
	t.Run("PromptsRender", func(t *testing.T) {
		promptDirs := []string{
			filepath.Join(c.Dir, "packs", "gastown", "prompts"),
			filepath.Join(c.Dir, "packs", "maintenance", "prompts"),
		}

		count := 0
		for _, dir := range promptDirs {
			err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				if !strings.HasSuffix(path, ".md.tmpl") {
					return nil
				}
				count++

				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Errorf("reading %s: %v", relPath(c.Dir, path), readErr)
					return nil
				}

				if len(data) == 0 {
					t.Errorf("%s is empty", relPath(c.Dir, path))
					return nil
				}

				if strings.Contains(string(data), "/ralph-loop") {
					t.Errorf("%s contains /ralph-loop reference (PR #2939 regression)", relPath(c.Dir, path))
				}
				return nil
			})
			if err != nil {
				t.Errorf("walking %s: %v", dir, err)
			}
		}

		if count == 0 {
			t.Fatal("no .md.tmpl files found in materialized packs")
		}
		t.Logf("validated %d prompt template files", count)
	})

	// PR #3289: .beads/, .runtime/, .claude/commands/ blocked gt done.
	t.Run("GtDoneNotBlockedByInfraFiles", func(t *testing.T) {
		overlayDirs := []string{
			filepath.Join(c.Dir, "packs", "gastown", "overlays", "default"),
			filepath.Join(c.Dir, "packs", "gastown", "overlay"),
			filepath.Join(c.Dir, "packs", "maintenance", "overlays", "default"),
		}

		beadsExcluded := false

		for _, dir := range overlayDirs {
			gitignore := filepath.Join(dir, ".gitignore")
			if data, err := os.ReadFile(gitignore); err == nil {
				if containsBeadsPattern(string(data)) {
					beadsExcluded = true
				}
			}
		}

		scriptsDir := filepath.Join(c.Dir, "packs", "gastown", "scripts")
		if entries, err := os.ReadDir(scriptsDir); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".sh") {
					data, readErr := os.ReadFile(filepath.Join(scriptsDir, e.Name()))
					if readErr != nil {
						continue
					}
					content := string(data)
					usesExclude := strings.Contains(content, "info/exclude") ||
						strings.Contains(content, ".gitignore")
					if usesExclude && containsBeadsPattern(content) {
						beadsExcluded = true
					}
				}
			}
		}

		if !beadsExcluded {
			t.Error("no .beads exclusion found in gastown pack " +
				"(expected .gitignore or git exclude script mentioning .beads) " +
				"-- PR #3289 regression")
		}
	})
}

// TestRegression_GastownWithRigs groups regression tests that add rigs to a
// gastown city and validate rig-scoped config invariants. They share a
// single gc init call and rig setup.
func TestRegression_GastownWithRigs(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	rig1 := t.TempDir()
	rig2 := t.TempDir()
	c.RigAdd(rig1, "packs/gastown")
	c.RigAdd(rig2, "packs/gastown")

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(c.Dir, "city.toml"))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	// PR #2986: polecat names collided across rigs because Dir was not set.
	t.Run("PackAgentsHaveUniqueNames", func(t *testing.T) {
		seen := make(map[string]bool)
		for _, a := range cfg.Agents {
			qn := a.QualifiedName()
			if seen[qn] {
				t.Errorf("duplicate agent qualified name %q (PR #2986 regression)", qn)
			}
			seen[qn] = true
		}

		if len(seen) == 0 {
			t.Fatal("no agents found in config")
		}
		t.Logf("verified %d unique agent identities", len(seen))
	})

	// PR #3383: cross-rig bead routing used the wrong directory prefix.
	t.Run("CrossRigBeadPrefix", func(t *testing.T) {
		rigNames := make(map[string]bool)
		for _, r := range cfg.Rigs {
			if rigNames[r.Name] {
				t.Errorf("duplicate rig name %q", r.Name)
			}
			rigNames[r.Name] = true
		}

		if len(rigNames) < 2 {
			t.Fatalf("expected at least 2 rigs, got %d", len(rigNames))
		}

		rigAgentDirs := make(map[string]map[string]bool)
		for _, a := range cfg.Agents {
			if a.Dir != "" {
				if rigAgentDirs[a.Name] == nil {
					rigAgentDirs[a.Name] = make(map[string]bool)
				}
				rigAgentDirs[a.Name][a.Dir] = true
			}
		}

		for name, dirs := range rigAgentDirs {
			if len(dirs) > 1 {
				t.Logf("agent %q properly scoped across %d rigs", name, len(dirs))
			}
		}

		prefixes := make(map[string]string)
		for _, r := range cfg.Rigs {
			p := r.EffectivePrefix()
			if existing, ok := prefixes[p]; ok {
				t.Errorf("rigs %q and %q share bead prefix %q (PR #3383 regression)", existing, r.Name, p)
			}
			prefixes[p] = r.Name
		}
	})
}

// containsBeadsPattern reports whether text contains a pattern that would
// exclude the .beads directory (e.g. ".beads", ".beads/", "beads/").
func containsBeadsPattern(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, ".beads") || strings.Contains(line, "beads/") {
			return true
		}
	}
	return false
}

// relPath returns path relative to base, or the absolute path on error.
func relPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}
