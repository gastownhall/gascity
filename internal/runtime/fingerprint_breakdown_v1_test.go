package runtime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestBreakdownV1MarshalRoundtrip enforces ga-s760.2 / MF-A: the
// BreakdownV1 struct must JSON-roundtrip with all fields preserved
// (Version, Fields, CopyFiles entries — including mixed probed and
// non-probed entries). The supervisor stores this JSON in session
// metadata under "core_hash_breakdown"; the reconciler reads it back
// at drift-detection time.
func TestBreakdownV1MarshalRoundtrip(t *testing.T) {
	original := BreakdownV1{
		Version: FingerprintVersion,
		Fields: map[string]string{
			"Command":            "abcdef0123456789",
			"Env":                "1111111111111111",
			"FPExtra":            "2222222222222222",
			"PreStart":           "3333333333333333",
			"SessionSetup":       "4444444444444444",
			"SessionSetupScript": "5555555555555555",
			"OverlayDir":         "6666666666666666",
			"CopyFiles":          "7777777777777777",
		},
		CopyFiles: []BreakdownCopyEntry{
			// Probed entry with a successful content hash.
			{RelDst: ".gc/scripts", Probed: true, ContentHash: "abc12345"},
			// Probed entry with no content hash (transient I/O error).
			{RelDst: ".claude/skills.broken", Probed: true, ContentHash: ""},
			// Config-derived (non-probed) entry — Src present, ContentHash empty.
			{RelDst: "config.toml", Src: "/etc/agent.toml"},
			// Mixed: another probed entry with a hash, to exercise sort/order
			// preservation through the round trip.
			{RelDst: ".claude/skills", Probed: true, ContentHash: "fed09876"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal(BreakdownV1) failed: %v", err)
	}

	var decoded BreakdownV1
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(BreakdownV1) failed: %v\nJSON: %s", err, data)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("BreakdownV1 round-trip mismatch:\noriginal=%+v\ndecoded =%+v\nJSON    =%s",
			original, decoded, data)
	}

	// Verify Version is non-empty after round trip — the legacy fallback
	// path in LogCoreFingerprintDrift uses an empty Version as the
	// "treat as map[string]string" signal.
	if decoded.Version == "" {
		t.Error("decoded BreakdownV1.Version is empty; legacy fallback would mis-trigger")
	}

	// Verify per-entry fields survived: probed-with-hash, probed-no-hash,
	// non-probed (Src), and the four-entry order.
	if len(decoded.CopyFiles) != 4 {
		t.Fatalf("decoded CopyFiles len=%d, want 4", len(decoded.CopyFiles))
	}
	if decoded.CopyFiles[0].RelDst != ".gc/scripts" || !decoded.CopyFiles[0].Probed || decoded.CopyFiles[0].ContentHash != "abc12345" {
		t.Errorf("entry 0 corrupted: %+v", decoded.CopyFiles[0])
	}
	if decoded.CopyFiles[1].ContentHash != "" || !decoded.CopyFiles[1].Probed {
		t.Errorf("entry 1 (probed-no-hash) corrupted: %+v", decoded.CopyFiles[1])
	}
	if decoded.CopyFiles[2].Src != "/etc/agent.toml" || decoded.CopyFiles[2].Probed {
		t.Errorf("entry 2 (non-probed) corrupted: %+v", decoded.CopyFiles[2])
	}
}

