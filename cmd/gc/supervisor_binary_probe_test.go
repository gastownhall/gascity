package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// stubSupervisorExeProbes saves the process-table seams readSupervisorExeState
// reads, restores them when t ends, and pins the platform to goos. The /proc
// and ps(1) readers fail until a test replaces them.
func stubSupervisorExeProbes(t *testing.T, goos string) {
	t.Helper()
	oldLink := readProcExeLinkHook
	oldComm := readProcessCommViaPSHook
	oldService := supervisorServiceBinaryHook
	oldGOOS := supervisorRuntimeGOOS
	t.Cleanup(func() {
		readProcExeLinkHook = oldLink
		readProcessCommViaPSHook = oldComm
		supervisorServiceBinaryHook = oldService
		supervisorRuntimeGOOS = oldGOOS
	})
	supervisorRuntimeGOOS = goos
	readProcExeLinkHook = func(int) (string, error) {
		return "", &fs.PathError{Op: "readlink", Path: "/proc/exe", Err: fs.ErrNotExist}
	}
	readProcessCommViaPSHook = func(int) (string, error) { return "", errors.New("ps not stubbed") }
	supervisorServiceBinaryHook = func() (string, string) { return "", "" }
}

// TestReadSupervisorExeState pins the decision table behind the
// supervisor-binary check's "executable removed or replaced" warning. Linux
// trusts the kernel's " (deleted)" mark. On macOS a ps(1) comm value is
// reported as deleted only when it is the managing service's launch path; a
// stale, truncated, relative, or non-file comm is an unreadable path, as
// verifiedPSExecutablePath treats it everywhere else.
func TestReadSupervisorExeState(t *testing.T) {
	present := writeFakeGCBinary(t, "present")
	gone := filepath.Join(t.TempDir(), "Cellar", "gc", "1.4.2", "bin", "gc")
	errPS := errors.New("ps -p 4242: exit status 1")

	cases := []struct {
		name          string
		goos          string
		link          string
		linkErr       error
		comm          string
		commErr       error
		serviceBinary string
		wantPath      string
		wantDeleted   bool
		wantErr       bool
		wantErrIs     error
	}{
		{
			name:     "linux running executable",
			goos:     "linux",
			link:     "/usr/local/bin/gc",
			wantPath: "/usr/local/bin/gc",
		},
		{
			name:        "linux executable unlinked or replaced",
			goos:        "linux",
			link:        "/usr/local/bin/gc (deleted)",
			wantPath:    "/usr/local/bin/gc",
			wantDeleted: true,
		},
		{
			name:      "linux unreadable /proc does not fall back to ps",
			goos:      "linux",
			linkErr:   &fs.PathError{Op: "readlink", Path: "/proc/4242/exe", Err: fs.ErrPermission},
			comm:      present,
			wantErr:   true,
			wantErrIs: fs.ErrPermission,
		},
		{
			name:     "darwin ps names an existing regular file",
			goos:     "darwin",
			comm:     present,
			wantPath: present,
		},
		{
			name:          "darwin service launch path removed",
			goos:          "darwin",
			comm:          gone,
			serviceBinary: gone,
			wantPath:      gone,
			wantDeleted:   true,
		},
		{
			name:          "darwin truncated comm is unreadable, not deleted",
			goos:          "darwin",
			comm:          present[:len(present)-1],
			serviceBinary: present,
			wantErr:       true,
			wantErrIs:     fs.ErrNotExist,
		},
		{
			name:      "darwin missing path without a service is unreadable",
			goos:      "darwin",
			comm:      gone,
			wantErr:   true,
			wantErrIs: fs.ErrNotExist,
		},
		{
			name:          "darwin relative comm is unreadable even if it matches the service",
			goos:          "darwin",
			comm:          "gc",
			serviceBinary: "gc",
			wantErr:       true,
		},
		{
			name:          "darwin directory is unreadable even if it matches the service",
			goos:          "darwin",
			comm:          filepath.Dir(present),
			serviceBinary: filepath.Dir(present),
			wantErr:       true,
		},
		{
			name:      "darwin ps failure",
			goos:      "darwin",
			commErr:   errPS,
			wantErr:   true,
			wantErrIs: errPS,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubSupervisorExeProbes(t, tc.goos)
			if tc.link != "" || tc.linkErr != nil {
				readProcExeLinkHook = func(pid int) (string, error) {
					if pid != 4242 {
						t.Errorf("readlink pid = %d, want 4242", pid)
					}
					return tc.link, tc.linkErr
				}
			}
			psCalled := false
			readProcessCommViaPSHook = func(pid int) (string, error) {
				psCalled = true
				if pid != 4242 {
					t.Errorf("ps pid = %d, want 4242", pid)
				}
				return tc.comm, tc.commErr
			}
			supervisorServiceBinaryHook = func() (string, string) {
				if tc.serviceBinary == "" {
					return "", ""
				}
				return tc.serviceBinary, `launchd service "com.gascity.supervisor"`
			}

			path, deleted, err := readSupervisorExeState(4242)
			if path != tc.wantPath || deleted != tc.wantDeleted {
				t.Errorf("readSupervisorExeState = (%q, deleted=%v), want (%q, deleted=%v)", path, deleted, tc.wantPath, tc.wantDeleted)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantErrIs)
			}
			if tc.goos != "darwin" && psCalled {
				t.Error("ps(1) was consulted off macOS")
			}
		})
	}
}

