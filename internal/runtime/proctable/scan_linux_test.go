//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestProcessIdentityDistinguishesGoneFromUnreadable(t *testing.T) {
	for _, test := range []struct {
		name    string
		readErr error
	}{
		{name: "missing proc entry", readErr: fs.ErrNotExist},
		{name: "wrapped missing proc entry", readErr: fmt.Errorf("reading stat: %w", &os.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.ENOENT})},
		{name: "process vanishes during read", readErr: syscall.ESRCH},
		{name: "wrapped process vanishes during read", readErr: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.ESRCH}},
		{name: "outer-wrapped process vanishes during read", readErr: fmt.Errorf("reading stat: %w", &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.ESRCH})},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := processIdentityWithReader(42, func(string) ([]byte, error) { return nil, test.readErr })
			if !errors.Is(err, ErrProcessGone) {
				t.Fatalf("missing process error = %v, want ErrProcessGone", err)
			}
		})
	}

	for _, test := range []struct {
		name    string
		readErr error
	}{
		{name: "permission denied", readErr: &os.PathError{Op: "open", Path: "/proc/42/stat", Err: fs.ErrPermission}},
		{name: "operation denied", readErr: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.EPERM}},
		{name: "I/O failure", readErr: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.EIO}},
		{name: "interrupted read", readErr: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.EINTR}},
		{name: "process table exhausted", readErr: &os.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.EMFILE}},
		{name: "system file table exhausted", readErr: &os.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.ENFILE}},
		{name: "invalid proc path", readErr: &os.PathError{Op: "open", Path: "/proc/42/stat", Err: syscall.ENOTDIR}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := processIdentityWithReader(42, func(string) ([]byte, error) { return nil, test.readErr })
			if err == nil || errors.Is(err, ErrProcessGone) || !errors.Is(err, test.readErr) {
				t.Fatalf("unreadable process error = %v, want propagated %v, not gone", err, test.readErr)
			}
		})
	}

	for _, test := range []struct {
		name string
		stat string
	}{
		{name: "missing command terminator", stat: "42 (malformed stat"},
		{name: "short stat", stat: "42 (cmd) S 1 2"},
		{name: "invalid parent PID", stat: "42 (cmd) S nope 12 13 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 98765 0"},
		{name: "invalid process group", stat: "42 (cmd) S 11 nope 13 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 98765 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := processIdentityWithReader(42, func(string) ([]byte, error) { return []byte(test.stat), nil })
			if err == nil || errors.Is(err, ErrProcessGone) {
				t.Fatalf("malformed process identity error = %v, want parse uncertainty, not gone", err)
			}
		})
	}
}

func TestParseProcStatCarriesParentGroupSessionAndStart(t *testing.T) {
	// The suffix begins at field 3 (state), so suffix index 19 is Linux
	// /proc/<pid>/stat field 22 (starttime); the trailing 0 is field 23.
	stat := "42 (cmd (worker)) S 11 12 13 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 98765 0"

	got, err := parseProcStat(stat)
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if want := (procStat{ppid: 11, pgid: 12, sid: 13, startTime: "98765"}); got != want {
		t.Fatalf("parseProcStat = %+v, want %+v", got, want)
	}
}

// buildFakeProc builds a minimal /proc-shaped fixture tree under root for pid
// with parent PID 1 (init). environ is written as NUL-delimited key=value pairs.
func buildFakeProc(t *testing.T, root string, pid int, env map[string]string) {
	t.Helper()
	buildFakeProcUnder(t, root, pid, 1, "cmd", env)
}

// buildFakeProcUnder is buildFakeProc for a process with a chosen parent and
// name: stat carries ppid, and comm is written newline-terminated, as /proc
// does. A parent of 1 (init) makes the process a root outright.
func buildFakeProcUnder(t *testing.T, root string, pid, ppid int, comm string, env map[string]string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	var buf []byte
	for k, v := range env {
		buf = append(buf, []byte(k+"="+v+"\x00")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), buf, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	stat := strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) + " 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("write comm: %v", err)
	}
}

func writeFakeProcessStat(t *testing.T, dir string, pid, ppid int, startTicks uint64) {
	t.Helper()
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[1] = strconv.Itoa(ppid)
	fields[19] = strconv.FormatUint(startTicks, 10)
	stat := strconv.Itoa(pid) + " (cmd) " + strings.Join(fields, " ")
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

func writeFakeBootTime(t *testing.T, root string, boot time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime "+strconv.FormatInt(boot.Unix(), 10)+"\n"), 0o644); err != nil {
		t.Fatalf("write proc stat: %v", err)
	}
}

func writeFakeProcessUID(t *testing.T, dir string, uid int) {
	t.Helper()
	writeFakeProcessUIDs(t, dir, uid, uid)
}

func writeFakeProcessUIDs(t *testing.T, dir string, realUID, effectiveUID int) {
	t.Helper()
	status := []byte(
		"Name:\ttest\nUid:\t" +
			strconv.Itoa(realUID) + "\t" +
			strconv.Itoa(effectiveUID) + "\t" +
			strconv.Itoa(effectiveUID) + "\t" +
			strconv.Itoa(realUID) + "\n",
	)
	if err := os.WriteFile(filepath.Join(dir, "status"), status, 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}
}

func writeFakeProcessEnviron(t *testing.T, dir string, env map[string]string) {
	t.Helper()
	var data []byte
	for key, value := range env {
		data = append(data, []byte(key+"="+value+"\x00")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), data, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
}

func writeFakeProcessCgroup(t *testing.T, dir, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}

func TestScanWithRootStatVanished(t *testing.T) {
	root := t.TempDir()
	pid := 500
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	// Write environ but no stat file (process died between environ read and stat check).
	env := []byte("GC_SESSION_ID=ga-test\x00")
	if err := os.WriteFile(filepath.Join(dir, "environ"), env, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	// stat file absent — simulates TOCTOU race.

	got, err := scanWithRoot(root, "ga-test")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("scanWithRoot returned %d runtimes for a vanished process, want 0", len(got))
	}
}

func TestScanWithRootEmptyReturnsNonNilSlice(t *testing.T) {
	root := t.TempDir()
	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if got == nil {
		t.Fatal("scanWithRoot returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("scanWithRoot returned %d runtimes, want 0", len(got))
	}
}

func TestScanWithRootFiltersBySessionID(t *testing.T) {
	root := t.TempDir()
	// pid 100: parent 1 (init), session ga-abc
	buildFakeProc(t, root, 100, map[string]string{"GC_SESSION_ID": "ga-abc"})
	// pid 200: parent 1 (init), session ga-xyz
	buildFakeProc(t, root, 200, map[string]string{"GC_SESSION_ID": "ga-xyz"})

	got, err := scanWithRoot(root, "ga-abc")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "ga-abc" || got[0].PID != 100 {
		t.Fatalf("scanWithRoot = %v, want [{ga-abc pid=100}]", got)
	}
}

func TestScanWithRootEmptyIDReturnsAll(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 100, map[string]string{"GC_SESSION_ID": "ga-abc"})
	buildFakeProc(t, root, 200, map[string]string{"GC_SESSION_ID": "ga-xyz"})

	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scanWithRoot = %d entries, want 2", len(got))
	}
}

func TestScanWithRootParsesEpoch(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 300, map[string]string{
		"GC_SESSION_ID":    "ga-epoch",
		"GC_RUNTIME_EPOCH": "42",
	})

	got, err := scanWithRoot(root, "ga-epoch")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Epoch != 42 {
		t.Fatalf("Epoch = %d, want 42", got[0].Epoch)
	}
}

func TestScanWithRootPopulatesCityFromGCPath(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 310, map[string]string{
		"GC_SESSION_ID": "ga-city",
		"GC_CITY_PATH":  "/tmp/primary-city",
		"GC_CITY":       "/tmp/fallback-city",
	})

	got, err := scanWithRoot(root, "ga-city")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].City != "/tmp/primary-city" {
		t.Fatalf("City = %q, want GC_CITY_PATH value", got[0].City)
	}
}

