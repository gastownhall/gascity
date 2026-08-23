package gchome

import (
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveHomeReportsBranchProvenanceWithoutPathGuessing(t *testing.T) {
	errNoHome := errors.New("no user home")
	errNoTemp := errors.New("temp unavailable")
	tests := []struct {
		name          string
		gcHome        string
		userHome      string
		userHomeErr   error
		temporaryHome string
		temporaryErr  error
		wantPath      string
		wantSource    Provenance
		wantUserCalls int
		wantTempCalls int
	}{
		{
			name:          "explicit GC_HOME that resembles temp remains explicit",
			gcHome:        "  /tmp/gc-home-explicit  ",
			wantPath:      "/tmp/gc-home-explicit",
			wantSource:    ProvenanceExplicit,
			wantUserCalls: 0,
			wantTempCalls: 0,
		},
		{
			name:          "user home",
			userHome:      "/home/alice",
			wantPath:      "/home/alice/.gc",
			wantSource:    ProvenanceUserHome,
			wantUserCalls: 1,
			wantTempCalls: 0,
		},
		{
			name:          "MkdirTemp fallback",
			userHomeErr:   errNoHome,
			temporaryHome: "/tmp/gc-home-random",
			wantPath:      "/tmp/gc-home-random",
			wantSource:    ProvenanceMkdirTemp,
			wantUserCalls: 1,
			wantTempCalls: 1,
		},
		{
			name:          "last resort fallback",
			userHomeErr:   errNoHome,
			temporaryErr:  errNoTemp,
			wantPath:      "/var/tmp/gc-home-4242",
			wantSource:    ProvenanceLastResort,
			wantUserCalls: 1,
			wantTempCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			userCalls, tempCalls := 0, 0
			deps := homeResolverDeps{
				getenv: func(name string) string {
					if name != "GC_HOME" {
						t.Fatalf("getenv(%q), want GC_HOME", name)
					}
					return test.gcHome
				},
				userHomeDir: func() (string, error) {
					userCalls++
					return test.userHome, test.userHomeErr
				},
				mkdirTemp: func(dir, pattern string) (string, error) {
					tempCalls++
					if dir != "" || pattern != "gc-home-*" {
						t.Fatalf("MkdirTemp(%q, %q)", dir, pattern)
					}
					return test.temporaryHome, test.temporaryErr
				},
				tempDir: func() string { return "/var/tmp" },
				pid:     func() int { return 4242 },
			}
			got := resolveHome(deps, true)
			if got.Path() != test.wantPath || got.Provenance() != test.wantSource {
				t.Fatalf("resolveHome() = (%q, %v), want (%q, %v)", got.Path(), got.Provenance(), test.wantPath, test.wantSource)
			}
			if userCalls != test.wantUserCalls || tempCalls != test.wantTempCalls {
				t.Fatalf("calls = user:%d temp:%d, want user:%d temp:%d", userCalls, tempCalls, test.wantUserCalls, test.wantTempCalls)
			}
		})
	}
}

func TestResolveHomeReadOnlyNeverCreatesFallback(t *testing.T) {
	tempCalls := 0
	deps := homeResolverDeps{
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return "", errors.New("no home") },
		mkdirTemp: func(string, string) (string, error) {
			tempCalls++
			return "/tmp/created", nil
		},
		tempDir: func() string { return "/tmp" },
		pid:     func() int { return 99 },
	}
	got := resolveHome(deps, false)
	if tempCalls != 0 {
		t.Fatalf("read-only resolve called MkdirTemp %d times", tempCalls)
	}
	if got.Path() != "/tmp/gc-home-99" || got.Provenance() != ProvenanceLastResort {
		t.Fatalf("read-only resolve = (%q, %v), want deterministic last-resort candidate", got.Path(), got.Provenance())
	}
}

func TestProvenanceStabilityIsClosed(t *testing.T) {
	for source, want := range map[Provenance]bool{
		Provenance(0):        false,
		ProvenanceExplicit:   true,
		ProvenanceUserHome:   true,
		ProvenanceMkdirTemp:  false,
		ProvenanceLastResort: false,
		Provenance(255):      false,
	} {
		if got := source.Stable(); got != want {
			t.Errorf("%v.Stable() = %v, want %v", source, got, want)
		}
	}
}

func TestProvenanceStringValuesAreStable(t *testing.T) {
	tests := []struct {
		name       string
		provenance Provenance
		want       string
	}{
		{name: "zero value is unknown", provenance: Provenance(0), want: "unknown"},
		{name: "explicit GC_HOME", provenance: ProvenanceExplicit, want: "explicit-gc-home"},
		{name: "user home", provenance: ProvenanceUserHome, want: "user-home"},
		{name: "temporary fallback", provenance: ProvenanceMkdirTemp, want: "temporary-fallback"},
		{name: "last-resort fallback", provenance: ProvenanceLastResort, want: "last-resort-fallback"},
		{name: "unrecognized value is unknown", provenance: Provenance(255), want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.provenance.String(); got != test.want {
				t.Fatalf("Provenance(%d).String() = %q, want %q", test.provenance, got, test.want)
			}
		})
	}
}

func TestPublicResolversUseExplicitGCHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "explicit-gc-home")
	t.Setenv("GC_HOME", "  "+dir+"  ")

	resolvers := []struct {
		name    string
		resolve func() ResolvedHome
	}{
		{name: "default", resolve: ResolveDefault},
		{name: "read-only", resolve: ResolveReadOnly},
	}
	for _, resolver := range resolvers {
		t.Run(resolver.name, func(t *testing.T) {
			got := resolver.resolve()
			if got.Path() != dir || got.Provenance() != ProvenanceExplicit {
				t.Fatalf("resolver = (%q, %v), want (%q, %v)", got.Path(), got.Provenance(), dir, ProvenanceExplicit)
			}
		})
	}
}

func TestResolveReadOnlyPublicNeverCreatesTemporaryFallback(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("controlled os.TempDir and os.UserHomeDir environment applies to Unix")
	}
	tempRoot := t.TempDir()
	t.Setenv("GC_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("TMPDIR", tempRoot)

	got := ResolveReadOnly()
	if got.Provenance() != ProvenanceLastResort {
		t.Fatalf("ResolveReadOnly provenance = %v, want %v", got.Provenance(), ProvenanceLastResort)
	}
	if filepath.Dir(got.Path()) != tempRoot {
		t.Fatalf("ResolveReadOnly path = %q, want last-resort candidate below %q", got.Path(), tempRoot)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", tempRoot, err)
	}
	if len(entries) != 0 {
		t.Fatalf("ResolveReadOnly created %d entries below %q, want none", len(entries), tempRoot)
	}
}

func TestProductUsageTrustImplementationBuildSelection(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tests := []struct {
		goos          string
		wantSupported bool
	}{
		{goos: "linux", wantSupported: true},
		{goos: "darwin", wantSupported: true},
		{goos: "android", wantSupported: false},
		{goos: "ios", wantSupported: false},
		{goos: "windows", wantSupported: false},
	}
	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			context := build.Default
			context.GOOS = test.goos
			for _, name := range []string{"trust_unix.go", "trust_unix_test.go"} {
				matched, err := context.MatchFile(dir, name)
				if err != nil {
					t.Fatalf("MatchFile(%q): %v", name, err)
				}
				if matched != test.wantSupported {
					t.Errorf("%s selection on %s = %v, want %v", name, test.goos, matched, test.wantSupported)
				}
			}
			matchedUnsupported, err := context.MatchFile(dir, "trust_unsupported.go")
			if err != nil {
				t.Fatalf("MatchFile(%q): %v", "trust_unsupported.go", err)
			}
			if matchedUnsupported == test.wantSupported {
				t.Errorf("trust_unsupported.go selection on %s = %v, want %v", test.goos, matchedUnsupported, !test.wantSupported)
			}
		})
	}
}

func TestInspectProductUsageHomeRejectsUnstableRelativeAndUncleanPaths(t *testing.T) {
	for name, home := range map[string]ResolvedHome{
		"unknown provenance": {path: "/safe/home", provenance: Provenance(0)},
		"temporary fallback": {path: "/safe/home", provenance: ProvenanceMkdirTemp},
		"last resort":        {path: "/safe/home", provenance: ProvenanceLastResort},
		"relative explicit":  {path: "relative/home", provenance: ProvenanceExplicit},
		"unclean explicit":   {path: "/safe/../home", provenance: ProvenanceExplicit},
		"trailing separator": {path: "/safe/home/", provenance: ProvenanceExplicit},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectProductUsageHome(home); err == nil {
				t.Fatal("InspectProductUsageHome unexpectedly accepted invalid home")
			}
		})
	}
}

func TestDefaultUsesGCHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	if got := Default(); got != dir {
		t.Fatalf("Default() = %q, want %q", got, dir)
	}
}

// TestDefaultAvoidsSharedTempFallback guards #3506: when GC_HOME is unset and
// the user home cannot be resolved, Default() must not hand back the shared
// os.TempDir()/.gc path. That path is world-writable and shared across every
// process and user on the host, so concurrent processes clobber each other's
// state and unrelated city scans pick it up as a real city.
func TestDefaultAvoidsSharedTempFallback(t *testing.T) {
	t.Setenv("GC_HOME", "")
	t.Setenv("HOME", "") // forces os.UserHomeDir() to fail on unix

	got := Default()

	if shared := filepath.Join(os.TempDir(), ".gc"); got == shared {
		t.Fatalf("Default() = %q, want a process-isolated path, not the shared %q", got, shared)
	}
	// Must never be empty: callers join the result into a path (e.g.
	// filepath.Join(home, "registries.toml")), so "" silently becomes a
	// CWD-relative path and writes state to the wrong place instead of failing.
	if got == "" {
		t.Fatal("Default() returned an empty path; callers would write state to a CWD-relative path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Default() = %q, want an absolute process-isolated path", got)
	}
}

func TestRegistryPathsUseHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "gc")

	if got, want := RegistriesPath(home), filepath.Join(home, "registries.toml"); got != want {
		t.Fatalf("RegistriesPath() = %q, want %q", got, want)
	}
	if got, want := RegistryCacheRoot(home), filepath.Join(home, "registry-cache"); got != want {
		t.Fatalf("RegistryCacheRoot() = %q, want %q", got, want)
	}
}