// TestReadSupervisorExePathStripsDeletedSuffix keeps gc start's auto-restart
// contract: it relaunches the file now at the path, so the path comes back
// without the kernel's " (deleted)" mark.
func TestReadSupervisorExePathStripsDeletedSuffix(t *testing.T) {
	stubSupervisorExeProbes(t, "linux")
	readProcExeLinkHook = func(int) (string, error) { return "/usr/local/bin/gc (deleted)", nil }
	if got, err := readSupervisorExePath(4242); err != nil || got != "/usr/local/bin/gc" {
		t.Fatalf("readSupervisorExePath = (%q, %v), want (%q, nil)", got, err, "/usr/local/bin/gc")
	}
}

// TestSupervisorBuildDiffers pins the doctor's drift verdict: sameGCBuildID
// for known builds, as in the mismatch gate's classify, with the normalized
// release version deciding only when a build ID is unknown.
func TestSupervisorBuildDiffers(t *testing.T) {
	cases := []struct {
		name  string
		local gcBinaryIdentity
		sv    SupervisorStatus
		want  bool
	}{
		{
			name:  "same build",
			local: gcBinaryIdentity{BuildID: "abc1234def0", Version: "1.5.0"},
			sv:    SupervisorStatus{BuildID: "abc1234def0", Version: "1.5.0"},
		},
		{
			name:  "abbreviated and full hash of one commit",
			local: gcBinaryIdentity{BuildID: "abc1234"},
			sv:    SupervisorStatus{BuildID: "abc1234def0"},
		},
		{
			name:  "case-only difference",
			local: gcBinaryIdentity{BuildID: "ABC1234DEF0"},
			sv:    SupervisorStatus{BuildID: "abc1234def0"},
		},
		{
			name:  "surrounding whitespace",
			local: gcBinaryIdentity{BuildID: " abc1234 "},
			sv:    SupervisorStatus{BuildID: "abc1234def0\n"},
		},
		{
			name:  "matching dirty suffix",
			local: gcBinaryIdentity{BuildID: "abc1234-dirty"},
			sv:    SupervisorStatus{BuildID: "abc1234def0-dirty"},
		},
		{
			name:  "dirty against clean",
			local: gcBinaryIdentity{BuildID: "abc1234-dirty"},
			sv:    SupervisorStatus{BuildID: "abc1234def0"},
			want:  true,
		},
		{
			name:  "different commits",
			local: gcBinaryIdentity{BuildID: "abc1234", Version: "1.5.0"},
			sv:    SupervisorStatus{BuildID: "def5678", Version: "1.5.0"},
			want:  true,
		},
		{
			name:  "known builds decide over versions",
			local: gcBinaryIdentity{BuildID: "abc1234", Version: "1.5.1"},
			sv:    SupervisorStatus{BuildID: "abc1234def0", Version: "1.5.0"},
		},
		{
			name:  "unknown builds with different versions",
			local: gcBinaryIdentity{BuildID: "unknown", Version: "1.5.1"},
			sv:    SupervisorStatus{BuildID: "unknown", Version: "1.5.0"},
			want:  true,
		},
		{
			name:  "unknown builds with the same version",
			local: gcBinaryIdentity{BuildID: "unknown", Version: "v1.5.0"},
			sv:    SupervisorStatus{BuildID: "unknown", Version: "1.5.0"},
		},
		{
			name:  "one unknown build with different versions",
			local: gcBinaryIdentity{BuildID: "unknown", Version: "1.5.1"},
			sv:    SupervisorStatus{BuildID: "abc1234", Version: "1.5.0"},
			want:  true,
		},
		{
			name:  "one unknown build without versions",
			local: gcBinaryIdentity{BuildID: "unknown", Version: "dev"},
			sv:    SupervisorStatus{BuildID: "abc1234"},
		},
		{
			name:  "nothing known",
			local: gcBinaryIdentity{BuildID: "unknown", Version: "dev"},
			sv:    SupervisorStatus{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorBuildDiffers(tc.local, tc.sv); got != tc.want {
				t.Fatalf("supervisorBuildDiffers(%+v, %+v) = %v, want %v", tc.local, tc.sv, got, tc.want)
			}
		})
	}
}

