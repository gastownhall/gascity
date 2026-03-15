package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"gc": func() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) },
		"bd": bdTestCmd,
	})
}

func TestTutorial01(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
	})
}

// --- gc version ---

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"version"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("run([version]) = %d, want 0", code)
	}
	out := stdout.String()
	// Default values when not built with ldflags.
	if !strings.Contains(out, "gc dev") {
		t.Errorf("stdout missing 'gc dev': %q", out)
	}
	if !strings.Contains(out, "commit:") {
		t.Errorf("stdout missing 'commit:': %q", out)
	}
	if !strings.Contains(out, "built:") {
		t.Errorf("stdout missing 'built:': %q", out)
	}
}

// --- findCity ---

func TestFindCity(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(dir)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", dir, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", dir, got, dir)
		}
	})

	t.Run("found", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(dir)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", dir, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", dir, got, dir)
		}
	})

	t.Run("nested", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(dir, "sub", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(nested)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", nested, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", nested, got, dir)
		}
	})

	t.Run("parent_canonical_outranks_child_legacy", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "city.toml"), []byte("[workspace]\nname = \"parent\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(parent, "child")
		if err := os.MkdirAll(filepath.Join(child, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(child, "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(nested)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", nested, err)
		}
		if got != parent {
			t.Errorf("findCity(%q) = %q, want %q", nested, got, parent)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		dir := t.TempDir() // no city.toml or .gc/ inside
		_, err := findCity(dir)
		if err == nil {
			t.Fatal("findCity() should fail without city.toml or .gc/")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})
}

// --- resolveCity ---

func TestResolveCityFlag(t *testing.T) {
	t.Run("flag_valid", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := cityFlag
		cityFlag = dir
		t.Cleanup(func() { cityFlag = old })

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if got != dir {
			t.Errorf("resolveCity() = %q, want %q", got, dir)
		}
	})

	t.Run("flag_no_gc_dir", func(t *testing.T) {
		dir := t.TempDir() // no .gc/ inside
		old := cityFlag
		cityFlag = dir
		t.Cleanup(func() { cityFlag = old })

		_, err := resolveCity()
		if err == nil {
			t.Fatal("resolveCity() should fail without .gc/")
		}
		if !strings.Contains(err.Error(), "not a city directory") {
			t.Errorf("error = %q, want 'not a city directory'", err)
		}
	})

	t.Run("flag_empty_fallback", func(t *testing.T) {
		// With empty flag, should fall back to cwd-based discovery.
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })

		orig, _ := os.Getwd()
		t.Cleanup(func() { _ = os.Chdir(orig) })
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if got != dir {
			t.Errorf("resolveCity() = %q, want %q", got, dir)
		}
	})
}

// --- doRigAdd (with fsys.Fake) ---

func TestDoRigAddCreatesDirIfMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "newproject") // does not exist yet
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Verify the rig directory was created.
	fi, err := os.Stat(rigPath)
	if err != nil {
		t.Fatalf("rig dir not created: %v", err)
	}
	if !fi.IsDir() {
		t.Error("rig path is not a directory")
	}
}

func TestDoRigAddMkdirRigPathFails(t *testing.T) {
	f := fsys.NewFake()
	// rigPath doesn't exist and MkdirAll will fail.
	f.Errors["/projects/myapp"] = fmt.Errorf("permission denied")

	var stderr bytes.Buffer
	code := doRigAdd(f, "/city", "/projects/myapp", "", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr = %q, want 'permission denied'", stderr.String())
	}
}

func TestDoRigAddNotADirectory(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/projects/myapp"] = []byte("not a dir") // file, not directory

	var stderr bytes.Buffer
	code := doRigAdd(f, "/city", "/projects/myapp", "", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "is not a directory") {
		t.Errorf("stderr = %q, want 'is not a directory'", stderr.String())
	}
}

func TestDoRigAddWithGit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	// Use real temp dirs so writeAllRoutes (which uses os.MkdirAll) works.
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Detected git repo") {
		t.Errorf("stdout missing 'Detected git repo': %q", out)
	}
	if !strings.Contains(out, "Rig added.") {
		t.Errorf("stdout missing 'Rig added.': %q", out)
	}
}

func TestDoRigAddWithoutGit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Detected git repo") {
		t.Errorf("stdout should not contain 'Detected git repo': %q", out)
	}
	if !strings.Contains(out, "Rig added.") {
		t.Errorf("stdout missing 'Rig added.': %q", out)
	}
}

// --- doRigList (with fsys.Fake) ---

func TestDoRigListConfigLoadFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", "city.toml")] = fmt.Errorf("no such file")

	var stderr bytes.Buffer
	code := doRigList(f, "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigList = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file") {
		t.Errorf("stderr = %q, want 'no such file'", stderr.String())
	}
}

func TestDoRigListSuccess(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/projects/alpha\"\n\n[[rigs]]\nname = \"beta\"\npath = \"/projects/beta\"\n")

	var stdout, stderr bytes.Buffer
	code := doRigList(f, "/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "alpha:") {
		t.Errorf("stdout missing 'alpha:': %q", out)
	}
	if !strings.Contains(out, "beta:") {
		t.Errorf("stdout missing 'beta:': %q", out)
	}
}

// --- sessionName ---

