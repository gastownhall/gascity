package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

func TestCheckHardDependenciesRespectsSessionProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	oldLookPath := initLookPath
	initLookPath = func(file string) (string, error) {
		if file == "tmux" {
			return "", errors.New("missing tmux")
		}
		return "/bin/" + file, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	cases := []struct {
		name        string
		provider    string
		envOverride string
		wantTmux    bool
	}{
		{name: "default provider requires tmux", wantTmux: true},
		{name: "subprocess skips tmux", provider: "subprocess"},
		{name: "acp skips tmux", provider: "acp"},
		{name: "exec skips tmux", provider: "exec:/tmp/spy"},
		{name: "k8s skips tmux", provider: "k8s"},
		{name: "hybrid still requires tmux", provider: "hybrid", wantTmux: true},
		{name: "env override can skip tmux", envOverride: "subprocess"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_SESSION", tc.envOverride)
			cityPath := writeSessionProviderTestCity(t, tc.provider)

			missing := checkHardDependencies(cityPath)
			if got := hasMissingDep(missing, "tmux"); got != tc.wantTmux {
				t.Fatalf("tmux missing = %v, want %v (missing=%v)", got, tc.wantTmux, missing)
			}
		})
	}
}

func TestRegisterCoreBinaryChecksRespectsSessionProvider(t *testing.T) {
	oldLookPath := doctorLookPath
	doctorLookPath = func(file string) (string, error) {
		if file == "tmux" {
			return "", errors.New("missing tmux")
		}
		return "/bin/" + file, nil
	}
	t.Cleanup(func() { doctorLookPath = oldLookPath })

	cases := []struct {
		name     string
		provider string
		wantTmux bool
	}{
		{name: "default provider checks tmux", wantTmux: true},
		{name: "subprocess skips tmux", provider: "subprocess"},
		{name: "exec skips tmux", provider: "exec:/tmp/spy"},
		{name: "hybrid checks tmux", provider: "hybrid", wantTmux: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &doctor.Doctor{}
			registerCoreBinaryChecks(d, tc.provider)

			var out bytes.Buffer
			report := d.Run(&doctor.CheckContext{CityPath: t.TempDir()}, &out, false)
			gotTmux := strings.Contains(out.String(), "tmux")
			if gotTmux != tc.wantTmux {
				t.Fatalf("tmux check output = %v, want %v\noutput=%s", gotTmux, tc.wantTmux, out.String())
			}

			wantFailed := 0
			if tc.wantTmux {
				wantFailed = 1
			}
			if report.Failed != wantFailed {
				t.Fatalf("failed checks = %d, want %d\noutput=%s", report.Failed, wantFailed, out.String())
			}
		})
	}
}
