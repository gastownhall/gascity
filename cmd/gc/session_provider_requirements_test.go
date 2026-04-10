package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeSessionProviderTestCity(t *testing.T, provider string) string {
	t.Helper()

	cityPath := t.TempDir()
	cfg := config.DefaultCity("bright-lights")
	cfg.Session.Provider = provider
	cfg.Beads.Provider = "file"
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityPath
}

func hasMissingDep(missing []missingDep, prefix string) bool {
	for _, dep := range missing {
		if strings.HasPrefix(dep.name, prefix) {
			return true
		}
	}
	return false
}

func fakeLookPath(missing ...string) func(string) (string, error) {
	return func(file string) (string, error) {
		if slices.Contains(missing, file) {
			return "", errors.New("missing " + file)
		}
		if filepath.IsAbs(file) || strings.Contains(file, "/") {
			return file, nil
		}
		return "/bin/" + file, nil
	}
}

func TestCheckHardDependenciesRespectsSessionProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cases := []struct {
		name            string
		provider        string
		envOverride     string
		missingBins     []string
		wantMissing     []string
		dontWantMissing []string
	}{
		{
			name:        "default provider requires tmux",
			missingBins: []string{"tmux"},
			wantMissing: []string{"tmux"},
		},
		{
			name:            "subprocess skips tmux",
			provider:        "subprocess",
			missingBins:     []string{"tmux"},
			dontWantMissing: []string{"tmux"},
		},
		{
			name:            "acp skips tmux",
			provider:        "acp",
			missingBins:     []string{"tmux"},
			dontWantMissing: []string{"tmux"},
		},
		{
			name:            "exec requires provider script but skips tmux",
			provider:        "exec:/tmp/spy",
			missingBins:     []string{"tmux", "/tmp/spy"},
			wantMissing:     []string{"/tmp/spy"},
			dontWantMissing: []string{"tmux"},
		},
		{
			name:            "k8s skips tmux",
			provider:        "k8s",
			missingBins:     []string{"tmux"},
			dontWantMissing: []string{"tmux"},
		},
		{
			name:        "hybrid still requires tmux",
			provider:    "hybrid",
			missingBins: []string{"tmux"},
			wantMissing: []string{"tmux"},
		},
		{
			name:            "env override can skip tmux",
			envOverride:     "subprocess",
			missingBins:     []string{"tmux"},
			dontWantMissing: []string{"tmux"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldLookPath := initLookPath
			initLookPath = fakeLookPath(tc.missingBins...)
			t.Cleanup(func() { initLookPath = oldLookPath })

			t.Setenv("GC_SESSION", tc.envOverride)
			cityPath := writeSessionProviderTestCity(t, tc.provider)

			missing := checkHardDependencies(cityPath)
			for _, prefix := range tc.wantMissing {
				if !hasMissingDep(missing, prefix) {
					t.Fatalf("expected missing dep %q (missing=%v)", prefix, missing)
				}
			}
			for _, prefix := range tc.dontWantMissing {
				if hasMissingDep(missing, prefix) {
					t.Fatalf("did not expect missing dep %q (missing=%v)", prefix, missing)
				}
			}
		})
	}
}

func TestRegisterCoreBinaryChecksRespectsSessionProvider(t *testing.T) {
	cases := []struct {
		name           string
		provider       string
		missingBins    []string
		wantOutput     []string
		dontWantOutput []string
		wantFailed     int
	}{
		{
			name:        "default provider checks tmux",
			missingBins: []string{"tmux"},
			wantOutput:  []string{"tmux"},
			wantFailed:  1,
		},
		{
			name:           "subprocess skips tmux",
			provider:       "subprocess",
			missingBins:    []string{"tmux"},
			dontWantOutput: []string{"tmux"},
		},
		{
			name:           "exec checks provider script but skips tmux",
			provider:       "exec:/tmp/spy",
			missingBins:    []string{"tmux", "/tmp/spy"},
			wantOutput:     []string{"/tmp/spy"},
			dontWantOutput: []string{"tmux"},
			wantFailed:     1,
		},
		{
			name:        "hybrid checks tmux",
			provider:    "hybrid",
			missingBins: []string{"tmux"},
			wantOutput:  []string{"tmux"},
			wantFailed:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &doctor.Doctor{}
			registerCoreBinaryChecks(d, tc.provider, fakeLookPath(tc.missingBins...))

			var out bytes.Buffer
			report := d.Run(&doctor.CheckContext{CityPath: t.TempDir()}, &out, false)
			for _, want := range tc.wantOutput {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("expected output to contain %q\noutput=%s", want, out.String())
				}
			}
			for _, dontWant := range tc.dontWantOutput {
				if strings.Contains(out.String(), dontWant) {
					t.Fatalf("did not expect output to contain %q\noutput=%s", dontWant, out.String())
				}
			}
			if report.Failed != tc.wantFailed {
				t.Fatalf("failed checks = %d, want %d\noutput=%s", report.Failed, tc.wantFailed, out.String())
			}
		})
	}
}

func TestProviderDependencyCheckIncludesExecConfigGuidance(t *testing.T) {
	dep := sessionProviderDependencies("exec:/tmp/spy")[0]
	check := newProviderDependencyCheck(dep, fakeLookPath("/tmp/spy"))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %d, want error", result.Status)
	}
	if !strings.Contains(result.Message, `exec:/tmp/spy`) {
		t.Fatalf("message = %q, want configured provider", result.Message)
	}
	if !strings.Contains(result.FixHint, `GC_SESSION=exec:/tmp/spy`) {
		t.Fatalf("FixHint = %q, want GC_SESSION example", result.FixHint)
	}
	if !strings.Contains(result.FixHint, `[session].provider = "exec:/tmp/spy"`) {
		t.Fatalf("FixHint = %q, want city.toml example", result.FixHint)
	}
}