func TestScanWithRootPopulatesCityFromGCCityFallback(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 320, map[string]string{
		"GC_SESSION_ID": "ga-city",
		"GC_CITY":       "/tmp/fallback-city",
	})

	got, err := scanWithRoot(root, "ga-city")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].City != "/tmp/fallback-city" {
		t.Fatalf("City = %q, want GC_CITY fallback value", got[0].City)
	}
}

func TestScanWithRootMissingEnvironSkipped(t *testing.T) {
	root := t.TempDir()
	// Directory exists but no environ (ENOENT) — should be skipped without error.
	dir := filepath.Join(root, "400")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0", len(got))
	}
}

func TestScanWithRootUnreadableEnvironReportsPartialScan(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "401")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, unrelated, os.Geteuid()+1)
	if err := os.WriteFile(filepath.Join(unrelated, "environ"), []byte("GC_SESSION_ID=ga-hidden\x00"), 0o000); err != nil {
		t.Fatalf("write environ: %v", err)
	}

	got, err := scanWithRoot(root, "ga-hidden")
	if err != nil {
		t.Fatalf("unreadable unrelated process made scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated scan returned %d runtimes, want 0", len(got))
	}

	relevant := filepath.Join(root, "402")
	if err := os.MkdirAll(relevant, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, relevant, os.Geteuid())
	if err := os.WriteFile(filepath.Join(relevant, "environ"), []byte("GC_SESSION_ID=ga-hidden\x00"), 0o000); err != nil {
		t.Fatalf("write environ: %v", err)
	}

	got, err = scanWithRoot(root, "ga-hidden")
	if err == nil {
		t.Fatalf("scanWithRoot = %v, nil error; want an incomplete-scan error", got)
	}
	if len(got) != 0 {
		t.Fatalf("scanWithRoot returned %d runtimes, want no unverified result", len(got))
	}
}

// Each unreadable entry reaches the caller as an EntryError, so loggers can
// summarize a scan that hit many of them instead of printing each one.
func TestScanWithRootReportsUnreadableEntriesAsEntryErrors(t *testing.T) {
	root := t.TempDir()
	for _, pid := range []string{"403", "402"} {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFakeProcessUID(t, dir, os.Geteuid())
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte("GC_SESSION_ID=ga-hidden\x00"), 0o000); err != nil {
			t.Fatalf("write environ: %v", err)
		}
	}

	_, err := scanWithRoot(root, "")
	if err == nil {
		t.Fatal("scanWithRoot error = nil, want the unreadable entries reported")
	}
	summary := SummarizeScanError(err)
	if !strings.HasPrefix(summary, "2 unreadable process entries in 1 classes: 2 like ") || !strings.HasSuffix(summary, "(pids 402, 403)") {
		t.Fatalf("SummarizeScanError = %q; want both entries counted as EntryErrors", summary)
	}
}

