package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
)

// scaffoldRepo builds a real git repo shaped like a live gc worktree: the
// tracked files gc rewrites in place are committed first, then gc's injections
// are applied on top.
//
// ".claude/skills" gets a committed file on purpose. That is what makes git
// descend into the directory and report gc's materialized skill entries
// individually instead of collapsing them into a single "?? .claude/", which
// is exactly the shape the gascity rig produces (its .gitignore un-ignores
// .claude/skills/ so the docs skill can be tracked).
func scaffoldRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustGit(t, repo, "init")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Gas City Test")

	// ".gc/" is committed as ignored, matching what gc init writes into a rig
	// repo: gc's runtime state directory never reaches porcelain in the field.
	writeRepoFile(t, repo, ".gitignore", "node_modules/\n.gc/\n")
	writeRepoFile(t, repo, ".mcp.json", `{"mcpServers":{"excalidraw":{"type":"http"}}}`+"\n")
	writeRepoFile(t, repo, filepath.Join(".claude", "skills", "docs-skill", "SKILL.md"), "docs\n")
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-m", "init")

	// --- everything below is gc scaffolding, not agent work ---

	writeRepoFile(t, repo, ".gitignore", "node_modules/\n.gc/\n\n# Gas City\n.mcp.json\nopencode.json\n")

	mcpContent := `{"mcpServers":{"bartertown":{"command":"/x/bartertown_mcp.py"}}}` + "\n"
	writeRepoFile(t, repo, ".mcp.json", mcpContent)
	sum := sha256.Sum256([]byte(mcpContent))
	marker, err := json.Marshal(map[string]string{
		"managed_by":     "gc",
		"provider":       "claude",
		"content_sha256": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("marshaling marker: %v", err)
	}
	writeRepoFile(t, repo, filepath.Join(".gc", "mcp-managed", "claude.json"), string(marker))

	manifest, err := json.Marshal(map[string]any{
		"targets": map[string]string{"core.gc-work": "/cache/skills/gc-work"},
	})
	if err != nil {
		t.Fatalf("marshaling manifest: %v", err)
	}
	writeRepoFile(t, repo, filepath.Join(".claude", "skills", ".gc-skill-ownership.json"), string(manifest))
	writeRepoFile(t, repo, filepath.Join(".claude", "skills", "core.gc-work"), "symlink-stand-in")

	return repo
}

func writeRepoFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
}

// The regression this whole change exists for: a worktree holding nothing but
// gc's injections is dirty to git and must still be disposable (gas-wr8).
func TestWorktreeHasUnsalvagedWorkIgnoresGCScaffolding(t *testing.T) {
	repo := scaffoldRepo(t)
	wg := git.New(repo)

	// Guard: the setup must actually reproduce the dirty tree, or the test
	// would pass for the wrong reason.
	porcelain, err := wg.StatusPorcelain()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if porcelain == "" {
		t.Fatal("setup did not produce a dirty worktree; nothing is being tested")
	}
	if !wg.HasUncommittedWork() {
		t.Fatal("setup did not trip the old bare-porcelain gate; nothing is being tested")
	}

	hasWork, err := worktreeHasUnsalvagedWork(wg, repo)
	if err != nil {
		t.Fatalf("probing: %v", err)
	}
	if hasWork {
		t.Fatalf("pure gc scaffolding reported as unsalvaged work:\n%s", porcelain)
	}
}

func TestWorktreeHasUnsalvagedWorkProtectsRealEdits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, repo string)
	}{
		{"tracked file modified", func(t *testing.T, repo string) {
			writeRepoFile(t, repo, filepath.Join(".claude", "skills", "docs-skill", "SKILL.md"), "edited\n")
		}},
		{"new untracked file", func(t *testing.T, repo string) {
			writeRepoFile(t, repo, "notes.md", "in progress\n")
		}},
		{"hand-placed skill beside gc's", func(t *testing.T, repo string) {
			writeRepoFile(t, repo, filepath.Join(".claude", "skills", "my-skill"), "mine\n")
		}},
		{"real edit alongside the Gas City block", func(t *testing.T, repo string) {
			writeRepoFile(t, repo, ".gitignore", "node_modules/\n.gc/\ncoverage.out\n\n# Gas City\n.mcp.json\nopencode.json\n")
		}},
		{"gc-managed MCP config edited by hand", func(t *testing.T, repo string) {
			writeRepoFile(t, repo, ".mcp.json", `{"mcpServers":{"mine":{}}}`+"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := scaffoldRepo(t)
			tc.apply(t, repo)

			hasWork, err := worktreeHasUnsalvagedWork(git.New(repo), repo)
			if err != nil {
				t.Fatalf("probing: %v", err)
			}
			if !hasWork {
				t.Fatal("real work was subtracted as scaffolding; a reap here would destroy it")
			}
		})
	}
}