func TestSessionName(t *testing.T) {
	got := sessionName(nil, "bright-lights", "mayor", "")
	want := "mayor"
	if got != want {
		t.Errorf("sessionName = %q, want %q", got, want)
	}
}

func TestSessionNameTmuxOverride(t *testing.T) {
	// GC_TMUX_SESSION overrides the computed session name, allowing
	// agents inside Docker/K8s containers to target the correct tmux
	// session for metadata (drain, restart).
	t.Setenv("GC_TMUX_SESSION", "agent")
	got := sessionName(nil, "bright-lights", "mayor", "")
	want := "agent"
	if got != want {
		t.Errorf("sessionName with GC_TMUX_SESSION = %q, want %q", got, want)
	}
}

func TestResolveSessionNameWithStore(t *testing.T) {
	store := beads.NewMemStore()

	// Create a session bead for "worker" template.
	b, err := store.Create(beads.Bead{
		Title: "worker",
		Type:  "session",
		Labels: []string{
			"gc:session",
			"template:worker",
		},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker",
			"session_name": "s-gc-42",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// lookupSessionNameOrLegacy should find the bead-derived name.
	got := lookupSessionNameOrLegacy(store, "city", "worker", "")
	if got != "s-gc-42" {
		t.Errorf("lookupSessionNameOrLegacy(store, worker) = %q, want %q", got, "s-gc-42")
	}

	// With nil store, should fall back to legacy.
	got = lookupSessionNameOrLegacy(nil, "city", "worker", "")
	if got != "worker" {
		t.Errorf("lookupSessionNameOrLegacy(nil, worker) = %q, want %q", got, "worker")
	}

	// sessionNameFromBeadID derivation.
	got = sessionNameFromBeadID(b.ID)
	want := "s-" + strings.ReplaceAll(b.ID, "/", "--")
	if got != want {
		t.Errorf("sessionNameFromBeadID(%q) = %q, want %q", b.ID, got, want)
	}
}

func TestFindSessionNameByTemplate_SkipsClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker",
			"session_name": "s-gc-99",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close the bead — it should be skipped.
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(closed bead) = %q, want empty", got)
	}
}

func TestFindSessionNameByTemplate_SkipsPoolSlotBeads(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker-1",
			"session_name": "s-gc-50",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Querying for the base template "worker" should NOT match the pool instance.
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(pool_slot bead) = %q, want empty", got)
	}
}

func TestFindSessionNameByTemplate_SkipsEmptySessionName(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":    "worker",
			"common_name": "worker",
			// session_name intentionally missing
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(empty session_name) = %q, want empty", got)
	}
}

func TestDiscoverSessionBeads_IncludesBeadCreatedSessions(t *testing.T) {
	store := beads.NewMemStore()

	// Create a session bead as if "gc session new" created it.
	_, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "helper", StartCommand: "echo"},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, store, desired, io.Discard)

	if _, ok := desired["s-gc-100"]; !ok {
		t.Errorf("expected bead-created session s-gc-100 in desired state, got keys: %v", mapKeys(desired))
	}
}

func TestDiscoverSessionBeads_SkipsAlreadyDesired(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   "session",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "helper"},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Pre-populate desired state — bead should be skipped.
	desired := map[string]TemplateParams{
		"s-gc-100": {SessionName: "s-gc-100"},
	}
	discoverSessionBeads(bp, cfg, store, desired, io.Discard)

	// Should still be exactly 1 entry (not duplicated).
	if len(desired) != 1 {
		t.Errorf("expected 1 desired entry, got %d", len(desired))
	}
}

func TestDiscoverSessionBeads_SkipsNoTemplate(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "orphan",
		Type:   "session",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "s-gc-200",
			"state":        "active",
			// No template metadata
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, store, desired, io.Discard)

	if len(desired) != 0 {
		t.Errorf("expected 0 desired entries for bead without template, got %d", len(desired))
	}
}

func TestFindSessionNameByTemplate_PrefersAgentNameMatch(t *testing.T) {
	store := beads.NewMemStore()

	// Create a managed agent bead (has agent_name from syncSessionBeads).
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "agent:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": "s-managed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create an ad-hoc session bead (no agent_name, from gc session new).
	_, err = store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "s-adhoc",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should prefer the managed bead (agent_name match).
	got := findSessionNameByTemplate(store, "worker")
	if got != "s-managed" {
		t.Errorf("findSessionNameByTemplate with managed + ad-hoc = %q, want s-managed", got)
	}
}

func TestFindSessionNameByTemplate_TemplateMismatchNotFound(t *testing.T) {
	store := beads.NewMemStore()

	// Create a bead with template "worker" but query "myrig/worker".
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "s-gc-99",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Querying for rig-qualified name should NOT match bare template.
	got := findSessionNameByTemplate(store, "myrig/worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(myrig/worker) = %q, want empty (template mismatch)", got)
	}
}