func TestScanWithRootSinceIgnoresUnreadableProcessProvenOlderThanIncarnation(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	dir := filepath.Join(root, "405")
	if err := os.MkdirAll(filepath.Join(dir, "environ"), 0o755); err != nil {
		t.Fatalf("create unreadable environ fixture: %v", err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	writeFakeProcessStat(t, dir, 405, 1, 800)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err != nil {
		t.Fatalf("old inaccessible process made exact incarnation scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("old inaccessible scan returned %d runtimes, want 0", len(got))
	}
}

func TestScanWithRootSinceUnreadableProcessInsideStartTimeUncertaintyRemainsIncomplete(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	dir := filepath.Join(root, "409")
	if err := os.MkdirAll(filepath.Join(dir, "environ"), 0o755); err != nil {
		t.Fatalf("create unreadable environ fixture: %v", err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	writeFakeProcessStat(t, dir, 409, 1, 950)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err == nil {
		t.Fatalf("uncertain inaccessible scan = %v, nil error; want incomplete", got)
	}
}

func TestScanWithRootSinceUnreadableCurrentProcessRemainsIncomplete(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	dir := filepath.Join(root, "406")
	if err := os.MkdirAll(filepath.Join(dir, "environ"), 0o755); err != nil {
		t.Fatalf("create unreadable environ fixture: %v", err)
	}
	writeFakeProcessUID(t, dir, os.Geteuid())
	writeFakeProcessStat(t, dir, 406, 1, 1000)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err == nil {
		t.Fatalf("current inaccessible scan = %v, nil error; want incomplete", got)
	}
	if len(got) != 0 {
		t.Fatalf("current inaccessible scan returned %d runtimes, want 0", len(got))
	}
}

func TestScanWithRootSinceIgnoresNewerUnreadableSudoChildProvenUnrelatedByTmuxScopeParent(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	const (
		parentPID = 450
		childPID  = 451
	)
	scope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-a1b2.scope"

	parentDir := filepath.Join(root, strconv.Itoa(parentPID))
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("create parent fixture: %v", err)
	}
	writeFakeProcessUID(t, parentDir, os.Geteuid())
	writeFakeProcessStat(t, parentDir, parentPID, 1, 800)
	writeFakeProcessCgroup(t, parentDir, scope)
	writeFakeProcessEnviron(t, parentDir, map[string]string{})

	childDir := filepath.Join(root, strconv.Itoa(childPID))
	if err := os.MkdirAll(filepath.Join(childDir, "environ"), 0o755); err != nil {
		t.Fatalf("create unreadable child environ fixture: %v", err)
	}
	// sudo is setuid: its real UID remains ours while its effective UID is root.
	writeFakeProcessUIDs(t, childDir, os.Geteuid(), 0)
	writeFakeProcessStat(t, childDir, childPID, parentPID, 1100)
	writeFakeProcessCgroup(t, childDir, scope)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err != nil {
		t.Fatalf("unrelated sudo-shaped child made exact scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated sudo-shaped child scan returned %d runtimes, want 0", len(got))
	}
}

func TestScanWithRootSinceUnreadableSudoChildProofFailsClosed(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0).UTC()
	const (
		parentPID = 460
		childPID  = 461
	)
	validScope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-a1b2.scope"

	tests := []struct {
		name        string
		parentEnv   map[string]string
		parentScope string
		childScope  string
		makeParent  bool
	}{
		{
			name:        "parent carries target identity",
			parentEnv:   map[string]string{"GC_SESSION_ID": "ga-target"},
			parentScope: validScope,
			childScope:  validScope,
			makeParent:  true,
		},
		{
			name:        "generic scope",
			parentEnv:   map[string]string{},
			parentScope: "/user.slice/user-1000.slice/session-1.scope",
			childScope:  "/user.slice/user-1000.slice/session-1.scope",
			makeParent:  true,
		},
		{
			name:        "different tmux scopes",
			parentEnv:   map[string]string{},
			parentScope: validScope,
			childScope:  "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-other.scope",
			makeParent:  true,
		},
		{
			name:       "missing parent",
			childScope: validScope,
			makeParent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeBootTime(t, root, boot)

			if tt.makeParent {
				parentDir := filepath.Join(root, strconv.Itoa(parentPID))
				if err := os.MkdirAll(parentDir, 0o755); err != nil {
					t.Fatalf("create parent fixture: %v", err)
				}
				writeFakeProcessUID(t, parentDir, os.Geteuid())
				writeFakeProcessStat(t, parentDir, parentPID, 1, 800)
				writeFakeProcessCgroup(t, parentDir, tt.parentScope)
				writeFakeProcessEnviron(t, parentDir, tt.parentEnv)
			}

			childDir := filepath.Join(root, strconv.Itoa(childPID))
			if err := os.MkdirAll(filepath.Join(childDir, "environ"), 0o755); err != nil {
				t.Fatalf("create unreadable child environ fixture: %v", err)
			}
			writeFakeProcessUIDs(t, childDir, os.Geteuid(), 0)
			writeFakeProcessStat(t, childDir, childPID, parentPID, 1100)
			writeFakeProcessCgroup(t, childDir, tt.childScope)

			got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
			if err == nil {
				t.Fatalf("unsafe proof scan = %v, nil error; want incomplete", got)
			}
		})
	}
}

func TestScanWithRootSinceUnreadableSudoChildRevalidationFailsClosed(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0).UTC()
	const (
		childPID  = 470
		parentPID = 480
	)
	scope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-a1b2.scope"

	tests := []struct {
		name   string
		mutate func(t *testing.T, parentDir, childDir string)
	}{
		{
			name: "parent identity changes",
			mutate: func(t *testing.T, parentDir, _ string) {
				writeFakeProcessStat(t, parentDir, parentPID, 1, 901)
			},
		},
		{
			name: "parent disappears",
			mutate: func(t *testing.T, parentDir, _ string) {
				if err := os.RemoveAll(parentDir); err != nil {
					t.Fatalf("remove parent fixture: %v", err)
				}
			},
		},
		{
			name: "candidate parent link changes",
			mutate: func(t *testing.T, _, childDir string) {
				writeFakeProcessStat(t, childDir, childPID, parentPID+1, 1100)
			},
		},
		{
			name: "candidate disappears",
			mutate: func(t *testing.T, _, childDir string) {
				if err := os.RemoveAll(childDir); err != nil {
					t.Fatalf("remove child fixture: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeBootTime(t, root, boot)

			parentDir := filepath.Join(root, strconv.Itoa(parentPID))
			if err := os.MkdirAll(parentDir, 0o755); err != nil {
				t.Fatalf("create parent fixture: %v", err)
			}
			writeFakeProcessUID(t, parentDir, os.Geteuid())
			writeFakeProcessStat(t, parentDir, parentPID, 1, 800)
			writeFakeProcessCgroup(t, parentDir, scope)
			parentEnviron := filepath.Join(parentDir, "environ")
			if err := syscall.Mkfifo(parentEnviron, 0o600); err != nil {
				t.Fatalf("create parent environ fifo: %v", err)
			}

			childDir := filepath.Join(root, strconv.Itoa(childPID))
			if err := os.MkdirAll(filepath.Join(childDir, "environ"), 0o755); err != nil {
				t.Fatalf("create unreadable child environ fixture: %v", err)
			}
			writeFakeProcessUIDs(t, childDir, os.Geteuid(), 0)
			writeFakeProcessStat(t, childDir, childPID, parentPID, 1100)
			writeFakeProcessCgroup(t, childDir, scope)

			type scanResult struct {
				err error
				got int
			}
			scanDone := make(chan scanResult, 1)
			go func() {
				got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
				scanDone <- scanResult{err: err, got: len(got)}
			}()

			type openResult struct {
				file *os.File
				err  error
			}
			parentEnvOpened := make(chan openResult, 1)
			go func() {
				file, err := os.OpenFile(parentEnviron, os.O_WRONLY, 0)
				parentEnvOpened <- openResult{file: file, err: err}
			}()

			var writer *os.File
			select {
			case opened := <-parentEnvOpened:
				if opened.err != nil {
					t.Fatalf("open parent environ fifo: %v", opened.err)
				}
				writer = opened.file
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("scan did not reach parent environ proof")
			}

			if err := os.Remove(parentEnviron); err != nil {
				t.Fatalf("unlink parent environ fifo: %v", err)
			}
			tt.mutate(t, parentDir, childDir)
			if _, err := writer.Write(nil); err != nil {
				t.Fatalf("release parent environ read: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close parent environ fifo: %v", err)
			}

			select {
			case result := <-scanDone:
				if result.err == nil {
					t.Fatalf("unstable proof scan returned %d runtimes and nil error; want incomplete", result.got)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("scan did not finish after parent proof was released")
			}
		})
	}
}

func TestScanWithRootSinceReturnsReadableMatchRegardlessOfBoundary(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)
	buildFakeProc(t, root, 407, map[string]string{"GC_SESSION_ID": "ga-target"})
	writeFakeProcessStat(t, filepath.Join(root, "407"), 407, 1, 100)

	got, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err != nil {
		t.Fatalf("readable matching scan failed: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "ga-target" || got[0].PID != 407 {
		t.Fatalf("readable matching scan = %v, want ga-target pid 407", got)
	}
}

func TestProcessPredatesIncarnationFutureBoundaryFailsClosed(t *testing.T) {
	root := t.TempDir()
	boot := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeFakeBootTime(t, root, boot)
	dir := filepath.Join(root, "408")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create process fixture: %v", err)
	}
	writeFakeProcessStat(t, dir, 408, 1, 100)

	irrelevant, err := processPredatesIncarnation(root, 408, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("future-boundary proof failed: %v", err)
	}
	if irrelevant {
		t.Fatal("future incarnation boundary classified an inaccessible process as irrelevant")
	}
}

// A tmux server founded by a session's first new-session call inherits that
// session's GC_SESSION_ID and reparents to init, so the parent test alone
// reports it as an agent root — and the orphan sweep then kills the one server
// every agent in the city shares (gastownhall/gascity#5392). The agent running
// beneath that server must still be reported: it is the root the sweep is for.
func TestScanWithRootNeverReportsInfrastructureAsRoot(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{"GC_SESSION_ID": "hq-session"}
	buildFakeProcUnder(t, root, 100, 1, "tmux: server", env)
	buildFakeProcUnder(t, root, 101, 100, "claude", env)

	got, err := scanWithRoot(root, "hq-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 || got[0].PID != 101 {
		t.Fatalf("scanWithRoot = %+v, want only the agent pid 101", got)
	}
}

// IsScanRoot is the single-pid form of the same decision; it must answer as
// the scan does: no for the tmux server, yes for the agent beneath it.
func TestIsScanRootNeverReportsInfrastructureAsRoot(t *testing.T) {
	root := t.TempDir()
	SetScanRootForTesting(t, root)
	env := map[string]string{"GC_SESSION_ID": "hq-session"}
	buildFakeProcUnder(t, root, 100, 1, "tmux: server", env)
	buildFakeProcUnder(t, root, 101, 100, "claude", env)

	if IsScanRoot(100) {
		t.Fatal("IsScanRoot classified the tmux server as an agent root")
	}
	if !IsScanRoot(101) {
		t.Fatal("IsScanRoot must keep the agent under an infrastructure parent as a root")
	}
}

// Infrastructure is matched by exact name. A substring test on "tmux" would
// also hide a tmux-* wrapper running as an agent root, and a root the scan
// hides is a runtime the orphan sweep can never reap.
func TestScanWithRootReportsTmuxWrapperAsRoot(t *testing.T) {
	root := t.TempDir()
	buildFakeProcUnder(t, root, 200, 1, "tmux-wrapper", map[string]string{"GC_SESSION_ID": "hq-session"})

	got, err := scanWithRoot(root, "hq-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 || got[0].PID != 200 {
		t.Fatalf("scanWithRoot = %+v, want the tmux-wrapper agent pid 200 reported as a root", got)
	}
}

// TestScanWithRootExcludesLiveParentSessionDescendant pins the SCAN-LAYER half
// of the two-layer protection around processes that merely inherited an agent's
// GC_SESSION_ID.
//
// A daemon spawned from an agent pane (the managed-Dolt scope watchdog is the
// in-tree instance) carries the seat's GC_SESSION_ID verbatim. While its
// spawning `gc` is still alive, the ONLY thing keeping it out of the escalation's
// kill set is this envelope rule: its parent carries the same session id and is
// not infrastructure, so it is not a root and never reaches the fence. Nothing
// downstream would save it — the tmux adapter stamps IsTracked/ProviderName by
// session id alone, and the fence's reparent arm does not fire because its
// parent is alive.
//
// Widening the envelope rule (e.g. to `return true, nil`, a plausible future
// change to catch Setpgid'd escapees whose spawner still lives) makes that
// watchdog a passing kill target, and KillByPID signals its whole process
// GROUP — whose SIGTERM handler stops the city's shared dolt sql-server. That
// change must fail here rather than silently in production.
func TestScanWithRootExcludesLiveParentSessionDescendant(t *testing.T) {
	const (
		sessionID    = "ga-seat"
		tmuxServer   = 400
		paneRoot     = 500
		liveGC       = 600
		watchdog     = 700
		otherSession = "ga-other"
	)
	root := t.TempDir()
	sessionEnv := map[string]string{"GC_SESSION_ID": sessionID, "GC_CITY_PATH": "/city"}

	// The tmux server: infrastructure, and carries no session id of its own.
	buildFakeProcUnder(t, root, tmuxServer, 1, "tmux", map[string]string{"GC_SESSION_ID": otherSession})
	// The pane's root process — the ONLY legitimate runtime for this seat.
	buildFakeProcUnder(t, root, paneRoot, tmuxServer, "claude", sessionEnv)
	// A live `gc` the agent invoked, still running, same session id.
	buildFakeProcUnder(t, root, liveGC, paneRoot, "gc", sessionEnv)
	// The scope watchdog it spawned, still parented to that live `gc`.
	buildFakeProcUnder(t, root, watchdog, liveGC, "gc", sessionEnv)

	found, err := scanWithRoot(root, sessionID)
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}

	var pids []int
	for _, r := range found {
		pids = append(pids, r.PID)
	}
	if len(pids) != 1 || pids[0] != paneRoot {
		t.Fatalf("scanWithRoot roots = %v, want exactly [%d] (the pane root). "+
			"A session-descendant with a LIVE parent must not be reported as an agent root: nothing downstream "+
			"discriminates it, so the drain-ack escalation would force-terminate its process group and stop the "+
			"city's shared dolt sql-server", pids, paneRoot)
	}
}

// The drain-ack escalation's kill fence is stated POSITIVELY — a candidate's
// parent must be the provider's own server — precisely so it does not depend on
// detecting a subreaper pid, which is derived from the CALLER's ancestry and is
// empty whenever the caller and the target descend from different trees. That
// fence is only worth anything if the scan actually reports the distinction, so
// it is pinned here, at the source, and not only in the consumer's fake.
func TestScanWithRootReportsWhetherTheParentIsProviderInfrastructure(t *testing.T) {
	root := t.TempDir()
	const sessionID = "ga-parentage"
	// The tmux server, which inherits the GC_SESSION_ID of the session that
	// founded it. Infrastructure is never a root itself.
	buildFakeProcUnder(t, root, 400, 1, "tmux: server", map[string]string{"GC_SESSION_ID": sessionID})
	// A pane root the server still owns: the seat's actual runtime.
	buildFakeProcUnder(t, root, 401, 400, "claude", map[string]string{"GC_SESSION_ID": sessionID})
	// A daemon that merely INHERITED the seat's environment and, once its
	// spawner exited, was adopted by `systemd --user` — a large, LIVE ppid that
	// no `ppid <= 1` test reads as an orphan.
	buildFakeProcUnder(t, root, 402, 3117, "gc", map[string]string{"GC_SESSION_ID": sessionID})
	buildFakeProcUnder(t, root, 3117, 1, "systemd", nil)

	got, err := scanWithRoot(root, sessionID)
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	parentIsInfra := make(map[int]bool, len(got))
	for _, live := range got {
		parentIsInfra[live.PID] = live.ParentIsProviderInfrastructure
	}
	if len(got) != 2 {
		t.Fatalf("scanWithRoot returned %d roots (%v), want the pane root and the inherited-env daemon", len(got), parentIsInfra)
	}
	if !parentIsInfra[401] {
		t.Error("the pane root's parent is the tmux server, but the scan did not attribute it to provider infrastructure; " +
			"a kill fence that requires this attribution would refuse the seat's own runtime and the escalation would never fire")
	}
	if parentIsInfra[402] {
		t.Error("attributed a `systemd --user` parent to provider infrastructure; that is the orphaned-watchdog shape, " +
			"and killing it signals the process group whose SIGTERM handler stops the city's shared dolt sql-server")
	}
}

// A same-uid parent whose environ the kernel will not show (a capability-holding
// subreaper such as `systemd --user`, or a setuid or non-dumpable parent) names
// no session the scan can see, so a session child it adopted sits outside the
// session's envelope and is a root. The parent's own unreadable environ is still
// reported by its own scan entry, so the scan stays honest about what it could
// not read.
func TestScanWithRootReportsChildOfUnreadableParentAsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes, so the 0o000 parent environ would stay readable")
	}
	const (
		sessionID = "ga-unreadable-parent"
		parent    = 3117
		child     = 402
	)
	root := t.TempDir()
	SetScanRootForTesting(t, root)
	buildFakeProcUnder(t, root, parent, 1, "systemd", nil)
	if err := os.Chmod(filepath.Join(root, strconv.Itoa(parent), "environ"), 0o000); err != nil {
		t.Fatalf("chmod parent environ: %v", err)
	}
	buildFakeProcUnder(t, root, child, parent, "gc", map[string]string{"GC_SESSION_ID": sessionID})

	got, err := scanWithRoot(root, sessionID)
	if len(got) != 1 || got[0].PID != child {
		t.Fatalf("scanWithRoot = %+v (error: %v), want the child of the unreadable parent reported as a root", got, err)
	}
	if got[0].ParentIsProviderInfrastructure {
		t.Error("attributed an unreadable `systemd` parent to provider infrastructure; the drain-ack kill fence needs that to stay false")
	}
	if err == nil || !strings.Contains(err.Error(), "reading environ for pid "+strconv.Itoa(parent)) {
		t.Errorf("scanWithRoot error = %v, want the parent's own unreadable environ still reported", err)
	}
	if !IsScanRoot(child) {
		t.Error("IsScanRoot disagrees with the scan: the child of an unreadable parent is a root")
	}
}

