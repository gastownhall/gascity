package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/transcriptmeta"
)

func TestFactoryReconcileTranscriptMetaPageWritesExactClaudeAndCodexSidecars(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()

	const claudeWorkDir = "/data/projects/claude-reconcile"
	claudeKey := "claude-reconcile-key"
	claudePath := filepath.Join(root, sessionlog.ProjectSlug(claudeWorkDir), claudeKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatalf("MkdirAll Claude directory: %v", err)
	}
	// This deliberately is not transcript JSON. The reconciler must resolve only
	// the persisted Claude key and never parse transcript content.
	if err := os.WriteFile(claudePath, []byte("not transcript content\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Claude transcript: %v", err)
	}

	const (
		codexWorkDir = "/data/projects/codex-reconcile"
		codexKey     = "019f0000-0000-7000-8000-000000000001"
	)
	codexPath := writeReconcileCodexRollout(t, root, codexWorkDir, codexKey, time.Now().UTC())
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		transcriptMetaSession("gc-01-claude", "claude", claudeWorkDir, claudeKey, time.Now().Add(-time.Minute)),
		transcriptMetaSession("gc-02-codex", "codex", codexWorkDir, codexKey, time.Now().Add(-time.Minute)),
	}, nil)
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake())
	factory, err := NewFactoryFromManager(manager, []string{root})
	if err != nil {
		t.Fatalf("NewFactoryFromManager: %v", err)
	}

	reconciler, err := factory.NewTranscriptMetaReconciler(2)
	if err != nil {
		t.Fatalf("NewTranscriptMetaReconciler: %v", err)
	}
	result, done, err := reconciler.Next(context.Background())
	if err != nil || !done {
		t.Fatalf("reconciler.Next: done=%v err=%v", done, err)
	}
	if result.Scanned != 2 || result.Resolved != 2 || result.Written != 2 {
		t.Fatalf("result = %+v, want two scanned/resolved/written exact sessions", result)
	}
	assertTranscriptMetaSidecar(t, claudePath, "gc-01-claude")
	assertTranscriptMetaSidecar(t, codexPath, "gc-02-codex")
}

func TestFactoryReconcileTranscriptMetaPageSkipsDisabledMissingAndAmbiguousRecords(t *testing.T) {
	root := t.TempDir()

	const (
		workDir = "/data/projects/reconcile-missing"
		key     = "missing-key"
	)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		transcriptMetaSession("gc-01-missing", "claude", workDir, key, time.Now()),
	}, nil)
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake())
	factory, err := NewFactoryFromManager(manager, []string{root})
	if err != nil {
		t.Fatalf("NewFactoryFromManager: %v", err)
	}

	reconciler, err := factory.NewTranscriptMetaReconciler(8)
	if err != nil {
		t.Fatalf("disabled NewTranscriptMetaReconciler: %v", err)
	}
	if reconciler != nil {
		t.Fatal("disabled reconciler must be nil")
	}

	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	const (
		ambiguousWorkDir = "/data/projects/reconcile-ambiguous"
		ambiguousKey     = "019f0000-0000-7000-8000-000000000002"
	)
	first := writeReconcileCodexRollout(t, root, ambiguousWorkDir, ambiguousKey, time.Now().UTC().Add(-time.Second))
	second := writeReconcileCodexRollout(t, root, ambiguousWorkDir, ambiguousKey, time.Now().UTC())
	if _, err := store.Create(transcriptMetaSession("gc-02-ambiguous", "codex", ambiguousWorkDir, ambiguousKey, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("Create ambiguous session: %v", err)
	}

	reconciler, err = factory.NewTranscriptMetaReconciler(8)
	if err != nil {
		t.Fatalf("enabled NewTranscriptMetaReconciler: %v", err)
	}
	result, done, err := reconciler.Next(context.Background())
	if err != nil || !done {
		t.Fatalf("reconciler.Next: done=%v err=%v", done, err)
	}
	if result.Scanned != 2 || result.Resolved != 0 || result.Written != 0 {
		t.Fatalf("result = %+v, want missing and ambiguous exact-path skips", result)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path + transcriptmeta.Suffix); !os.IsNotExist(err) {
			t.Fatalf("ambiguous sidecar %q stat error = %v, want absent", path+transcriptmeta.Suffix, err)
		}
	}
}

