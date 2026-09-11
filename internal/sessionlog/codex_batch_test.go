package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFindCodexSessionFilesByIDPreservesScalarSemantics(t *testing.T) {
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)

	t.Run("four-digit lifecycle years are not restricted to the current century", func(t *testing.T) {
		for _, year := range []int{1999, 2100} {
			t.Run(fmt.Sprintf("year-%d", year), func(t *testing.T) {
				root := t.TempDir()
				rolloutAt := time.Date(year, 6, 10, 14, 30, 0, 0, time.Local)
				sessionID := fmt.Sprintf("four-digit-year-%d", year)
				workDir := fmt.Sprintf("/work/four-digit-year-%d", year)
				want := writeBatchCodexRolloutAt(t, root, rolloutAt, sessionID, workDir)
				target := CodexSessionTarget{
					Key:       sessionID,
					WorkDir:   workDir,
					SessionID: sessionID,
					NotBefore: rolloutAt,
					NotAfter:  rolloutAt,
				}

				got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
				if got[target.Key] != want {
					t.Fatalf("FindCodexSessionFilesByID[%q] = %q, want %q", target.Key, got[target.Key], want)
				}
			})
		}
	})

	t.Run("UUIDv7 creation day finds rollout predating bead range", func(t *testing.T) {
		root := t.TempDir()
		const workDir = "/work/batch-v7-pre-bead"
		rolloutAt := now.AddDate(0, -3, 0)
		sessionID := codexBatchUUIDv7At(rolloutAt)
		want := writeBatchCodexRolloutAt(t, root, rolloutAt, sessionID, workDir)
		target := CodexSessionTarget{
			Key:       "adopted-session",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if got[target.Key] != want {
			t.Fatalf("FindCodexSessionFilesByID[%q] = %q, want pre-bead v7 rollout %q", target.Key, got[target.Key], want)
		}
		if scalar := FindCodexSessionFileByID([]string{root}, workDir, sessionID, target.NotBefore, target.NotAfter); scalar != want {
			t.Fatalf("FindCodexSessionFileByID = %q, want pre-bead v7 rollout %q", scalar, want)
		}
	})

	t.Run("UUIDv7 creation day padding is inclusive and bounded", func(t *testing.T) {
		root := t.TempDir()
		creationAt := now.AddDate(0, -3, 0)
		beforeID := codexBatchUUIDv7At(creationAt)
		afterID := codexBatchUUIDv7At(creationAt.Add(time.Millisecond))
		outsideID := codexBatchUUIDv7At(creationAt.Add(2 * time.Millisecond))
		beforeWant := writeBatchCodexRolloutAt(t, root, creationAt.AddDate(0, 0, -2), beforeID, "/work/batch-v7-before")
		afterWant := writeBatchCodexRolloutAt(t, root, creationAt.AddDate(0, 0, 2), afterID, "/work/batch-v7-after")
		writeBatchCodexRolloutAt(t, root, creationAt.AddDate(0, 0, 3), outsideID, "/work/batch-v7-outside")
		targets := []CodexSessionTarget{
			{Key: "before", WorkDir: "/work/batch-v7-before", SessionID: beforeID, NotBefore: now.AddDate(0, 0, -1), NotAfter: now},
			{Key: "after", WorkDir: "/work/batch-v7-after", SessionID: afterID, NotBefore: now.AddDate(0, 0, -1), NotAfter: now},
			{Key: "outside", WorkDir: "/work/batch-v7-outside", SessionID: outsideID, NotBefore: now.AddDate(0, 0, -1), NotAfter: now},
		}

		got := FindCodexSessionFilesByID([]string{root}, targets)
		if got["before"] != beforeWant {
			t.Errorf("FindCodexSessionFilesByID[before] = %q, want -2 day path %q", got["before"], beforeWant)
		}
		if got["after"] != afterWant {
			t.Errorf("FindCodexSessionFilesByID[after] = %q, want +2 day path %q", got["after"], afterWant)
		}
		if _, ok := got["outside"]; ok {
			t.Errorf("FindCodexSessionFilesByID[outside] = %q, want +3 day path excluded", got["outside"])
		}
	})

	t.Run("exact match is returned by caller key", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir   = "/work/batch-exact"
			sessionID = "019e9966-aaaa-7000-8000-26a2dd7e1501"
		)
		want := writeBatchCodexRolloutAt(t, root, now.AddDate(0, 0, -5), sessionID, workDir)
		target := CodexSessionTarget{
			Key:       "worker-a",
			WorkDir:   "  " + workDir + "  ",
			SessionID: "  " + sessionID + "  ",
			NotBefore: now.AddDate(0, 0, -6),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if got[target.Key] != want {
			t.Fatalf("FindCodexSessionFilesByID[%q] = %q, want %q", target.Key, got[target.Key], want)
		}
	})

	t.Run("session ID match is exact rather than a suffix", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir = "/work/batch-exact-session-id"
			shortID = "abc"
			longID  = "x-abc"
		)
		want := writeBatchCodexRolloutAt(t, root, now, longID, workDir)
		targets := []CodexSessionTarget{
			{Key: "short", WorkDir: workDir, SessionID: shortID, NotBefore: now, NotAfter: now},
			{Key: "long", WorkDir: workDir, SessionID: longID, NotBefore: now, NotAfter: now},
		}

		got := FindCodexSessionFilesByID([]string{root}, targets)
		if path, ok := got["short"]; ok {
			t.Fatalf("short suffix-only target resolved to %q, want absent", path)
		}
		if got["long"] != want {
			t.Fatalf("exact long target = %q, want %q", got["long"], want)
		}
	})

	t.Run("session ID parser rejects a malformed rollout timestamp", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir   = "/work/batch-malformed-rollout-time"
			sessionID = "malformed-time-session"
		)
		validPath := writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		malformedName := "rollout-" + strings.Repeat("X", len("2006-01-02T15-04-05")) + "-" + sessionID + ".jsonl"
		malformedPath := filepath.Join(filepath.Dir(validPath), malformedName)
		if err := os.Rename(validPath, malformedPath); err != nil {
			t.Fatalf("Rename malformed rollout: %v", err)
		}
		target := CodexSessionTarget{
			Key:       "malformed-time",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now,
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if path, ok := got[target.Key]; ok {
			t.Fatalf("malformed rollout timestamp resolved to %q, want absent", path)
		}
	})

	t.Run("shared session ID keeps each target's eligible window", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir   = "/work/batch-shared-id-windows"
			sessionID = "shared-id-different-windows"
		)
		older := now.AddDate(0, 0, -10)
		newerWant := writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		olderWant := writeBatchCodexRolloutAt(t, root, older, sessionID, workDir)
		targets := []CodexSessionTarget{
			{Key: "newer", WorkDir: workDir, SessionID: sessionID, NotBefore: now, NotAfter: now},
			{Key: "older", WorkDir: workDir, SessionID: sessionID, NotBefore: older, NotAfter: older},
		}

		got := FindCodexSessionFilesByID([]string{root}, targets)
		if got["newer"] != newerWant {
			t.Fatalf("newer target = %q, want %q", got["newer"], newerWant)
		}
		if got["older"] != olderWant {
			t.Fatalf("older target = %q, want %q", got["older"], olderWant)
		}
	})

	t.Run("session meta cwd mismatch is absent", func(t *testing.T) {
		root := t.TempDir()
		const sessionID = "019e9966-aaaa-7000-8000-26a2dd7e1502"
		writeBatchCodexRolloutAt(t, root, now.Add(-time.Hour), sessionID, "/some/other/dir")
		target := CodexSessionTarget{
			Key:       "worker-b",
			WorkDir:   "/work/batch-cwd",
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if _, ok := got[target.Key]; ok {
			t.Fatalf("FindCodexSessionFilesByID unexpectedly returned cwd-mismatched path %q", got[target.Key])
		}
	})

	t.Run("two distinct physical matches are absent", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir   = "/work/batch-ambiguous"
			sessionID = "019e9966-aaaa-7000-8000-26a2dd7e1503"
		)
		writeBatchCodexRolloutAt(t, root, now.Add(-time.Hour), sessionID, workDir)
		// Ambiguity is decided by exact filename identity before the sole
		// candidate's cwd is confirmed, matching the scalar resolver.
		writeBatchCodexRolloutAt(t, root, now.AddDate(0, 0, -2), sessionID, "/wrong/cwd")
		target := CodexSessionTarget{
			Key:       "worker-c",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -3),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if _, ok := got[target.Key]; ok {
			t.Fatalf("FindCodexSessionFilesByID unexpectedly returned ambiguous path %q", got[target.Key])
		}
	})

	t.Run("lifecycle and UUIDv7 hint matches are ambiguous", func(t *testing.T) {
		root := t.TempDir()
		const workDir = "/work/batch-cross-window-ambiguous"
		creationAt := now.AddDate(0, -3, 0)
		sessionID := codexBatchUUIDv7At(creationAt)
		writeBatchCodexRolloutAt(t, root, creationAt, sessionID, workDir)
		writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		target := CodexSessionTarget{
			Key:       "cross-window-ambiguous",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root}, []CodexSessionTarget{target})
		if _, ok := got[target.Key]; ok {
			t.Fatalf("FindCodexSessionFilesByID unexpectedly preferred one cross-window match: %q", got[target.Key])
		}
	})

	t.Run("symlink alias and direct root identify one physical rollout", func(t *testing.T) {
		root := t.TempDir()
		targetRoot := t.TempDir()
		if err := os.Symlink(targetRoot, filepath.Join(root, "aimux-account")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		const (
			workDir   = "/work/batch-symlink"
			sessionID = "019e9966-aaaa-7000-8000-26a2dd7e1504"
		)
		writeBatchCodexRolloutAt(t, targetRoot, now.Add(-time.Hour), sessionID, workDir)
		target := CodexSessionTarget{
			Key:       "worker-d",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{root, targetRoot}, []CodexSessionTarget{target})
		path := got[target.Key]
		if path == "" {
			t.Fatal("FindCodexSessionFilesByID = empty, want rollout behind symlinked extra root")
		}
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatalf("FindCodexSessionFilesByID = %q, want first symlink-lexical path under %q", path, root)
		}
	})

	t.Run("relative and absolute root aliases identify one physical rollout", func(t *testing.T) {
		root := t.TempDir()
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("os.Getwd: %v", err)
		}
		relRoot, err := filepath.Rel(cwd, root)
		if err != nil {
			t.Fatalf("filepath.Rel: %v", err)
		}
		const (
			workDir   = "/work/batch-path-alias"
			sessionID = "019e9966-aaaa-4000-8000-26a2dd7e1508"
		)
		writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		target := CodexSessionTarget{
			Key:       "worker-path-alias",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}

		got := FindCodexSessionFilesByID([]string{relRoot, root}, []CodexSessionTarget{target})
		if got[target.Key] == "" {
			t.Fatal("FindCodexSessionFilesByID = empty, want relative/absolute aliases deduplicated")
		}
	})

	t.Run("newest 370 local days are searched and older days are capped", func(t *testing.T) {
		root := t.TempDir()
		const workDir = "/work/batch-cap"
		writeBatchCodexRolloutAt(t, root, now.AddDate(0, 0, -300), "019e9966-aaaa-7000-8000-26a2dd7e1505", workDir)
		writeBatchCodexRolloutAt(t, root, now.AddDate(0, 0, -400), "019e9966-aaaa-7000-8000-26a2dd7e1506", workDir)
		targets := []CodexSessionTarget{
			{
				Key:       "inside-cap",
				WorkDir:   workDir,
				SessionID: "019e9966-aaaa-7000-8000-26a2dd7e1505",
				NotBefore: now.AddDate(0, 0, -500),
				NotAfter:  now,
			},
			{
				Key:       "outside-cap",
				WorkDir:   workDir,
				SessionID: "019e9966-aaaa-7000-8000-26a2dd7e1506",
				NotBefore: now.AddDate(0, 0, -500),
				NotAfter:  now,
			},
		}

		got := FindCodexSessionFilesByID([]string{root}, targets)
		if got["inside-cap"] == "" {
			t.Fatal("FindCodexSessionFilesByID omitted rollout inside newest-day cap")
		}
		if _, ok := got["outside-cap"]; ok {
			t.Fatalf("FindCodexSessionFilesByID returned capped rollout %q", got["outside-cap"])
		}
	})

	t.Run("invalid and duplicate caller keys are absent", func(t *testing.T) {
		root := t.TempDir()
		const (
			workDir   = "/work/batch-invalid"
			sessionID = "019e9966-aaaa-7000-8000-26a2dd7e1507"
		)
		writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		targets := []CodexSessionTarget{
			{Key: "", WorkDir: workDir, SessionID: sessionID, NotBefore: now, NotAfter: now},
			{Key: "duplicate", WorkDir: workDir, SessionID: sessionID, NotBefore: now, NotAfter: now},
			{Key: "duplicate", WorkDir: workDir, SessionID: sessionID, NotBefore: now, NotAfter: now},
			{Key: "empty-workdir", SessionID: sessionID, NotBefore: now, NotAfter: now},
			{Key: "empty-session", WorkDir: workDir, NotBefore: now, NotAfter: now},
			{Key: "zero-not-after", WorkDir: workDir, SessionID: sessionID, NotBefore: now},
			{Key: "reversed-range", WorkDir: workDir, SessionID: sessionID, NotBefore: now, NotAfter: now.AddDate(0, 0, -3)},
		}

		got := FindCodexSessionFilesByID([]string{root}, targets)
		if len(got) != 0 {
			t.Fatalf("FindCodexSessionFilesByID invalid targets = %#v, want empty map", got)
		}
	})
}

