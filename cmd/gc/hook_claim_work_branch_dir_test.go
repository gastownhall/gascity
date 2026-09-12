package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// workBranchDirSpy answers ResolveWorkBranch per directory and records the
// directory the claim asked about, so every row asserts both the directory
// the claim chose and the branch that ends up stamped as gc.work_branch.
type workBranchDirSpy struct {
	branches map[string]string
	dir      string
}

func (s *workBranchDirSpy) resolve(dir string) string {
	s.dir = dir
	return s.branches[dir]
}

func workBranchDirClaimOps(spy *workBranchDirSpy, stamped *map[string]string) hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"gp-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		ResolveWorkBranch: spy.resolve,
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _, _ string, patch map[string]string) error {
			*stamped = patch
			return nil
		},
		PublishRunMap: noopPublishRunMap,
	}
}

// A session that starts in its own worktree (agent work_dir) exports GC_DIR;
// the claim must read the work branch there, not from the store dir, which for
// a rig-scoped bead is the rig root and usually sits on main. Before the fix a
// worker whose lane was on gp-1 got gc.work_branch=main stamped.
//
// Contract pinned here: GC_DIR wins when it is an absolute path naming an
// existing directory (the runtime always exports an absolute path; a symlink
// is passed through as given and git resolves it); anything else — unset,
// empty, relative, missing, or a regular file — falls back to the store dir.
// Duplicate entries follow hookClaimEnvValue: the last one wins.
func TestDoHookClaimResolvesWorkBranchFromSessionWorkDir(t *testing.T) {
	storeDir := t.TempDir()   // the rig root, checked out on main
	sessionDir := t.TempDir() // the worker's lane, checked out on gp-1
	missingDir := filepath.Join(storeDir, "does-not-exist")
	regularFile := filepath.Join(storeDir, "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "lane-link")
	if err := os.Symlink(sessionDir, linkDir); err != nil {
		t.Fatal(err)
	}
	branches := map[string]string{storeDir: "main", sessionDir: "gp-1", linkDir: "gp-1"}

	cases := map[string]struct {
		env        []string
		wantDir    string
		wantBranch string
	}{
		"GC_DIR names the session worktree": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + sessionDir}, wantDir: sessionDir, wantBranch: "gp-1",
		},
		"GC_DIR names the store dir itself (no work_dir: the scope root is the workdir)": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + storeDir}, wantDir: storeDir, wantBranch: "main",
		},
		"no GC_DIR falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1"}, wantDir: storeDir, wantBranch: "main",
		},
		"empty GC_DIR falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR="}, wantDir: storeDir, wantBranch: "main",
		},
		"GC_DIR that does not exist falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + missingDir}, wantDir: storeDir, wantBranch: "main",
		},
		"GC_DIR naming a regular file falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + regularFile}, wantDir: storeDir, wantBranch: "main",
		},
		"relative GC_DIR falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=."}, wantDir: storeDir, wantBranch: "main",
		},
		"symlinked GC_DIR is used as given": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + linkDir}, wantDir: linkDir, wantBranch: "gp-1",
		},
		"duplicate GC_DIR entries: the last one wins": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + storeDir, "GC_DIR=" + sessionDir}, wantDir: sessionDir, wantBranch: "gp-1",
		},
		"trailing empty GC_DIR override falls back to the store dir": {
			env: []string{"GC_SESSION_ID=s1", "GC_DIR=" + sessionDir, "GC_DIR="}, wantDir: storeDir, wantBranch: "main",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spy := &workBranchDirSpy{branches: branches}
			var stamped map[string]string
			opts := hookClaimOptions{
				Assignee:           "s1",
				IdentityCandidates: []string{"s1"},
				RouteTargets:       []string{"worker"},
				Env:                tc.env,
				JSON:               true,
			}
			var stdout, stderr bytes.Buffer
			if code := doHookClaim("bd ready --json", storeDir, opts, workBranchDirClaimOps(spy, &stamped), &stdout, &stderr); code != 0 {
				t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
			}
			if spy.dir != tc.wantDir {
				t.Fatalf("ResolveWorkBranch dir = %q, want %q", spy.dir, tc.wantDir)
			}
			if got := stamped["gc.work_branch"]; got != tc.wantBranch {
				t.Fatalf("stamped gc.work_branch = %q, want %q (patch %v)", got, tc.wantBranch, stamped)
			}
		})
	}
}
