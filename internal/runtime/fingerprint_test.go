package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestConfigFingerprintDeterministic(t *testing.T) {
	cfg := Config{Command: "claude --skip", Env: map[string]string{"GC_CITY": "1", "GC_RIG": "2"}}
	h1 := ConfigFingerprint(cfg)
	h2 := ConfigFingerprint(cfg)
	if h1 != h2 {
		t.Errorf("same config produced different hashes: %q vs %q", h1, h2)
	}
}

func TestConfigFingerprintDifferentCommand(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "codex"}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different commands should produce different hashes")
	}
}

func TestConfigFingerprintDifferentEnv(t *testing.T) {
	a := Config{Command: "claude", Env: map[string]string{"GC_CITY": "1"}}
	b := Config{Command: "claude", Env: map[string]string{"GC_CITY": "2"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different env values should produce different hashes")
	}
}

func TestConfigFingerprintEnvOrderIndependent(t *testing.T) {
	// Go maps don't guarantee order, but we verify via two configs
	// with the same key-value pairs that the hash is stable.
	a := Config{Command: "claude", Env: map[string]string{"GC_CITY": "last", "GC_RIG": "first", "GC_TEMPLATE": "mid"}}
	b := Config{Command: "claude", Env: map[string]string{"GC_TEMPLATE": "mid", "GC_RIG": "first", "GC_CITY": "last"}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("env order should not affect hash")
	}
}

func TestConfigFingerprintIgnoresNonGCEnv(t *testing.T) {
	// Non-GC_ prefixed env vars (PATH, CLAUDECODE, OTel vars, etc.)
	// should NOT affect the hash — they're ambient runtime details
	// that differ between the gc init process and the supervisor.
	a := Config{Command: "claude", Env: map[string]string{"GC_CITY": "bd"}}
	b := Config{Command: "claude", Env: map[string]string{
		"GC_CITY":                      "bd",
		"PATH":                         "/usr/local/bin:/usr/bin",
		"CLAUDECODE":                   "1",
		"CLAUDE_CODE_ENTRYPOINT":       "/usr/bin/claude",
		"BD_OTEL_METRICS_URL":          "http://localhost:4317",
		"OTEL_RESOURCE_ATTRIBUTES":     "service.name=gc",
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
	}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("non-GC_ prefixed env vars should not affect hash")
	}
}

func TestConfigFingerprintIgnoresReadyDelayMs(t *testing.T) {
	a := Config{Command: "claude", ReadyDelayMs: 0}
	b := Config{Command: "claude", ReadyDelayMs: 5000}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("ReadyDelayMs should not affect hash")
	}
}

func TestConfigFingerprintIgnoresReadyPromptPrefix(t *testing.T) {
	a := Config{Command: "claude", ReadyPromptPrefix: ""}
	b := Config{Command: "claude", ReadyPromptPrefix: "> "}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("ReadyPromptPrefix should not affect hash")
	}
}

func TestConfigFingerprintNilVsEmptyEnv(t *testing.T) {
	a := Config{Command: "claude", Env: nil}
	b := Config{Command: "claude", Env: map[string]string{}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("nil and empty env should produce the same hash")
	}
}

func TestConfigFingerprintIgnoresProcessNames(t *testing.T) {
	a := Config{Command: "claude", ProcessNames: nil}
	b := Config{Command: "claude", ProcessNames: []string{"claude", "node"}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("ProcessNames should not affect hash")
	}
}

func TestConfigFingerprintIgnoresEmitsPermissionWarning(t *testing.T) {
	a := Config{Command: "claude", EmitsPermissionWarning: false}
	b := Config{Command: "claude", EmitsPermissionWarning: true}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("EmitsPermissionWarning should not affect hash")
	}
}

func TestConfigFingerprintIgnoresWorkDir(t *testing.T) {
	a := Config{Command: "claude", WorkDir: "/tmp"}
	b := Config{Command: "claude", WorkDir: "/home/user"}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("WorkDir should not affect hash")
	}
}