func TestFactoryReconcileTranscriptMetaPageIsLexicalAndIdempotent(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()

	paths := make(map[string]string)
	records := make([]beads.Bead, 0, 3)
	for _, id := range []string{"gc-z", "gc-a", "gc-m"} {
		workDir := "/data/projects/" + id
		key := id + "-key"
		path := filepath.Join(root, sessionlog.ProjectSlug(workDir), key+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not parsed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		records = append(records, transcriptMetaSession(id, "claude", workDir, key, time.Now()))
		paths[id] = path
	}
	store := beads.NewMemStoreFrom(3, records, nil)
	manager := sessionpkg.NewManagerWithOptions(store, runtime.NewFake())
	factory, err := NewFactoryFromManager(manager, []string{root})
	if err != nil {
		t.Fatalf("NewFactoryFromManager: %v", err)
	}

	reconciler, err := factory.NewTranscriptMetaReconciler(2)
	if err != nil {
		t.Fatalf("new reconciler: %v", err)
	}
	first, done, err := reconciler.Next(context.Background())
	if err != nil || done || first.Scanned != 2 {
		t.Fatalf("first page = %+v, want lexical gc-a,gc-m", first)
	}
	assertTranscriptMetaSidecar(t, paths["gc-a"], "gc-a")
	assertTranscriptMetaSidecar(t, paths["gc-m"], "gc-m")
	if _, err := os.Stat(paths["gc-z"] + transcriptmeta.Suffix); !os.IsNotExist(err) {
		t.Fatalf("gc-z sidecar stat error = %v, want absent before next page", err)
	}

	second, done, err := reconciler.Next(context.Background())
	if err != nil || !done || second.Scanned != 1 || second.Written != 1 {
		t.Fatalf("second page = %+v, want terminal gc-z page", second)
	}
	assertTranscriptMetaSidecar(t, paths["gc-z"], "gc-z")

	before, err := os.Stat(paths["gc-a"] + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("Stat sidecar before repeat: %v", err)
	}
	reconciler, err = factory.NewTranscriptMetaReconciler(2)
	if err != nil {
		t.Fatalf("repeat reconciler: %v", err)
	}
	repeated, _, err := reconciler.Next(context.Background())
	if err != nil {
		t.Fatalf("repeat batch: %v", err)
	}
	after, err := os.Stat(paths["gc-a"] + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("Stat sidecar after repeat: %v", err)
	}
	if repeated.Written != 2 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("repeat = %+v, sidecar modtimes %s -> %s; want idempotent unchanged sidecars", repeated, before.ModTime(), after.ModTime())
	}
}

