package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeSessionProviderTestCity(t *testing.T, provider string) string {
	return writeSessionProviderTestCityWithProviders(t, provider, "file")
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

func writeSessionProviderTestCityWithProviders(t *testing.T, sessionProvider, beadsProvider string) string {
	t.Helper()

	cityPath := t.TempDir()
	cfg := config.DefaultCity("bright-lights")
	cfg.Session.Provider = sessionProvider
	cfg.Beads.Provider = beadsProvider
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityPath
}

func writeSessionProviderTestCityWithBeadsProvider(t *testing.T, sessionProvider, beadsProvider string) string {
	t.Helper()
	return writeSessionProviderTestCityWithProviders(t, sessionProvider, beadsProvider)
}

func dependencyNames(deps []binaryDependency) []string {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, dep.name)
	}
	return names
}

func withInitRunVersion(t *testing.T, versions map[string]string) {
	t.Helper()
	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		if version, ok := versions[binary]; ok {
			return version, nil
		}
		return "", errors.New("not checked")
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })
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
		{
			name:        "unknown provider follows tmux fallback",
			provider:    "mystery-provider",
			missingBins: []string{"tmux"},
			wantMissing: []string{"tmux"},
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

func TestCoreBinaryDependenciesPackManagedDeps(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cases := []struct {
		name     string
		provider string
		opts     coreBinaryDependencyOptions
		want     []string
		dontWant []string
	}{
		{
			name:     "bd provider gets pack-managed deps",
			provider: "bd",
			opts: coreBinaryDependencyOptions{
				includePackManaged: true,
			},
			want: []string{"jq", "git", "pgrep", "lsof", "dolt", "bd", "flock", "timeout/gtimeout/python3"},
		},
		{
			name:     "file provider skips pack-managed deps",
			provider: "file",
			opts: coreBinaryDependencyOptions{
				includePackManaged: true,
			},
			dontWant: []string{"dolt", "bd", "flock", "timeout/gtimeout/python3"},
		},
		{
			name:     "opt-out excludes pack-managed deps even for bd",
			provider: "bd",
			opts: coreBinaryDependencyOptions{
				includePackManaged: false,
			},
			dontWant: []string{"dolt", "bd", "flock", "timeout/gtimeout/python3"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := coreBinaryDependencies("", tc.provider, tc.opts)
			got := dependencyNames(deps)

			for _, name := range tc.want {
				if !slices.Contains(got, name) {
					t.Fatalf("expected dependency %q in %v", name, got)
				}
			}
			for _, name := range tc.dontWant {
				if slices.Contains(got, name) {
					t.Fatalf("did not expect dependency %q in %v", name, got)
				}
			}
		})
	}
}

func TestCoreBinaryDependenciesExecProviderPreservesScriptDependency(t *testing.T) {
	deps := coreBinaryDependencies("exec:/tmp/spy", "bd", coreBinaryDependencyOptions{includePackManaged: false})
	for _, dep := range deps {
		if dep.lookupName == "/tmp/spy" {
			if dep.provider != "exec:/tmp/spy" {
				t.Fatalf("dep.provider=%q, want exec:/tmp/spy", dep.provider)
			}
			if dep.kind != binaryDependencyKindExecSessionProvider {
				t.Fatalf("dep.kind=%d, want %d", dep.kind, binaryDependencyKindExecSessionProvider)
			}
			if dep.installHint == "" {
				t.Fatal("expected exec-session install hint")
			}
			return
		}
	}
	t.Fatal("expected exec provider dependency")
}

func TestSessionProviderDependenciesExecWithoutScriptPathUsesPlaceholderName(t *testing.T) {
	deps := sessionProviderDependencies("exec:")
	if len(deps) != 1 {
		t.Fatalf("len(deps) = %d, want 1", len(deps))
	}
	dep := deps[0]
	if dep.name != "exec session provider script" {
		t.Fatalf("dep.name = %q, want placeholder script name", dep.name)
	}
	if dep.lookupName != "" {
		t.Fatalf("dep.lookupName = %q, want empty", dep.lookupName)
	}
	if !strings.Contains(dep.installHint, "exec:/path/to/script") {
		t.Fatalf("dep.installHint = %q, want default example path", dep.installHint)
	}
	if sessionProviderRequiresTmux("exec:") {
		t.Fatal("sessionProviderRequiresTmux(exec:) = true, want false")
	}
}

func TestCheckHardDependenciesRespectsMinVersions(t *testing.T) {
	oldLookPath := initLookPath
	t.Cleanup(func() { initLookPath = oldLookPath })
	initLookPath = fakeLookPath()

	cityPath := writeSessionProviderTestCityWithBeadsProvider(t, "", "bd")
	t.Setenv("GC_BEADS", "bd")

	t.Run("sufficient versions", func(t *testing.T) {
		withInitRunVersion(t, map[string]string{
			"dolt": "dolt version " + doltMinVersion,
			"bd":   "bd version 2.0.0",
		})

		missing := checkHardDependencies(cityPath)
		if len(missing) != 0 {
			t.Fatalf("missing = %v", missing)
		}
	})

	t.Run("below minimum versions", func(t *testing.T) {
		withInitRunVersion(t, map[string]string{
			"dolt": "dolt version 1.10.0",
			"bd":   "bd version 0.9.9",
		})

		missing := checkHardDependencies(cityPath)
		if !hasMissingDep(missing, fmt.Sprintf("dolt (found v1.10.0, need v%s+)", doltMinVersion)) {
			t.Fatalf("expected dolt version gap, missing=%v", missing)
		}
		if !hasMissingDep(missing, "bd (found v0.9.9, need v1.0.0+)") {
			t.Fatalf("expected bd version gap, missing=%v", missing)
		}
	})

	t.Run("non-parseable versions are ignored", func(t *testing.T) {
		withInitRunVersion(t, map[string]string{
			"dolt": "dolt version unknown",
			"bd":   "bd version ???",
		})

		missing := checkHardDependencies(cityPath)
		if hasMissingDep(missing, "dolt (found") || hasMissingDep(missing, "bd (found") {
			t.Fatalf("did not expect version-missing dependency from non-parseable output, missing=%v", missing)
		}
	})
}

func TestParseDepVersion(t *testing.T) {
	t.Run("extracts first dotted version", func(t *testing.T) {
		withInitRunVersion(t, map[string]string{"dolt": "dolt version 1.86.1"})

		if got := parseDepVersion("dolt"); got != "1.86.1" {
			t.Fatalf("parseDepVersion = %q, want 1.86.1", got)
		}
	})

	t.Run("parsing non-numeric prefix fails", func(t *testing.T) {
		withInitRunVersion(t, map[string]string{"dolt": "dolt version v1.86.1"})
		if got := parseDepVersion("dolt"); got != "" {
			t.Fatalf("parseDepVersion = %q, want empty", got)
		}
	})
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2", "1.2.0", 0},
		{"1.10", "1.2", 1},
		{"1.2.0", "1.3", -1},
		{"2", "1.9.9", 1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
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
		{
			name:        "unknown provider checks tmux via fallback",
			provider:    "mystery-provider",
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

func TestRegisterCoreBinaryChecksNilLookPathStillRegistersChecks(t *testing.T) {
	d := &doctor.Doctor{}
	registerCoreBinaryChecks(d, "exec:", nil)

	checks := reflect.ValueOf(d).Elem().FieldByName("checks")
	if got := checks.Len(); got == 0 {
		t.Fatal("registerCoreBinaryChecks() registered no checks")
	}
}

func TestProviderDependencyCheckIncludesExecConfigGuidance(t *testing.T) {
	dep := sessionProviderDependencies("exec:/tmp/spy")[0]
	check := newBinaryDependencyCheck(dep, fakeLookPath("/tmp/spy"))

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

func TestBinaryDependencyCheckRunReportsMissingExecScriptPath(t *testing.T) {
	dep := sessionProviderDependencies("exec:")[0]
	check := newBinaryDependencyCheck(dep, fakeLookPath())

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %d, want error", result.Status)
	}
	if !strings.Contains(result.Message, "missing a script path") {
		t.Fatalf("message = %q, want missing-script-path guidance", result.Message)
	}
	if !strings.Contains(result.FixHint, "exec:/path/to/script") {
		t.Fatalf("FixHint = %q, want default exec example", result.FixHint)
	}
}

func TestBinaryDependencyCheckRunReportsMissingBinary(t *testing.T) {
	check := newBinaryDependencyCheck(binaryDependency{
		name:        "jq",
		lookupName:  "jq",
		installHint: "brew install jq",
	}, fakeLookPath("jq"))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %d, want error", result.Status)
	}
	if result.Message != "jq not found" {
		t.Fatalf("message = %q, want jq not found", result.Message)
	}
	if result.FixHint != "brew install jq" {
		t.Fatalf("FixHint = %q, want brew install jq", result.FixHint)
	}
}

func TestBinaryDependencyCheckNameFallbacks(t *testing.T) {
	cases := []struct {
		name string
		dep  binaryDependency
		want string
	}{
		{
			name: "uses dependency name when lookup missing",
			dep: binaryDependency{
				name:       "exec session provider script",
				lookupName: "",
			},
			want: "session-provider-exec-session-provider-script",
		},
		{
			name: "uses generic placeholder when both names are empty",
			dep: binaryDependency{
				name:       "   ",
				lookupName: "",
			},
			want: "session-provider-session-provider",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := newBinaryDependencyCheck(tc.dep, fakeLookPath())
			if got := check.Name(); got != tc.want {
				t.Fatalf("Name() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckHardDependenciesValidatesExecProviderRuntimeSmokeCheck(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")

	oldLookPath := initLookPath
	initLookPath = fakeLookPath()
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldSmokeCheck := runExecSessionProviderSmokeCheck
	runExecSessionProviderSmokeCheck = func(path string) error {
		if path == "/tmp/spy" {
			return errors.New("validate operation failed: exec format error")
		}
		return nil
	}
	t.Cleanup(func() { runExecSessionProviderSmokeCheck = oldSmokeCheck })

	cityPath := writeSessionProviderTestCity(t, "exec:/tmp/spy")
	missing := checkHardDependencies(cityPath)
	if !hasMissingDep(missing, "/tmp/spy (validate operation failed: exec format error)") {
		t.Fatalf("expected exec validation failure, missing=%v", missing)
	}
}

func TestRegisterCoreBinaryChecksValidatesExecProviderRuntimeSmokeCheck(t *testing.T) {
	oldSmokeCheck := runExecSessionProviderSmokeCheck
	runExecSessionProviderSmokeCheck = func(path string) error {
		if path == "/tmp/spy" {
			return errors.New("validate operation failed: bad interpreter")
		}
		return nil
	}
	t.Cleanup(func() { runExecSessionProviderSmokeCheck = oldSmokeCheck })

	d := &doctor.Doctor{}
	registerCoreBinaryChecks(d, "exec:/tmp/spy", fakeLookPath())

	var out bytes.Buffer
	report := d.Run(&doctor.CheckContext{CityPath: t.TempDir()}, &out, false)
	if report.Failed != 1 {
		t.Fatalf("failed checks = %d, want 1\noutput=%s", report.Failed, out.String())
	}
	if !strings.Contains(out.String(), `exec session provider "exec:/tmp/spy" is not runnable`) {
		t.Fatalf("expected exec runtime validation error in output\noutput=%s", out.String())
	}
	if !strings.Contains(out.String(), `bad interpreter`) {
		t.Fatalf("expected validation stderr in output\noutput=%s", out.String())
	}
}

func TestExecSessionProviderSmokeCheckTreatsExit2AsRunnable(t *testing.T) {
	script := filepath.Join(t.TempDir(), "provider.sh")
	content := "#!/bin/sh\nexit 2\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", script, err)
	}
	if err := execSessionProviderSmokeCheck(script); err != nil {
		t.Fatalf("execSessionProviderSmokeCheck(%s): %v", script, err)
	}
}

func TestExecSessionProviderSmokeCheckTreatsExit0AsRunnable(t *testing.T) {
	script := filepath.Join(t.TempDir(), "provider.sh")
	content := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", script, err)
	}
	if err := execSessionProviderSmokeCheck(script); err != nil {
		t.Fatalf("execSessionProviderSmokeCheck(%s): %v", script, err)
	}
}

func TestExecSessionProviderSmokeCheckReportsScriptFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "provider.sh")
	content := "#!/bin/sh\necho 'bad interpreter' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", script, err)
	}
	err := execSessionProviderSmokeCheck(script)
	if err == nil {
		t.Fatalf("execSessionProviderSmokeCheck(%s): expected error", script)
	}
	if !strings.Contains(err.Error(), "bad interpreter") {
		t.Fatalf("error = %q, want stderr text", err.Error())
	}
}

func TestExecSessionProviderSmokeCheckFallsBackToRunErrorWithoutStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "provider.sh")
	content := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", script, err)
	}

	err := execSessionProviderSmokeCheck(script)
	if err == nil {
		t.Fatalf("execSessionProviderSmokeCheck(%s): expected error", script)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %q, want fallback run error", err.Error())
	}
}

func TestBinaryDependencyCheckCanFixIsNoop(t *testing.T) {
	check := newBinaryDependencyCheck(binaryDependency{name: "jq", lookupName: "jq"}, fakeLookPath())
	if check.CanFix() {
		t.Fatal("CanFix() = true, want false")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix() = %v, want nil", err)
	}
}