func TestFindCodexSessionFilesByIDReadsEachRootDayOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	const workDir = "/work/batch-scan-count"
	targets := make([]CodexSessionTarget, 8)
	for i := range targets {
		sessionID := codexBatchUUIDv7At(now.Add(time.Duration(i) * time.Millisecond))
		writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		targets[i] = CodexSessionTarget{
			Key:       sessionID,
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now.AddDate(0, 0, -1),
			NotAfter:  now,
		}
	}

	readCounts := make(map[string]int)
	readDir := func(path string) ([]os.DirEntry, error) {
		readCounts[filepath.Clean(path)]++
		return os.ReadDir(path)
	}
	got := findCodexSessionFilesByIDWithReadDir([]string{root}, targets, readDir)
	if len(got) != len(targets) {
		t.Fatalf("findCodexSessionFilesByIDWithReadDir returned %d paths, want %d", len(got), len(targets))
	}

	if got := readCounts[filepath.Clean(root)]; got != 1 {
		t.Fatalf("root ReadDir calls = %d, want 1", got)
	}
	firstDay := startOfLocalDay(targets[0].NotBefore.In(time.Local)).AddDate(0, 0, -1)
	lastDay := startOfLocalDay(targets[0].NotAfter.In(time.Local)).AddDate(0, 0, 1)
	for day := firstDay; !day.After(lastDay); day = day.AddDate(0, 0, 1) {
		dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
		if got := readCounts[filepath.Clean(dayDir)]; got != 1 {
			t.Errorf("ReadDir(%q) calls = %d, want 1 for eight same-range targets", dayDir, got)
		}
	}
	createdAt, ok := codexUUIDv7CreationTime(targets[0].SessionID)
	if !ok {
		t.Fatalf("test session ID %q is not UUIDv7", targets[0].SessionID)
	}
	creationDay := startOfLocalDay(createdAt.In(time.Local))
	for offset := -2; offset <= 2; offset++ {
		day := creationDay.AddDate(0, 0, offset)
		dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
		if got := readCounts[filepath.Clean(dayDir)]; got != 1 {
			t.Errorf("UUIDv7-derived ReadDir(%q) calls = %d, want 1", dayDir, got)
		}
	}
	for path, count := range readCounts {
		if count != 1 {
			t.Errorf("ReadDir(%q) calls = %d, want at most once per physical root/day", path, count)
		}
	}
}

