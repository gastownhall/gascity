package config

import (
	"os"
	"path/filepath"
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

// TestNonPersistentRigPathWarningIsExemptFromStrictMode is the control for the
// audit's warn-only contract, at the seam where that contract is actually
// decided.
//
// The warning rides Provenance.Warnings into `gc start`, where strict mode (on
// by default) promotes every warning it does not recognize into a fatal error.
// Without the IsNonFatalSiteBindingWarning exemption the audit does the exact
// opposite of what its doc comment promises: a city with one doomed rig path —
// including one `gc rig add --allow-ephemeral` just created on purpose — stops
// booting entirely, with `--no-strict` hidden and refused outside the legacy
// standalone path.
//
// The text is taken from the real producer rather than a literal, so this
// cannot pass against a message the audit no longer emits.
func TestNonPersistentRigPathWarningIsExemptFromStrictMode(t *testing.T) {
	stubDurability(t, map[string]pathdurability.Result{
		"/tmp/adopt": {Class: pathdurability.Ephemeral, Filesystem: "tmpfs", Probed: "/tmp"},
	})

	cityRoot := t.TempDir()
	cfg := bindRigs(t, cityRoot, map[string]string{"adopt": "/tmp/adopt"})

	warnings, err := ApplySiteBindings(fsys.OSFS{}, cityRoot, cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}

	// Control for the control: a run that produced no durability warning would
	// otherwise satisfy the loop below vacuously.
	var durability []string
	for _, w := range warnings {
		if strings.Contains(w, "/tmp/adopt") {
			durability = append(durability, w)
		}
	}
	if len(durability) != 1 {
		t.Fatalf("want exactly one durability warning to classify, got %d: %q", len(durability), warnings)
	}

	if !IsNonFatalSiteBindingWarning(durability[0]) {
		t.Fatalf("the boot audit's warning is fatal in strict mode, so a city carrying a doomed rig "+
			"path refuses to start instead of warning: %q", durability[0])
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

// TestApplySiteBindingsJudgesRelativeRigPathsAgainstTheCity runs the audit
// against the real probe, because this is the one caller that reaches it with a
// relative path and every other test in this file stubs the probe with a
// function that ignores cityRoot entirely.
//
// city.toml may write a rig path relative to the city, and the rest of the
// codebase joins it onto the city root (internal/rig/topology.go). This audit
// reads rig.Path verbatim, so before the resolution moved into Classify the
// verdict came from wherever the process happened to be running — a controller
// started from a tmpfs working directory warned that a rig safely under the city
// was about to be lost, and told the operator to move it under the city
// directory it was already in.
//
// The registration guard cannot exercise this: validateRequest refuses a
// non-absolute ProvisionRequest.Path before the check runs. The boot audit is
// where the bug was reachable, so it is where the real probe belongs.
func TestApplySiteBindingsJudgesRelativeRigPathsAgainstTheCity(t *testing.T) {
	cityRoot := t.TempDir()

	// tmpfs is what makes the working directory discriminating. Without it the
	// pre-fix and post-fix verdicts agree and a pass would mean nothing, so skip
	// loudly instead.
	ephemeralDir, err := os.MkdirTemp("/dev/shm", "gc-site-binding-cwd-*")
	if err != nil {
		t.Skipf("no usable tmpfs to run from: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(ephemeralDir); err != nil {
			t.Logf("removing %s: %v", ephemeralDir, err)
		}
	})
	if got := pathdurability.Classify(cityRoot, ephemeralDir).Class; got != pathdurability.Ephemeral {
		t.Skipf("working directory classifies as %q, not %q; the relative-path bug is invisible here", got, pathdurability.Ephemeral)
	}

	// doomed is the control. It is an absolute path on the same tmpfs, so it must
	// warn in this very run — otherwise silence for the relative binding would
	// prove only that the audit had stopped warning about anything.
	doomed := filepath.Join(ephemeralDir, "doomed")
	cfg := bindRigs(t, cityRoot, map[string]string{
		"backend": "rigs/backend",
		"doomed":  doomed,
	})

	t.Chdir(ephemeralDir)

	warnings, err := ApplySiteBindings(fsys.OSFS{}, cityRoot, cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	var control, relative []string
	for _, w := range warnings {
		switch {
		case strings.Contains(w, doomed):
			control = append(control, w)
		case strings.Contains(w, "rigs/backend"):
			relative = append(relative, w)
		}
	}
	if len(control) != 1 {
		t.Fatalf("the control binding on tmpfs did not warn exactly once (got %d), so this test cannot "+
			"tell a fixed audit from a silent one: %q", len(control), warnings)
	}
	if len(relative) != 0 {
		t.Fatalf("a rig path relative to the city was judged against the working directory: %q", relative)
	}
}