// Only a permission denial reads as "outside the envelope". Any other failure to
// read the parent's environ (here the read fails with EISDIR) leaves the child's
// root status unproven, so the scan omits the child and reports why.
func TestScanWithRootOmitsChildWhenParentEnvironFailsForOtherReasons(t *testing.T) {
	const (
		sessionID = "ga-broken-parent"
		parent    = 3117
		child     = 402
	)
	root := t.TempDir()
	buildFakeProcUnder(t, root, parent, 1, "systemd", nil)
	environ := filepath.Join(root, strconv.Itoa(parent), "environ")
	if err := os.Remove(environ); err != nil {
		t.Fatalf("remove parent environ: %v", err)
	}
	if err := os.Mkdir(environ, 0o755); err != nil {
		t.Fatalf("mkdir parent environ: %v", err)
	}
	buildFakeProcUnder(t, root, child, parent, "gc", map[string]string{"GC_SESSION_ID": sessionID})

	got, err := scanWithRoot(root, sessionID)
	if len(got) != 0 {
		t.Fatalf("scanWithRoot = %+v, want the child omitted: a parent environ that fails for a reason other than permission proves nothing about the envelope", got)
	}
	if err == nil || !strings.Contains(err.Error(), "checking root for pid "+strconv.Itoa(child)) {
		t.Fatalf("scanWithRoot error = %v, want it to name the child it could not classify", err)
	}
}

// Kills: a scan that leaves StartIdentity or StartedAt unset, reads them from
// another field, or reports a start time without a boot time.
func TestScanWithRootReportsStartIdentity(t *testing.T) {
	root := t.TempDir()
	boot := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	buildFakeProc(t, root, 100, map[string]string{"GC_SESSION_ID": "ga-start"})
	writeFakeProcessStat(t, filepath.Join(root, strconv.Itoa(100)), 100, 1, 12345)

	got, err := scanWithRoot(root, "ga-start")
	if err != nil || len(got) != 1 {
		t.Fatalf("scanWithRoot = %v, %v; want one root", got, err)
	}
	if got[0].StartIdentity != "12345" || !got[0].StartedAt.IsZero() {
		t.Fatalf("without btime: StartIdentity=%q StartedAt=%v, want 12345 and zero", got[0].StartIdentity, got[0].StartedAt)
	}

	writeFakeBootTime(t, root, boot)
	got, err = scanWithRoot(root, "ga-start")
	if err != nil || len(got) != 1 {
		t.Fatalf("scanWithRoot = %v, %v; want one root", got, err)
	}
	if want := boot.Add(123*time.Second + 450*time.Millisecond); !got[0].StartedAt.Equal(want) || got[0].StartIdentity != "12345" {
		t.Fatalf("StartIdentity=%q StartedAt=%v, want 12345 and %v", got[0].StartIdentity, got[0].StartedAt, want)
	}
}