func TestFindCodexSessionFilesByIDSharesCandidateInspectionAcrossDuplicateSessionIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	const (
		workDir         = "/work/batch-shared-session-id"
		sessionID       = "shared-session-id"
		targetsN        = 1000
		irrelevantFiles = 10_000
	)
	dayDir := filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"))
	entries := make([]os.DirEntry, 0, irrelevantFiles+1)
	for i := 0; i < irrelevantFiles; i++ {
		entries = append(entries, codexBatchFakeDirEntry{name: fmt.Sprintf("rollout-2026-06-10T14-30-00-irrelevant-%05d.jsonl", i)})
	}
	candidateName := "rollout-2026-06-10T14-30-00-" + sessionID + ".jsonl"
	entries = append(entries, codexBatchFakeDirEntry{name: candidateName})
	want := filepath.Join(dayDir, candidateName)
	targets := make([]CodexSessionTarget, targetsN)
	for i := range targets {
		targets[i] = CodexSessionTarget{
			Key:       fmt.Sprintf("shared-%04d", i),
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now,
			NotAfter:  now,
		}
	}

	readDir := func(path string) ([]os.DirEntry, error) {
		switch filepath.Clean(path) {
		case filepath.Clean(root):
			return []os.DirEntry{codexBatchFakeDirEntry{name: now.Format("2006"), mode: os.ModeDir}}, nil
		case filepath.Clean(dayDir):
			return entries, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	entryLookups := 0
	cwdReads := 0
	got := findCodexSessionFilesByIDWithReaders(
		[]string{root},
		targets,
		readDir,
		func(path string) string {
			cwdReads++
			if path != want {
				t.Fatalf("readSessionCWD path = %q, want %q", path, want)
			}
			return workDir
		},
		func(name string, targetsBySessionID map[string][]int) (string, bool) {
			entryLookups++
			return lookupCodexBatchEntrySessionID(name, targetsBySessionID)
		},
	)
	if len(got) != targetsN {
		t.Fatalf("duplicate-session targets resolved = %d, want %d", len(got), targetsN)
	}
	if cwdReads != 1 {
		t.Fatalf("candidate cwd reads = %d, want one shared inspection", cwdReads)
	}
	if entryLookups != len(entries) {
		t.Fatalf("entry/session-ID lookups = %d, want one per entry (%d)", entryLookups, len(entries))
	}
}

func TestFindCodexSessionFilesByIDCapsDayReadsPerRoot(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	for year := 2025; year <= 2026; year++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%04d", year)), 0o755); err != nil {
			t.Fatalf("MkdirAll year: %v", err)
		}
	}
	target := CodexSessionTarget{
		Key:       "bounded",
		WorkDir:   "/work/batch-bounded",
		SessionID: "019e9966-aaaa-4000-8000-26a2dd7e1599",
		NotBefore: now.AddDate(0, 0, -1000),
		NotAfter:  now,
	}

	readCounts := make(map[string]int)
	readDir := func(path string) ([]os.DirEntry, error) {
		readCounts[filepath.Clean(path)]++
		return os.ReadDir(path)
	}
	_ = findCodexSessionFilesByIDWithReadDir([]string{root}, []CodexSessionTarget{target}, readDir)

	dayReads := 0
	for path, count := range readCounts {
		if path != filepath.Clean(root) && strings.HasPrefix(path, filepath.Clean(root)+string(filepath.Separator)) {
			dayReads += count
		}
	}
	if dayReads != codexByIDDayDirCap {
		t.Fatalf("day-directory ReadDir calls = %d, want cap %d", dayReads, codexByIDDayDirCap)
	}
	lastDay := startOfLocalDay(target.NotAfter.In(time.Local)).AddDate(0, 0, 1)
	oldestScanned := lastDay.AddDate(0, 0, -(codexByIDDayDirCap - 1))
	oldestDir := filepath.Join(root, oldestScanned.Format("2006"), oldestScanned.Format("01"), oldestScanned.Format("02"))
	if got := readCounts[filepath.Clean(oldestDir)]; got != 1 {
		t.Fatalf("oldest in-cap day ReadDir calls = %d, want 1", got)
	}
	tooOld := oldestScanned.AddDate(0, 0, -1)
	tooOldDir := filepath.Join(root, tooOld.Format("2006"), tooOld.Format("01"), tooOld.Format("02"))
	if got := readCounts[filepath.Clean(tooOldDir)]; got != 0 {
		t.Fatalf("first out-of-cap day ReadDir calls = %d, want 0", got)
	}
}