func TestConfigFingerprintIgnoresGCDir(t *testing.T) {
	a := Config{Command: "claude", Env: map[string]string{"GC_DIR": "/tmp"}}
	b := Config{Command: "claude", Env: map[string]string{"GC_DIR": "/home/user"}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("GC_DIR should not affect hash")
	}
}

func TestConfigFingerprintIgnoresNonAllowedGCVars(t *testing.T) {
	// GC_* vars not on the allow list should not affect the hash.
	// This is the core invariant: new env vars are safe by default.
	base := Config{Command: "claude", Env: map[string]string{"GC_CITY": "/gc"}}
	withExtra := Config{Command: "claude", Env: map[string]string{
		"GC_CITY":               "/gc",
		"GC_SESSION_NAME":       "corp--sky",
		"GC_AGENT":              "corp/sky",
		"GC_INSTANCE_TOKEN":     "abc123",
		"GC_CONTINUATION_EPOCH": "5",
		"GC_RUNTIME_EPOCH":      "47",
		"GC_HOME":               "/home/user/.gc",
		"GC_API_HOST":           "0.0.0.0",
		"GC_API_PORT":           "8372",
		"GC_CTRL_XYZ_PORT":      "tcp://10.0.0.1:8080",
		"GC_SESSION_ID":         "gc-tyyt",
		"GC_PUBLICATIONS_FILE":  "/tmp/pub.json",
		"GC_BIN":                "/usr/local/bin/gc",
	}}
	if ConfigFingerprint(base) != ConfigFingerprint(withExtra) {
		t.Error("non-allowed GC_* vars should not affect hash")
	}
}

func TestConfigFingerprintEmptyConfig(t *testing.T) {
	h := ConfigFingerprint(Config{})
	if h == "" {
		t.Error("empty config should still produce a hash")
	}
	// Verify stability.
	if h != ConfigFingerprint(Config{}) {
		t.Error("empty config hash not stable")
	}
}

func TestConfigFingerprintExtraChangesHash(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "5"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("FingerprintExtra should change the hash")
	}
}

func TestConfigFingerprintExtraDeterministic(t *testing.T) {
	cfg := Config{
		Command:          "claude",
		FingerprintExtra: map[string]string{"pool.min": "1", "pool.max": "5"},
	}
	h1 := ConfigFingerprint(cfg)
	h2 := ConfigFingerprint(cfg)
	if h1 != h2 {
		t.Errorf("same FingerprintExtra produced different hashes: %q vs %q", h1, h2)
	}
}

func TestConfigFingerprintExtraDifferentValues(t *testing.T) {
	a := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "3"}}
	b := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "10"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different FingerprintExtra values should produce different hashes")
	}
}

func TestConfigFingerprintNilVsEmptyExtra(t *testing.T) {
	a := Config{Command: "claude", FingerprintExtra: nil}
	b := Config{Command: "claude", FingerprintExtra: map[string]string{}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("nil and empty FingerprintExtra should produce the same hash")
	}
}

func TestConfigFingerprintIncludesNudge(t *testing.T) {
	a := Config{Command: "claude", Nudge: ""}
	b := Config{Command: "claude", Nudge: "hello agent"}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different Nudge should produce different hashes")
	}
}

func TestConfigFingerprintIncludesPreStart(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", PreStart: []string{"mkdir -p /tmp/work"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different PreStart should produce different hashes")
	}
}

func TestConfigFingerprintIncludesSessionSetup(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", SessionSetup: []string{"tmux set-option -t {{.Session}} remain-on-exit on"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different SessionSetup should produce different hashes")
	}
}

func TestConfigFingerprintIncludesSessionSetupScript(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", SessionSetupScript: "/path/to/setup.sh"}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different SessionSetupScript should produce different hashes")
	}
}

func TestConfigFingerprintIncludesOverlayDir(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", OverlayDir: "/path/to/overlay"}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different OverlayDir should produce different hashes")
	}
}

func TestConfigFingerprintIncludesCopyFiles(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{Command: "claude", CopyFiles: []CopyEntry{{Src: "/tmp/foo", RelDst: "bar"}}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different CopyFiles should produce different hashes")
	}
}

