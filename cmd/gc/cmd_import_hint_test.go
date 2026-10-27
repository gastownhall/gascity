package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/packman"
)

func authErrorSeam(matched bool) error {
	return fmt.Errorf("head probe failed: %w", &gitcred.AuthError{
		Host:       "github.com",
		OrgPrefix:  "github.com/gascity",
		Repo:       "https://github.com/gascity/repo",
		Matched:    matched,
		RuleOrigin: "/city/.gc/credentials.toml",
		Output:     "terminal prompts disabled",
	})
}

func setupHintCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return city
}

func stubHeadCommitAuthError(t *testing.T, matched bool) {
	t.Helper()
	prevResolve := resolveImportVersion
	prevHead := resolveImportHeadCommit
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		resolveImportHeadCommit = prevHead
	})
	resolveImportVersion = func(_, _, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, packman.ErrNoSemverTags
	}
	resolveImportHeadCommit = func(_, _ string) (string, error) {
		return "", authErrorSeam(matched)
	}
}

func TestDoImportAddPrintsUnmatchedHint(t *testing.T) {
	city := setupHintCity(t)
	stubHeadCommitAuthError(t, false)
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if !strings.Contains(stderr.String(), "register a pack credential and retry") {
		t.Fatalf("unmatched hint missing: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc import credential add github.com/gascity") {
		t.Fatalf("hint missing the suggested command: %q", stderr.String())
	}
}

func TestDoImportAddPrintsMatchedHint(t *testing.T) {
	city := setupHintCity(t)
	stubHeadCommitAuthError(t, true)
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if !strings.Contains(stderr.String(), "matched but the remote rejected it") {
		t.Fatalf("matched hint missing: %q", stderr.String())
	}
}

func TestDoImportAddNoHintForNonAuthError(t *testing.T) {
	city := setupHintCity(t)
	prevResolve := resolveImportVersion
	prevHead := resolveImportHeadCommit
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		resolveImportHeadCommit = prevHead
	})
	resolveImportVersion = func(_, _, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, packman.ErrNoSemverTags
	}
	resolveImportHeadCommit = func(_, _ string) (string, error) {
		return "", fmt.Errorf("some network blip")
	}
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected failure")
	}
	if strings.Contains(stderr.String(), "hint:") {
		t.Fatalf("non-auth error must not print a hint: %q", stderr.String())
	}
}

const (
	wantUnmatchedCredentialHint = "hint: this source requires authentication; register a pack credential and retry:\n" +
		"  gc import credential add github.com/gascity --helper 'gh auth token'\n" +
		"  (--token-file, --token-env, and --ssh-key-file are the non-interactive alternatives; see gc import credential add --help)\n"
	wantMatchedCredentialHint = "hint: credential rule from /city/.gc/credentials.toml matched but the remote rejected it; check the token is valid and has contents:read on this repository (gc import credential list)\n"
)

// The CLI credential hint is pinned byte-for-byte: gc import add/install/
// upgrade and gc rig add all print it through printCredentialHint.
func TestPrintCredentialHintBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "unmatched", err: authErrorSeam(false), want: wantUnmatchedCredentialHint},
		{name: "matched", err: authErrorSeam(true), want: wantMatchedCredentialHint},
		{name: "not auth-classified", err: fmt.Errorf("some network blip"), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			printCredentialHint(&stderr, tc.err)
			if got := stderr.String(); got != tc.want {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDoImportInstallPrintsCredentialHintBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matched bool
		want    string
	}{
		{name: "unmatched", matched: false, want: wantUnmatchedCredentialHint},
		{name: "matched", matched: true, want: wantMatchedCredentialHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			city := setupHintCity(t)
			prevSync := syncImports
			t.Cleanup(func() { syncImports = prevSync })
			authErr := authErrorSeam(tc.matched)
			syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
				return nil, authErr
			}
			var stdout, stderr strings.Builder
			if rc := doImportInstall(city, &stdout, &stderr); rc == 0 {
				t.Fatalf("expected failure")
			}
			want := "gc import install: " + authErr.Error() + "\n" + tc.want
			if got := stderr.String(); got != want {
				t.Fatalf("stderr = %q, want %q", got, want)
			}
		})
	}
}

// wantDoctorCredentialFixHint is the FixHint gc doctor --fix reports for an
// auth-classified fix failure: the text gc import install prints after "hint: ".
func wantDoctorCredentialFixHint(matched bool) string {
	want := wantUnmatchedCredentialHint
	if matched {
		want = wantMatchedCredentialHint
	}
	return strings.TrimSuffix(strings.TrimPrefix(want, "hint: "), "\n")
}

// runDoctorFix runs check through the doctor runner with --fix and returns
// its reported result.
func runDoctorFix(t *testing.T, check doctor.Check, cityPath string) *doctor.CheckResult {
	t.Helper()
	d := &doctor.Doctor{}
	d.Register(check)
	var out strings.Builder
	r := d.Run(&doctor.CheckContext{CityPath: cityPath}, &out, true)
	if len(r.Results) != 1 {
		t.Fatalf("Results = %d, want 1; output:\n%s", len(r.Results), out.String())
	}
	return r.Results[0]
}
