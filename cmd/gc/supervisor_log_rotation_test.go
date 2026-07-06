package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// setSupervisorLogRotation lowers the supervisor log rotation tunables for
// the duration of a test and restores the defaults afterwards.
func setSupervisorLogRotation(t *testing.T, maxBytes int64, keep int) {
	t.Helper()
	prevMax, prevKeep := supervisorLogMaxBytes, supervisorLogKeepArchives
	supervisorLogMaxBytes, supervisorLogKeepArchives = maxBytes, keep
	t.Cleanup(func() {
		supervisorLogMaxBytes, supervisorLogKeepArchives = prevMax, prevKeep
	})
}

// gunzipSupervisorLogArchive decompresses one supervisor log archive and
// returns its contents.
func gunzipSupervisorLogArchive(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only test handle
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader for %s: %v", path, err)
	}
	defer gz.Close() //nolint:errcheck // read-only test handle
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	return string(data)
}

// listSupervisorLogArchives returns the sorted archive paths next to the
// given supervisor log path.
func listSupervisorLogArchives(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".archive-*.gz")
	if err != nil {
		t.Fatalf("glob archives for %s: %v", path, err)
	}
	sort.Strings(matches)
	return matches
}

func TestMaybeRotateSupervisorLogBelowCapIsNoop(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1<<20, 3)
	content := "steady operational line\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if err := maybeRotateSupervisorLog(logPath, time.Now(), io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != content {
		t.Fatalf("log = %q, want untouched %q", got, content)
	}
	if archives := listSupervisorLogArchives(t, logPath); len(archives) != 0 {
		t.Fatalf("archives = %v, want none below cap", archives)
	}
}

func TestMaybeRotateSupervisorLogMissingFileIsNoop(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1, 3)

	if err := maybeRotateSupervisorLog(logPath, time.Now(), io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog on missing file: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("stat log = %v, want not-exist (rotation must not create it)", err)
	}
}

