package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// kiroAgentsMDStagingCity builds a city whose kiro agent both stages the real
// embedded core kiro overlay (as a city with the core pack does in production)
// and installs kiro hooks into the same work dir, so both writers of the
// work dir's AGENTS.md run on every tick. It returns the work dir's AGENTS.md.
func kiroAgentsMDStagingCity(t *testing.T) (cityPath string, cfg *config.City, workFile string) {
	t.Helper()
	cityPath = t.TempDir()
	packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")
	for _, rel := range []string{"AGENTS.md", ".kiro/agents/gascity.json"} {
		data, err := core.PackFS.ReadFile("overlay/per-provider/kiro/" + rel)
		if err != nil {
			t.Fatalf("read embedded kiro overlay %s: %v", rel, err)
		}
		dst := filepath.Join(packOverlay, "per-provider", "kiro", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir kiro overlay: %v", err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("write kiro overlay %s: %v", rel, err)
		}
	}
	cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"kiro": {Command: "echo", PromptMode: "none"},
		},
		PackOverlayDirs: []string{packOverlay},
		Agents: []config.Agent{{
			Name:              "run-operator",
			Provider:          "kiro",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
			WorkDir:           "worker",
			InstallAgentHooks: []string{"kiro"},
		}},
	}
	workFile = filepath.Join(cityPath, "worker", "AGENTS.md")
	return cityPath, cfg, workFile
}

// A work dir that holds a released Kiro fallback AGENTS.md (one that says plain
// gc hook claims work) is upgraded on the reconcile tick, before the session's
// fingerprint is taken, and session-start staging then leaves it alone. A
// repository's own AGENTS.md is kept by both writers. Neither case may move
// the core fingerprint between the started tick and the next one, or drain the
// fresh session for config drift.
func TestConfigDrift_KiroAgentsMDUpgradeIsNotDrift(t *testing.T) {
	current, err := core.PackFS.ReadFile("overlay/per-provider/kiro/AGENTS.md")
	if err != nil {
		t.Fatalf("read embedded kiro AGENTS.md: %v", err)
	}
	repoOwned := []byte("# Repository agent instructions\n\nRun make test.\n")
	type tc struct {
		seed []byte
		want []byte
	}
	cases := map[string]tc{"repository-owned body": {seed: repoOwned, want: repoOwned}}
	for version, body := range overlay.ReleasedKiroAgentsMDBodies() {
		cases["released "+version+" body"] = tc{seed: body, want: current}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cityPath, cfg, workFile := kiroAgentsMDStagingCity(t)
			if err := os.MkdirAll(filepath.Dir(workFile), 0o755); err != nil {
				t.Fatalf("mkdir work dir: %v", err)
			}
			if err := os.WriteFile(workFile, c.seed, 0o644); err != nil {
				t.Fatalf("seed %s: %v", workFile, err)
			}

			first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
			sessionName, firstTP := onlyDesiredSession(t, first.State)
			if got, _ := os.ReadFile(workFile); !bytes.Equal(got, c.want) {
				t.Fatalf("tick left %s as:\n%s\nwant:\n%s", workFile, got, c.want)
			}
			startedCfg := templateParamsToConfig(firstTP)
			if got := startedCfg.ProviderOverlayName; got != "kiro" {
				t.Fatalf("ProviderOverlayName = %q, want kiro (the test must stage the kiro overlay)", got)
			}
			startedHash := runtime.CoreFingerprint(startedCfg)

			if err := runtime.StageSessionWorkDir(startedCfg); err != nil {
				t.Fatalf("StageSessionWorkDir: %v", err)
			}
			if got, _ := os.ReadFile(workFile); !bytes.Equal(got, c.want) {
				t.Fatalf("session-start staging changed %s to:\n%s", workFile, got)
			}
			if matches, _ := filepath.Glob(workFile + ".bak*"); len(matches) > 0 {
				t.Fatalf("unexpected AGENTS.md backups: %v", matches)
			}

			second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
			secondTP, ok := second.State[sessionName]
			if !ok {
				t.Fatalf("second desired state missing %q: %#v", sessionName, second.State)
			}
			currentCfg := templateParamsToConfig(secondTP)
			if got := runtime.CoreFingerprint(currentCfg); got != startedHash {
				t.Errorf("core fingerprint moved across the kiro AGENTS.md upgrade: started=%s current=%s drifted=%v",
					startedHash, got, runtime.CoreFingerprintDriftFields(runtime.CoreFingerprintBreakdown(startedCfg), currentCfg))
			}

			// The reconciler's drift check over the same two ticks.
			env := newReconcilerTestEnv(t)
			env.cfg = cfg
			env.desiredState[sessionName] = secondTP
			if err := env.sp.Start(context.Background(), sessionName, startedCfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			session := env.createSessionBead(sessionName, secondTP.TemplateName)
			env.markSessionActive(&session)
			env.setSessionMetadata(&session, map[string]string{
				"started_config_hash": startedHash,
				"started_live_hash":   runtime.LiveFingerprint(startedCfg),
				"live_hash":           runtime.LiveFingerprint(startedCfg),
			})
			env.reconcile([]beads.Bead{session})
			if ds := env.dt.get(session.ID); ds != nil && ds.reason == "config-drift" {
				t.Fatalf("fresh kiro session drained for config-drift\nstderr=%s", env.stderr.String())
			}
			if strings.Contains(env.stderr.String(), "config-drift "+sessionName) {
				t.Fatalf("reconciler reported config drift for the kiro AGENTS.md:\n%s", env.stderr.String())
			}
		})
	}
}

