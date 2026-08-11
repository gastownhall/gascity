package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/transcriptmeta"
)

func TestCityRuntimeHistoricalTranscriptMetaPatrolRequiresSupervisorEnablementAndCompletesOnce(t *testing.T) {
	transcriptmeta.SetEnabled(true)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	root := t.TempDir()
	const workDir = "/data/projects/supervisor-transcriptmeta"
	path := filepath.Join(root, sessionlog.ProjectSlug(workDir), "supervisor-key.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("conversation body is never parsed here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:        "gc-supervisor-meta",
		Type:      session.BeadType,
		Status:    "closed",
		CreatedAt: time.Now(),
		Labels:    []string{session.LabelSession},
		Metadata: map[string]string{
			"provider":      "claude",
			"provider_kind": "claude",
			"work_dir":      workDir,
			"session_key":   "supervisor-key",
		},
	}}, nil)

	disabled := &CityRuntime{
		cityPath:              t.TempDir(),
		cfg:                   &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{root}}},
		sp:                    runtime.NewFake(),
		standaloneCityStore:   store,
		stderr:                io.Discard,
		transcriptMetaEnabled: false,
	}
	disabled.startHistoricalTranscriptMetaReconcile(context.Background())
	if _, err := os.Stat(path + transcriptmeta.Suffix); !os.IsNotExist(err) {
		t.Fatalf("disabled supervisor sidecar stat error = %v, want absent", err)
	}

	shutdown := &CityRuntime{
		cityPath:              t.TempDir(),
		cfg:                   &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{root}}},
		sp:                    runtime.NewFake(),
		standaloneCityStore:   store,
		stderr:                io.Discard,
		transcriptMetaEnabled: true,
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	shutdown.startHistoricalTranscriptMetaReconcile(canceled)
	waitTranscriptMetaPatrol(t, shutdown)
	if _, err := os.Stat(path + transcriptmeta.Suffix); !os.IsNotExist(err) {
		t.Fatalf("canceled patrol sidecar stat error = %v, want absent", err)
	}

	enabled := &CityRuntime{
		cityPath:              t.TempDir(),
		cfg:                   &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{root}}},
		sp:                    runtime.NewFake(),
		standaloneCityStore:   store,
		stderr:                io.Discard,
		transcriptMetaEnabled: true,
	}
	enabled.startHistoricalTranscriptMetaReconcile(context.Background())
	waitTranscriptMetaPatrol(t, enabled)
	assertSupervisorTranscriptMeta(t, path, "gc-supervisor-meta")

	if err := os.Remove(path + transcriptmeta.Suffix); err != nil {
		t.Fatalf("Remove sidecar: %v", err)
	}
	// A completed supervisor lifetime stays complete: new-session metadata is
	// owned by the post-turn retry, and historical replay happens only after a
	// restart. Removing the sidecar therefore cannot trigger a steady-state loop.
	enabled.startHistoricalTranscriptMetaReconcile(context.Background())
	if _, err := os.Stat(path + transcriptmeta.Suffix); !os.IsNotExist(err) {
		t.Fatalf("completed patrol sidecar stat error = %v, want no repeat", err)
	}
}

func waitTranscriptMetaPatrol(t *testing.T, cr *CityRuntime) {
	t.Helper()
	select {
	case <-cr.transcriptMetaDone:
	case <-time.After(2 * time.Second):
		t.Fatal("historical transcript metadata patrol did not complete")
	}
}

func assertSupervisorTranscriptMeta(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path + transcriptmeta.Suffix)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path+transcriptmeta.Suffix, err)
	}
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("sidecar = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}