func TestMaybeRotateSupervisorLogArchivesAndTruncates(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1024, 3)
	content := strings.Repeat("api: listen 127.0.0.1:8372 failed: bind: address already in use\n", 64)
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	now := time.Date(2026, 7, 3, 10, 11, 12, 0, time.UTC)
	if err := maybeRotateSupervisorLog(logPath, now, io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat active log after rotation: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("active log size = %d, want 0 after rotation", info.Size())
	}

	archives := listSupervisorLogArchives(t, logPath)
	want := []string{logPath + ".archive-20260703T101112Z.gz"}
	if len(archives) != 1 || archives[0] != want[0] {
		t.Fatalf("archives = %v, want %v", archives, want)
	}
	if got := gunzipSupervisorLogArchive(t, archives[0]); got != content {
		t.Fatalf("archive content mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
	if _, err := os.Stat(logPath + ".archive.tmp"); !os.IsNotExist(err) {
		t.Fatalf("stat archive tmp = %v, want not-exist after successful rotation", err)
	}
}

// TestMaybeRotateSupervisorLogPreservesAppendWriters proves the
// copy-then-truncate strategy keeps already-open O_APPEND writers attached
// to the active file — the shape service managers use (systemd
// StandardOutput=append:, launchd StandardOutPath). A rename-based rotation
// would divert those writers into the archive.
func TestMaybeRotateSupervisorLogPreservesAppendWriters(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 8, 3)
	if err := os.WriteFile(logPath, []byte("historic crash-loop noise\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	writer, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append writer: %v", err)
	}
	defer writer.Close() //nolint:errcheck // test handle

	if err := maybeRotateSupervisorLog(logPath, time.Now(), io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	if _, err := writer.WriteString("post-rotation line\n"); err != nil {
		t.Fatalf("write via pre-rotation fd: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	if string(got) != "post-rotation line\n" {
		t.Fatalf("active log = %q, want only the post-rotation line at offset 0", got)
	}
}

func TestMaybeRotateSupervisorLogSameSecondRotationsGetUniqueNames(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 8, 5)
	now := time.Date(2026, 7, 3, 10, 11, 12, 0, time.UTC)

	for _, generation := range []string{"first generation\n", "second generation\n"} {
		if err := os.WriteFile(logPath, []byte(generation), 0o644); err != nil {
			t.Fatalf("seed log: %v", err)
		}
		if err := maybeRotateSupervisorLog(logPath, now, io.Discard); err != nil {
			t.Fatalf("maybeRotateSupervisorLog(%q): %v", generation, err)
		}
	}

	archives := listSupervisorLogArchives(t, logPath)
	want := []string{
		logPath + ".archive-20260703T101112Z-1.gz",
		logPath + ".archive-20260703T101112Z.gz",
	}
	if len(archives) != 2 || archives[0] != want[0] || archives[1] != want[1] {
		t.Fatalf("archives = %v, want %v", archives, want)
	}
	if got := gunzipSupervisorLogArchive(t, logPath+".archive-20260703T101112Z.gz"); got != "first generation\n" {
		t.Fatalf("first archive = %q, want first generation", got)
	}
	if got := gunzipSupervisorLogArchive(t, logPath+".archive-20260703T101112Z-1.gz"); got != "second generation\n" {
		t.Fatalf("second archive = %q, want second generation", got)
	}
}

func TestMaybeRotateSupervisorLogPrunesOldestArchives(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "supervisor.log")
	setSupervisorLogRotation(t, 8, 2)

	now := time.Date(2026, 7, 3, 10, 11, 12, 0, time.UTC)
	preexisting := []struct {
		name string
		mod  time.Time
	}{
		{"supervisor.log.archive-20260703T070000Z.gz", now.Add(-3 * time.Hour)},
		{"supervisor.log.archive-20260703T080000Z.gz", now.Add(-2 * time.Hour)},
		{"supervisor.log.archive-20260703T090000Z.gz", now.Add(-1 * time.Hour)},
	}
	for _, a := range preexisting {
		p := filepath.Join(dir, a.name)
		if err := os.WriteFile(p, []byte("old archive"), 0o644); err != nil {
			t.Fatalf("seed archive %s: %v", a.name, err)
		}
		if err := os.Chtimes(p, a.mod, a.mod); err != nil {
			t.Fatalf("chtimes %s: %v", a.name, err)
		}
	}
	if err := os.WriteFile(logPath, []byte("over-cap active log\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if err := maybeRotateSupervisorLog(logPath, now, io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	archives := listSupervisorLogArchives(t, logPath)
	want := []string{
		logPath + ".archive-20260703T090000Z.gz",
		logPath + ".archive-20260703T101112Z.gz",
	}
	if len(archives) != 2 || archives[0] != want[0] || archives[1] != want[1] {
		t.Fatalf("archives = %v, want oldest pruned down to %v", archives, want)
	}
}

func TestMaybeRotateSupervisorLogDirectoryPathErrors(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1, 3)
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatalf("seed log path as directory: %v", err)
	}

	err := maybeRotateSupervisorLog(logPath, time.Now(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("maybeRotateSupervisorLog on directory = %v, want is-a-directory error", err)
	}
}

// TestMaybeRotateSupervisorLogBoundsArchiveAndWarnsOnDroppedTail pins the
// archive bound: an oversized log is archived only up to
// supervisorLogArchiveMaxBytes, the tail beyond the bound is dropped with a
// warning, and the active log is still truncated.
func TestMaybeRotateSupervisorLogBoundsArchiveAndWarnsOnDroppedTail(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1024, 3)
	prevBound := supervisorLogArchiveMaxBytes
	supervisorLogArchiveMaxBytes = 2048
	t.Cleanup(func() { supervisorLogArchiveMaxBytes = prevBound })

	content := strings.Repeat("b", 8192)
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	var warn bytes.Buffer
	if err := maybeRotateSupervisorLog(logPath, time.Now(), &warn); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	archives := listSupervisorLogArchives(t, logPath)
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want exactly one", archives)
	}
	if got := gunzipSupervisorLogArchive(t, archives[0]); got != content[:2048] {
		t.Fatalf("archive holds %d bytes, want the first 2048 bytes of the log", len(got))
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat active log: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("active log size = %d, want 0 after rotation", info.Size())
	}
	if !strings.Contains(warn.String(), "archive bound") {
		t.Fatalf("warn = %q, want dropped-tail notice mentioning the archive bound", warn.String())
	}
}

// TestMaybeRotateSupervisorLogSweepsStagingLeftovers pins the staging
// sweep: .archive-*.tmp leftovers from rotations that crashed between
// staging and rename are removed by the next successful rotation.
func TestMaybeRotateSupervisorLogSweepsStagingLeftovers(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	setSupervisorLogRotation(t, 1024, 3)
	leftover := logPath + ".archive-12345.tmp"
	if err := os.WriteFile(leftover, []byte("half-staged archive"), 0o644); err != nil {
		t.Fatalf("seed staging leftover: %v", err)
	}
	content := strings.Repeat("api: listen 127.0.0.1:8372 failed\n", 64)
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	if err := maybeRotateSupervisorLog(logPath, time.Now(), io.Discard); err != nil {
		t.Fatalf("maybeRotateSupervisorLog: %v", err)
	}

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("stat staging leftover = %v, want swept away", err)
	}
	if archives := listSupervisorLogArchives(t, logPath); len(archives) != 1 {
		t.Fatalf("archives = %v, want exactly one intact archive", archives)
	}
}

