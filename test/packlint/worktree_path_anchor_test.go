package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// worktreePathFromCwdRE matches a worktree path built from the current
// directory: $(pwd), `pwd`, $PWD or ${PWD}, optionally closed by a quote, then
// /worktrees. Issue #6014: formula steps share one long-lived shell, so the
// current directory is wherever an earlier step or the prompt left it, often
// inside the tracked rig checkout.
var worktreePathFromCwdRE = regexp.MustCompile("(?:\\$\\(pwd\\)|`pwd`|\\$PWD|\\$\\{PWD\\})[\"']?/worktrees")

func TestPackScriptsDoNotDeriveWorktreePathFromCwd(t *testing.T) {
	root := repoRoot()
	var violations []string
	for _, dir := range scanDirs {
		abs := filepath.Join(root, dir)
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !scanExts[filepath.Ext(path)] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNum, line := range strings.Split(string(data), "\n") {
				if !worktreePathFromCwdRE.MatchString(line) {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, rel+":"+strconv.Itoa(lineNum+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(violations) > 0 {
		t.Errorf("worktree paths derived from the current directory (see issue #6014).\n"+
			"The shell's cwd is wherever an earlier step left it, so the worktree can land inside a tracked checkout.\n"+
			"Fix: anchor on the session work dir, e.g. \"$GC_DIR/worktrees/$WORK_BEAD_ID\".\n\n%s",
			strings.Join(violations, "\n"))
	}
}

func TestWorktreePathFromCwdPattern(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{line: `WORKTREE_PATH=$(pwd)/worktrees/x`, want: true},
		{line: `WORKTREE_PATH="$(pwd)"/worktrees/"$ID"`, want: true},
		{line: "WORKTREE_PATH=`pwd`/worktrees/x", want: true},
		{line: `WORKTREE_PATH=$PWD/worktrees/x`, want: true},
		{line: `WORKTREE_PATH=${PWD}/worktrees/x`, want: true},
		{line: `WORKTREE_PATH='$PWD'/worktrees/x`, want: true},
		{line: `WORKTREE_PATH=$(pwd)`, want: false},
		{line: `WORKTREE_PATH="$GC_DIR/worktrees/$ID"`, want: false},
		{line: `ls .gc/worktrees/<rig>`, want: false},
	}
	for _, tc := range cases {
		if got := worktreePathFromCwdRE.MatchString(tc.line); got != tc.want {
			t.Errorf("worktreePathFromCwdRE.MatchString(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
