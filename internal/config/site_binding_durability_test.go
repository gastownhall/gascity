package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathdurability"
)

// stubDurability replaces the durability probe with a fixed path->verdict table
// for the duration of a test.
func stubDurability(t *testing.T, byPath map[string]pathdurability.Result) {
	t.Helper()
	orig := classifyRigPathDurability
	t.Cleanup(func() { classifyRigPathDurability = orig })
	classifyRigPathDurability = func(_, path string) pathdurability.Result {
		if res, ok := byPath[path]; ok {
			return res
		}
		return pathdurability.Result{Class: pathdurability.Unknown, Probed: path}
	}
}

// bindRigs writes a site binding for the given name->path pairs and returns the
// city config declaring them, mirroring how a real city carries rig paths.
func bindRigs(t *testing.T, cityRoot string, paths map[string]string) *City {
	t.Helper()
	var rigs []Rig
	for name, path := range paths {
		rigs = append(rigs, Rig{Name: name, Path: path})
	}
	if err := PersistRigSiteBindings(fsys.OSFS{}, cityRoot, rigs); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	declared := make([]Rig, 0, len(rigs))
	for _, r := range rigs {
		declared = append(declared, Rig{Name: r.Name})
	}
	return &City{Rigs: declared}
}

// TestApplySiteBindingsWarnsOnNonPersistentRigPath is the audit that names the
// risk while it is still recoverable, rather than after a restart has taken the
// rig content with it.
func TestApplySiteBindingsWarnsOnNonPersistentRigPath(t *testing.T) {
	stubDurability(t, map[string]pathdurability.Result{
		"/tmp/adopt": {Class: pathdurability.Ephemeral, Filesystem: "tmpfs", Probed: "/tmp"},
	})

	cityRoot := t.TempDir()
	cfg := bindRigs(t, cityRoot, map[string]string{"adopt": "/tmp/adopt"})

	warnings, err := ApplySiteBindings(fsys.OSFS{}, cityRoot, cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	found := ""
	for _, w := range warnings {
		if strings.Contains(w, "/tmp/adopt") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("no warning naming /tmp/adopt; got %q", warnings)
	}
	for _, want := range []string{`"adopt"`, "tmpfs", "restart"} {
		if !strings.Contains(found, want) {
			t.Fatalf("warning %q does not mention %q", found, want)
		}
	}
	// The audit reports; it must never refuse. A boot refusal would brick a
	// running city that a warning could have saved.
	if cfg.Rigs[0].Path != "/tmp/adopt" {
		t.Fatalf("binding was not applied: Path = %q", cfg.Rigs[0].Path)
	}
}

// TestApplySiteBindingsIsSilentForDurableRigPaths is the regression direction:
// the bindings a healthy hosted city carries must produce no warning at all.
func TestApplySiteBindingsIsSilentForDurableRigPaths(t *testing.T) {
	paths := map[string]string{
		"backend":  "/city/rigs/backend",
		"frontend": "/city/rigs/frontend",
		"project":  "/city/project",
		"project2": "/city/project2",
	}
	durable := map[string]pathdurability.Result{}
	for _, p := range paths {
		durable[p] = pathdurability.Result{Class: pathdurability.CityDevice, Probed: p}
	}
	stubDurability(t, durable)

	cityRoot := t.TempDir()
	cfg := bindRigs(t, cityRoot, paths)

	warnings, err := ApplySiteBindings(fsys.OSFS{}, cityRoot, cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("healthy city-rooted bindings warned: %q", warnings)
	}
}

// TestApplySiteBindingsDoesNotWarnOnOtherDevices keeps the boot audit to the one
// condition it can prove. A different durable mount is warned about once at
// registration; repeating it on every config load would only train operators to
// ignore the warning that matters.
func TestApplySiteBindingsDoesNotWarnOnOtherDevices(t *testing.T) {
	stubDurability(t, map[string]pathdurability.Result{
		"/data/projects/rig": {Class: pathdurability.OtherDevice, Probed: "/data/projects/rig"},
	})

	cityRoot := t.TempDir()
	cfg := bindRigs(t, cityRoot, map[string]string{"proj": "/data/projects/rig"})

	warnings, err := ApplySiteBindings(fsys.OSFS{}, cityRoot, cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("other-device binding warned: %q", warnings)
	}
}

// TestApplySiteBindingsSkipsDurabilityAuditOnSyntheticFS proves the audit does
// not probe the host filesystem for paths that only exist inside a test fake.
// Without this the classification would answer about the real /tmp rather than
// about the FS the caller actually handed in.
func TestApplySiteBindingsSkipsDurabilityAuditOnSyntheticFS(t *testing.T) {
	stubDurability(t, map[string]pathdurability.Result{
		"/tmp/adopt": {Class: pathdurability.Ephemeral, Filesystem: "tmpfs", Probed: "/tmp"},
	})

	fs := fsys.NewFake()
	if err := PersistRigSiteBindings(fs, "/city", []Rig{{Name: "adopt", Path: "/tmp/adopt"}}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	cfg := &City{Rigs: []Rig{{Name: "adopt"}}}

	warnings, err := ApplySiteBindings(fs, "/city", cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("synthetic FS was probed against the host: %q", warnings)
	}
}