func TestDiscoverSessionBeads_RigQualifiedTemplate(t *testing.T) {
	store := beads.NewMemStore()

	// Create a bead with a rig-qualified template (as cmdSessionNew now stores).
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:myrig/worker"},
		Metadata: map[string]string{
			"template":     "myrig/worker",
			"session_name": "s-gc-300",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", StartCommand: "echo"},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, store, desired, io.Discard)

	if _, ok := desired["s-gc-300"]; !ok {
		t.Errorf("expected rig-qualified bead session s-gc-300 in desired state, got keys: %v", mapKeys(desired))
	}
}

func TestDiscoverSessionBeads_ForkGetsOwnSessionNameInEnv(t *testing.T) {
	store := beads.NewMemStore()

	// Create the primary (managed) session bead — has agent_name, as if
	// syncSessionBeads created it.
	_, err := store.Create(beads.Bead{
		Title:  "overseer",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "agent:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"agent_name":   "overseer",
			"session_name": "s-primary",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a fork bead — no agent_name, as if "gc session new" created it.
	_, err = store.Create(beads.Bead{
		Title:  "overseer fork",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"session_name": "s-fork-1",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "overseer", StartCommand: "echo"},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Phase 1: the primary should be selected by resolveSessionName.
	desired := make(map[string]TemplateParams)
	// Simulate Phase 1 by adding the primary to desired.
	desired["s-primary"] = TemplateParams{
		SessionName: "s-primary",
		Env:         map[string]string{"GC_SESSION_NAME": "s-primary"},
	}

	// Phase 2: discover the fork.
	discoverSessionBeads(bp, cfg, store, desired, io.Discard)

	// Fork must be in desired state.
	forkTP, ok := desired["s-fork-1"]
	if !ok {
		t.Fatalf("expected fork s-fork-1 in desired state, got keys: %v", mapKeys(desired))
	}

	// GC_SESSION_NAME must be the fork's own session name, not the primary's.
	if got := forkTP.Env["GC_SESSION_NAME"]; got != "s-fork-1" {
		t.Errorf("fork GC_SESSION_NAME = %q, want %q", got, "s-fork-1")
	}
}

func mapKeys(m map[string]TemplateParams) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- gc init (doInit with fsys.Fake) ---

func TestDoInitSuccess(t *testing.T) {
	f := fsys.NewFake()
	// No pre-existing files — doInit creates everything from scratch.

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing 'Welcome to Gas City!': %q", out)
	}
	if !strings.Contains(out, "Initialized city") {
		t.Errorf("stdout missing 'Initialized city': %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}

	// Verify .gc/ and prompts/ were created (no rigs/ — created on demand by gc rig add).
	if !f.Dirs[filepath.Join("/bright-lights", ".gc")] {
		t.Error(".gc/ not created")
	}
	if f.Dirs[filepath.Join("/bright-lights", "rigs")] {
		t.Error("rigs/ should not be created by init")
	}
	if !f.Dirs[filepath.Join("/bright-lights", "prompts")] {
		t.Error("prompts/ not created")
	}

	// Verify prompt files were written.
	if _, ok := f.Files[filepath.Join("/bright-lights", "prompts", "mayor.md")]; !ok {
		t.Error("prompts/mayor.md not written")
	}
	if _, ok := f.Files[filepath.Join("/bright-lights", "prompts", "worker.md")]; !ok {
		t.Error("prompts/worker.md not written")
	}

	// Verify written config parses correctly.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Agents[0].PromptTemplate != "prompts/mayor.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", cfg.Agents[0].PromptTemplate, "prompts/mayor.md")
	}
}

func TestDoInitWritesExpectedTOML(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	got := string(f.Files[filepath.Join("/bright-lights", "city.toml")])
	want := `[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if got != want {
		t.Errorf("city.toml content:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestDoInitAlreadyInitialized(t *testing.T) {
	f := fsys.NewFake()
	// .gc/ already exists.
	f.Dirs[filepath.Join("/city", ".gc")] = true

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestDoInitBootstrapsExistingCityToml(t *testing.T) {
	f := fsys.NewFake()
	original := []byte("[workspace]\nname = \"city\"\n")
	f.Files[filepath.Join("/city", "city.toml")] = original

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("doInit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Bootstrapped city") {
		t.Errorf("stdout = %q, want bootstrap message", stdout.String())
	}
	if got := string(f.Files[filepath.Join("/city", "city.toml")]); got != string(original) {
		t.Errorf("city.toml overwritten:\ngot:\n%s\nwant:\n%s", got, original)
	}
	if !f.Dirs[filepath.Join("/city", ".gc")] {
		t.Error(".gc/ should be created during bootstrap")
	}
	if _, ok := f.Files[filepath.Join("/city", "hooks", "claude.json")]; !ok {
		t.Error("hooks/claude.json should be created during bootstrap")
	}
}

func TestDoInitMkdirGCFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", ".gc")] = fmt.Errorf("permission denied")

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr = %q, want 'permission denied'", stderr.String())
	}
}

func TestDoInitWriteFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", "city.toml")] = fmt.Errorf("read-only fs")

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "read-only fs") {
		t.Errorf("stderr = %q, want 'read-only fs'", stderr.String())
	}
}

// --- settings.json ---

func TestDoInitCreatesSettings(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	settingsPath := filepath.Join("/bright-lights", "hooks", "claude.json")
	data, ok := f.Files[settingsPath]
	if !ok {
		t.Fatal("hooks/claude.json not created")
	}
	if len(data) == 0 {
		t.Fatal("hooks/claude.json is empty")
	}
}

func TestDoInitSettingsIsValidJSON(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	settingsPath := filepath.Join("/bright-lights", "hooks", "claude.json")
	data := f.Files[settingsPath]

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}

	// Verify hooks structure exists.
	hooks, ok := parsed["hooks"]
	if !ok {
		t.Fatal("settings.json missing 'hooks' key")
	}
	hookMap, ok := hooks.(map[string]any)
	if !ok {
		t.Fatal("settings.json 'hooks' is not an object")
	}
	for _, event := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "Stop"} {
		if _, ok := hookMap[event]; !ok {
			t.Errorf("settings.json missing hook event %q", event)
		}
	}
}

func TestDoInitDoesNotOverwriteExistingSettings(t *testing.T) {
	f := fsys.NewFake()
	// Pre-populate .gc/ and settings.json with custom content.
	// doInit will see .gc/ exists and return "already initialized".
	// So test installClaudeHooks directly instead.
	settingsPath := filepath.Join("/city", "hooks", "claude.json")
	f.Dirs[filepath.Join("/city", "hooks")] = true
	f.Files[settingsPath] = []byte(`{"custom": true}`)

	code := installClaudeHooks(f, "/city", &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("installClaudeHooks = %d, want 0", code)
	}
	got := string(f.Files[settingsPath])
	if got != `{"custom": true}` {
		t.Errorf("settings.json was overwritten: %q", got)
	}
}

// --- settings flag injection ---

func TestSettingsArgsClaude(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(hooksDir, "claude.json")
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := settingsArgs(dir, "claude")
	want := "--settings .gc/settings.json"
	if got != want {
		t.Errorf("settingsArgs(claude) = %q, want %q", got, want)
	}
}

func TestSettingsArgsNonClaude(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "claude.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, provider := range []string{"codex", "gemini", "cursor", "copilot", "amp", "opencode"} {
		got := settingsArgs(dir, provider)
		if got != "" {
			t.Errorf("settingsArgs(%q) = %q, want empty", provider, got)
		}
	}
}

func TestSettingsArgsMissingFile(t *testing.T) {
	dir := t.TempDir()
	got := settingsArgs(dir, "claude")
	if got != "" {
		t.Errorf("settingsArgs(claude, no file) = %q, want empty", got)
	}
}

// --- runWizard ---

func TestRunWizardDefaults(t *testing.T) {
	// Two enters → default template (tutorial) + default agent (claude).
	stdin := strings.NewReader("\n\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if !wiz.interactive {
		t.Error("expected interactive = true")
	}
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "claude" {
		t.Errorf("provider = %q, want %q", wiz.provider, "claude")
	}
	// Verify both prompts were printed.
	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City SDK!") {
		t.Errorf("stdout missing welcome message: %q", out)
	}
	if !strings.Contains(out, "Choose a config template:") {
		t.Errorf("stdout missing template prompt: %q", out)
	}
	if !strings.Contains(out, "Choose your coding agent:") {
		t.Errorf("stdout missing agent prompt: %q", out)
	}
}

func TestRunWizardNilStdin(t *testing.T) {
	var stdout bytes.Buffer
	wiz := runWizard(nil, &stdout)

	if wiz.interactive {
		t.Error("expected interactive = false for nil stdin")
	}
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty", wiz.provider)
	}
	// No prompts should be printed.
	if stdout.Len() > 0 {
		t.Errorf("unexpected stdout for nil stdin: %q", stdout.String())
	}
}

func TestRunWizardSelectGemini(t *testing.T) {
	// Default template + Gemini CLI.
	stdin := strings.NewReader("\nGemini CLI\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "gemini" {
		t.Errorf("provider = %q, want %q", wiz.provider, "gemini")
	}
}

func TestRunWizardSelectCodex(t *testing.T) {
	// Default template + Codex by number.
	stdin := strings.NewReader("\n2\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "codex" {
		t.Errorf("provider = %q, want %q", wiz.provider, "codex")
	}
}

func TestRunWizardCustomTemplate(t *testing.T) {
	// Select custom template → skips agent question, returns minimal config.
	stdin := strings.NewReader("2\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.configName != "custom" {
		t.Errorf("configName = %q, want %q", wiz.configName, "custom")
	}
	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty for custom", wiz.provider)
	}
	if wiz.startCommand != "" {
		t.Errorf("startCommand = %q, want empty for custom", wiz.startCommand)
	}
	// Agent prompt should NOT appear.
	out := stdout.String()
	if strings.Contains(out, "Choose your coding agent:") {
		t.Errorf("stdout should not contain agent prompt for custom template: %q", out)
	}
}

func TestRunWizardSelectCursorByNumber(t *testing.T) {
	// Cursor is #4 in the order.
	stdin := strings.NewReader("\n4\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "cursor" {
		t.Errorf("provider = %q, want %q", wiz.provider, "cursor")
	}
}

func TestRunWizardSelectCopilotByName(t *testing.T) {
	stdin := strings.NewReader("\nGitHub Copilot\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "copilot" {
		t.Errorf("provider = %q, want %q", wiz.provider, "copilot")
	}
}

func TestRunWizardSelectByProviderKey(t *testing.T) {
	stdin := strings.NewReader("\namp\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "amp" {
		t.Errorf("provider = %q, want %q", wiz.provider, "amp")
	}
}

func TestRunWizardCustomCommand(t *testing.T) {
	// Default template + custom command (last option = len(providers)+1).
	customNum := len(config.BuiltinProviderOrder()) + 1
	stdin := strings.NewReader(fmt.Sprintf("\n%d\nmy-agent --auto --skip-confirm\n", customNum))
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty for custom command", wiz.provider)
	}
	if wiz.startCommand != "my-agent --auto --skip-confirm" {
		t.Errorf("startCommand = %q, want %q", wiz.startCommand, "my-agent --auto --skip-confirm")
	}
}

func TestRunWizardEOFStdin(t *testing.T) {
	stdin := strings.NewReader("")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	// EOF means default for both questions.
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "claude" {
		t.Errorf("provider = %q, want %q", wiz.provider, "claude")
	}
}

func TestDoInitWithWizardConfig(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive: true,
		configName:  "tutorial",
		provider:    "claude",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	// Verify output message.
	out := stdout.String()
	if !strings.Contains(out, "Created tutorial config") {
		t.Errorf("stdout missing wizard message: %q", out)
	}

	// Verify written config has one agent and provider.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	// Verify provider appears in TOML.
	if !strings.Contains(string(data), `provider = "claude"`) {
		t.Errorf("city.toml missing provider:\n%s", data)
	}
}

func TestDoInitWithCustomCommand(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive:  true,
		configName:   "tutorial",
		startCommand: "my-agent --auto",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify written config has start_command and no provider.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.StartCommand != "my-agent --auto" {
		t.Errorf("Workspace.StartCommand = %q, want %q", cfg.Workspace.StartCommand, "my-agent --auto")
	}
	if cfg.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", cfg.Workspace.Provider)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
}

func TestDoInitWithCustomTemplate(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive: true,
		configName:  "custom",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/my-city", wiz, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Custom template → DefaultCity (one mayor, no provider).
	data := f.Files[filepath.Join("/my-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", cfg.Workspace.Provider)
	}
}

func TestDoInitWithProviderFlagAndBootstrapProfile(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		configName:       "tutorial",
		provider:         "codex",
		bootstrapProfile: bootstrapProfileK8sCell,
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/hosted-city", wiz, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `default provider "codex"`) {
		t.Errorf("stdout missing provider message: %q", out)
	}

	data := f.Files[filepath.Join("/hosted-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "codex" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "codex")
	}
	if cfg.API.Bind != "0.0.0.0" {
		t.Errorf("API.Bind = %q, want %q", cfg.API.Bind, "0.0.0.0")
	}
	if cfg.API.Port != config.DefaultAPIPort {
		t.Errorf("API.Port = %d, want %d", cfg.API.Port, config.DefaultAPIPort)
	}
	if !cfg.API.AllowMutations {
		t.Error("API.AllowMutations = false, want true")
	}
}

func TestInitWizardConfigRejectsUnknownProvider(t *testing.T) {
	if _, err := initWizardConfig("not-a-provider", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestInitWizardConfigNormalizesBootstrapAliases(t *testing.T) {
	wiz, err := initWizardConfig("codex", "kubernetes")
	if err != nil {
		t.Fatalf("initWizardConfig returned error: %v", err)
	}
	if wiz.bootstrapProfile != bootstrapProfileK8sCell {
		t.Errorf("bootstrapProfile = %q, want %q", wiz.bootstrapProfile, bootstrapProfileK8sCell)
	}
}

// --- cmdInitFromTOMLFile ---

func TestCmdInitFromTOMLFileSuccess(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	// Use real temp dirs since cmdInitFromTOMLFile calls initBeads which
	// uses real filesystem via beadsProvider.
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, "my-config.toml")
	tomlContent := []byte(`[workspace]
name = "placeholder"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "worker"

[agent.pool]
min = 0
max = 5
check = "echo 3"
`)
	if err := os.WriteFile(src, tomlContent, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFile(fsys.OSFS{}, src, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdInitFromTOMLFile = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing welcome: %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}
	if !strings.Contains(out, "my-config.toml") {
		t.Errorf("stdout missing source filename: %q", out)
	}

	// Verify city.toml was written with updated name.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q (should be overridden)", cfg.Workspace.Name, "bright-lights")
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[1].Name != "worker" {
		t.Errorf("Agents[1].Name = %q, want %q", cfg.Agents[1].Name, "worker")
	}
	if cfg.Agents[1].Pool == nil {
		t.Fatal("Agents[1].Pool is nil, want non-nil")
	}
	if cfg.Agents[1].Pool.Max != 5 {
		t.Errorf("Agents[1].Pool.Max = %d, want 5", cfg.Agents[1].Pool.Max)
	}
}

func TestCmdInitFromTOMLFileNotFound(t *testing.T) {
	f := fsys.NewFake()
	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, "/nonexistent.toml", "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reading") {
		t.Errorf("stderr = %q, want reading error", stderr.String())
	}
}

func TestCmdInitFromTOMLFileInvalidTOML(t *testing.T) {
	f := fsys.NewFake()
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(src, []byte("[[[invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "parsing") {
		t.Errorf("stderr = %q, want parsing error", stderr.String())
	}
}

func TestCmdInitFromTOMLFileAlreadyInitialized(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs[filepath.Join("/city", ".gc")] = true

	dir := t.TempDir()
	src := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestCmdInitFromTOMLFileAlreadyInitializedByCityToml(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", "city.toml")] = []byte("[workspace]\nname = \"city\"\n")

	dir := t.TempDir()
	src := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

// --- gc init --from tests ---

func TestDoInitFromDirSuccess(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	dir := t.TempDir()

	// Create a minimal source city.
	srcDir := filepath.Join(dir, "my-template")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"template\"\nprovider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, ".gc", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".gc", "prompts", "mayor.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing welcome: %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}

	// Verify city.toml was copied and name updated.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}

	// Verify files were copied.
	if _, err := os.Stat(filepath.Join(cityPath, "prompts", "mayor.md")); err != nil {
		t.Errorf("prompts/mayor.md not copied: %v", err)
	}

	// Verify .gc/ was created.
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		t.Errorf(".gc/ not created: %v", err)
	}
}

func TestDoInitFromDirSkipsGCDir(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	dir := t.TempDir()

	// Source with a .gc/ directory.
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, ".gc", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".gc", "state.json"), []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	// .gc/ should exist (created fresh by init), but should NOT contain
	// the source's state.json or agents/ subdir.
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "state.json")); !os.IsNotExist(err) {
		t.Error(".gc/state.json should not have been copied from source")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "agents")); !os.IsNotExist(err) {
		t.Error(".gc/agents/ should not have been copied from source")
	}
}

func TestDoInitFromDirSkipsTestFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "gastown_test.go"),
		[]byte("package test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "helper.go"),
		[]byte("package helper"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Test files should be skipped.
	if _, err := os.Stat(filepath.Join(cityPath, "gastown_test.go")); !os.IsNotExist(err) {
		t.Error("gastown_test.go should not have been copied")
	}
	// Non-test Go files should be copied.
	if _, err := os.Stat(filepath.Join(cityPath, "helper.go")); err != nil {
		t.Errorf("helper.go should have been copied: %v", err)
	}
}

func TestDoInitFromDirNoCityToml(t *testing.T) {
	srcDir := t.TempDir() // no city.toml

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, filepath.Join(t.TempDir(), "dst"), &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no city.toml") {
		t.Errorf("stderr = %q, want 'no city.toml'", stderr.String())
	}
}

func TestDoInitFromDirAlreadyInitialized(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestDoInitFromDirAlreadyInitializedByCityToml(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"dst\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestDoInitFromDirPreservesPermissions(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(srcDir, "scripts", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hello"), 0o755); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	info, err := os.Stat(filepath.Join(cityPath, "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions = %o, want 755", info.Mode().Perm())
	}
}

func TestInitFromSkip(t *testing.T) {
	tests := []struct {
		relPath string
		isDir   bool
		want    bool
	}{
		{".gc", true, false},
		{".gc/state.json", false, true},
		{filepath.Join(".gc", "agents", "mayor.json"), false, true},
		{filepath.Join(".gc", "prompts"), true, false},
		{filepath.Join(".gc", "prompts", "mayor.md"), false, false},
		{"gastown_test.go", false, true},
		{filepath.Join("sub", "foo_test.go"), false, true},
		{"city.toml", false, false},
		{"scripts", true, false},
		{"helper.go", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			got := initFromSkip(tt.relPath, tt.isDir)
			if got != tt.want {
				t.Errorf("initFromSkip(%q, %v) = %v, want %v", tt.relPath, tt.isDir, got, tt.want)
			}
		})
	}
}

// --- gc stop (doStop with runtime.Fake) ---

func TestDoStopOneAgentRunning(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Stopped agent 'mayor'") {
		t.Errorf("stdout missing stop message: %q", out)
	}
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
}

func TestDoStopNoAgents(t *testing.T) {
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	code := doStop(nil, sp, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
	// Should not contain any "Stopped agent" messages.
	if strings.Contains(out, "Stopped agent") {
		t.Errorf("stdout should not contain 'Stopped agent' with no agents: %q", out)
	}
}

func TestDoStopAgentNotRunning(t *testing.T) {
	sp := runtime.NewFake()
	// "mayor" not started in provider — IsRunning returns false.

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
	// Should not contain "Stopped agent" since session wasn't running.
	if strings.Contains(out, "Stopped agent") {
		t.Errorf("stdout should not contain 'Stopped agent' for non-running session: %q", out)
	}
}

func TestDoStopMultipleAgents(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	_ = sp.Start(context.Background(), "worker", runtime.Config{})

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor", "worker"}, sp, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Stopped agent 'mayor'") {
		t.Errorf("stdout missing stop message for mayor: %q", out)
	}
	if !strings.Contains(out, "Stopped agent 'worker'") {
		t.Errorf("stdout missing stop message for worker: %q", out)
	}
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
}

func TestDoStopStopError(t *testing.T) {
	sp := runtime.NewFailFake() // Stop will fail

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0 (errors are non-fatal); stderr: %s", code, stderr.String())
	}
	// FailFake makes IsRunning return false, so no stop attempt.
	// Should still print "City stopped."
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", stdout.String())
	}
}

// --- doAgentAdd (with fsys.Fake) ---

func TestDoAgentAddSuccess(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added agent 'worker'") {
		t.Errorf("stdout = %q, want 'Added agent'", stdout.String())
	}

	// Verify the written config has both agents.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(got.Agents))
	}
	if got.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", got.Agents[0].Name, "mayor")
	}
	if got.Agents[1].Name != "worker" {
		t.Errorf("Agents[1].Name = %q, want %q", got.Agents[1].Name, "worker")
	}
}

func TestDoAgentAddDuplicate(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "mayor", "", "", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr.String())
	}
}

func TestDoAgentAddLoadFails(t *testing.T) {
	f := fsys.NewFake()
	// No city.toml → load fails.

	var stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "", "", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc agent add") {
		t.Errorf("stderr = %q, want 'gc agent add' prefix", stderr.String())
	}
}

// --- doAgentAdd with --prompt-template ---

// --- mergeEnv ---

func TestMergeEnvNil(t *testing.T) {
	got := mergeEnv(nil, nil)
	if got != nil {
		t.Errorf("mergeEnv(nil, nil) = %v, want nil", got)
	}
}

func TestMergeEnvSingle(t *testing.T) {
	got := mergeEnv(map[string]string{"A": "1"})
	if got["A"] != "1" {
		t.Errorf("got[A] = %q, want %q", got["A"], "1")
	}
}

func TestMergeEnvOverride(t *testing.T) {
	got := mergeEnv(
		map[string]string{"A": "base", "B": "keep"},
		map[string]string{"A": "override", "C": "new"},
	)
	if got["A"] != "override" {
		t.Errorf("got[A] = %q, want %q (later map wins)", got["A"], "override")
	}
	if got["B"] != "keep" {
		t.Errorf("got[B] = %q, want %q", got["B"], "keep")
	}
	if got["C"] != "new" {
		t.Errorf("got[C] = %q, want %q", got["C"], "new")
	}
}

func TestMergeEnvProviderEnvFlowsThrough(t *testing.T) {
	// Simulate what cmd_start does: provider env + GC_AGENT.
	providerEnv := map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`}
	got := mergeEnv(providerEnv, map[string]string{"GC_AGENT": "worker"})
	if got["OPENCODE_PERMISSION"] != `{"*":"allow"}` {
		t.Errorf("provider env lost: %v", got)
	}
	if got["GC_AGENT"] != "worker" {
		t.Errorf("GC_AGENT lost: %v", got)
	}
}

// --- resolveAgentChoice ---

func TestResolveAgentChoiceEmpty(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("", order, builtins, len(order)+1)
	if got != order[0] {
		t.Errorf("resolveAgentChoice('') = %q, want %q", got, order[0])
	}
}

func TestResolveAgentChoiceByNumber(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("2", order, builtins, len(order)+1)
	if got != order[1] {
		t.Errorf("resolveAgentChoice('2') = %q, want %q", got, order[1])
	}
}

func TestResolveAgentChoiceByDisplayName(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("Gemini CLI", order, builtins, len(order)+1)
	if got != "gemini" {
		t.Errorf("resolveAgentChoice('Gemini CLI') = %q, want %q", got, "gemini")
	}
}

func TestResolveAgentChoiceByKey(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("amp", order, builtins, len(order)+1)
	if got != "amp" {
		t.Errorf("resolveAgentChoice('amp') = %q, want %q", got, "amp")
	}
}

func TestResolveAgentChoiceOutOfRange(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	customNum := len(order) + 1

	for _, input := range []string{"0", "-1", "99", fmt.Sprintf("%d", customNum)} {
		got := resolveAgentChoice(input, order, builtins, customNum)
		if got != "" {
			t.Errorf("resolveAgentChoice(%q) = %q, want empty", input, got)
		}
	}
}

func TestResolveAgentChoiceUnknown(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("vim", order, builtins, len(order)+1)
	if got != "" {
		t.Errorf("resolveAgentChoice('vim') = %q, want empty", got)
	}
}

func TestDoAgentAddWithPromptTemplate(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "prompts/worker.md", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify the written config has the prompt_template.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(got.Agents))
	}
	if got.Agents[1].PromptTemplate != "prompts/worker.md" {
		t.Errorf("Agents[1].PromptTemplate = %q, want %q", got.Agents[1].PromptTemplate, "prompts/worker.md")
	}
}

// --- gc prime tests ---

func TestDoPrimeWithKnownAgent(t *testing.T) {
	// Set up a temp city with a mayor agent that has a prompt_template.
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, ".gc", "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Chdir into the city so findCity works.
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}
}

