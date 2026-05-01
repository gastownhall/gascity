package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrependGCBinDirToPATH_NoGCBin_NoOp(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	prependGCBinDirToPATH(env, "")
	if env["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH should be unchanged when GC_BIN empty, got %q", env["PATH"])
	}
}

func TestPrependGCBinDirToPATH_AddsToExistingPATH(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	prependGCBinDirToPATH(env, "/Users/jbb/go/bin/gc")
	want := "/Users/jbb/go/bin" + string(os.PathListSeparator) + "/usr/bin:/bin"
	if env["PATH"] != want {
		t.Fatalf("PATH=%q, want %q", env["PATH"], want)
	}
}

func TestPrependGCBinDirToPATH_FallsBackToOSPATH(t *testing.T) {
	env := map[string]string{}
	t.Setenv("PATH", "/usr/bin:/bin")
	prependGCBinDirToPATH(env, "/opt/gc/bin/gc")
	want := "/opt/gc/bin" + string(os.PathListSeparator) + "/usr/bin:/bin"
	if env["PATH"] != want {
		t.Fatalf("PATH=%q, want %q", env["PATH"], want)
	}
}

func TestPrependGCBinDirToPATH_ExplicitEmptyPATHUsesOnlyGCBinDir(t *testing.T) {
	dir := "/opt/gc/bin"
	env := map[string]string{"PATH": ""}
	prependGCBinDirToPATH(env, filepath.Join(dir, "gc"))
	if env["PATH"] != dir {
		t.Fatalf("PATH=%q, want only gc bin dir %q", env["PATH"], dir)
	}
}

func TestPrependGCBinDirToPATH_UnsetPATHWithEmptyOSPATHUsesOnlyGCBinDir(t *testing.T) {
	dir := "/opt/gc/bin"
	env := map[string]string{}
	t.Setenv("PATH", "")
	prependGCBinDirToPATH(env, filepath.Join(dir, "gc"))
	if env["PATH"] != dir {
		t.Fatalf("PATH=%q, want only gc bin dir %q", env["PATH"], dir)
	}
}

func TestPrependGCBinDirToPATH_AlreadyFirst_NoDuplicate(t *testing.T) {
	dir := "/Users/jbb/go/bin"
	env := map[string]string{"PATH": dir + string(os.PathListSeparator) + "/usr/bin"}
	prependGCBinDirToPATH(env, filepath.Join(dir, "gc"))
	parts := strings.Split(env["PATH"], string(os.PathListSeparator))
	if parts[0] != dir {
		t.Fatalf("first PATH entry %q, want %q", parts[0], dir)
	}
	count := 0
	for _, p := range parts {
		if p == dir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dir %q should appear exactly once, found %d times in %q", dir, count, env["PATH"])
	}
}

func TestPrependGCBinDirToPATH_PresentNotFirst_MovesToFront(t *testing.T) {
	dir := "/Users/jbb/go/bin"
	env := map[string]string{"PATH": "/opt/homebrew/bin" + string(os.PathListSeparator) + dir + string(os.PathListSeparator) + "/usr/bin"}
	prependGCBinDirToPATH(env, filepath.Join(dir, "gc"))
	parts := strings.Split(env["PATH"], string(os.PathListSeparator))
	if parts[0] != dir {
		t.Fatalf("first PATH entry %q, want %q (full PATH=%q)", parts[0], dir, env["PATH"])
	}
	count := 0
	for _, p := range parts {
		if p == dir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dir %q should appear exactly once, found %d times in %q", dir, count, env["PATH"])
	}
}

func TestPrependGCBinDirToPATH_PreservesLeadingEmptyEntry(t *testing.T) {
	dir := "/Users/jbb/go/bin"
	sep := string(os.PathListSeparator)
	env := map[string]string{"PATH": sep + "/usr/bin"}
	prependGCBinDirToPATH(env, filepath.Join(dir, "gc"))
	want := dir + sep + sep + "/usr/bin"
	if env["PATH"] != want {
		t.Fatalf("PATH=%q, want %q", env["PATH"], want)
	}
}

func TestPrependGCBinDirToPATH_EmptyDir_NoOp(t *testing.T) {
	// edge: GC_BIN is just "gc" with no directory part — skip prepend.
	env := map[string]string{"PATH": "/usr/bin"}
	prependGCBinDirToPATH(env, "gc")
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("PATH should be unchanged when GC_BIN has no dir, got %q", env["PATH"])
	}
}