func TestFindCodexSessionFilesByIDCapsRequestDayUnion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll default Codex root: %v", err)
	}

	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	targets := make([]CodexSessionTarget, 1000)
	for i := range targets {
		day := now.AddDate(0, 0, -5*i)
		sessionID := fmt.Sprintf("disjoint-session-%04d", i)
		targets[i] = CodexSessionTarget{
			Key:       sessionID,
			WorkDir:   "/work/batch-request-day-cap",
			SessionID: sessionID,
			NotBefore: day,
			NotAfter:  day,
		}
	}
	for year := 2012; year <= 2026; year++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%04d", year)), 0o755); err != nil {
			t.Fatalf("MkdirAll year: %v", err)
		}
	}

	dayReads := 0
	readDir := func(path string) ([]os.DirEntry, error) {
		if filepath.Clean(path) != filepath.Clean(root) {
			dayReads++
		}
		return os.ReadDir(path)
	}
	_ = findCodexSessionFilesByIDWithReadDir([]string{root}, targets, readDir)

	const requestDayCap = codexByIDDayDirCap + 5
	if dayReads != requestDayCap {
		t.Fatalf("request day-directory ReadDir calls = %d, want cap %d", dayReads, requestDayCap)
	}
}

func TestFindCodexSessionFilesByIDRejectsPartiallyPlannedTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)

	// The first target fills the entire request union: 370 lifecycle days
	// plus five non-overlapping UUIDv7 creation-hint days.
	anchorID := codexBatchUUIDv7At(now.AddDate(0, 0, -1000))
	partialID := "partial-window-session"
	coveredID := "covered-window-session"
	partialWant := writeBatchCodexRolloutAt(t, root, now, partialID, "/work/batch-partial-window")
	coveredWant := writeBatchCodexRolloutAt(t, root, now, coveredID, "/work/batch-covered-window")
	targets := []CodexSessionTarget{
		{
			Key:       "anchor",
			WorkDir:   "/work/batch-anchor-window",
			SessionID: anchorID,
			NotBefore: now.AddDate(0, 0, -500),
			NotAfter:  now,
		},
		{
			// The padded [now, now+2] range overlaps the anchor except for
			// now+2. Even though its real rollout is in an overlap day, the
			// target must be omitted rather than resolved from a partial
			// ambiguity window.
			Key:       "partial",
			WorkDir:   "/work/batch-partial-window",
			SessionID: partialID,
			NotBefore: now.AddDate(0, 0, 1),
			NotAfter:  now.AddDate(0, 0, 1),
		},
		{
			// This target's full padded range is already in the anchor union,
			// so it remains eligible even after the preceding target is skipped.
			Key:       "covered",
			WorkDir:   "/work/batch-covered-window",
			SessionID: coveredID,
			NotBefore: now,
			NotAfter:  now,
		},
	}

	got := FindCodexSessionFilesByID([]string{root}, targets)
	if path, ok := got["partial"]; ok {
		t.Fatalf("partially planned target resolved to %q (fixture %q), want absent", path, partialWant)
	}
	if got["covered"] != coveredWant {
		t.Fatalf("fully covered target = %q, want %q", got["covered"], coveredWant)
	}
}

