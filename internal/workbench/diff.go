// Package workbench projects Gas City lifecycle state for the dashboard
// Workbench: execution attempts, their worktrees, and the read-only views over
// them. It owns no lifecycle of its own — Sessions, worktrees, retries, pools
// and agent lifecycle stay Gas City's.
package workbench

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
)

// DiffState is the bounded, explicit state of a worktree diff read.
type DiffState string

const (
	// DiffOK means a non-empty diff was read.
	DiffOK DiffState = "ok"
	// DiffEmpty means the worktree exists and has no changes.
	DiffEmpty DiffState = "empty"
	// DiffMissingWorktree means the attempt names no worktree, or it is gone.
	DiffMissingWorktree DiffState = "missing_worktree"
	// DiffUnavailable means git could not be run (missing, not a repo, error).
	DiffUnavailable DiffState = "unavailable"
)

// DefaultMaxDiffBytes bounds the returned diff text (256 KiB).
const DefaultMaxDiffBytes = 256 * 1024

// Diff is a read-only projection of a worktree's uncommitted changes.
type Diff struct {
	Worktree  string    `json:"worktree"`
	State     DiffState `json:"state"`
	Text      string    `json:"text,omitempty"`
	Truncated bool      `json:"truncated"`
	Binary    bool      `json:"binary"`
	Bytes     int       `json:"bytes"`
}

// WorktreeDiff returns the read-only `git diff` for dir. It NEVER stages,
// discards, commits, or otherwise mutates the worktree: it only reads the diff
// between the index and the working tree. States are explicit so callers can
// render missing/empty/unavailable without treating them as failures.
func WorktreeDiff(ctx context.Context, dir string, maxBytes int) Diff {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Diff{State: DiffMissingWorktree}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return Diff{Worktree: dir, State: DiffMissingWorktree}
	}

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--no-color", "--no-ext-diff")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Diff{Worktree: dir, State: DiffUnavailable}
	}
	raw := stdout.Bytes()
	if len(bytes.TrimSpace(raw)) == 0 {
		return Diff{Worktree: dir, State: DiffEmpty}
	}

	if maxBytes <= 0 {
		maxBytes = DefaultMaxDiffBytes
	}
	truncated := len(raw) > maxBytes
	text := string(raw)
	if truncated {
		text = string(raw[:maxBytes])
	}
	binary := strings.Contains(text, "Binary files") || strings.Contains(text, "GIT binary patch")
	return Diff{
		Worktree:  dir,
		State:     DiffOK,
		Text:      text,
		Truncated: truncated,
		Binary:    binary,
		Bytes:     len(raw),
	}
}