// tryGunzipSupervisorLogArchive decompresses one archive, returning an
// error instead of failing the test so concurrency tests can treat a
// corrupt archive as "contributes nothing" rather than aborting.
func tryGunzipSupervisorLogArchive(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only test handle
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close() //nolint:errcheck // read-only test handle
	data, err := io.ReadAll(gz)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// TestMaybeRotateSupervisorLogConcurrentRotationsNeverLoseContent is the
// concurrency regression for the #3897 hardening. Rotation used to stage
// every racer's archive at one fixed path (<log>.archive.tmp), so one
// racer's O_TRUNC wiped another's staged bytes before its rename: the
// corrupt archive was renamed into place and the truncate then destroyed
// the only intact copy of the log. Production serializes rotation under
// the supervisor single-instance lock; this test drives the unlocked
// rotation directly to pin the second layer of defense — per-rotation
// os.CreateTemp staging must never lose content even without the lock.
func TestMaybeRotateSupervisorLogConcurrentRotationsNeverLoseContent(t *testing.T) {
	// keep must exceed the racer count: pruning a racer's full archive
	// mid-race would read as a false content loss.
	setSupervisorLogRotation(t, 1024, 32)
	// Multi-megabyte content forces the gzip copy through several write
	// syscalls, so racing rotations genuinely interleave instead of each
	// landing one atomic write.
	content := strings.Repeat("api: listen 127.0.0.1:8372 failed: bind: address already in use\n", 32*1024)

	const rounds = 10
	const racers = 16
	for round := 0; round < rounds; round++ {
		logPath := filepath.Join(t.TempDir(), "supervisor.log")
		if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
			t.Fatalf("round %d: seed log: %v", round, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				// Stagger arrivals across the rotation window so late
				// racers hit every phase of an in-flight rotation, the
				// shape of supervisor processes racing at startup.
				time.Sleep(time.Duration(i) * 500 * time.Microsecond)
				// Individual racers may error (losing a staging or rename
				// race); the invariant under test is content survival, not
				// per-call success.
				_ = maybeRotateSupervisorLog(logPath, time.Now(), io.Discard)
			}(i)
		}
		close(start)
		wg.Wait()

		fullCopies := 0
		archives := listSupervisorLogArchives(t, logPath)
		for _, a := range archives {
			if got, err := tryGunzipSupervisorLogArchive(a); err == nil && got == content {
				fullCopies++
			}
		}
		if active, err := os.ReadFile(logPath); err == nil && string(active) == content {
			fullCopies++
		}
		if fullCopies == 0 {
			t.Fatalf("round %d: original log content lost: no intact copy across %d archives and the active log", round, len(archives))
		}
	}
}

// TestRunSupervisorRotatesOversizedLogAtStartup confirms the wiring: a
// supervisor start with an over-cap supervisor.log archives it before any
// new output is appended, so a service-manager crash-loop (the #3897
// incident shape: launchd KeepAlive relaunching a bind-failing supervisor
// every few seconds for two days, 645MB log) is bounded at the cap plus a
// handful of compressed archives.
func TestRunSupervisorRotatesOversizedLogAtStartup(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	setSupervisorLogRotation(t, 1024, 3)

	logPath := filepath.Join(gcHome, "supervisor.log")
	historic := strings.Repeat("api: listen 127.0.0.1:8372 failed: bind: address already in use\n", 64)
	if err := os.WriteFile(logPath, []byte(historic), 0o644); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}

	oldLoadConfig := supervisorLoadConfig
	supervisorLoadConfig = func(string) (supervisor.Config, error) {
		return supervisor.Config{}, errors.New("stop after log setup")
	}
	t.Cleanup(func() { supervisorLoadConfig = oldLoadConfig })

	var stdout, stderr bytes.Buffer
	if code := runSupervisor(&stdout, &stderr); code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	archives := listSupervisorLogArchives(t, logPath)
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want exactly one after startup rotation", archives)
	}
	if got := gunzipSupervisorLogArchive(t, archives[0]); got != historic {
		t.Fatalf("archive content mismatch: got %d bytes, want %d bytes", len(got), len(historic))
	}

	active, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	if strings.Contains(string(active), "address already in use") {
		t.Fatalf("active log still holds pre-rotation content: %q", active)
	}
	if !strings.Contains(string(active), "stop after log setup") {
		t.Fatalf("active log = %q, want the new instance's output after rotation", active)
	}
}