// TestGatherSupervisorBinary wires the process-table and /health probes into
// the doctor.SupervisorBinary the supervisor-binary check consumes. The drift
// verdict is decided here; unknown identities reach the check as "".
func TestGatherSupervisorBinary(t *testing.T) {
	const apiURL = "http://supervisor.test"
	errHealth := errors.New("connection refused")
	errURL := errors.New("no supervisor config")

	cases := []struct {
		name          string
		localBuildID  string
		localVersion  string
		link          string
		linkErr       error
		baseURLErr    error
		health        SupervisorStatus
		healthErr     error
		want          doctor.SupervisorBinary
		wantExeErr    error
		wantStatusErr error
	}{
		{
			name:         "same commit from a replaced executable",
			localBuildID: "abc1234def0", localVersion: "1.5.0",
			link:   "/usr/local/bin/gc (deleted)",
			health: SupervisorStatus{BuildID: "ABC1234", Version: "v1.5.0"},
			want: doctor.SupervisorBinary{
				ExePath: "/usr/local/bin/gc", ExeDeleted: true,
				BuildID: "ABC1234", Version: "v1.5.0",
				LocalBuildID: "abc1234def0", LocalVersion: "1.5.0",
			},
		},
		{
			name:         "different commit",
			localBuildID: "abc1234def0", localVersion: "1.5.0",
			link:   "/usr/local/bin/gc",
			health: SupervisorStatus{BuildID: "def5678", Version: "1.5.0"},
			want: doctor.SupervisorBinary{
				ExePath: "/usr/local/bin/gc",
				BuildID: "def5678", Version: "1.5.0",
				LocalBuildID: "abc1234def0", LocalVersion: "1.5.0",
				BuildDiffers: true,
			},
		},
		{
			name:         "unknown builds fall back to release versions",
			localBuildID: "unknown", localVersion: "1.5.1",
			link:   "/opt/homebrew/Cellar/gc/1.5.1/bin/gc",
			health: SupervisorStatus{BuildID: "unknown", Version: "1.5.0"},
			want: doctor.SupervisorBinary{
				ExePath: "/opt/homebrew/Cellar/gc/1.5.1/bin/gc",
				Version: "1.5.0", LocalVersion: "1.5.1",
				BuildDiffers: true,
			},
		},
		{
			name:         "probe failures are carried, not decided",
			localBuildID: "abc1234def0", localVersion: "dev",
			linkErr:       &fs.PathError{Op: "readlink", Path: "/proc/4242/exe", Err: fs.ErrPermission},
			healthErr:     errHealth,
			want:          doctor.SupervisorBinary{LocalBuildID: "abc1234def0"},
			wantExeErr:    fs.ErrPermission,
			wantStatusErr: errHealth,
		},
		{
			name:         "unresolvable API URL",
			localBuildID: "abc1234def0", localVersion: "1.5.0",
			link:          "/usr/local/bin/gc",
			baseURLErr:    errURL,
			want:          doctor.SupervisorBinary{ExePath: "/usr/local/bin/gc", LocalBuildID: "abc1234def0", LocalVersion: "1.5.0"},
			wantStatusErr: errURL,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubSupervisorExeProbes(t, "linux")
			t.Setenv(supervisorSystemdUnitEnv, "")
			oldBaseURL := supervisorAPIBaseURLHook
			oldHealth := supervisorHealthStatusHook
			oldVersion, oldCommit := version, commit
			t.Cleanup(func() {
				supervisorAPIBaseURLHook = oldBaseURL
				supervisorHealthStatusHook = oldHealth
				version, commit = oldVersion, oldCommit
			})
			version, commit = tc.localVersion, tc.localBuildID
			readProcExeLinkHook = func(pid int) (string, error) {
				if pid != 4242 {
					t.Errorf("readlink pid = %d, want 4242", pid)
				}
				return tc.link, tc.linkErr
			}
			supervisorAPIBaseURLHook = func() (string, error) { return apiURL, tc.baseURLErr }
			healthCalled := false
			supervisorHealthStatusHook = func(ctx context.Context, baseURL string) (SupervisorStatus, error) {
				healthCalled = true
				if baseURL != apiURL {
					t.Errorf("/health base URL = %q, want %q", baseURL, apiURL)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("/health read has no deadline")
				}
				return tc.health, tc.healthErr
			}

			info := gatherSupervisorBinary(4242)
			if !errors.Is(info.ExeErr, tc.wantExeErr) {
				t.Errorf("ExeErr = %v, want %v", info.ExeErr, tc.wantExeErr)
			}
			if !errors.Is(info.StatusErr, tc.wantStatusErr) {
				t.Errorf("StatusErr = %v, want %v", info.StatusErr, tc.wantStatusErr)
			}
			if tc.baseURLErr != nil && healthCalled {
				t.Error("/health was read without a resolved API URL")
			}
			// TestGatherSupervisorBinaryFixHint covers the remediation.
			info.ExeErr, info.StatusErr, info.FixHint = nil, nil, ""
			if info != tc.want {
				t.Errorf("gatherSupervisorBinary =\n  %+v\nwant\n  %+v", info, tc.want)
			}
		})
	}
}