// TestLogDriftHandlesLegacyMapBreakdown enforces ga-s760.2 / MF-A's
// upgrade-compat clause: when the stored breakdown JSON is a legacy
// map[string]string (no Version field, no CopyFiles array), the
// renderer must fall back to the existing per-field diff output —
// no panic, no missing-field log, no [+]/[-]/[~]/[ ] markers, and no
// "stored=<N> entries" line.
//
// During the upgrade window, both formats coexist in supervisor logs;
// the legacy path must continue to emit byte-for-byte the same lines
// it did before this bead.
func TestLogDriftHandlesLegacyMapBreakdown(t *testing.T) {
	// Build a legacy map[string]string with at least one field that
	// will differ from the current config, so the diff path runs.
	legacy := map[string]string{
		"Command":            "old_command_hash",
		"Env":                "old_env_hash",
		"FPExtra":            "old_fpextra_hash",
		"PreStart":           "old_prestart_hash",
		"SessionSetup":       "old_sessionsetup_hash",
		"SessionSetupScript": "old_setupscript_hash",
		"OverlayDir":         "old_overlay_hash",
		"CopyFiles":          "old_copyfiles_hash",
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy map: %v", err)
	}

	current := Config{
		Command:   "claude --new",
		CopyFiles: []CopyEntry{{RelDst: "bar", Probed: true, ContentHash: "h1"}},
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "legacy-agent", string(legacyJSON), current)
	out := buf.String()

	if out == "" {
		t.Fatal("expected diagnostic output for legacy map breakdown, got empty")
	}

	// Legacy header still appears.
	if !strings.Contains(out, "config-drift-diag legacy-agent") {
		t.Errorf("missing config-drift-diag header in legacy output:\n%s", out)
	}
	if !strings.Contains(out, "drifted fields:") {
		t.Errorf("missing drifted-fields line in legacy output:\n%s", out)
	}
	if !strings.Contains(out, "stored-hash=") || !strings.Contains(out, "current-hash=") {
		t.Errorf("missing stored-hash/current-hash columns in legacy output:\n%s", out)
	}

	// Legacy MUST NOT emit any of the new BreakdownV1 markers — they
	// are how operators distinguish the two formats at a glance
	// (designer §4).
	newMarkers := []string{
		"[ ] ", "[~] ", "[+] ", "[-] ",
		" entries  current=",
	}
	for _, m := range newMarkers {
		if strings.Contains(out, m) {
			t.Errorf("legacy fallback emitted new-format marker %q (should only appear with BreakdownV1):\n%s", m, out)
		}
	}

	// Legacy CopyFiles diagnostic uses the existing "CopyFiles[N]:" prefix.
	// (Per designer §4: this prefix is how the legacy path is recognized.)
	if !regexp.MustCompile(`CopyFiles\[\d+\]:`).MatchString(out) {
		t.Errorf("legacy fallback missing CopyFiles[N]: per-entry prefix:\n%s", out)
	}
}

