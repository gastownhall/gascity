package config

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDiscoverPackCommands_BasicAndNested(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/status/help.md", "status help")
	writeTestFile(t, packDir, "commands/repo/sync/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commands, want 2", len(got))
	}

	if got[0].Name != "repo/sync" {
		t.Fatalf("got first command %q, want %q", got[0].Name, "repo/sync")
	}
	if !reflect.DeepEqual(got[0].Command, []string{"repo", "sync"}) {
		t.Fatalf("repo/sync words = %#v, want %#v", got[0].Command, []string{"repo", "sync"})
	}

	if got[1].Name != "status" {
		t.Fatalf("got second command %q, want %q", got[1].Name, "status")
	}
	if !reflect.DeepEqual(got[1].Command, []string{"status"}) {
		t.Fatalf("status words = %#v, want %#v", got[1].Command, []string{"status"})
	}
	if got[1].HelpFile == "" {
		t.Fatal("status HelpFile = empty, want discovered help.md")
	}
}

func TestDiscoverPackCommands_ManifestOverride(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/repo-sync/command.toml", `
command = ["repo", "sync"]
description = "Sync the repo"
run = "../../shared/entry.sh"
`)
	writeTestFile(t, packDir, "shared/entry.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d commands, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].Command, []string{"repo", "sync"}) {
		t.Fatalf("command words = %#v, want %#v", got[0].Command, []string{"repo", "sync"})
	}
	if got[0].Description != "Sync the repo" {
		t.Fatalf("description = %q, want %q", got[0].Description, "Sync the repo")
	}
	wantRun := filepath.Join(packDir, "shared", "entry.sh")
	if got[0].RunScript != wantRun {
		t.Fatalf("RunScript = %q, want %q", got[0].RunScript, wantRun)
	}
}

func TestDiscoverPackCommands_RejectsEscapingOrAbsoluteRunPaths(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	tests := []struct {
		name string
		run  string
	}{
		{name: "absolute", run: "/tmp/outside.sh"},
		{name: "escape", run: "../../../outside.sh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeTestFile(t, packDir, "commands/status/command.toml", "run = "+`"`+tt.run+`"`+"\n")
			writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")

			_, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
			if err == nil {
				t.Fatal("DiscoverPackCommands error = nil, want containment error")
			}
		})
	}
}

func TestDiscoverPackCommands_SkipsHiddenAndUnderscoreDirs(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/.hidden/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/_private/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/repo/_internal/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d commands, want 1", len(got))
	}
	if got[0].Name != "status" {
		t.Fatalf("got command %q, want %q", got[0].Name, "status")
	}
}

func TestDiscoverPackCommands_NoCommandsDir(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d commands, want 0", len(got))
	}
}

func TestDiscoverPackCommands_BadManifest(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/status/command.toml", "command = [")
	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")

	_, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err == nil {
		t.Fatal("DiscoverPackCommands error = nil, want manifest parse error")
	}
}

func TestDiscoverPackCommands_TreatsLeafAsTerminalAndIgnoresNestedRunSh(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/repo/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/repo/sync/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d commands, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].Command, []string{"repo"}) {
		t.Fatalf("command words = %#v, want %#v", got[0].Command, []string{"repo"})
	}
}

func TestDiscoverPackCommands_AllowsVisibleAssetSubdirsUnderLeaf(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "commands/deploy/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/deploy/templates/example.txt", "template")

	got, err := DiscoverPackCommands(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackCommands: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d commands, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].Command, []string{"deploy"}) {
		t.Fatalf("command words = %#v, want %#v", got[0].Command, []string{"deploy"})
	}
}
