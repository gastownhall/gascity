package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestBuildPreparedStartPreservesAllocatedKeyUntilFirstTranscript(t *testing.T) {
	candidate, cfg, store := newForkSessionCandidate(t, forkClaude(), "", "")
	candidate.tp.ResolvedProvider.BuiltinAncestor = "claude"
	key := "11111111-2222-4333-8444-555555555555"
	patch := sessionpkg.MetadataPatch{"session_key": key, "started_config_hash": ""}
	if err := sessionFrontDoor(store).ApplyPatch(candidate.info.ID, patch); err != nil {
		t.Fatal(err)
	}
	candidate.info = candidate.info.ApplyPatch(patch)
	for attempt := 0; attempt < 2; attempt++ {
		prepared, info, err := buildPreparedStart(candidate, cfg, store)
		if err != nil {
			t.Fatal(err)
		}
		if info.SessionKey != key {
			t.Fatalf("preparation %d rotated unlaunched key to %q", attempt, info.SessionKey)
		}
		if !strings.Contains(prepared.cfg.Command, "--session-id "+key) {
			t.Fatalf("fresh command = %q", prepared.cfg.Command)
		}
		stored, err := store.Get(candidate.info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Metadata["session_key"] != key {
			t.Fatalf("stored key = %q", stored.Metadata["session_key"])
		}
		candidate.info = info
	}
}

func TestBuildPreparedStartResumesExactObservedTranscript(t *testing.T) {
	candidate, cfg, store := newForkSessionCandidate(t, forkClaude(), "", "")
	candidate.tp.ResolvedProvider.BuiltinAncestor = "claude"
	key := candidate.info.SessionKey
	privateConfig := t.TempDir()
	projects := filepath.Join(privateConfig, "projects")
	directory := filepath.Join(projects, sessionlog.ProjectSlug(candidate.info.WorkDir))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, key+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.ObservePaths = []string{projects}
	candidate.tp.ResolvedProvider.Env = map[string]string{"CLAUDE_CONFIG_DIR": privateConfig}
	prepared, info, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if info.SessionKey != key {
		t.Fatalf("existing observed transcript key replaced: %q", info.SessionKey)
	}
	if !strings.Contains(prepared.cfg.Command, "--resume "+key) || strings.Contains(prepared.cfg.Command, "--session-id") {
		t.Fatalf("resume command = %q", prepared.cfg.Command)
	}
	stored, err := store.Get(candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["session_key"] != key {
		t.Fatalf("stored key = %q", stored.Metadata["session_key"])
	}
}
