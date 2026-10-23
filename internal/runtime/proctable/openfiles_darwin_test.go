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