func TestConfigFingerprintPreStartOrderMatters(t *testing.T) {
	a := Config{Command: "claude", PreStart: []string{"a", "b"}}
	b := Config{Command: "claude", PreStart: []string{"b", "a"}}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("different PreStart order should produce different hashes")
	}
}

func TestContentHashChangesFingerprintDifferentlyThanSrc(t *testing.T) {
	base := Config{Command: "claude"}
	withSrc := Config{Command: "claude", CopyFiles: []CopyEntry{{Src: "/tmp/foo", RelDst: "bar"}}}
	withHash := Config{Command: "claude", CopyFiles: []CopyEntry{{RelDst: "bar", Probed: true, ContentHash: "abc123"}}}

	baseH := CoreFingerprint(base)
	srcH := CoreFingerprint(withSrc)
	hashH := CoreFingerprint(withHash)

	if baseH == srcH {
		t.Error("CopyEntry with Src should change fingerprint vs empty")
	}
	if baseH == hashH {
		t.Error("CopyEntry with ContentHash should change fingerprint vs empty")
	}
	if srcH == hashH {
		t.Error("CopyEntry with Src vs ContentHash should produce different fingerprints")
	}
}

func TestProbedEntryWithFailedHashUsesStableSentinel(t *testing.T) {
	// A probed entry with empty ContentHash (transient I/O error) should
	// produce a stable fingerprint, not fall back to Src-based hashing.
	probedOK := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/tmp/skills", RelDst: ".claude/skills", Probed: true, ContentHash: "abc123"},
	}}
	probedFail := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/tmp/skills", RelDst: ".claude/skills", Probed: true, ContentHash: ""},
	}}
	configDerived := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/tmp/skills", RelDst: ".claude/skills"},
	}}

	hashOK := CoreFingerprint(probedOK)
	hashFail := CoreFingerprint(probedFail)
	hashConfig := CoreFingerprint(configDerived)

	// Failed probed hash should differ from successful (different content input).
	if hashOK == hashFail {
		t.Error("probed entry with hash vs without should differ")
	}
	// Failed probed hash should NOT equal config-derived (different mode).
	if hashFail == hashConfig {
		t.Error("probed entry with failed hash should not fall back to config-derived fingerprint")
	}
	// Running twice with failed hash should be stable.
	hashFail2 := CoreFingerprint(probedFail)
	if hashFail != hashFail2 {
		t.Error("probed entry with failed hash should produce stable fingerprint")
	}
}

func TestCoreFingerprintBreakdownConsistency(t *testing.T) {
	cfgs := []Config{
		{Command: "claude"},
		{Command: "claude", Env: map[string]string{"GC_CITY": "/x"}},
		{Command: "claude", CopyFiles: []CopyEntry{{Src: "/a", RelDst: "b"}}},
		{Command: "claude", CopyFiles: []CopyEntry{{RelDst: "b", Probed: true, ContentHash: "h1"}}},
		{Command: "claude", PreStart: []string{"echo hi"}},
		{Command: "claude", SessionSetup: []string{"set -x"}},
		{Command: "claude", OverlayDir: "/overlay"},
	}
	for i, a := range cfgs {
		for j, b := range cfgs {
			if i == j {
				continue
			}
			coreA := CoreFingerprint(a)
			coreB := CoreFingerprint(b)
			bdA := CoreFingerprintBreakdown(a)
			bdB := CoreFingerprintBreakdown(b)

			if coreA == coreB {
				continue // same core hash, nothing to check
			}
			// Core hashes differ — at least one breakdown field must differ.
			anyDiff := false
			for field, va := range bdA {
				if va != bdB[field] {
					anyDiff = true
					break
				}
			}
			if !anyDiff {
				t.Errorf("configs %d vs %d: CoreFingerprint differs but no CoreFingerprintBreakdown field differs", i, j)
			}
		}
	}
}

func TestHashPathContentFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1 := HashPathContent(f)
	if h1 == "" {
		t.Fatal("expected non-empty hash for file")
	}

	// Same content → same hash.
	h2 := HashPathContent(f)
	if h1 != h2 {
		t.Errorf("same file content produced different hashes: %s vs %s", h1, h2)
	}

	// Different content → different hash.
	if err := os.WriteFile(f, []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3 := HashPathContent(f)
	if h3 == h1 {
		t.Error("different file content should produce different hash")
	}
}

func TestHashPathContentDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "skills")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1 := HashPathContent(sub)
	if h1 == "" {
		t.Fatal("expected non-empty hash for directory")
	}

	// Same content → same hash.
	h2 := HashPathContent(sub)
	if h1 != h2 {
		t.Error("same directory content produced different hashes")
	}

	// Change a file → different hash.
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	h3 := HashPathContent(sub)
	if h3 == h1 {
		t.Error("different directory content should produce different hash")
	}
}

func TestHashPathContentMissingPath(t *testing.T) {
	h := HashPathContent("/nonexistent/path/that/does/not/exist")
	if h != "" {
		t.Errorf("expected empty hash for missing path, got %q", h)
	}
}

func TestHashPathContentUnreadableChild(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "skills")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "good.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a file then make it unreadable.
	bad := filepath.Join(sub, "bad.txt")
	if err := os.WriteFile(bad, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	h := HashPathContent(sub)
	if h != "" {
		t.Errorf("expected empty hash when child is unreadable, got %q", h)
	}
}

func TestLogCoreFingerprintDriftCopyFiles(t *testing.T) {
	stored := map[string]string{
		"CopyFiles": "oldhash",
		"Command":   "samehash",
	}
	current := Config{
		Command:   "claude",
		CopyFiles: []CopyEntry{{RelDst: "bar", ContentHash: "h1"}},
	}
	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "test-agent", stored, current)
	out := buf.String()
	if out == "" {
		t.Fatal("expected diagnostic output")
	}
	if !bytes.Contains([]byte(out), []byte("CopyFiles")) {
		t.Errorf("expected CopyFiles in drift output, got: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("RelDst")) {
		t.Errorf("expected RelDst detail in CopyFiles drift output, got: %s", out)
	}
}

// TestSkipFingerprintExcludesFromCoreHash locks in the fix for issue #682:
// CopyEntry values marked SkipFingerprint must not influence CoreFingerprint
// regardless of their ContentHash, Src, or RelDst values.
func TestSkipFingerprintExcludesFromCoreHash(t *testing.T) {
	base := Config{Command: "claude"}
	skipA := Config{Command: "claude", CopyFiles: []CopyEntry{
		{
			Src: "/tmp/worktree/.claude/skills", RelDst: ".claude/skills",
			Probed: true, ContentHash: "aaaa", SkipFingerprint: true,
		},
	}}
	skipB := Config{Command: "claude", CopyFiles: []CopyEntry{
		{
			Src: "/tmp/worktree/.claude/skills", RelDst: ".claude/skills",
			Probed: true, ContentHash: "bbbb", SkipFingerprint: true,
		},
	}}
	skipEmpty := Config{Command: "claude", CopyFiles: []CopyEntry{
		{
			Src: "/tmp/worktree/.claude/skills", RelDst: ".claude/skills",
			Probed: true, ContentHash: "", SkipFingerprint: true,
		},
	}}

	baseH := CoreFingerprint(base)
	if got := CoreFingerprint(skipA); got != baseH {
		t.Errorf("skipped entry changed fingerprint: base=%s got=%s", baseH, got)
	}
	if got := CoreFingerprint(skipB); got != baseH {
		t.Errorf("skipped entry with different ContentHash changed fingerprint: base=%s got=%s", baseH, got)
	}
	if got := CoreFingerprint(skipEmpty); got != baseH {
		t.Errorf("skipped entry with empty ContentHash changed fingerprint: base=%s got=%s", baseH, got)
	}
	// Breakdown must also report the same CopyFiles field hash as the base.
	baseBD := CoreFingerprintBreakdown(base)
	skipABD := CoreFingerprintBreakdown(skipA)
	if baseBD["CopyFiles"] != skipABD["CopyFiles"] {
		t.Errorf("skipped entry changed breakdown: base=%s got=%s", baseBD["CopyFiles"], skipABD["CopyFiles"])
	}
}

