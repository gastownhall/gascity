package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func stage1SkipTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	return cityPath, &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}
}

func runStage1ForSkipTest(t *testing.T, cityPath string, cfg *config.City, stderr *bytes.Buffer) {
	t.Helper()
	if err := runStage1SkillMaterialization(cityPath, cfg, stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}
}

// Stage-1 materialization re-runs on every config reload. User content at a
// sink path is skipped every time, and the identical skip line used to print
// on every pass. It must print once per episode: when the skip appears, when
// its reason changes, and again after a pass that no longer skips the path.
func TestStage1SkillSkipReportsOncePerEpisode(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "")
	cityPath, cfg := stage1SkipTestCity(t)
	sink := filepath.Join(cityPath, ".claude", "skills", "plan")
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	for range 3 {
		runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	}
	const userDir = "user content at sink path"
	if got := strings.Count(stderr.String(), userDir); got != 1 {
		t.Fatalf("standing skip reported %d times over 3 passes, want 1:\n%s", got, stderr.String())
	}

	// The reason changes: the user replaces the directory with their own
	// symlink to content outside every owned root.
	if err := os.RemoveAll(sink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), sink); err != nil {
		t.Fatal(err)
	}
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	const userLink = "user-owned symlink at sink path"
	if got := strings.Count(stderr.String(), userLink); got != 1 {
		t.Fatalf("changed skip reason reported %d times over 2 passes, want 1:\n%s", got, stderr.String())
	}

	// The conflict clears and the skill materializes, then the user puts
	// content back: a new episode that must be reported again.
	if err := os.Remove(sink); err != nil {
		t.Fatal(err)
	}
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	if info, err := os.Lstat(sink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skill was not materialized after the conflict cleared (err=%v)", err)
	}
	if err := os.Remove(sink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	if got := strings.Count(stderr.String(), userDir); got != 2 {
		t.Fatalf("recurring skip reported %d times in total, want 2:\n%s", got, stderr.String())
	}
}

func TestStage1SkillSkipReportsEveryPassUnderGCDebug(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "1")
	cityPath, cfg := stage1SkipTestCity(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".claude", "skills", "plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	for range 3 {
		runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	}
	if got := strings.Count(stderr.String(), "skipped skill"); got != 3 {
		t.Fatalf("GC_DEBUG reported %d skips over 3 passes, want 3:\n%s", got, stderr.String())
	}
}

// An agent whose materialization fails in a pass keeps its reported skips, so
// a transient failure does not make a standing skip print again.
func TestSkillSkipPassCarriesIncompleteAgents(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	log := &skillSkipLog{}
	city := "/city"

	p := log.beginPass(city)
	if !p.shouldReport("a", "/city/.claude/skills/x", "skip x") {
		t.Fatal("first sighting must report")
	}
	if !p.shouldReport("b", "/city/.claude/skills/y", "skip y") {
		t.Fatal("first sighting must report")
	}
	p.finish()

	p = log.beginPass(city)
	p.markIncomplete("a") // a failed this pass; b ran and no longer skips y
	p.finish()

	p = log.beginPass(city)
	if p.shouldReport("a", "/city/.claude/skills/x", "skip x") {
		t.Fatal("skip of an agent that failed one pass reported again")
	}
	if !p.shouldReport("b", "/city/.claude/skills/y", "skip y") {
		t.Fatal("skip that cleared and recurred was not reported")
	}
	p.finish()
}
