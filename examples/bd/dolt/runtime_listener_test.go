package dolt_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// neverRunningPID names a process no host can have: it is past Linux's
// PID_MAX_LIMIT (2^22) and every other kernel's pid range, so pid_is_running
// rejects it however busy the host's process table is.
const neverRunningPID = "99999999"

// spoofedSocketLinks maps fd names to link texts that only contain the
// socket:[4242001] name of listenerProbeTCPFixture's 3307 listener: one as
// a suffix, one after an embedded newline that fakes an `ls -l` line. The
// listener probe matches the kernel-rendered link text whole, so neither
// makes its process a holder.
var spoofedSocketLinks = map[string]string{
	"8": "/tmp/a\nlrwx------ 1 0 0 64 Oct 26 12:00 9 -> socket:[4242001]",
	"9": "/tmp/x 9 -> socket:[4242001]",
}

// writeSpoofedSocketLinks adds spoofedSocketLinks to pid's fd directory in
// a writeProcFixture root.
func writeSpoofedSocketLinks(t *testing.T, root, pid string) {
	t.Helper()
	dir := filepath.Join(root, pid, "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for fd, target := range spoofedSocketLinks {
		if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
			t.Fatalf("symlink %s/fd/%s: %v", pid, fd, err)
		}
	}
}

// TestManagedRuntimeListenerPIDPrefersRunningCandidate pins the candidate
// rule managed_runtime_port and the health command rely on: of the
// processes holding the 3307 LISTEN socket in a kernel-table fixture, the
// first running CANDIDATE answers, and only when no candidate is a running
// holder does the lookup fall back to the first running holder. PID 1 and
// this test process are the running holders; lsof is a tripwire that must
// never run while the tables are readable.
func TestManagedRuntimeListenerPIDPrefersRunningCandidate(t *testing.T) {
	self := strconv.Itoa(os.Getpid())

	tests := []struct {
		name       string
		holders    map[string]string // "<pid>/fd/<n>" -> socket inode, as writeProcFixture takes
		spoofers   []string          // pids whose fd links only spoof the listener's socket name
		candidates []string
		want       string
	}{
		{
			name:    "without candidates the lowest running holder answers",
			holders: map[string]string{"1/fd/3": "4242001", self + "/fd/3": "4242001"},
			want:    "1",
		},
		{
			name:       "candidate holding the socket wins over a lower holder",
			holders:    map[string]string{"1/fd/3": "4242001", self + "/fd/3": "4242001"},
			candidates: []string{self},
			want:       self,
		},
		{
			name:       "candidate holding nothing falls back to the lowest running holder",
			holders:    map[string]string{"1/fd/3": "4242001"},
			candidates: []string{self},
			want:       "1",
		},
		{
			name:       "candidate holding the socket but not running falls back to the walk",
			holders:    map[string]string{"1/fd/3": "4242001", neverRunningPID + "/fd/3": "4242001"},
			candidates: []string{neverRunningPID},
			want:       "1",
		},
		{
			name: "a candidate that is not running yields to the next one",
			holders: map[string]string{
				"1/fd/3":                  "4242001",
				neverRunningPID + "/fd/3": "4242001",
				self + "/fd/3":            "4242001",
			},
			candidates: []string{neverRunningPID, self},
			want:       self,
		},
		{
			name:       "spoofed candidate is not a holder",
			holders:    map[string]string{"1/fd/3": "4242001"},
			spoofers:   []string{self},
			candidates: []string{self},
			want:       "1",
		},
		{
			name:     "spoofed link text never makes a lower pid the holder",
			holders:  map[string]string{self + "/fd/3": "4242001"},
			spoofers: []string{"1"},
			want:     self,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeProcFixture(t, listenerProbeTCPFixture, "", tt.holders)
			for _, pid := range tt.spoofers {
				writeSpoofedSocketLinks(t, root, pid)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "lsof-ran")
			writeLsofRecordingShim(t, bin)

			code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
				strings.Join(append([]string{"managed_runtime_listener_pid", "3307"}, tt.candidates...), " "),
				"GC_DOLT_PORT=4406",
				"GC_DOLT_PROC_ROOT="+root,
				"LSOF_MARKER="+marker,
			)
			if code != 0 {
				t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
			}
			if got := strings.TrimSpace(out); got != tt.want {
				t.Fatalf("managed_runtime_listener_pid 3307 %s = %q, want %q\n%s", strings.Join(tt.candidates, " "), got, tt.want, out)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("lsof ran; with readable kernel socket tables the candidate lookup must answer without it (gascity#7553)\n%s", out)
			}
		})
	}
}

// TestManagedRuntimeListenerPIDPrefersCandidateFromLsof holds the same
// candidate rule where no kernel socket table is readable: the holders come
// from lsof, in lsof's order, and a running candidate among them answers
// ahead of the first one lsof lists.
func TestManagedRuntimeListenerPIDPrefersCandidateFromLsof(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "lsof"), "#!/bin/sh\nprintf '1\\n"+self+"\\n'\n")

	for _, tt := range []struct {
		candidate string
		want      string
	}{
		{candidate: "", want: "1"},
		{candidate: self, want: self},
	} {
		code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
			strings.TrimSpace("managed_runtime_listener_pid 4407 "+tt.candidate),
			"GC_DOLT_PORT=4406",
			"GC_DOLT_PROC_ROOT="+t.TempDir(),
		)
		if code != 0 {
			t.Fatalf("candidate %q: managed_runtime_listener_pid exited %d\n%s", tt.candidate, code, out)
		}
		if got := strings.TrimSpace(out); got != tt.want {
			t.Fatalf("candidate %q: managed_runtime_listener_pid = %q, want %q\n%s", tt.candidate, got, tt.want, out)
		}
	}
}

// TestRuntimeScriptPortResolutionAcceptsStatePIDSharingItsListener covers
// the candidate managed_runtime_port passes: managed state names this
// process, which holds the 3307 listener together with the lower PID 1 (an
// inherited or reuse-port descriptor). The state pid holds the socket, so
// sourcing runtime.sh resolves GC_DOLT_PORT to the state port instead of
// rejecting it as a foreign holder's, and lsof never runs.
func TestRuntimeScriptPortResolutionAcceptsStatePIDSharingItsListener(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	root := writeProcFixture(t, listenerProbeTCPFixture, "", map[string]string{
		"1/fd/3":       "4242001",
		self + "/fd/7": "4242001",
	})

	cityPath := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)
	writeManagedRuntimeStateForScript(t, cityPath, 3307)

	code, out := runRuntimeScriptShell(t, cityPath, bin,
		`printf '%s\n' "$GC_DOLT_PORT"`,
		"GC_DOLT_PROC_ROOT="+root,
		"LSOF_MARKER="+marker,
	)
	if code != 0 {
		t.Fatalf("runtime.sh exited %d; the state pid holds the listener, so its port must resolve\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3307\n%s", got, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran during port resolution; the kernel socket tables already answer (gascity#7553)\n%s", out)
	}
}
