package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// HostStagingFixture supplies the RUNTIME-CONTRACT-022 cases: a runtime
// whose sessions share the controller's host filesystem and user (an exec
// pack declaring fs.host), for which the provider does the session's host
// work itself.
type HostStagingFixture struct {
	// New returns such a provider, and a function reporting the top-level
	// fields of the start request its backend received for a session, nil
	// when no start request reached the backend.
	New func(t *testing.T) (sp runtime.Provider, received func(name string) []string)
}

// HostHandledStartFields are the start-request fields a provider that did
// their work on the host leaves out, so the backend does exactly what the
// request carries and never repeats it.
var HostHandledStartFields = []string{"copy_files", "overlay_dir", "pack_overlay_dirs", "pre_start", "session_setup", "session_setup_script"}

// hostStagingProvider is the provider (overlay) slot the cases stage for.
const hostStagingProvider = "claude"

// hostStagingCase is one session the cases start: its config, work dir and
// the fixture's view of what the backend received.
type hostStagingCase struct {
	sp       runtime.Provider
	received func(name string) []string
	name     string
	workDir  string
	cfg      runtime.Config
}

// newHostStagingCase builds a config carrying every kind of host work: a
// provider-aware pack overlay, an agent overlay, a copied file, pre_start,
// session_setup (whose first command fails) and session_live.
func newHostStagingCase(t *testing.T, f HostStagingFixture, name string) hostStagingCase {
	t.Helper()
	sp, received := f.New(t)
	src, workDir := t.TempDir(), t.TempDir()
	packOverlay, agentOverlay := filepath.Join(src, "pack"), filepath.Join(src, "agent")
	for path, body := range map[string]string{
		filepath.Join(packOverlay, "per-provider", hostStagingProvider, "gc-provider-file"): "mine",
		filepath.Join(packOverlay, "per-provider", "another-provider", "gc-foreign-file"):   "theirs",
		filepath.Join(agentOverlay, "gc-agent-file"):                                        "agent",
		filepath.Join(src, "gc-copied"):                                                     "copied",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = sp.Stop(name) })
	return hostStagingCase{sp: sp, received: received, name: name, workDir: workDir, cfg: runtime.Config{
		WorkDir:              workDir,
		Command:              "sleep 30",
		ProviderName:         hostStagingProvider,
		PackOverlayDirs:      []string{packOverlay},
		OverlayDir:           agentOverlay,
		CopyFiles:            []runtime.CopyEntry{{Src: filepath.Join(src, "gc-copied"), RelDst: "gc-copied"}},
		PreStart:             []string{"touch " + filepath.Join(workDir, "gc-pre-start")},
		SessionSetup:         []string{"exit 7", "touch " + filepath.Join(workDir, "gc-session-setup")},
		SessionLive:          []string{"touch " + filepath.Join(workDir, "gc-session-live")},
		AcceptStartupDialogs: new(bool),
	}}
}

// has reports whether rel exists under the case's work dir.
func (c hostStagingCase) has(rel string) bool {
	_, err := os.Stat(filepath.Join(c.workDir, rel))
	return err == nil
}