// TestLogDriftPrintsCopyFilesEntryDiff enforces ga-s760.2 / MF-A: when
// the stored breakdown has three entries and the current config differs
// in exactly one entry's ContentHash, the diff output must mark that
// one entry with [~] and the other two with [ ]. This is the bug the
// investigator hit — without per-entry diff, the operator could not
// tell which CopyFiles entry was drifting when the aggregate hash
// changed.
func TestLogDriftPrintsCopyFilesEntryDiff(t *testing.T) {
	// Three entries; one of them (.gc/settings.json) has a different
	// ContentHash in stored vs current. The other two are identical.
	storedEntries := []BreakdownCopyEntry{
		{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
		{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
		{RelDst: ".gc/settings.json", Probed: true, ContentHash: "cccc3333"},
	}
	currentCfg := Config{
		Command: "claude",
		CopyFiles: []CopyEntry{
			{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
			{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
			{RelDst: ".gc/settings.json", Probed: true, ContentHash: "DDDD4444"}, // differs
		},
	}

	stored := buildStoredBreakdownForCopyFilesDiff(t, currentCfg, storedEntries)
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal stored breakdown: %v", err)
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent", string(storedJSON), currentCfg)
	out := buf.String()

	if out == "" {
		t.Fatal("expected diagnostic output for drift, got empty")
	}

	// Header signals CopyFiles is the drifted field.
	if !strings.Contains(out, "drifted fields:") || !strings.Contains(out, "CopyFiles") {
		t.Errorf("expected drifted-fields header naming CopyFiles:\n%s", out)
	}

	// Entry-count line (new format, per designer §3).
	if !strings.Contains(out, "stored=3 entries") || !strings.Contains(out, "current=3 entries") {
		t.Errorf("expected entry-count line 'stored=3 entries  current=3 entries':\n%s", out)
	}

	// Two unchanged entries get the [ ] marker.
	if !containsLineWithBoth(out, "[ ]", ".claude/skills") {
		t.Errorf("expected unchanged entry .claude/skills marked [ ]:\n%s", out)
	}
	if !containsLineWithBoth(out, "[ ]", ".gc/scripts") {
		t.Errorf("expected unchanged entry .gc/scripts marked [ ]:\n%s", out)
	}

	// The one changed entry gets [~].
	if !containsLineWithBoth(out, "[~]", ".gc/settings.json") {
		t.Errorf("expected changed entry .gc/settings.json marked [~]:\n%s", out)
	}

	// The changed entry must NOT carry [ ]/[+]/[-] markers.
	for _, badMarker := range []string{"[ ]", "[+]", "[-]"} {
		if containsLineWithBoth(out, badMarker, ".gc/settings.json") {
			t.Errorf("changed entry .gc/settings.json incorrectly marked %s:\n%s", badMarker, out)
		}
	}

	// stored= and current= columns must show the differing hashes
	// (truncated to 8 hex per designer §7 validation checklist).
	settingsLine := lineContaining(out, ".gc/settings.json")
	if settingsLine == "" {
		t.Fatalf("could not locate .gc/settings.json line in output:\n%s", out)
	}
	if !strings.Contains(settingsLine, "stored=cccc3333") {
		t.Errorf(".gc/settings.json line missing stored=cccc3333: %q", settingsLine)
	}
	if !strings.Contains(settingsLine, "current=DDDD4444") {
		t.Errorf(".gc/settings.json line missing current=DDDD4444: %q", settingsLine)
	}
	if !strings.Contains(settingsLine, "(probed)") {
		t.Errorf(".gc/settings.json line missing (probed) literal: %q", settingsLine)
	}

	// Designer §1: column separator between stored= and current= is
	// exactly two spaces. Search for the two-space pattern in the
	// changed line.
	if !regexp.MustCompile(`stored=\S.*?  current=`).MatchString(settingsLine) {
		t.Errorf(".gc/settings.json line missing two-space separator between stored= and current=: %q", settingsLine)
	}
}

// TestLogDriftPrintsCopyFilesAddedAndRemovedEntries enforces ga-s760.2
// / MF-A: when an entry exists in stored but not in current, it
// renders with [-] and current=(absent); when an entry exists in
// current but not in stored, it renders with [+] and stored=(absent).
func TestLogDriftPrintsCopyFilesAddedAndRemovedEntries(t *testing.T) {
	t.Run("added", func(t *testing.T) {
		storedEntries := []BreakdownCopyEntry{
			{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
			{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
			{RelDst: ".gc/settings.json", Probed: true, ContentHash: "cccc3333"},
		}
		currentCfg := Config{
			Command: "claude",
			CopyFiles: []CopyEntry{
				{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
				{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
				{RelDst: ".gc/settings.json", Probed: true, ContentHash: "cccc3333"},
				// New entry only present in current.
				{RelDst: ".gc/added.toml", Probed: true, ContentHash: "eeee5555"},
			},
		}

		stored := buildStoredBreakdownForCopyFilesDiff(t, currentCfg, storedEntries)
		storedJSON, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("marshal stored breakdown: %v", err)
		}

		var buf bytes.Buffer
		LogCoreFingerprintDrift(&buf, "agent-add", string(storedJSON), currentCfg)
		out := buf.String()

		if !strings.Contains(out, "stored=3 entries") || !strings.Contains(out, "current=4 entries") {
			t.Errorf("expected entry-count 'stored=3 entries  current=4 entries':\n%s", out)
		}

		// Added entry marked [+], stored side is (absent).
		if !containsLineWithBoth(out, "[+]", ".gc/added.toml") {
			t.Errorf("expected new entry .gc/added.toml marked [+]:\n%s", out)
		}
		addedLine := lineContaining(out, ".gc/added.toml")
		if addedLine == "" {
			t.Fatalf("could not locate .gc/added.toml line in output:\n%s", out)
		}
		if !strings.Contains(addedLine, "stored=(absent)") {
			t.Errorf("[+] entry must show stored=(absent): %q", addedLine)
		}
		if !strings.Contains(addedLine, "current=eeee5555") {
			t.Errorf("[+] entry must show current=eeee5555 (probed): %q", addedLine)
		}

		// The unchanged entries should not be marked as added/removed.
		for _, rel := range []string{".claude/skills", ".gc/scripts", ".gc/settings.json"} {
			if containsLineWithBoth(out, "[+]", rel) {
				t.Errorf("unchanged entry %s incorrectly marked [+]:\n%s", rel, out)
			}
			if containsLineWithBoth(out, "[-]", rel) {
				t.Errorf("unchanged entry %s incorrectly marked [-]:\n%s", rel, out)
			}
		}
	})

	t.Run("removed", func(t *testing.T) {
		storedEntries := []BreakdownCopyEntry{
			{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
			{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
			{RelDst: ".gc/skills.old", Probed: true, ContentHash: "cccc3333"},
		}
		currentCfg := Config{
			Command: "claude",
			CopyFiles: []CopyEntry{
				{RelDst: ".claude/skills", Probed: true, ContentHash: "aaaa1111"},
				{RelDst: ".gc/scripts", Probed: true, ContentHash: "bbbb2222"},
			},
		}

		stored := buildStoredBreakdownForCopyFilesDiff(t, currentCfg, storedEntries)
		storedJSON, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("marshal stored breakdown: %v", err)
		}

		var buf bytes.Buffer
		LogCoreFingerprintDrift(&buf, "agent-remove", string(storedJSON), currentCfg)
		out := buf.String()

		if !strings.Contains(out, "stored=3 entries") || !strings.Contains(out, "current=2 entries") {
			t.Errorf("expected entry-count 'stored=3 entries  current=2 entries':\n%s", out)
		}

		// Removed entry marked [-], current side is (absent).
		if !containsLineWithBoth(out, "[-]", ".gc/skills.old") {
			t.Errorf("expected removed entry .gc/skills.old marked [-]:\n%s", out)
		}
		removedLine := lineContaining(out, ".gc/skills.old")
		if removedLine == "" {
			t.Fatalf("could not locate .gc/skills.old line in output:\n%s", out)
		}
		if !strings.Contains(removedLine, "current=(absent)") {
			t.Errorf("[-] entry must show current=(absent): %q", removedLine)
		}
		if !strings.Contains(removedLine, "stored=cccc3333") {
			t.Errorf("[-] entry must show stored=cccc3333 (probed): %q", removedLine)
		}
	})
}

// TestLogDriftPrintsCopyFilesProbedFlagFlip enforces ga-s760.2 / MF-A:
// when a stored entry was probed and the current entry for the same
// RelDst is non-probed (or vice versa), the diff must mark the entry
// with [~] and render the modes differently — `<hex> (probed)` on the
// probed side and `src=<path>` on the non-probed side. This catches
// the failure mode where probed entries silently fall back to
// path-based hashing across restarts.
func TestLogDriftPrintsCopyFilesProbedFlagFlip(t *testing.T) {
	// Stored side: probed=true, with content hash.
	// Current side: probed=false, with src path. Same RelDst.
	storedEntries := []BreakdownCopyEntry{
		{RelDst: "config/agent.toml", Probed: true, ContentHash: "11223344"},
	}
	currentCfg := Config{
		Command: "claude",
		CopyFiles: []CopyEntry{
			{RelDst: "config/agent.toml", Src: "/etc/agent.toml", Probed: false},
		},
	}

	stored := buildStoredBreakdownForCopyFilesDiff(t, currentCfg, storedEntries)
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal stored breakdown: %v", err)
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent-flip", string(storedJSON), currentCfg)
	out := buf.String()

	if !containsLineWithBoth(out, "[~]", "config/agent.toml") {
		t.Errorf("expected mode-flip entry config/agent.toml marked [~]:\n%s", out)
	}

	flipLine := lineContaining(out, "config/agent.toml")
	if flipLine == "" {
		t.Fatalf("could not locate config/agent.toml line in output:\n%s", out)
	}
	// Stored side: probed-with-hash → "11223344 (probed)"
	if !strings.Contains(flipLine, "stored=11223344 (probed)") {
		t.Errorf("expected stored side to render as '11223344 (probed)': %q", flipLine)
	}
	// Current side: non-probed → "src=/etc/agent.toml"
	if !strings.Contains(flipLine, "current=src=/etc/agent.toml") {
		t.Errorf("expected current side to render as 'src=/etc/agent.toml': %q", flipLine)
	}
}

// buildStoredBreakdownForCopyFilesDiff constructs a BreakdownV1 that
// matches `currentCfg`'s breakdown on every field EXCEPT `CopyFiles`,
// then plants `entries` as the stored CopyFiles list. The resulting
// stored breakdown forces `CopyFiles` (and only CopyFiles) into the
// drifted-fields set, isolating the per-entry diff under test.
//
// Without this helper, every test would either need to compute the
// current config's per-field hashes by hand or accept noisy output
// from spurious drifts in unrelated fields.
func buildStoredBreakdownForCopyFilesDiff(t *testing.T, currentCfg Config, entries []BreakdownCopyEntry) BreakdownV1 {
	t.Helper()
	currentBd := CoreFingerprintBreakdown(currentCfg)
	stored := BreakdownV1{
		Version:   FingerprintVersion,
		Fields:    make(map[string]string, len(currentBd.Fields)),
		CopyFiles: entries,
	}
	for k, v := range currentBd.Fields {
		stored.Fields[k] = v
	}
	// Force CopyFiles into the drifted-fields set; any value other
	// than the current's CopyFiles aggregate hash works.
	stored.Fields["CopyFiles"] = "STORED_AGGREGATE_DIFFERS_FROM_CURRENT"
	return stored
}

// containsLineWithBoth reports whether `out` contains a single line
// (newline-separated) that includes both substrings.
func containsLineWithBoth(out, a, b string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, a) && strings.Contains(line, b) {
			return true
		}
	}
	return false
}

// lineContaining returns the first line of `out` that contains
// `substr`, or empty string if none.
func lineContaining(out, substr string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