// entryErrorPIDs returns the pid of every [EntryError] in err's tree, in
// order. Tests assert on it instead of searching the message, which carries
// the fixture path.
func entryErrorPIDs(err error) []int {
	var pids []int
	switch e := err.(type) { //nolint:errorlint // walks the joined error tree by hand; errors.As finds only the first entry
	case nil:
	case *EntryError:
		pids = append(pids, e.PID)
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			pids = append(pids, entryErrorPIDs(child)...)
		}
	case interface{ Unwrap() error }:
		pids = entryErrorPIDs(e.Unwrap())
	}
	return pids
}

// TestOrphanCandidateScanReadsOnlyReparentedProcesses (mc-ygved): the
// empty-id scan reports the roots reparented to init or the user subreaper
// and reads no other process's environment: a non-candidate whose environ
// cannot be read adds no error, while a candidate's unreadable parent is
// the entry's error (fail-closed). The id-scoped scan is unchanged.
func TestOrphanCandidateScanReadsOnlyReparentedProcesses(t *testing.T) {
	// The fixture path is part of every entry error's message, so one case
	// roots it at a fixed name that contains the digits of another fixture pid.
	for _, tc := range []struct{ name, dir string }{
		{"tempdir", ""},
		{"path containing 103", "pid103"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), tc.dir)
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFakeBootTime(t, root, time.Unix(1700000000, 0))
			const subreaper = 50
			buildFakeProcUnder(t, root, subreaper, 1, "systemd", nil)
			buildFakeProcUnder(t, root, 100, 1, "agent", map[string]string{"GC_SESSION_ID": "gc-init"})
			buildFakeProcUnder(t, root, 101, subreaper, "agent", map[string]string{"GC_SESSION_ID": "gc-sub"})
			buildFakeProcUnder(t, root, 200, 300, "tmux: server", nil)
			buildFakeProcUnder(t, root, 300, 1, "bash", nil)
			buildFakeProcUnder(t, root, 102, 300, "agent", map[string]string{"GC_SESSION_ID": "gc-paned"}) // its parent (bash) carries no session: a root, but no orphan
			buildFakeProcUnder(t, root, 103, 300, "agent", map[string]string{"GC_SESSION_ID": "gc-x"})
			if err := os.Chmod(filepath.Join(root, "103", "environ"), 0); err != nil { // a non-candidate's environ unreadable
				t.Fatal(err)
			}
			buildFakeProcUnder(t, root, 104, 1, "agent", map[string]string{"GC_SESSION_ID": "gc-y"})
			if err := os.Chmod(filepath.Join(root, "104", "stat"), 0); err != nil { // a process whose parent cannot be read
				t.Fatal(err)
			}
			if os.Geteuid() == 0 {
				t.Skip("root reads every file; the unreadable fixtures need an unprivileged user")
			}

			got, err := scanOrphanCandidatesWithRoot(root, subreaper)
			var ids []string
			for _, r := range got {
				ids = append(ids, r.SessionID)
			}
			if strings.Join(ids, ",") != "gc-init,gc-sub" {
				t.Fatalf("orphan candidates %v, want gc-init and gc-sub only", ids)
			}
			if pids := entryErrorPIDs(err); !slices.Equal(pids, []int{104}) || HasWholeScanFailure(err) {
				t.Fatalf("scan error %v has entry errors for pids %v, want only pid 104's unreadable parent, as an entry error", err, pids)
			}
			all, _ := scanWithRoot(root, "gc-paned")
			if len(all) != 1 || all[0].PID != 102 {
				t.Fatalf("id-scoped scan %+v, want the paned root still found", all)
			}
			// ScanBySessionID: the empty id is the orphan scan, a session id the
			// full one.
			SetScanRootForTesting(t, root)
			savedDetect := detectSubreaper
			detectSubreaper = func(int) int { return subreaper }
			t.Cleanup(func() { detectSubreaper = savedDetect })
			public, _ := ScanBySessionID("")
			var publicIDs []string
			for _, r := range public {
				publicIDs = append(publicIDs, r.SessionID)
			}
			if strings.Join(publicIDs, ",") != "gc-init,gc-sub" { // the subreaper's orphan included (a systemd --user supervisor)
				t.Fatalf("ScanBySessionID(\"\") = %v, want gc-init and gc-sub only", publicIDs)
			}
			if scoped, _ := ScanBySessionID("gc-paned"); len(scoped) != 1 {
				t.Fatalf("ScanBySessionID(gc-paned) = %+v, want the paned root", scoped)
			}
		})
	}
}

// TestOrphanCandidate (mc-ygved): init and the user subreaper are the
// orphan parents; no subreaper, or any other parent, is not.
func TestOrphanCandidate(t *testing.T) {
	for _, tc := range []struct {
		ppid, subreaper int
		want            bool
	}{{1, 0, true}, {1, 50, true}, {50, 50, true}, {50, 0, false}, {77, 50, false}, {0, 0, false}} {
		if got := OrphanCandidate(tc.ppid, tc.subreaper); got != tc.want {
			t.Errorf("OrphanCandidate(%d, %d) = %t, want %t", tc.ppid, tc.subreaper, got, tc.want)
		}
	}
}