// RunHostStagingContract runs the RUNTIME-CONTRACT-022 cases: for a runtime
// sharing the host filesystem the provider stages the provider-aware
// overlays and copied files and runs pre_start on the host before the
// session exists, a failing pre_start failing the start with no session; it
// leaves every field it handled out of the start request; it runs
// session_setup on the host after the start, a failure there only a
// warning; it runs no session_live; it does none of that host work into a
// name a live session already holds; and a relaunch of a session (when the
// provider implements [runtime.RelaunchProvider]) runs its session_setup on
// the host again.
func RunHostStagingContract(t *testing.T, f HostStagingFixture) {
	t.Helper()
	t.Run("HostStaging_StagesAndRunsPreStartOnTheHost", func(t *testing.T) {
		if err := checkHostStagingStaged(newHostStagingCase(t, f, "host-staging-staged")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_PreStartFailureIsFatal", func(t *testing.T) {
		if err := checkHostStagingPreStartFatal(newHostStagingCase(t, f, "host-staging-prestart")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_StartRequestOmitsHandledWork", func(t *testing.T) {
		if err := checkHostStagingOmits(newHostStagingCase(t, f, "host-staging-omits")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_SessionSetupIsNonFatal", func(t *testing.T) {
		if err := checkHostStagingSessionSetup(newHostStagingCase(t, f, "host-staging-setup")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_SessionLiveIsNotRun", func(t *testing.T) {
		if err := checkHostStagingNoSessionLive(newHostStagingCase(t, f, "host-staging-live")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_NoHostWorkIntoALiveSession", func(t *testing.T) {
		if err := checkHostStagingNotIntoALiveSession(newHostStagingCase(t, f, "host-staging-occupied")); err != nil {
			t.Error(err)
		}
	})
	t.Run("HostStaging_RelaunchRerunsSessionSetup", func(t *testing.T) {
		if err := checkHostStagingRelaunchSetup(newHostStagingCase(t, f, "host-staging-relaunch")); err != nil {
			t.Error(err)
		}
	})
}

// hostWorkMarkers are the work-dir files the case's host work leaves.
var hostWorkMarkers = []string{"gc-provider-file", "gc-agent-file", "gc-copied", "gc-pre-start", "gc-session-setup"}

// clearHostWork removes every file the case's host work left in its work
// dir, so a later start shows whether it did that work again.
func (c hostStagingCase) clearHostWork() error {
	for _, rel := range hostWorkMarkers {
		if err := os.Remove(filepath.Join(c.workDir, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c hostStagingCase) start() error {
	return c.sp.Start(context.Background(), c.name, c.cfg)
}

// checkHostStagingStaged reports how the provider breaks RUNTIME-CONTRACT-022
// staging: the provider's own per-provider overlay, the agent overlay and the
// copied file are in the work dir, another provider's overlay is not, and
// pre_start ran.
func checkHostStagingStaged(c hostStagingCase) error {
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start = %w, want nil", err)
	}
	for _, rel := range []string{"gc-provider-file", "gc-agent-file", "gc-copied", "gc-pre-start"} {
		if !c.has(rel) {
			return fmt.Errorf("runtime contract 022: %s is not in the work dir after Start: the host work was not done", rel)
		}
	}
	if c.has("gc-foreign-file") {
		return errors.New("runtime contract 022: another provider's per-provider overlay was staged; staging must be provider-aware")
	}
	return nil
}

// checkHostStagingPreStartFatal reports how the provider breaks
// RUNTIME-CONTRACT-022 for a failing pre_start: Start fails, no start request
// reached the backend, and no session runs.
func checkHostStagingPreStartFatal(c hostStagingCase) error {
	c.cfg.PreStart = []string{"exit 3"}
	if err := c.start(); err == nil {
		return errors.New("runtime contract 022: Start with a failing pre_start = nil, want an error: pre_start failures are fatal")
	}
	if got := c.received(c.name); got != nil {
		return fmt.Errorf("runtime contract 022: a start request reached the backend after a fatal pre_start (fields %v)", got)
	}
	if c.sp.IsRunning(c.name) {
		return errors.New("runtime contract 022: a session runs after a fatal pre_start")
	}
	return nil
}

// checkHostStagingOmits reports how the provider breaks RUNTIME-CONTRACT-022
// in the start request: no field whose work it did, and every other field it
// sends.
func checkHostStagingOmits(c hostStagingCase) error {
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start = %w, want nil", err)
	}
	got := c.received(c.name)
	if got == nil {
		return errors.New("runtime contract 022: no start request reached the backend")
	}
	for _, field := range HostHandledStartFields {
		if slices.Contains(got, field) {
			return fmt.Errorf("runtime contract 022: the start request carries %q, work the provider already did on the host (fields %v)", field, got)
		}
	}
	for _, field := range []string{"work_dir", "command", "session_live"} {
		if !slices.Contains(got, field) {
			return fmt.Errorf("runtime contract 022: the start request lacks %q, which the provider did not handle (fields %v)", field, got)
		}
	}
	return nil
}

// checkHostStagingSessionSetup reports how the provider breaks
// RUNTIME-CONTRACT-022 for session_setup: a failing command does not fail
// Start, and the commands after it still run on the host.
func checkHostStagingSessionSetup(c hostStagingCase) error {
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start with a failing session_setup = %w, want nil: session_setup failures are warnings", err)
	}
	if !c.has("gc-session-setup") {
		return errors.New("runtime contract 022: session_setup did not run on the host after the start")
	}
	if !c.sp.IsRunning(c.name) {
		return errors.New("runtime contract 022: a failing session_setup stopped the session")
	}
	return nil
}

// checkHostStagingNoSessionLive reports how the provider breaks
// RUNTIME-CONTRACT-022 for session_live: it is not run on the host.
func checkHostStagingNoSessionLive(c hostStagingCase) error {
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start = %w, want nil", err)
	}
	if c.has("gc-session-live") {
		return errors.New("runtime contract 022: session_live ran on the host; the provider runs no session_live")
	}
	return nil
}

// checkHostStagingNotIntoALiveSession reports how the provider breaks
// RUNTIME-CONTRACT-022 for a start into a name a live session holds: it
// stages nothing and runs no pre_start or session_setup on the host, which
// would redo them in that live session's work dir. Whether the start itself
// fails is the backend's to say.
func checkHostStagingNotIntoALiveSession(c hostStagingCase) error {
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start = %w, want nil", err)
	}
	if err := c.clearHostWork(); err != nil {
		return fmt.Errorf("runtime contract 022: clearing the first start's host work: %w", err)
	}
	_ = c.start() // the backend may refuse the name; the host work is the check
	for _, rel := range hostWorkMarkers {
		if c.has(rel) {
			return fmt.Errorf("runtime contract 022: a start into a name a live session holds did host work (%s); host work goes only into a vacant name", rel)
		}
	}
	return nil
}

// checkHostStagingRelaunchSetup reports how the provider breaks
// RUNTIME-CONTRACT-022 on a relaunch: a provider implementing
// [runtime.RelaunchProvider] runs the session's session_setup on the host
// again, as a fresh start would. A provider without a relaunch passes.
func checkHostStagingRelaunchSetup(c hostStagingCase) error {
	rp, ok := c.sp.(runtime.RelaunchProvider)
	if !ok {
		return nil
	}
	if err := c.start(); err != nil {
		return fmt.Errorf("runtime contract 022: Start = %w, want nil", err)
	}
	if err := c.clearHostWork(); err != nil {
		return fmt.Errorf("runtime contract 022: clearing the start's host work: %w", err)
	}
	if err := rp.Relaunch(context.Background(), c.name, c.cfg); err != nil {
		return fmt.Errorf("runtime contract 022: Relaunch = %w, want nil", err)
	}
	if !c.has("gc-session-setup") {
		return errors.New("runtime contract 022: Relaunch did not run session_setup on the host again")
	}
	return nil
}
