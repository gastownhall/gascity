package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// HashHookSettingsContent canonicalizes JSON only for overlay.IsMergeablePath
// files, so that a compact document and its pretty-printed equivalent produce
// the same fingerprint. Everything else — non-mergeable paths, non-JSON bodies,
// unreadable/missing files — must fall back to raw HashPathContent.
//
// These cases pin the fingerprint contract directly. The convergence tests
// exercise it only indirectly, so a regression that made canonicalization a
// no-op (or applied it too widely) would otherwise surface as spurious
// core-fingerprint drift and an extra agent restart, not as a test failure.

// writeHashTestFile writes body to a temp file named after relPath's base and
// returns its absolute path.
func writeHashTestFile(t *testing.T, relPath, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.Base(relPath))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestHashHookSettingsContent_MergeableCanonicalizesJSON(t *testing.T) {
	const relPath = ".codex/hooks.json"
	compact := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[]}]}}`
	pretty := `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": []
      }
    ]
  }
}
`
	compactHash := HashHookSettingsContent(writeHashTestFile(t, relPath, compact), relPath)
	prettyHash := HashHookSettingsContent(writeHashTestFile(t, relPath, pretty), relPath)

	if compactHash == "" || prettyHash == "" {
		t.Fatalf("empty hash: compact=%q pretty=%q", compactHash, prettyHash)
	}
	if compactHash != prettyHash {
		t.Errorf("compact and pretty-printed %s must hash identically:\n compact=%s\n pretty =%s",
			relPath, compactHash, prettyHash)
	}
}

func TestHashHookSettingsContent_MergeableIgnoresKeyOrder(t *testing.T) {
	const relPath = ".claude/settings.json"
	first := HashHookSettingsContent(
		writeHashTestFile(t, relPath, `{"alpha":1,"beta":2}`), relPath)
	second := HashHookSettingsContent(
		writeHashTestFile(t, relPath, `{"beta":2,"alpha":1}`), relPath)

	if first != second {
		t.Errorf("canonical JSON must sort keys, so these must hash identically:\n first =%s\n second=%s",
			first, second)
	}
}

func TestHashHookSettingsContent_NonMergeablePathFallsBackToRaw(t *testing.T) {
	// Same logical document, different serialization, on a path that is NOT
	// mergeable: no canonicalization, so the raw bytes decide the hash.
	const relPath = ".codex/config.json"
	compactPath := writeHashTestFile(t, relPath, `{"a":1}`)
	prettyPath := writeHashTestFile(t, relPath, "{\n  \"a\": 1\n}\n")

	compactHash := HashHookSettingsContent(compactPath, relPath)
	prettyHash := HashHookSettingsContent(prettyPath, relPath)

	if want := HashPathContent(compactPath); compactHash != want {
		t.Errorf("non-mergeable path must fall back to HashPathContent: got %s want %s", compactHash, want)
	}
	if compactHash == prettyHash {
		t.Errorf("non-mergeable path must NOT canonicalize; compact and pretty both hashed %s", compactHash)
	}
}

func TestHashHookSettingsContent_NonJSONBodyFallsBackToRaw(t *testing.T) {
	// A mergeable path whose content does not parse: CanonicalJSON fails and the
	// raw content hash is used rather than an empty or panicking result.
	const relPath = ".codex/hooks.json"
	path := writeHashTestFile(t, relPath, "this is not json\n")

	got := HashHookSettingsContent(path, relPath)
	if want := HashPathContent(path); got != want {
		t.Errorf("unparseable mergeable file must fall back to HashPathContent: got %s want %s", got, want)
	}
}

func TestHashHookSettingsContent_MissingFileMatchesRawHash(t *testing.T) {
	// An absent probe target must agree with HashPathContent so a file that has
	// not been staged yet does not read as a distinct fingerprint.
	const relPath = ".codex/hooks.json"
	missing := filepath.Join(t.TempDir(), "hooks.json")

	got := HashHookSettingsContent(missing, relPath)
	if want := HashPathContent(missing); got != want {
		t.Errorf("missing mergeable file must match HashPathContent: got %s want %s", got, want)
	}
}
