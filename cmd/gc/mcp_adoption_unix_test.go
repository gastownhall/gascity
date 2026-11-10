//go:build unix

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A provider file that is a symlink is refused without reading what it
// points at: a repository can commit .mcp.json -> /dev/zero, which never
// ends, or a link to a named pipe, which blocks. Stage 1 returns at once and
// warns once.
func TestRunStage1MCPProjectionNeverReadsThroughASymlinkedTarget(t *testing.T) {
	cityPath := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	target := filepath.Join(cityPath, ".mcp.json")
	if err := os.Symlink(fifo, target); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Agents:     []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
	}
	var stderr bytes.Buffer
	for tick := 0; tick < 3; tick++ {
		var err error
		returnsPromptlyCmd(t, fifo, func() { err = runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr) })
		if err != nil {
			t.Fatalf("tick %d: runStage1MCPProjection: %v", tick, err)
		}
	}
	if got := strings.Count(stderr.String(), target+" is a symlink"); got != 1 {
		t.Fatalf("symlink refusal warned %d times over 3 ticks, want once:\n%s", got, stderr.String())
	}
}

// mcpTargetStamp hashes a regular file of at most the cap; anything else it
// stamps by type or size, unread.
func TestMCPTargetStampReadsOnlyRegularFilesUpToTheCap(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(regular, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if stamp := mcpTargetStamp(fsys.OSFS{}, regular); !strings.HasPrefix(stamp, "sha256:") {
		t.Fatalf("stamp(regular) = %q", stamp)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	var stamp string
	returnsPromptlyCmd(t, fifo, func() { stamp = mcpTargetStamp(fsys.OSFS{}, fifo) })
	if !strings.HasPrefix(stamp, "type:p") {
		t.Fatalf("stamp(named pipe) = %q", stamp)
	}
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	guard := &noReadFS{guarded: map[string]bool{big: true}}
	if stamp := mcpTargetStamp(guard, big); stamp != "size:1073741824" || len(guard.reads) != 0 {
		t.Fatalf("stamp(1 GiB) = %q, reads %v; want a size stamp, unread", stamp, guard.reads)
	}
}