func TestFactoryTranscriptMetaReconcilerUsesOneSnapshotAcrossBatchesAndCompletes(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()
	base := beads.NewMemStoreFrom(3, []beads.Bead{
		transcriptMetaSession("gc-a", "claude", "/data/projects/a", "key-a", time.Now()),
		transcriptMetaSession("gc-b", "claude", "/data/projects/b", "key-b", time.Now()),
		transcriptMetaSession("gc-c", "claude", "/data/projects/c", "key-c", time.Now()),
	}, nil)
	store := &countingListStore{MemStore: base}
	for _, tc := range []struct{ workDir, key string }{
		{"/data/projects/a", "key-a"},
		{"/data/projects/b", "key-b"},
		{"/data/projects/c", "key-c"},
	} {
		path := filepath.Join(root, sessionlog.ProjectSlug(tc.workDir), tc.key+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not parsed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := NewFactoryFromManager(sessionpkg.NewManagerWithOptions(store, runtime.NewFake()), []string{root})
	if err != nil {
		t.Fatalf("NewFactoryFromManager: %v", err)
	}
	reconciler, err := factory.NewTranscriptMetaReconciler(2)
	if err != nil {
		t.Fatalf("NewTranscriptMetaReconciler: %v", err)
	}
	if got := store.listCalls; got != 2 {
		t.Fatalf("snapshot List calls = %d, want type+label union exactly once", got)
	}

	first, done, err := reconciler.Next(context.Background())
	if err != nil || done || first.Scanned != 2 || first.Written != 2 {
		t.Fatalf("first batch = %+v, done=%v, err=%v; want two-row nonterminal batch", first, done, err)
	}
	second, done, err := reconciler.Next(context.Background())
	if err != nil || !done || second.Scanned != 1 || second.Written != 1 {
		t.Fatalf("second batch = %+v, done=%v, err=%v; want terminal one-row batch", second, done, err)
	}
	if got := store.listCalls; got != 2 {
		t.Fatalf("List calls after all batches = %d, want one initial union snapshot", got)
	}
	third, done, err := reconciler.Next(context.Background())
	if err != nil || !done || third != (TranscriptMetaReconcilePage{}) {
		t.Fatalf("completed reconciler Next = %+v, done=%v, err=%v; want no steady-state repeat", third, done, err)
	}
}

func TestTranscriptMetaReconcilerContinuesAfterPerSidecarWriteFailure(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()
	const firstWorkDir = "/data/projects/failing-sidecar"
	failingPath := filepath.Join(root, sessionlog.ProjectSlug(firstWorkDir), "failing-key.jsonl")
	if err := os.MkdirAll(filepath.Dir(failingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/status", failingPath); err != nil {
		t.Fatalf("Symlink failing transcript: %v", err)
	}

	const secondWorkDir = "/data/projects/working-sidecar"
	workingPath := filepath.Join(root, sessionlog.ProjectSlug(secondWorkDir), "working-key.jsonl")
	if err := os.MkdirAll(filepath.Dir(workingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workingPath, []byte("not parsed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStoreFrom(2, []beads.Bead{
		transcriptMetaSession("gc-a-fails", "claude", firstWorkDir, "failing-key", time.Now()),
		transcriptMetaSession("gc-b-works", "claude", secondWorkDir, "working-key", time.Now()),
	}, nil)
	factory, err := NewFactoryFromManager(sessionpkg.NewManagerWithOptions(store, runtime.NewFake()), []string{root})
	if err != nil {
		t.Fatalf("NewFactoryFromManager: %v", err)
	}
	reconciler, err := factory.NewTranscriptMetaReconciler(64)
	if err != nil {
		t.Fatalf("NewTranscriptMetaReconciler: %v", err)
	}
	result, done, err := reconciler.Next(context.Background())
	if err != nil || !done || result.WriteFailures != 1 || result.Written != 1 {
		t.Fatalf("batch = %+v, done=%v, err=%v; want one failure and the later successful sidecar", result, done, err)
	}
	assertTranscriptMetaSidecar(t, workingPath, "gc-b-works")
}

type countingListStore struct {
	*beads.MemStore
	listCalls int
}

func (s *countingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.MemStore.List(query)
}

func transcriptMetaSession(id, provider, workDir, key string, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     id,
		Type:      sessionpkg.BeadType,
		Status:    "closed",
		CreatedAt: createdAt,
		Labels:    []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"provider":      provider,
			"provider_kind": provider,
			"work_dir":      workDir,
			"session_key":   key,
		},
	}
}

func writeReconcileCodexRollout(t *testing.T, root, workDir, key string, at time.Time) string {
	t.Helper()
	day := at.In(time.Local)
	dir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+at.UTC().Format("2006-01-02T15-04-05")+"-"+key+".jsonl")
	content := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":%q}}`+"\n", at.Format(time.RFC3339Nano), key, workDir, at.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertTranscriptMetaSidecar(t *testing.T, transcriptPath, want string) {
	t.Helper()
	got, err := os.ReadFile(transcriptPath + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", transcriptPath+transcriptmeta.Suffix, err)
	}
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("sidecar %q = %q, want %q", transcriptPath, strings.TrimSpace(string(got)), want)
	}
}