// TestGatherSupervisorBinaryFixHint pins the supervisor-binary check's
// remediation to the deployment it finds, and runs each hint through the
// install guard the hint leads to: it names 'supervisor install --force'
// exactly when a plain install would refuse to replace the service file.
func TestGatherSupervisorBinaryFixHint(t *testing.T) {
	const restart = "restart the supervisor so it runs this binary: '{SELF} supervisor stop --wait', then '{SELF} start'"
	cases := []struct {
		name string
		// exe and service are install-guard sentinels (CURRENT, OTHER,
		// MISSING) for the file the supervisor runs and the binary its
		// managing service launches; service "" means no managing service.
		exe, service string
		// deleted marks the running executable " (deleted)".
		deleted bool
		// sameBuild makes the supervisor serve this gc's build; otherwise
		// it serves an older one.
		sameBuild   bool
		unit, scope string
		// want and reject are hint substrings; {OTHER} and {MISSING} stand
		// for those sentinels' paths, and {SELF} for this gc's.
		want, reject []string
		// wantServiceBinary is the sentinel ServiceBinary must name, or "".
		wantServiceBinary string
	}{
		{
			name: "service launches another gc that still exists",
			exe:  "OTHER", service: "OTHER",
			want:   []string{"supervisor install --force", "or keep the running supervisor and use its gc: {OTHER}"},
			reject: []string{restart},
		},
		{
			name: "service launches this gc after an in-place upgrade",
			exe:  "CURRENT", service: "CURRENT", deleted: true,
			want:   []string{restart},
			reject: []string{"--force", "no longer exists"},
		},
		{
			// No build drift makes gc start restart this supervisor, so
			// only the stop's wait keeps gc start from accepting it while
			// it exits.
			name: "service launches this gc after a same-build reinstall",
			exe:  "CURRENT", service: "CURRENT", deleted: true, sameBuild: true,
			want:   []string{restart},
			reject: []string{"--force", "no longer exists"},
		},
		{
			name: "service gc was removed",
			exe:  "MISSING", service: "MISSING", deleted: true,
			want:   []string{"no longer exists", "'{SELF} supervisor stop --wait', then '{SELF} start'"},
			reject: []string{restart, "--force", "use its gc", "{MISSING}"},
		},
		{
			name: "service already launches this gc",
			exe:  "OTHER", service: "CURRENT",
			want:              []string{restart},
			reject:            []string{"--force"},
			wantServiceBinary: "CURRENT",
		},
		{
			name:   "unmanaged supervisor runs another gc",
			exe:    "OTHER",
			want:   []string{"stop the other supervisor", "use its gc: {OTHER}"},
			reject: []string{"--force"},
		},
		{
			name: "delegated unit",
			exe:  "OTHER", unit: "gascity.service",
			want:   []string{"point systemd unit gascity.service at this gc binary", "'systemctl restart gascity.service'"},
			reject: []string{"gc supervisor install", "gc supervisor stop"},
		},
		{
			name: "invalid delegation scope",
			exe:  "OTHER", unit: "gascity.service", scope: "bogus",
			want:   []string{supervisorSystemdScopeEnv, "bogus"},
			reject: []string{"gc supervisor", "systemctl"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			current := writeFakeGCBinary(t, "current")
			path := func(sentinel string) string {
				if sentinel == "" {
					return ""
				}
				return supervisorInstallGuardExistingBinary(t, homeDir, current, sentinel)
			}
			exe, serviceBinary := path(tc.exe), path(tc.service)
			service := ""
			if serviceBinary != "" {
				service = `systemd user unit "gascity-supervisor.service"`
			}
			supervisorVersion, supervisorBuildID := "1.5.0", "def5678"
			if tc.sameBuild {
				supervisorVersion, supervisorBuildID = "1.6.0", "abc1234def0"
			}
			stubSupervisorExeProbes(t, "linux")
			stubSupervisorBinary(t, supervisorBinaryFixture{
				supervisorVersion: supervisorVersion, supervisorBuildID: supervisorBuildID,
				serviceBinary: serviceBinary, service: service,
				localExe: current, localVersion: "1.6.0", localBuildID: "abc1234def0",
			})
			t.Setenv(supervisorSystemdUnitEnv, tc.unit)
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
			link := exe
			if tc.deleted {
				link += " (deleted)"
			}
			readProcExeLinkHook = func(int) (string, error) { return link, nil }

			info := gatherSupervisorBinary(4242)
			if info.BuildDiffers == tc.sameBuild || info.ExePath != exe {
				t.Fatalf("gatherSupervisorBinary = %+v, want BuildDiffers=%v from %q", info, !tc.sameBuild, exe)
			}
			placeholders := strings.NewReplacer("{OTHER}", path("OTHER"), "{MISSING}", path("MISSING"),
				"{SELF}", shellQuotePath(resolveExecutablePath(current)))
			for _, want := range tc.want {
				if want = placeholders.Replace(want); !strings.Contains(info.FixHint, want) {
					t.Errorf("FixHint = %q, want it to contain %q", info.FixHint, want)
				}
			}
			for _, reject := range tc.reject {
				if reject = placeholders.Replace(reject); strings.Contains(info.FixHint, reject) {
					t.Errorf("FixHint = %q, want it not to contain %q", info.FixHint, reject)
				}
			}
			refused := supervisorInstallBinaryGuard(serviceBinary, current) == supervisorInstallBinaryDifferent
			if forced := strings.Contains(info.FixHint, "supervisor install --force"); forced != refused {
				t.Errorf("FixHint = %q names install --force = %v, but a plain install over %q refuses = %v", info.FixHint, forced, serviceBinary, refused)
			}
			if want := path(tc.wantServiceBinary); info.ServiceBinary != want {
				t.Errorf("ServiceBinary = %q, want %q", info.ServiceBinary, want)
			}
		})
	}
}
