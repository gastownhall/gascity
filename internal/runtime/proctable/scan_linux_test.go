//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

func TestParseProcStatIdentityCarriesParentGroupAndStart(t *testing.T) {
	// The suffix begins at field 3 (state), so suffix index 19 is Linux
	// /proc/<pid>/stat field 22 (starttime); the trailing 0 is field 23.
	stat := "42 (cmd (worker)) S 11 12 13 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 98765 0"

	ppid, pgid, startTime, ok, err := parseProcStatIdentity(stat)
	if err != nil {
		t.Fatalf("parseProcStatIdentity: %v", err)
	}
	if !ok {
		t.Fatal("parseProcStatIdentity reported missing record")
	}
	if ppid != 11 || pgid != 12 || startTime != "98765" {
		t.Fatalf("parseProcStatIdentity = (%d, %d, %q), want (11, 12, %q)", ppid, pgid, startTime, "98765")
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
	restore := SetScanRootForTesting(root)
	defer restore()
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
	restore := SetScanRootForTesting(root)
	defer restore()
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
