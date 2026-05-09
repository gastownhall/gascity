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

var positionalSessionPeekLineCountRE = regexp.MustCompile(`(?:\bgc|\{\{[^}]+\}\})\s+session\s+peek\s+(?:\{\{[^}]+\}\}\S*|\S+)\s+[0-9]+(?:\D|$)`)

var sessionPeekScanDirs = []string{
	"examples",
	"internal/bootstrap/packs",
	"docs",
}

var sessionPeekScanExts = map[string]bool{
	".toml": true,
	".md":   true,
	".sh":   true,
}

var sessionPeekAllowlistFiles = map[string]bool{}

func TestGcSessionPeekLineCountUsesFlag(t *testing.T) {
	root := repoRoot()
	var violations []string
	for _, dir := range sessionPeekScanDirs {
		abs := filepath.Join(root, dir)
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !sessionPeekScanExts[filepath.Ext(path)] {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if sessionPeekAllowlistFiles[rel] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNum, line := range strings.Split(string(data), "\n") {
				if positionalSessionPeekLineCountRE.MatchString(line) {
					violations = append(violations, rel+":"+strconv.Itoa(lineNum+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(violations) > 0 {
		t.Errorf("gc session peek line counts must use `--lines <n>` instead of a second positional argument.\n"+
			"Fix: replace `gc session peek <target> <n>` with `gc session peek <target> --lines <n>`.\n\n%s",
			strings.Join(violations, "\n"))
	}
}

func TestPositionalSessionPeekLineCountPattern(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		violation bool
	}{
		{name: "positional target and line count", line: `gc session peek <target> 50`, violation: true},
		{name: "template target and line count", line: `gc session peek {{target}} 1`, violation: true},
		{name: "spaced template target and line count", line: `gc session peek {{ .RigName }}/<polecat-name> 50`, violation: true},
		{name: "semicolon delimiter", line: `gc session peek deacon 50; echo done`, violation: true},
		{name: "pipe delimiter", line: `gc session peek deacon 50 | sed -n '1p'`, violation: true},
		{name: "closing paren delimiter", line: `$(gc session peek deacon 50)`, violation: true},
		{name: "backtick delimiter", line: "`gc session peek deacon 50`", violation: true},
		{name: "templated command prefix", line: `{{ cmd }} session peek deacon 30`, violation: true},
		{name: "line count flag", line: `gc session peek <target> --lines 50`, violation: false},
		{name: "default line count", line: `gc session peek <target>`, violation: false},
		{name: "templated command prefix with flag", line: `{{ cmd }} session peek deacon --lines 30`, violation: false},
		{name: "other command", line: `gc session nudge <target> "message"`, violation: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := positionalSessionPeekLineCountRE.MatchString(tc.line)
			if got != tc.violation {
				t.Fatalf("pattern match = %v, want %v for %q", got, tc.violation, tc.line)
			}
		})
	}
}