func TestDoPrimeUsesGCAgentEnv(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, ".gc", "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "mayor")

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}
}

func TestDoPrimeWithUnknownAgent(t *testing.T) {
	// Set up a temp city with a mayor agent.
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"nonexistent"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0", code)
	}
	if stdout.String() != defaultPrimePrompt {
		t.Errorf("stdout = %q, want default prompt", stdout.String())
	}
}

func TestDoPrimeNoArgs(t *testing.T) {
	// Outside any city — should still output default prompt.
	dir := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0", code)
	}
	if stdout.String() != defaultPrimePrompt {
		t.Errorf("stdout = %q, want default prompt", stdout.String())
	}
}

func TestDoPrimeBareName(t *testing.T) {
	// "gc prime polecat" should find agent with name="polecat" even when
	// it has dir="myrig" — bare template name lookup for pool agents.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, ".gc", "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are a pool worker.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "polecat.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlContent := `[workspace]
name = "test-city"

[[agent]]
name = "polecat"
dir = "myrig"
prompt_template = "prompts/polecat.md"

[agent.pool]
min = 1
max = 3
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(tomlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"polecat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want pool worker prompt %q", stdout.String(), promptContent)
	}
}

func TestDoPrimePoolAgentFallback(t *testing.T) {
	// An explicit pool agent with no prompt_template reads the materialized
	// pool-worker.md from .gc/system/prompts/ on disk.
	dir := t.TempDir()
	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}
	tomlContent := `[workspace]
name = "test-city"

[[agent]]
name = "polecat"
dir = "myrig"
start_command = "echo"

[agent.pool]
min = 0
max = -1
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(tomlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"polecat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Should get pool-worker prompt, not the generic default.
	if out == defaultPrimePrompt {
		t.Error("pool agent got generic defaultPrimePrompt, want pool-worker prompt")
	}
	if !strings.Contains(out, "Molecules") {
		t.Error("pool-worker prompt missing molecule instructions")
	}
	if !strings.Contains(out, "GUPP") {
		t.Error("pool-worker prompt missing GUPP")
	}
}

func TestDoPrimeHookPersistsSessionID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, ".gc", "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "mayor")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldPrimeStdin := primeStdin
	primeStdin = func() *os.File { return reader }
	t.Cleanup(func() {
		primeStdin = oldPrimeStdin
		_ = reader.Close()
	})
	if err := json.NewEncoder(writer).Encode(map[string]string{
		"session_id": "sess-123",
		"source":     "startup",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".runtime", "session_id"))
	if err != nil {
		t.Fatalf("reading persisted session ID: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "sess-123" {
		t.Errorf("persisted session ID = %q, want %q", got, "sess-123")
	}
}

// --- findEnclosingRig tests ---

func TestFindEnclosingRig(t *testing.T) {
	rigs := []config.Rig{
		{Name: "alpha", Path: "/projects/alpha"},
		{Name: "beta", Path: "/projects/beta"},
	}

	// Exact match.
	name, rp, found := findEnclosingRig("/projects/alpha", rigs)
	if !found || name != "alpha" || rp != "/projects/alpha" {
		t.Errorf("exact match: name=%q path=%q found=%v", name, rp, found)
	}

	// Subdirectory match.
	name, rp, found = findEnclosingRig("/projects/beta/src/main", rigs)
	if !found || name != "beta" || rp != "/projects/beta" {
		t.Errorf("subdir match: name=%q path=%q found=%v", name, rp, found)
	}

	// No match.
	_, _, found = findEnclosingRig("/other/project", rigs)
	if found {
		t.Error("expected no match for /other/project")
	}

	// Picks correct rig (not prefix collision).
	rigs2 := []config.Rig{
		{Name: "app", Path: "/projects/app"},
		{Name: "app-web", Path: "/projects/app-web"},
	}
	name, _, found = findEnclosingRig("/projects/app-web/src", rigs2)
	if !found || name != "app-web" {
		t.Errorf("prefix collision: name=%q found=%v, want app-web", name, found)
	}
}

// --- doAgentAdd with --dir and --suspended ---

func TestDoAgentAddWithDir(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "builder", "prompts/worker.md", "hello-world", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(got.Agents))
	}
	if got.Agents[1].Dir != "hello-world" {
		t.Errorf("Agents[1].Dir = %q, want %q", got.Agents[1].Dir, "hello-world")
	}
}

func TestDoAgentAddWithSuspended(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "builder", "prompts/worker.md", "hello-world", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(got.Agents))
	}
	if !got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = false, want true")
	}
	if got.Agents[1].Dir != "hello-world" {
		t.Errorf("Agents[1].Dir = %q, want %q", got.Agents[1].Dir, "hello-world")
	}
}

// --- doAgentSuspend ---

func TestDoAgentSuspend(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "builder"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(f, "/city", "builder", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentSuspend = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended agent 'builder'") {
		t.Errorf("stdout = %q, want suspend message", stdout.String())
	}

	// Verify config was updated.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if !got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = false after suspend, want true")
	}
	// Verify TOML contains the field.
	if !strings.Contains(string(written), "suspended = true") {
		t.Errorf("written TOML missing 'suspended = true':\n%s", written)
	}
}

func TestDoAgentSuspendNotFound(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stderr bytes.Buffer
	code := doAgentSuspend(f, "/city", "nonexistent", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentSuspend = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

// --- doAgentResume ---

func TestDoAgentResume(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "builder", Suspended: true},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentResume(f, "/city", "builder", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentResume = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed agent 'builder'") {
		t.Errorf("stdout = %q, want resume message", stdout.String())
	}

	// Verify config was updated.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = true after resume, want false")
	}
	// Verify TOML omits the field (omitempty).
	if strings.Contains(string(written), "suspended") {
		t.Errorf("written TOML should omit 'suspended' when false:\n%s", written)
	}
}

func TestDoAgentResumeNotFound(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stderr bytes.Buffer
	code := doAgentResume(f, "/city", "nonexistent", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentResume = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}