// TestUnreadableEnvironUserManagerScope covers a host helper the scanner's uid
// owns but cannot ptrace-read, such as the systemd user manager's (sd-pam):
// its exact own-user-manager unified cgroup scope plus a stable identity that
// definitely predates the read prove it cannot be a runtime, so it adds no
// EntryError (ga-zpcphq, PR #8182) — in the unbounded id-scoped scan and the
// orphan-candidate scan, on a pure-v2 host and a hybrid v1/v2 host alike.
func TestUnreadableEnvironUserManagerScope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes, so the 0o000 environ would stay readable")
	}
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	boot := time.Now().Add(-time.Hour).UTC()
	const (
		helperPID   = 2642
		helperTicks = 600 // six seconds after boot: definitely older than the read
	)
	newFixture := func(t *testing.T, cgroupLines string) string {
		t.Helper()
		root := t.TempDir()
		writeFakeBootTime(t, root, boot)
		dir := filepath.Join(root, strconv.Itoa(helperPID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFakeProcessUID(t, dir, euid)
		writeFakeProcessStat(t, dir, helperPID, 1, helperTicks)
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("(sd-pam)\n"), 0o644); err != nil {
			t.Fatalf("write comm: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroupLines), 0o644); err != nil {
			t.Fatalf("write cgroup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), nil, 0o644); err != nil {
			t.Fatalf("write environ: %v", err)
		}
		if err := os.Chmod(filepath.Join(dir, "environ"), 0o000); err != nil {
			t.Fatalf("chmod environ: %v", err)
		}
		return root
	}
	scans := map[string]func(root string) error{
		"id-scoped scan": func(root string) error {
			_, err := scanWithRoot(root, "ga-target")
			return err
		},
		"orphan-candidate scan": func(root string) error {
			_, err := scanOrphanCandidatesWithRoot(root, 0)
			return err
		},
	}
	cgroups := map[string]string{
		"pure v2": "0::" + scope + "\n",
		"hybrid":  "1:name=systemd:/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope\n0::" + scope + "\n",
	}
	for scanName, scan := range scans {
		for cgroupName, cgroupLines := range cgroups {
			t.Run(scanName+", "+cgroupName, func(t *testing.T) {
				if err := scan(newFixture(t, cgroupLines)); err != nil {
					t.Fatalf("scan error = %v (%s); want the user-manager helper excused", err, SummarizeScanError(err))
				}
			})
		}
	}
}

// TestUnreadableEnvironUserManagerDirectPermissionError proves the
// user-manager-scope exemption through unreadableEnvironScanError itself,
// with a wrapped permission error, so the classification holds regardless of
// file modes and root: a direct error and a 0o000 environ read are the same
// failure to the scanner.
func TestUnreadableEnvironUserManagerDirectPermissionError(t *testing.T) {
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	boot := time.Now().Add(-time.Hour).UTC()
	const (
		helperPID   = 2642
		helperTicks = 600
	)
	root := t.TempDir()
	writeFakeBootTime(t, root, boot)
	dir := filepath.Join(root, strconv.Itoa(helperPID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, dir, euid)
	writeFakeProcessStat(t, dir, helperPID, 1, helperTicks)
	writeFakeProcessCgroup(t, dir, scope)

	readErr := &fs.PathError{Op: "open", Path: filepath.Join(dir, "environ"), Err: syscall.EACCES}
	err := unreadableEnvironScanError(root, helperPID, "ga-target", time.Time{}, time.Now(), euid, readErr)
	if err != nil {
		t.Fatalf("unreadableEnvironScanError = %v (%s); want the user-manager helper excused", err, SummarizeScanError(err))
	}
}

// TestUnreadableEnvironUserManagerScopeUnderIncarnationBound covers the
// bounded id-scoped scan (ScanBySessionIDSince): a user-manager helper that
// started after the incarnation cannot be excused as predating it and sits
// in no tmux-spawn scope, yet its own-manager init.scope identity still
// proves it no runtime, so the exact-incarnation scan stays complete
// (ga-zpcphq). The direct-error variant runs the same classification with a
// wrapped permission error, whatever the file modes.
func TestUnreadableEnvironUserManagerScopeUnderIncarnationBound(t *testing.T) {
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	// The helper started six seconds after boot, three seconds after the
	// incarnation: newer than the incarnation, definitely older than any read.
	boot := time.Unix(1_700_000_000, 0).UTC()
	incarnation := boot.Add(3 * time.Second)
	const (
		helperPID   = 2642
		helperTicks = 600
	)
	newFixture := func(t *testing.T, unreadable bool) string {
		t.Helper()
		root := t.TempDir()
		writeFakeBootTime(t, root, boot)
		dir := filepath.Join(root, strconv.Itoa(helperPID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFakeProcessUID(t, dir, euid)
		writeFakeProcessStat(t, dir, helperPID, 1, helperTicks)
		writeFakeProcessCgroup(t, dir, scope)
		if err := os.WriteFile(filepath.Join(dir, "environ"), nil, 0o644); err != nil {
			t.Fatalf("write environ: %v", err)
		}
		if unreadable {
			if err := os.Chmod(filepath.Join(dir, "environ"), 0o000); err != nil {
				t.Fatalf("chmod environ: %v", err)
			}
		}
		return root
	}
	t.Run("scan", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file modes, so the 0o000 environ would stay readable")
		}
		if _, err := scanWithRootSince(newFixture(t, true), "ga-target", incarnation); err != nil {
			t.Fatalf("bounded scan error = %v (%s); want the user-manager helper excused without predating the incarnation", err, SummarizeScanError(err))
		}
	})
	t.Run("direct permission error", func(t *testing.T) {
		root := newFixture(t, false)
		readErr := &fs.PathError{Op: "open", Path: filepath.Join(root, strconv.Itoa(helperPID), "environ"), Err: syscall.EACCES}
		err := unreadableEnvironScanError(root, helperPID, "ga-target", incarnation, time.Now(), euid, readErr)
		if err != nil {
			t.Fatalf("unreadableEnvironScanError = %v (%s); want the user-manager helper excused without predating the incarnation", err, SummarizeScanError(err))
		}
	})
}

// unreadableEnvironCaseFixture builds the base fixture of the fail-closed
// table: pid 2742, owned by the scanner's uid, started well before any read,
// with a cgroup file and stat the caller can specialize per case.
func unreadableEnvironCaseFixture(t *testing.T, cgroupLines string) (string, string) {
	t.Helper()
	euid := os.Geteuid()
	boot := time.Unix(1_700_000_000, 0).UTC()
	const casePID = 2742
	root := t.TempDir()
	writeFakeBootTime(t, root, boot)
	dir := filepath.Join(root, strconv.Itoa(casePID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, dir, euid)
	writeFakeProcessStat(t, dir, casePID, 1, 600)
	if cgroupLines != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroupLines), 0o644); err != nil {
			t.Fatalf("write cgroup: %v", err)
		}
	}
	return root, dir
}

// TestUnreadableEnvironUserManagerProofFailsClosed pins the exemption's other
// edge (ga-zpcphq): an unreadable process the structural proof cannot decide
// keeps its environ failure, and the proof's own read failures are retained
// beside it — the exemption never weakens the fail-closed signal for
// anything that could be a runtime. Names, dumpability and ancestry are not
// evidence; non-permission failures are never excused.
func TestUnreadableEnvironUserManagerProofFailsClosed(t *testing.T) {
	euid := os.Geteuid()
	ownScope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	otherScope := "/user.slice/user-" + strconv.Itoa(euid+1) + ".slice/user@" + strconv.Itoa(euid+1) + ".service/init.scope"
	const casePID = 2742
	permErr := func(dir string) error {
		return &fs.PathError{Op: "open", Path: filepath.Join(dir, "environ"), Err: syscall.EACCES}
	}
	cases := []struct {
		name      string
		cgroup    string
		commute   func(t *testing.T, dir string)
		readErr   func(dir string) error
		wantProof bool // the proof's own failure is retained beside the environ error
	}{
		{
			name:   "hardened runtime CLI in a tmux-spawn scope",
			cgroup: "0::/user.slice/user-" + strconv.Itoa(euid) + ".slice/session-1.scope/tmux-spawn-abc.scope\n",
			commute: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("codex\n"), 0o644); err != nil {
					t.Fatalf("write comm: %v", err)
				}
			},
			readErr: permErr,
		},
		{
			name:   "credential agent name, comm and argv agree",
			cgroup: "0::/user.slice/user-" + strconv.Itoa(euid) + ".slice/session-3.scope\n",
			commute: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("ssh-agent\n"), 0o644); err != nil {
					t.Fatalf("write comm: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/usr/bin/ssh-agent\x00"), 0o644); err != nil {
					t.Fatalf("write cmdline: %v", err)
				}
			},
			readErr: permErr,
		},
		{
			name:   "credential agent name, comm and argv disagree",
			cgroup: "0::/user.slice/user-" + strconv.Itoa(euid) + ".slice/session-3.scope\n",
			commute: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("ssh-agent\n"), 0o644); err != nil {
					t.Fatalf("write comm: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/usr/bin/gc\x00"), 0o644); err != nil {
					t.Fatalf("write cmdline: %v", err)
				}
			},
			readErr: permErr,
		},
		{
			name:    "another user's manager scope",
			cgroup:  "0::" + otherScope + "\n",
			readErr: permErr,
		},
		{
			name:    "system service scope",
			cgroup:  "0::/system.slice/sshd.service\n",
			readErr: permErr,
		},
		{
			name:    "bare user service without the init scope",
			cgroup:  "0::/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service\n",
			readErr: permErr,
		},
		{
			name:    "nested under the init scope",
			cgroup:  "0::" + ownScope + "/child.scope\n",
			readErr: permErr,
		},
		{
			name:    "trailing slash on the raw path",
			cgroup:  "0::" + ownScope + "/\n",
			readErr: permErr,
		},
		{
			name:    "missing cgroup file",
			cgroup:  "",
			readErr: permErr,
		},
		{
			name:    "v1-only cgroup file",
			cgroup:  "1:name=systemd:/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope\n",
			readErr: permErr,
		},
		{
			name:      "malformed cgroup line",
			cgroup:    "not a cgroup line\n",
			readErr:   permErr,
			wantProof: true,
		},
		{
			name:      "duplicate unified entries",
			cgroup:    "0::" + ownScope + "\n0::" + ownScope + "\n",
			readErr:   permErr,
			wantProof: true,
		},
		{
			name:      "empty unified path",
			cgroup:    "0::\n",
			readErr:   permErr,
			wantProof: true,
		},
		{
			name: "cgroup read failure",
			commute: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "cgroup")); err != nil {
					t.Fatalf("remove cgroup: %v", err)
				}
				if err := os.Mkdir(filepath.Join(dir, "cgroup"), 0o755); err != nil {
					t.Fatalf("mkdir cgroup: %v", err)
				}
			},
			cgroup:    "0::" + ownScope + "\n",
			readErr:   permErr,
			wantProof: true,
		},
		{
			name: "process newer than the read",
			commute: func(t *testing.T, dir string) {
				// A boot three seconds ago with start ticks of 600 puts the
				// process three seconds in the future of any read that follows.
				writeFakeBootTime(t, filepath.Dir(dir), time.Now().Add(-3*time.Second))
			},
			cgroup:  "0::" + ownScope + "\n",
			readErr: permErr,
		},
		{
			name: "process age inside the start-time uncertainty",
			commute: func(t *testing.T, dir string) {
				// A boot six seconds ago with start ticks of 600 puts the
				// process exactly at the read: not definitely older.
				writeFakeBootTime(t, filepath.Dir(dir), time.Now().Add(-6*time.Second))
			},
			cgroup:  "0::" + ownScope + "\n",
			readErr: permErr,
		},
		{
			name:   "non-permission environ failure is never excused",
			cgroup: "0::" + ownScope + "\n",
			readErr: func(dir string) error {
				return &fs.PathError{Op: "read", Path: filepath.Join(dir, "environ"), Err: syscall.EIO}
			},
		},
		{
			name:   "EISDIR environ failure is never excused",
			cgroup: "0::" + ownScope + "\n",
			readErr: func(dir string) error {
				return &fs.PathError{Op: "read", Path: filepath.Join(dir, "environ"), Err: syscall.EISDIR}
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root, dir := unreadableEnvironCaseFixture(t, tt.cgroup)
			if tt.commute != nil {
				tt.commute(t, dir)
			}
			err := unreadableEnvironScanError(root, casePID, "ga-target", time.Time{}, time.Now(), euid, tt.readErr(dir))
			if err == nil {
				t.Fatalf("unreadableEnvironScanError = nil; want the environ failure to remain observable")
			}
			if !strings.Contains(err.Error(), "reading environ for pid "+strconv.Itoa(casePID)) {
				t.Fatalf("error = %v; want the environ failure retained", err)
			}
			hasProof := strings.Contains(err.Error(), "proving non-runtime for pid "+strconv.Itoa(casePID))
			if hasProof != tt.wantProof {
				t.Fatalf("error = %v; proof failure retained = %t, want %t", err, hasProof, tt.wantProof)
			}
		})
	}
}

