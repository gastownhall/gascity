package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBatchGuardBdStub replaces the fixture's bd with one that logs every
// call and answers `show` from the case arms given.
func writeBatchGuardBdStub(t *testing.T, showArms string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"if [ \"$1\" = show ]; then\n  case \"$*\" in\n" + showArms + "\n  esac\n  exit 0\nfi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// A bulk close (the maintenance orders close stale wisps in batches) verifies
// every id with one bd show instead of one or two bd forks per id.
func TestGcBdBulkCloseVerifiesIDsWithOneShow(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-a demo-wisp-b demo-c")
      printf '[{"id":"demo-a","status":"open"},{"id":"demo-wisp-b","status":"open"},{"id":"demo-c","status":"open"}]\n' ;;`)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-a", "demo-wisp-b", "demo-c", "--reason", "stale"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd close = %d, stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if got := strings.Count(log, "show "); got != 1 {
		t.Fatalf("bd show ran %d times, want 1 batched verification:\n%s", got, log)
	}
	if !strings.Contains(log, "close demo-a demo-wisp-b demo-c --reason stale") {
		t.Fatalf("the verified close was not forwarded to bd:\n%s", log)
	}
}

// The batch read never weakens the exact-ID guard (gcy-g4o): an id bd did not
// answer verbatim is resolved on its own, and a substring collision still
// refuses the whole write.
func TestGcBdBulkCloseStillRefusesASubstringCollision(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-a demo-dv7")
      printf '[{"id":"demo-a","status":"open"},{"id":"demo-wisp-dv78","status":"open"}]\n' ;;
    "show --json demo-dv7")
      printf '[{"id":"demo-wisp-dv78","status":"open"}]\n' ;;`)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-a", "demo-dv7"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBd close = %d, want 1 for a substring collision; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "substring collision") {
		t.Fatalf("stderr = %q, want the collision refusal", stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "close ") {
		t.Fatalf("bd close ran despite the collision:\n%s", data)
	}
}