func TestFindCodexSessionFilesByIDCapsReadDirAcrossRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	const (
		workDir   = "/work/batch-root-budget"
		sessionID = "root-budget-session"
	)
	writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)

	extraBase := t.TempDir()
	searchPaths := make([]string, 0, 21)
	searchPaths = append(searchPaths, root)
	for i := 0; i < 20; i++ {
		extraRoot := filepath.Join(extraBase, fmt.Sprintf("root-%02d", i))
		for year := 2025; year <= 2026; year++ {
			if err := os.MkdirAll(filepath.Join(extraRoot, fmt.Sprintf("%04d", year)), 0o755); err != nil {
				t.Fatalf("MkdirAll extra root year: %v", err)
			}
		}
		searchPaths = append(searchPaths, extraRoot)
	}

	target := CodexSessionTarget{
		Key:       "bounded-roots",
		WorkDir:   workDir,
		SessionID: sessionID,
		NotBefore: now.AddDate(0, 0, -500),
		NotAfter:  now,
	}
	readCalls := 0
	readDir := func(path string) ([]os.DirEntry, error) {
		readCalls++
		return os.ReadDir(path)
	}
	got := findCodexSessionFilesByIDWithReadDir(searchPaths, []CodexSessionTarget{target}, readDir)

	const requestReadDirCap = 4096
	if readCalls != requestReadDirCap {
		t.Fatalf("request ReadDir calls = %d, want exhausted cap %d", readCalls, requestReadDirCap)
	}
	if path, ok := got[target.Key]; ok {
		t.Fatalf("budget-exhausted lookup returned early partial match %q, want fail closed", path)
	}
}