// TestSkipFingerprintIgnoredOnConfigDerivedEntries ensures the doc contract
// is enforced: SkipFingerprint is only honored on probed entries. A
// config-derived entry (Probed=false) with SkipFingerprint=true must still
// contribute to CoreFingerprint so real user edits drive drain.
func TestSkipFingerprintIgnoredOnConfigDerivedEntries(t *testing.T) {
	base := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/user/config.json", RelDst: "config.json"},
	}}
	withSkip := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/user/config.json", RelDst: "config.json", SkipFingerprint: true},
	}}
	if CoreFingerprint(base) != CoreFingerprint(withSkip) {
		t.Error("SkipFingerprint must be ignored on config-derived (Probed=false) entries")
	}
	// Changing the Src on a config-derived entry must still drive drift,
	// even if SkipFingerprint is set.
	edited := Config{Command: "claude", CopyFiles: []CopyEntry{
		{Src: "/user/config-edited.json", RelDst: "config.json", SkipFingerprint: true},
	}}
	if CoreFingerprint(withSkip) == CoreFingerprint(edited) {
		t.Error("config-derived Src change must drive drift even with SkipFingerprint=true")
	}
}

// TestSkipFingerprintStableUnderFilesystemChurn is the regression guard for
// issue #682: a probed entry whose underlying directory is populated between
// probes (simulating pre_start staging) must produce a stable CoreFingerprint
// when marked SkipFingerprint, and a drifting one otherwise.
func TestSkipFingerprintStableUnderFilesystemChurn(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Phase 1: empty skills dir — resembles template-resolve BEFORE pre_start
	// has finished populating the worktree.
	before := Config{Command: "claude", CopyFiles: []CopyEntry{{
		Src: skillsDir, RelDst: ".claude/skills",
		Probed: true, ContentHash: HashPathContent(skillsDir),
	}}}
	// Phase 2: pre_start completes and drops a file in the worktree.
	if err := os.WriteFile(filepath.Join(skillsDir, "new.md"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	after := Config{Command: "claude", CopyFiles: []CopyEntry{{
		Src: skillsDir, RelDst: ".claude/skills",
		Probed: true, ContentHash: HashPathContent(skillsDir),
	}}}

	// Without SkipFingerprint the hash MUST drift (this is the bug).
	beforeH := CoreFingerprint(before)
	afterH := CoreFingerprint(after)
	if beforeH == afterH {
		t.Fatal("test precondition: HashPathContent must observe filesystem change")
	}

	// With SkipFingerprint the same mutation must produce a stable hash.
	beforeSkip := before
	beforeSkip.CopyFiles = slices.Clone(before.CopyFiles)
	beforeSkip.CopyFiles[0].SkipFingerprint = true
	afterSkip := after
	afterSkip.CopyFiles = slices.Clone(after.CopyFiles)
	afterSkip.CopyFiles[0].SkipFingerprint = true

	if got1, got2 := CoreFingerprint(beforeSkip), CoreFingerprint(afterSkip); got1 != got2 {
		t.Errorf("SkipFingerprint did not stabilize CopyFiles hash under churn: before=%s after=%s", got1, got2)
	}
}

// TestLogCoreFingerprintDriftSkipsExcludedEntries ensures diagnostic output
// does not leak skipped entries, which would otherwise confuse operators
// debugging real drift.
func TestLogCoreFingerprintDriftSkipsExcludedEntries(t *testing.T) {
	stored := map[string]string{
		"CopyFiles": "oldhash",
	}
	current := Config{
		Command: "claude",
		CopyFiles: []CopyEntry{
			{RelDst: "real", ContentHash: "h1"},
			{RelDst: "skipped-churn", Probed: true, ContentHash: "h2", SkipFingerprint: true},
		},
	}
	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent", stored, current)
	out := buf.String()
	if !bytes.Contains([]byte(out), []byte("real")) {
		t.Errorf("expected non-skipped RelDst in drift output, got: %s", out)
	}
	if bytes.Contains([]byte(out), []byte("skipped-churn")) {
		t.Errorf("skipped entry leaked into drift output: %s", out)
	}
}
