package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Launch preflight must use the same configured transcript roots as history;
// otherwise a valid isolated Claude conversation is silently replaced.
func TestBuildPreparedStartUsesConfiguredTranscriptRoots(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ownPresent    bool
		parent        string
		parentPresent bool
	}{
		{name: "resume present conversation", ownPresent: true},
		{name: "missing own conversation remains stale"},
		{name: "fork present parent", parent: "parent-conversation", parentPresent: true},
		{name: "missing fork parent remains stale", parent: "parent-conversation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := forkClaude()
			rp.Command = "claude"
			rp.BuiltinAncestor = "claude"
			candidate, cfg, store := newForkSessionCandidate(t, rp, tc.parent, "")
			key := candidate.info.SessionKey
			root := filepath.Join(t.TempDir(), "isolated-claude", "projects")
			cfg.Daemon.ObservePaths = []string{root}
			project := filepath.Join(root, sessionlog.ProjectSlug(candidate.info.WorkDir))
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			for sessionKey, present := range map[string]bool{key: tc.ownPresent, tc.parent: tc.parentPresent} {
				if present {
					if err := os.WriteFile(filepath.Join(project, sessionKey+".jsonl"), []byte("{}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			prepared, after, err := buildPreparedStart(candidate, cfg, store)
			if tc.parent != "" && !tc.parentPresent {
				if err == nil || !strings.Contains(err.Error(), "missing on disk") {
					t.Fatalf("missing parent must fail: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := store.Get(candidate.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.ownPresent:
				if after.SessionKey != key || persisted.Metadata["session_key"] != key || after.ContinuationResetPending == "true" ||
					after.StartedConfigHash != candidate.info.StartedConfigHash || persisted.Metadata["started_config_hash"] != candidate.info.StartedConfigHash ||
					!strings.Contains(prepared.cfg.Command, "--resume "+key) {
					t.Fatalf("valid conversation reset: key=%q reset=%q command=%q", after.SessionKey, after.ContinuationResetPending, prepared.cfg.Command)
				}
			case tc.parentPresent:
				if !strings.Contains(prepared.cfg.Command, "--resume "+tc.parent+" --fork-session") {
					t.Fatalf("valid parent not used for fork: %q", prepared.cfg.Command)
				}
			case after.SessionKey == key || after.ContinuationResetPending != "true":
				t.Fatalf("missing own transcript no longer invalidates stale key: key=%q reset=%q", after.SessionKey, after.ContinuationResetPending)
			}
		})
	}
}