// TestUnreadableEnvironUserManagerHybridKeepsTmuxProofError covers the
// exemption's error accounting on a hybrid host (ga-zpcphq): when the new
// structural proof excuses a helper's environ failure, an earlier proof
// failure — here the strict tmux-cgroup proof choking on the same hybrid
// file — is still reported, exactly as the tmux exception keeps its own
// earlier errors. The bounded scan stays incomplete; only the environ line
// is gone.
func TestUnreadableEnvironUserManagerHybridKeepsTmuxProofError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes, so the 0o000 environ would stay readable")
	}
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	boot := time.Unix(1_700_000_000, 0).UTC()
	const helperPID = 2642
	root := t.TempDir()
	writeFakeBootTime(t, root, boot)
	dir := filepath.Join(root, strconv.Itoa(helperPID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeProcessUID(t, dir, euid)
	// Six seconds after boot, past the incarnation at three seconds: newer
	// than the incarnation, definitely older than any read.
	writeFakeProcessStat(t, dir, helperPID, 1, 600)
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(
		"1:name=systemd:"+scope+"\n0::"+scope+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), nil, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "environ"), 0o000); err != nil {
		t.Fatalf("chmod environ: %v", err)
	}

	_, err := scanWithRootSince(root, "ga-target", boot.Add(3*time.Second))
	if err == nil {
		t.Fatalf("bounded scan error = nil; want the retained tmux proof failure to keep the scan incomplete")
	}
	summary := SummarizeScanError(err)
	if !strings.Contains(summary, "proving tmux parent for pid "+strconv.Itoa(helperPID)) {
		t.Fatalf("summary = %q; want the earlier tmux proof failure retained", summary)
	}
	if strings.Contains(summary, "reading environ for pid "+strconv.Itoa(helperPID)) {
		t.Fatalf("summary = %q; want the excused environ failure gone", summary)
	}
}

// TestUnreadableEnvironUserManagerProofRevalidation drives the proof's two
// censuses directly, mutating the fixture between them (ga-zpcphq): a process
// whose identity moved between the capture and the re-read is unproven — a
// pid recycled during the proof abandons it — while failed or malformed
// re-reads are errors, and a capture whose scope, age or boot time cannot be
// established never starts.
func TestUnreadableEnvironUserManagerProofRevalidation(t *testing.T) {
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	boot := time.Unix(1_700_000_000, 0).UTC()
	const (
		helperPID   = 2842
		helperTicks = 600
	)
	newFixture := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		writeFakeBootTime(t, root, boot)
		dir := filepath.Join(root, strconv.Itoa(helperPID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFakeProcessUID(t, dir, euid)
		writeFakeProcessStat(t, dir, helperPID, 1, helperTicks)
		writeFakeProcessCgroup(t, dir, scope)
		return root
	}
	capture := func(root string, readStartedAt time.Time) (userManagerScopeSnapshot, bool) {
		t.Helper()
		snapshot, ok, err := captureUserManagerScopeIdentity(root, helperPID, euid, readStartedAt)
		if err != nil {
			t.Fatalf("captureUserManagerScopeIdentity: %v", err)
		}
		if !ok {
			t.Fatalf("captureUserManagerScopeIdentity ok = false, want a proof to start")
		}
		return snapshot, true
	}
	mutations := []struct {
		name    string
		mutate  func(t *testing.T, root, dir string)
		wantErr bool
	}{
		{
			name: "start ticks change",
			mutate: func(t *testing.T, _, dir string) {
				writeFakeProcessStat(t, dir, helperPID, 1, helperTicks+1)
			},
		},
		{
			name: "parent changes",
			mutate: func(t *testing.T, _, dir string) {
				writeFakeProcessStat(t, dir, helperPID, 2, helperTicks)
			},
		},
		{
			name: "cgroup path changes",
			mutate: func(t *testing.T, _, dir string) {
				writeFakeProcessCgroup(t, dir, "/user.slice/user-"+strconv.Itoa(euid)+".slice/session-1.scope")
			},
		},
		{
			name: "process disappears",
			mutate: func(t *testing.T, _, dir string) {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatalf("remove fixture: %v", err)
				}
			},
		},
		{
			name: "cgroup file disappears",
			mutate: func(t *testing.T, _, dir string) {
				if err := os.Remove(filepath.Join(dir, "cgroup")); err != nil {
					t.Fatalf("remove cgroup: %v", err)
				}
			},
		},
		{
			name: "stat becomes malformed",
			mutate: func(t *testing.T, _, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("2842 (broken"), 0o644); err != nil {
					t.Fatalf("write stat: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "cgroup read fails",
			mutate: func(t *testing.T, _, dir string) {
				if err := os.Remove(filepath.Join(dir, "cgroup")); err != nil {
					t.Fatalf("remove cgroup: %v", err)
				}
				if err := os.Mkdir(filepath.Join(dir, "cgroup"), 0o755); err != nil {
					t.Fatalf("mkdir cgroup: %v", err)
				}
			},
			wantErr: true,
		},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			root := newFixture(t)
			dir := filepath.Join(root, strconv.Itoa(helperPID))
			snapshot, _ := capture(root, time.Now())
			tt.mutate(t, root, dir)
			still, err := snapshot.stillCurrent(root)
			if still {
				t.Fatalf("stillCurrent = true after %s; want the proof abandoned", tt.name)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("stillCurrent error = %v, want error = %t", err, tt.wantErr)
			}
		})
	}
	t.Run("capture needs a readable identity", func(t *testing.T) {
		root := newFixture(t)
		dir := filepath.Join(root, strconv.Itoa(helperPID))
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove fixture: %v", err)
		}
		if _, ok, err := captureUserManagerScopeIdentity(root, helperPID, euid, time.Now()); ok || err != nil {
			t.Fatalf("capture of a vanished process = ok %t, err %v; want ok false, nil", ok, err)
		}
	})
	t.Run("capture needs the boot time", func(t *testing.T) {
		root := newFixture(t)
		if err := os.Remove(filepath.Join(root, "stat")); err != nil {
			t.Fatalf("remove boot time: %v", err)
		}
		_, ok, err := captureUserManagerScopeIdentity(root, helperPID, euid, time.Now())
		if ok || err == nil {
			t.Fatalf("capture without a boot time = ok %t, err %v; want ok false, an error", ok, err)
		}
	})
	t.Run("capture requires an age beyond the read's start", func(t *testing.T) {
		root := newFixture(t)
		// readStartedAt exactly at the process start: not definitely older.
		notOlder := processStartedAt(boot, helperTicks)
		if _, ok, err := captureUserManagerScopeIdentity(root, helperPID, euid, notOlder); ok || err != nil {
			t.Fatalf("capture at the process start = ok %t, err %v; want ok false, nil", ok, err)
		}
	})
	t.Run("capture requires the exact own scope", func(t *testing.T) {
		root := newFixture(t)
		writeFakeProcessCgroup(t, filepath.Join(root, strconv.Itoa(helperPID)), scope+"/child.scope")
		if _, ok, err := captureUserManagerScopeIdentity(root, helperPID, euid, time.Now()); ok || err != nil {
			t.Fatalf("capture outside the scope = ok %t, err %v; want ok false, nil", ok, err)
		}
	})
}

