package tmux

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// cachedOffLaunch is what a start resolved while the city switch was off
// carries to the leaf (config.ProgramStatusLaunchFor): the arm a resolution
// cached across the reload still holds, and its forced env.
func cachedOffLaunch(t *testing.T) (runtime.ProgramStatusArm, map[string]string) {
	t.Helper()
	relay := true
	off := &config.City{Session: config.SessionConfig{ProgramStatus: string(runtime.ProgramStatusOff)}}
	resolved := &config.ResolvedProvider{Name: "seat", ProgramStatus: config.ProgramStatusBinding{
		Protocol: config.ProgramStatusProtocolOSC7501,
		App:      "claude-code",
		Relay:    &relay,
		Env:      map[string]string{"PROGRAM_STATUS_TEST_FLAG": "1"},
	}}
	return config.ProgramStatusLaunchFor(off, resolved, "")
}

// A start whose arm was resolved while the switch was off, started right
// after the leaf's live switch went shadow or on, is armed (relay wrap and
// tap) on create and on respawn: the leaf's live switch decides, not the
// switch the cached resolution was derived under (ga-f4ovjm.74).
func TestArmResolvedUnderOffFollowsLiveFlipOn(t *testing.T) {
	arm, env := cachedOffLaunch(t)
	if env != nil {
		t.Fatalf("launch resolved under off forced env %v; env must follow the switch at resolution", env)
	}
	relayed := shellquote.Join([]string{testCompanion, "relay", "--", testShell, "-c", "claude"})
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		t.Run(string(mode), func(t *testing.T) {
			tm, rec, _, _ := captureTestTmux(t)
			tm.cfg.ProgramStatus = runtime.ProgramStatusOff
			tm.SetProgramStatusMode(mode)
			rec.outs = []string{"%1"}
			if err := tm.newSessionWithCommandAndEnv("gc-cached", "/w", "claude", nil, arm); err != nil {
				t.Fatal(err)
			}
			if len(rec.calls) == 0 || !slices.Contains(rec.calls[0], "exec "+relayed) || !slices.Contains(rec.calls[0], programTapOption) {
				t.Fatalf("create after the flip with an arm resolved under off is not relay-wrapped and tapped: %q", rec.calls)
			}
			rec.calls = nil
			if err := tm.respawnPaneWithWorkDir("%1", "/w", "claude", arm); err != nil {
				t.Fatal(err)
			}
			if len(rec.calls) != 1 || !slices.Contains(rec.calls[0], "exec "+relayed) || !slices.Contains(rec.calls[0], programTapOption) {
				t.Fatalf("respawn after the flip with an arm resolved under off is not relay-wrapped and tapped: %q", rec.calls)
			}
		})
	}
}

// The same cached launch under the leaf's switch off arms nothing: create and
// respawn run today's exact argv, and the capability gate is never probed.
func TestArmResolvedUnderOffArmsNothingWhileOff(t *testing.T) {
	arm, _ := cachedOffLaunch(t)
	tm, rec, warnings, probes := captureTestTmux(t)
	tm.cfg.ProgramStatus = runtime.ProgramStatusOff
	rec.outs = []string{"%1"}
	if err := tm.newSessionWithCommandAndEnv("gc-cached", "/w", "claude", nil, arm); err != nil {
		t.Fatal(err)
	}
	wantCreate := []string{"-u", "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-cached", "-c", "/w", "claude"}
	if !slices.ContainsFunc(rec.calls, func(c []string) bool { return slices.Equal(c, wantCreate) }) {
		t.Fatalf("create under switch off is not today's argv %q: %q", wantCreate, rec.calls)
	}
	if got := callsContaining(rec.calls, programstatus.CompanionName); len(got) != 0 || len(rec.staged) != 0 {
		t.Fatalf("create under switch off armed: calls %q staged %q", got, rec.staged)
	}
	rec.calls = nil
	if err := tm.respawnPaneWithWorkDir("%1", "/w", "claude", arm); err != nil {
		t.Fatal(err)
	}
	wantRespawn := append([]string{"-u"}, tmuxArgvList([][]string{
		{"respawn-pane", "-k", "-t", "%1", "-c", "/w", "claude"},
		{"display-message", "-p", "-t", "%1", "#{pane_id}"},
		{"if-shell", "-F", "-t", "%1", "#{m:*.#{pane_id},#{@gc_ps_tap}}", offDisarmLine("%1")},
	})...)
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], wantRespawn) {
		t.Fatalf("respawn under switch off\n got %q\nwant [%q]", rec.calls, wantRespawn)
	}
	if *probes != 0 {
		t.Fatalf("switch off evaluated the capability gate %d times", *probes)
	}
	if warnings.Len() != 0 {
		t.Fatalf("switch off logged capture warnings: %q", warnings.String())
	}
}