// Once the first tick has staged gc's own Kiro fallback into a fresh work dir,
// every later desired-state build and every session start re-stage the same
// overlay over it. Keeping a file that already matches the overlay preserves
// nothing, so none of those passes may log a preservation warning. A
// repository's own AGENTS.md is still reported on every pass, which also shows
// that each pass really re-stages the overlay.
func TestKiroAgentsMDRestageWarnsOnlyForRepositoryInstructions(t *testing.T) {
	current, err := core.PackFS.ReadFile("overlay/per-provider/kiro/AGENTS.md")
	if err != nil {
		t.Fatalf("read embedded kiro AGENTS.md: %v", err)
	}
	repoOwned := []byte("# Repository agent instructions\n\nRun make test.\n")
	cases := map[string]struct {
		seed        []byte
		want        []byte
		wantWarning bool
	}{
		"gc's own fallback":     {seed: nil, want: current, wantWarning: false},
		"repository-owned body": {seed: repoOwned, want: repoOwned, wantWarning: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cityPath, cfg, workFile := kiroAgentsMDStagingCity(t)
			if c.seed != nil {
				if err := os.MkdirAll(filepath.Dir(workFile), 0o755); err != nil {
					t.Fatalf("mkdir work dir: %v", err)
				}
				if err := os.WriteFile(workFile, c.seed, 0o644); err != nil {
					t.Fatalf("seed %s: %v", workFile, err)
				}
			}
			checkPass := func(pass, output string) {
				t.Helper()
				if got := strings.Contains(output, overlay.PreserveExistingWarningPrefix); got != c.wantWarning {
					t.Errorf("%s: preservation warning logged = %v, want %v; output:\n%s", pass, got, c.wantWarning, output)
				}
				if got, _ := os.ReadFile(workFile); !bytes.Equal(got, c.want) {
					t.Fatalf("%s left %s as:\n%s\nwant:\n%s", pass, workFile, got, c.want)
				}
			}

			var firstBuild bytes.Buffer
			first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, &firstBuild)
			checkPass("first desired-state build", firstBuild.String())
			_, tp := onlyDesiredSession(t, first.State)

			var sessionStart bytes.Buffer
			if err := runtime.StageSessionWorkDirWithWarnings(templateParamsToConfig(tp), &sessionStart); err != nil {
				t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
			}
			checkPass("session-start staging", sessionStart.String())

			var secondBuild bytes.Buffer
			buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, &secondBuild)
			checkPass("second desired-state build", secondBuild.String())
		})
	}
}