// TestReadUnifiedCgroup covers the unified-cgroup reader on its own: v1
// controller entries are ignored, a missing file or unified entry is merely
// unproven, and an unreadable, malformed, duplicate or empty unified entry is
// an error. The path is returned raw.
func TestReadUnifiedCgroup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2942")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(t *testing.T, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
			t.Fatalf("write cgroup: %v", err)
		}
	}
	t.Run("pure v2", func(t *testing.T) {
		write(t, "0::/user.slice/init.scope\n")
		path, exists, err := readUnifiedCgroup(root, 2942)
		if err != nil || !exists || path != "/user.slice/init.scope" {
			t.Fatalf("readUnifiedCgroup = %q, %t, %v; want the unified path", path, exists, err)
		}
	})
	t.Run("hybrid v1 and v2", func(t *testing.T) {
		write(t, "1:name=systemd:/user.slice\n2:cpu:/user.slice\n0::/user.slice/init.scope\n")
		path, exists, err := readUnifiedCgroup(root, 2942)
		if err != nil || !exists || path != "/user.slice/init.scope" {
			t.Fatalf("readUnifiedCgroup = %q, %t, %v; want the unified path among the v1 entries", path, exists, err)
		}
	})
	t.Run("v1 only is unproven", func(t *testing.T) {
		write(t, "1:name=systemd:/user.slice/init.scope\n")
		path, exists, err := readUnifiedCgroup(root, 2942)
		if err != nil || exists || path != "" {
			t.Fatalf("readUnifiedCgroup = %q, %t, %v; want unproven without error", path, exists, err)
		}
	})
	t.Run("missing file is unproven", func(t *testing.T) {
		if err := os.Remove(filepath.Join(dir, "cgroup")); err != nil {
			t.Fatalf("remove cgroup: %v", err)
		}
		path, exists, err := readUnifiedCgroup(root, 2942)
		if err != nil || exists || path != "" {
			t.Fatalf("readUnifiedCgroup = %q, %t, %v; want unproven without error", path, exists, err)
		}
	})
	t.Run("empty file is unproven", func(t *testing.T) {
		write(t, "")
		path, exists, err := readUnifiedCgroup(root, 2942)
		if err != nil || exists || path != "" {
			t.Fatalf("readUnifiedCgroup = %q, %t, %v; want unproven without error", path, exists, err)
		}
	})
	t.Run("path is returned raw", func(t *testing.T) {
		write(t, "0::/user.slice//init.scope\n")
		path, _, err := readUnifiedCgroup(root, 2942)
		if err != nil || path != "/user.slice//init.scope" {
			t.Fatalf("readUnifiedCgroup = %q, %v; want the raw path, uncleaned", path, err)
		}
	})
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"malformed line", "not a cgroup line\n"},
		{"controller list on the unified entry", "0:name=systemd:/user.slice\n"},
		{"duplicate unified entries", "0::/a\n0::/b\n"},
		{"empty unified path", "0::\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write(t, tt.content)
			if _, _, err := readUnifiedCgroup(root, 2942); err == nil {
				t.Fatalf("readUnifiedCgroup of %q = nil error; want an error", tt.content)
			}
		})
	}
	t.Run("unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file modes, so the 0o000 cgroup would stay readable")
		}
		write(t, "0::/user.slice/init.scope\n")
		if err := os.Chmod(filepath.Join(dir, "cgroup"), 0o000); err != nil {
			t.Fatalf("chmod cgroup: %v", err)
		}
		if _, _, err := readUnifiedCgroup(root, 2942); err == nil {
			t.Fatal("readUnifiedCgroup of an unreadable file = nil error; want an error")
		}
	})
}

// TestUnreadableEnvironUserManagerMixedScan runs both unbounded scans over a
// host with an exempt manager helper, an unproven unreadable process and a
// readable orphan root (ga-zpcphq): the scan error names only the unproven
// process, the readable root is still returned, and a readable credential
// agent carrying GC_SESSION_ID is reported like any other root — names never
// exclude a root the scan can read.
func TestUnreadableEnvironUserManagerMixedScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes, so the 0o000 environ would stay readable")
	}
	euid := os.Geteuid()
	scope := "/user.slice/user-" + strconv.Itoa(euid) + ".slice/user@" + strconv.Itoa(euid) + ".service/init.scope"
	boot := time.Unix(1_700_000_000, 0).UTC()
	const (
		helperPID    = 2642
		unprovenPID  = 5120
		readablePID  = 6030
		agentRootPID = 6040
	)
	root := t.TempDir()
	writeFakeBootTime(t, root, boot)
	unreadableEnviron := func(t *testing.T, pid int, cgroupPath string, comm string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFakeProcessUID(t, dir, euid)
		writeFakeProcessStat(t, dir, pid, 1, 600)
		writeFakeProcessCgroup(t, dir, cgroupPath)
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatalf("write comm: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "environ"), nil, 0o644); err != nil {
			t.Fatalf("write environ: %v", err)
		}
		if err := os.Chmod(filepath.Join(dir, "environ"), 0o000); err != nil {
			t.Fatalf("chmod environ: %v", err)
		}
	}
	unreadableEnviron(t, helperPID, scope, "(sd-pam)")
	unreadableEnviron(t, unprovenPID, "/user.slice/user-"+strconv.Itoa(euid)+".slice/session-1.scope", "codex")
	buildFakeProcUnder(t, root, readablePID, 1, "claude", map[string]string{"GC_SESSION_ID": "ga-target"})
	buildFakeProcUnder(t, root, agentRootPID, 1, "ssh-agent", map[string]string{"GC_SESSION_ID": "ga-target"})

	scans := map[string]func(root string) ([]int, error){
		"id-scoped scan": func(root string) ([]int, error) {
			got, err := scanWithRoot(root, "ga-target")
			var pids []int
			for _, live := range got {
				pids = append(pids, live.PID)
			}
			return pids, err
		},
		"orphan-candidate scan": func(root string) ([]int, error) {
			got, err := scanOrphanCandidatesWithRoot(root, 0)
			var pids []int
			for _, live := range got {
				pids = append(pids, live.PID)
			}
			return pids, err
		},
	}
	for scanName, scan := range scans {
		t.Run(scanName, func(t *testing.T) {
			pids, err := scan(root)
			if err == nil {
				t.Fatalf("scan error = nil; want the unproven process reported")
			}
			if !slices.Equal(pids, []int{readablePID, agentRootPID}) {
				t.Fatalf("scan returned pids %v, want the readable root %d and the readable credential-agent root %d", pids, readablePID, agentRootPID)
			}
			var entries []*EntryError
			var other []string
			collectScanErrors(err, &entries, &other)
			if len(other) != 0 {
				t.Fatalf("scan error = %v; want no whole-scan failure", err)
			}
			if len(entries) != 1 || entries[0].PID != unprovenPID {
				t.Fatalf("scan error entries = %v; want exactly the unproven pid %d", entries, unprovenPID)
			}
			if summary := SummarizeScanError(err); !strings.Contains(summary, "pid "+strconv.Itoa(unprovenPID)) || strings.Contains(summary, strconv.Itoa(helperPID)) {
				t.Fatalf("summary = %q; want it to identify the unproven process, not the excused helper", summary)
			}
		})
	}
}