func TestFindCodexSessionFilesByIDSkipsRootsWithoutEligibleYear(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	const (
		workDir   = "/work/batch-year-index"
		sessionID = "year-index-session"
	)
	want := writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
	target := CodexSessionTarget{
		Key:       "year-index",
		WorkDir:   workDir,
		SessionID: sessionID,
		NotBefore: now.AddDate(0, 0, -500),
		NotAfter:  now,
	}

	// Mirror a live multi-provider search layout: ten physical roots carry
	// relevant Codex years (the default plus nine extras), fourteen existing
	// roots do not, and two configured roots are missing.
	extraBase := t.TempDir()
	searchPaths := make([]string, 0, 26)
	searchPaths = append(searchPaths, root)
	for i := 0; i < 25; i++ {
		extraRoot := filepath.Join(extraBase, fmt.Sprintf("root-%02d", i))
		switch {
		case i < 9:
			for year := 2025; year <= 2026; year++ {
				if err := os.MkdirAll(filepath.Join(extraRoot, fmt.Sprintf("%04d", year)), 0o755); err != nil {
					t.Fatalf("MkdirAll relevant root year: %v", err)
				}
			}
		case i < 23:
			if err := os.MkdirAll(extraRoot, 0o755); err != nil {
				t.Fatalf("MkdirAll irrelevant root: %v", err)
			}
			// The final two roots deliberately remain absent.
		}
		searchPaths = append(searchPaths, extraRoot)
	}

	readCalls := 0
	readDir := func(path string) ([]os.DirEntry, error) {
		readCalls++
		return os.ReadDir(path)
	}
	got := findCodexSessionFilesByIDWithReadDir(searchPaths, []CodexSessionTarget{target}, readDir)
	if got[target.Key] != want {
		t.Fatalf("many-root lookup = %q, want %q", got[target.Key], want)
	}
	const requestReadDirCap = 4096
	if readCalls >= requestReadDirCap {
		t.Fatalf("many-root ReadDir calls = %d, want below cap %d after year pruning", readCalls, requestReadDirCap)
	}
}

