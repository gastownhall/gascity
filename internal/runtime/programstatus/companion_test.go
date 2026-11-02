package programstatus

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCompanionVersionRoundTrips(t *testing.T) {
	v := CompanionVersion{Version: "v1.2.3", Codec: CodecVersion, Relay: 0}
	line := v.String()
	if line != "gc-osc7501 v1.2.3 codec=1 relay=0" {
		t.Fatalf("String() = %q", line)
	}
	got, err := ParseCompanionVersion(line + "\n")
	if err != nil {
		t.Fatalf("ParseCompanionVersion(%q): %v", line, err)
	}
	if got != v {
		t.Fatalf("ParseCompanionVersion = %+v, want %+v", got, v)
	}
}

func TestParseCompanionVersionRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{
		"",
		"gc v1 codec=1 relay=0",
		"gc-osc7501 v1 codec=1",
		"gc-osc7501 v1 relay=0 codec=1",
		"gc-osc7501 v1 codec=x relay=0",
		"gc-osc7501 v1 codec=01 relay=0",
		"gc-osc7501 v1 codec=1 relay=-1",
		"gc-osc7501  codec=1 relay=0",
		"gc-osc7501 v1 codec=1 relay=0 extra",
		"gc-osc7501 v1 codec=1 relay=0\nsecond line",
	} {
		if _, err := ParseCompanionVersion(line); err == nil {
			t.Errorf("ParseCompanionVersion(%q) succeeded, want an error", line)
		}
	}
}

func TestCheckCompanionVersionRequiresMatchingCodec(t *testing.T) {
	if err := CheckCompanionVersion(CompanionVersion{Version: "v1", Codec: CodecVersion}); err != nil {
		t.Fatalf("matching codec: %v", err)
	}
	err := CheckCompanionVersion(CompanionVersion{Version: "v1", Codec: CodecVersion + 1})
	if !errors.Is(err, ErrCompanionCodec) {
		t.Fatalf("other codec: err = %v, want ErrCompanionCodec", err)
	}
}

// writeExecutable creates an executable file at dir/name.
func writeExecutable(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func noPath(string) (string, error) { return "", errors.New("not on PATH") }

func TestLookupCompanionPrefersSiblingOfExecutable(t *testing.T) {
	dir := t.TempDir()
	gc := writeExecutable(t, dir, "gc", 0o755)
	want := writeExecutable(t, dir, CompanionName, 0o755)
	got, err := lookupCompanion(gc, nil, func(string) (string, error) {
		t.Fatal("PATH consulted although the sibling exists")
		return "", nil
	})
	if err != nil || got != want {
		t.Fatalf("lookupCompanion = %q, %v; want %q", got, err, want)
	}
}

func TestLookupCompanionKeepsUnresolvedExecutableDir(t *testing.T) {
	// A symlinked gc (Homebrew's stable bin/) resolves its companion next to
	// the link, not next to the link's target.
	linkDir, cellar := t.TempDir(), t.TempDir()
	target := writeExecutable(t, cellar, "gc", 0o755)
	link := filepath.Join(linkDir, "gc")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	want := writeExecutable(t, linkDir, CompanionName, 0o755)
	got, err := lookupCompanion(link, nil, noPath)
	if err != nil || got != want {
		t.Fatalf("lookupCompanion = %q, %v; want %q", got, err, want)
	}
}

func TestLookupCompanionFallsBackToPATH(t *testing.T) {
	dir := t.TempDir()
	gc := writeExecutable(t, dir, "gc", 0o755)
	writeExecutable(t, dir, CompanionName, 0o644) // not executable: skipped
	onPath := "/opt/bin/" + CompanionName
	var asked string
	got, err := lookupCompanion(gc, nil, func(name string) (string, error) {
		asked = name
		return onPath, nil
	})
	if err != nil || got != onPath || asked != CompanionName {
		t.Fatalf("lookupCompanion = %q, %v (asked %q); want %q from PATH", got, err, asked, onPath)
	}
}

func TestLookupCompanionSkipsSiblingDirectory(t *testing.T) {
	dir := t.TempDir()
	gc := writeExecutable(t, dir, "gc", 0o755)
	if err := os.Mkdir(filepath.Join(dir, CompanionName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupCompanion(gc, nil, noPath); !errors.Is(err, ErrCompanionNotFound) {
		t.Fatalf("err = %v, want ErrCompanionNotFound", err)
	}
}

func TestLookupCompanionUsesPATHWhenExecutableUnknown(t *testing.T) {
	got, err := lookupCompanion("", errors.New("no /proc"), func(string) (string, error) {
		return "/usr/local/bin/" + CompanionName, nil
	})
	if err != nil || got != "/usr/local/bin/"+CompanionName {
		t.Fatalf("lookupCompanion = %q, %v", got, err)
	}
}

func TestLookupCompanionReportsMissing(t *testing.T) {
	dir := t.TempDir()
	gc := writeExecutable(t, dir, "gc", 0o755)
	_, err := lookupCompanion(gc, nil, noPath)
	if !errors.Is(err, ErrCompanionNotFound) {
		t.Fatalf("err = %v, want ErrCompanionNotFound", err)
	}
}
