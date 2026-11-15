//go:build darwin

package proctable

import (
	"slices"
	"testing"
)

// OpenFiles reads a darwin process's open files from lsof's field output.
// This pins that parse, the half a test can drive: liveScanGuard refuses the
// live process table under go test (gastownhall/gascity#2839).
func TestParseLsofNamePathsKeepsAbsolutePaths(t *testing.T) {
	const rollout = "/Users/user/.codex/sessions/2026/10/15/rollout-2026-10-15T14-36-37-01a13ffe-60c1-7c61-bc2c-87801bfee88f.jsonl"
	out := "p4242\nfcwd\nn/Users/user/city\nf3\nn" + rollout + "\nf7\nn->0x1234\nf9\nnlocalhost:51992\n"
	if got, want := parseLsofNamePaths(out), []string{"/Users/user/city", rollout}; !slices.Equal(got, want) {
		t.Fatalf("parseLsofNamePaths = %q, want %q", got, want)
	}
}

// ProcessCwd reads a darwin process's working directory from lsof's field
// output for its cwd descriptor, which must name exactly one absolute path.
func TestParseLsofCwd(t *testing.T) {
	if got, err := parseLsofCwd(4242, "p4242\nfcwd\nn/Users/user/city\n"); err != nil || got != "/Users/user/city" {
		t.Fatalf("parseLsofCwd = %q, %v; want /Users/user/city", got, err)
	}
	for _, out := range []string{"", "p4242\nfcwd\nn->0x1234\n", "p4242\nfcwd\nn/a\nfcwd\nn/b\n"} {
		if got, err := parseLsofCwd(4242, out); err == nil {
			t.Fatalf("parseLsofCwd(%q) = %q, want an error", out, got)
		}
	}
}
