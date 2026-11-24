package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBdBlockerPatchCoversWriteSpellings(t *testing.T) {
	for _, args := range [][]string{
		{"update", "gc-a", "--set-metadata", "gc.blocked_on=wait_client", "--set-metadata", "gc.blocker.v2={}"},
		{"create", "new", "--metadata", `{"gc.blocked_on":"wait_client","gc.blocker.v2":"{}"}`},
		{"update", "gc-a", "--set-metadata=gc.blocked_on=wait_client", "--set-metadata=gc.blocker.v2={}"},
	} {
		patch, touched, err := bdBlockerPatch(args)
		if err != nil || !touched || patch["gc.blocked_on"] != "wait_client" || patch["gc.blocker.v2"] != "{}" {
			t.Fatalf("args=%v patch=%v touched=%v err=%v", args, patch, touched, err)
		}
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--description", "--set-metadata gc.blocked_on=bad"}); err != nil || touched {
		t.Fatalf("description parsed as blocker metadata: touched=%v err=%v", touched, err)
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--set-metadata", "gc.blocked_on=x", "--metadata", `{}`}); err == nil || !touched {
		t.Fatalf("mixed forms not refused: touched=%v err=%v", touched, err)
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--unset-metadata", "gc.blocker.v2"}); err == nil || !touched {
		t.Fatalf("typed contract removal not refused: touched=%v err=%v", touched, err)
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--unset-metadata", "gc.blocked_on", "--unset-metadata", "gc.blocker.v2"}); err == nil || !touched {
		t.Fatalf("paired release bypassed guard: touched=%v err=%v", touched, err)
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--unset-metadata", "gc.blocked_on"}); err == nil || !touched {
		t.Fatalf("legacy blocker release bypassed guard: touched=%v err=%v", touched, err)
	}
	if _, touched, err := bdBlockerPatch([]string{"update", "gc-a", "--metadata", "@metadata.json"}); err == nil || !touched {
		t.Fatalf("@file metadata bypassed atomic preflight: touched=%v err=%v", touched, err)
	}
}

func TestBdBlockerWriteRefusalRunsBeforeStoreWrite(t *testing.T) {
	city := t.TempDir()
	validator := filepath.Join(city, "validator.py")
	if err := os.WriteFile(validator, []byte("import json,sys\np=json.load(sys.stdin)\nif 'gc.blocker.v2' not in p:\n print('missing gc.blocker.v2',file=sys.stderr);sys.exit(2)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, refused := bdBlockerWriteRefusal(city, "validator.py", []string{"update", "gc-a", "--set-metadata", "gc.blocked_on=human-gate"}); !refused || !strings.Contains(msg, "missing gc.blocker.v2") {
		t.Fatalf("invalid write reached store: refused=%v msg=%q", refused, msg)
	}
	if data, err := os.ReadFile(filepath.Join(city, ".gc", "runtime", "blocker-write-guard", "refusals.jsonl")); err != nil || !strings.Contains(string(data), "gc_blocker_write_refused_total") {
		t.Fatalf("missing durable refusal metric: %s %v", data, err)
	}
	if msg, refused := bdBlockerWriteRefusal(city, "validator.py", []string{"update", "gc-a", "--set-metadata", "gc.blocked_on=human-gate", "--set-metadata", "gc.blocker.v2={}"}); refused {
		t.Fatalf("valid pair refused: %q", msg)
	}
	for _, args := range [][]string{
		{"update", "gc-a", "--unset-metadata", "gc.blocked_on"},
		{"update", "gc-a", "--unset-metadata", "gc.blocked_on", "--unset-metadata", "gc.blocker.v2"},
	} {
		if msg, refused := bdBlockerWriteRefusal(city, "validator.py", args); !refused || !strings.Contains(msg, "evidence-backed reconciliation writer") {
			t.Fatalf("release bypassed preflight: args=%v refused=%v msg=%q", args, refused, msg)
		}
	}
}
