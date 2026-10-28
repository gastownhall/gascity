//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// installFakeBdOnPath puts a fake bd first on PATH for the rest of the test,
// and returns the file its runs are recorded in, or "" where nothing records
// them. On Unix the fake is posix, a POSIX script written as an executable bd.
// Windows cannot run a POSIX script, so here the fake is the recording native
// one (provider_owned_fake_bd_test.go), answering with replies: the reply under
// the longest prefix of its space-joined arguments, "" matching every run.
//
// The native fake is this test binary, so while the test runs, any copy of the
// binary it starts is the fake as well.
func installFakeBdOnPath(t *testing.T, _ string, replies map[string]nativeFakeBdReply) string {
	t.Helper()
	bin := nativeFakeBd(t)
	encoded, err := json.Marshal(replies)
	if err != nil {
		t.Fatalf("encoding the fake bd's replies: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "bd-calls.jsonl")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(nativeFakeBdLogEnv, logPath)
	t.Setenv(nativeFakeBdRepliesEnv, string(encoded))
	return logPath
}