func TestFindCodexSessionFilesByIDIgnoresRegularFileNamedAsEligibleYear(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".codex", "sessions")
	now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
	const (
		workDir   = "/work/batch-regular-year-file"
		sessionID = "regular-year-file-session"
	)
	want := writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
	misleadingRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(misleadingRoot, now.Format("2006")), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile misleading year: %v", err)
	}
	target := CodexSessionTarget{
		Key:       "regular-year-file",
		WorkDir:   workDir,
		SessionID: sessionID,
		NotBefore: now,
		NotAfter:  now,
	}

	got := FindCodexSessionFilesByID([]string{root, misleadingRoot}, []CodexSessionTarget{target})
	if got[target.Key] != want {
		t.Fatalf("lookup with regular year file = %q, want %q", got[target.Key], want)
	}
}

func TestFindCodexSessionFilesByIDFailsClosedOnReadDirError(t *testing.T) {
	newFixture := func(t *testing.T) (string, string, time.Time, CodexSessionTarget) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, ".codex", "sessions")
		now := time.Date(2026, 6, 10, 14, 30, 0, 0, time.Local)
		extraRoot := filepath.Join(t.TempDir(), "extra-root")
		if err := os.MkdirAll(filepath.Join(extraRoot, now.Format("2006")), 0o755); err != nil {
			t.Fatalf("MkdirAll extra root year: %v", err)
		}
		const (
			workDir   = "/work/batch-read-error"
			sessionID = "read-error-session"
		)
		writeBatchCodexRolloutAt(t, root, now, sessionID, workDir)
		return root, extraRoot, now, CodexSessionTarget{
			Key:       "read-error",
			WorkDir:   workDir,
			SessionID: sessionID,
			NotBefore: now,
			NotAfter:  now,
		}
	}

	t.Run("unreadable root can conceal a duplicate", func(t *testing.T) {
		root, extraRoot, _, target := newFixture(t)
		readDir := func(path string) ([]os.DirEntry, error) {
			if filepath.Clean(path) == filepath.Clean(extraRoot) {
				return nil, os.ErrPermission
			}
			return os.ReadDir(path)
		}

		got := findCodexSessionFilesByIDWithReadDir([]string{root, extraRoot}, []CodexSessionTarget{target}, readDir)
		if path, ok := got[target.Key]; ok {
			t.Fatalf("lookup returned %q after unreadable root, want fail closed", path)
		}
	})

	t.Run("unreadable eligible day can conceal a duplicate", func(t *testing.T) {
		root, extraRoot, now, target := newFixture(t)
		unreadableDay := filepath.Join(extraRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
		readDir := func(path string) ([]os.DirEntry, error) {
			if filepath.Clean(path) == filepath.Clean(unreadableDay) {
				return nil, syscall.EIO
			}
			return os.ReadDir(path)
		}

		got := findCodexSessionFilesByIDWithReadDir([]string{root, extraRoot}, []CodexSessionTarget{target}, readDir)
		if path, ok := got[target.Key]; ok {
			t.Fatalf("lookup returned %q after unreadable eligible day, want fail closed", path)
		}
	})

	t.Run("unreadable symlinked extra root can conceal a duplicate", func(t *testing.T) {
		root, extraRoot, _, target := newFixture(t)
		linkedRoot := filepath.Join(root, "aimux-account")
		if err := os.Symlink(extraRoot, linkedRoot); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		readDir := func(path string) ([]os.DirEntry, error) {
			if filepath.Clean(path) == filepath.Clean(linkedRoot) {
				return nil, os.ErrPermission
			}
			return os.ReadDir(path)
		}

		got := findCodexSessionFilesByIDWithReadDir([]string{root}, []CodexSessionTarget{target}, readDir)
		if path, ok := got[target.Key]; ok {
			t.Fatalf("lookup returned %q after unreadable symlinked root, want fail closed", path)
		}
	})

	t.Run("wrapped missing root remains a safe negative", func(t *testing.T) {
		root, _, _, target := newFixture(t)
		missingRoot := filepath.Join(t.TempDir(), "missing-root")
		readDir := func(path string) ([]os.DirEntry, error) {
			if filepath.Clean(path) == filepath.Clean(missingRoot) {
				return nil, &os.PathError{Op: "readdir", Path: path, Err: syscall.ENOENT}
			}
			return os.ReadDir(path)
		}

		got := findCodexSessionFilesByIDWithReadDir([]string{root, missingRoot}, []CodexSessionTarget{target}, readDir)
		if got[target.Key] == "" {
			t.Fatal("lookup omitted exact match after wrapped ENOENT, want missing root ignored")
		}
	})
}

func codexBatchUUIDv7At(ts time.Time) string {
	millis := fmt.Sprintf("%012x", ts.UTC().UnixMilli())
	return millis[:8] + "-" + millis[8:] + "-7000-8000-000000000001"
}

func writeBatchCodexRolloutAt(t *testing.T, root string, ts time.Time, sessionID, cwd string) string {
	t.Helper()
	local := ts.In(time.Local)
	dir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
	entry := struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			CWD       string `json:"cwd"`
		} `json:"payload"`
	}{
		Timestamp: ts.UTC().Format(time.RFC3339Nano),
		Type:      "session_meta",
	}
	entry.Payload.ID = sessionID
	entry.Payload.Timestamp = entry.Timestamp
	entry.Payload.CWD = cwd
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

type codexBatchFakeDirEntry struct {
	name string
	mode os.FileMode
}

func (e codexBatchFakeDirEntry) Name() string               { return e.name }
func (e codexBatchFakeDirEntry) IsDir() bool                { return e.mode.IsDir() }
func (e codexBatchFakeDirEntry) Type() os.FileMode          { return e.mode.Type() }
func (e codexBatchFakeDirEntry) Info() (os.FileInfo, error) { return nil, os.ErrInvalid }
