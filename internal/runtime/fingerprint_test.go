package runtime

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestConfigFingerprintIgnoresGCAlias(t *testing.T) {
	base := Config{Command: "claude", Env: map[string]string{
		"GC_CITY":     "/gc",
		"GC_TEMPLATE": "repo/coder",
	}}
	withAlias := Config{Command: "claude", Env: map[string]string{
		"GC_CITY":     "/gc",
		"GC_TEMPLATE": "repo/coder",
		"GC_ALIAS":    "repo/coder-1",
	}}
	if ConfigFingerprint(base) != ConfigFingerprint(withAlias) {
		t.Error("GC_ALIAS should not affect config fingerprint")
	}
	if CoreFingerprint(base) != CoreFingerprint(withAlias) {
		t.Error("GC_ALIAS should not affect core config fingerprint")
	}
	if CoreFingerprintBreakdown(base)["Env"] != CoreFingerprintBreakdown(withAlias)["Env"] {
		t.Error("GC_ALIAS should not affect core env fingerprint breakdown")
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

func TestConfigFingerprintIncludesMCPServers(t *testing.T) {
	a := Config{Command: "claude"}
	b := Config{
		Command: "claude",
		MCPServers: []MCPServerConfig{{
			Name:      "filesystem",
			Transport: MCPTransportStdio,
			Command:   "/bin/mcp",
			Args:      []string{"--stdio"},
		}},
	}
	if ConfigFingerprint(a) == ConfigFingerprint(b) {
		t.Error("MCPServers should change the config fingerprint")
	}
}

func TestConfigFingerprintMCPServersOrderIndependent(t *testing.T) {
	a := Config{
		Command: "claude",
		MCPServers: []MCPServerConfig{
			{Name: "remote", Transport: MCPTransportHTTP, URL: "https://mcp.example", Headers: map[string]string{"Authorization": "token"}},
			{Name: "filesystem", Transport: MCPTransportStdio, Command: "/bin/mcp", Args: []string{"--stdio"}, Env: map[string]string{"TOKEN": "abc"}},
		},
	}
	b := Config{
		Command: "claude",
		MCPServers: []MCPServerConfig{
			{Name: "filesystem", Transport: MCPTransportStdio, Command: "/bin/mcp", Args: []string{"--stdio"}, Env: map[string]string{"TOKEN": "abc"}},
			{Name: "remote", Transport: MCPTransportHTTP, URL: "https://mcp.example", Headers: map[string]string{"Authorization": "token"}},
		},
	}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("MCPServers order should not affect hash")
	}
}

func TestConfigFingerprintNilVsEmptyExtra(t *testing.T) {
	a := Config{Command: "claude", FingerprintExtra: nil}
	b := Config{Command: "claude", FingerprintExtra: map[string]string{}}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("nil and empty FingerprintExtra should produce the same hash")
	}
}

func TestConfigFingerprintIgnoresNudge(t *testing.T) {
	a := Config{Command: "claude", Nudge: ""}
	b := Config{Command: "claude", Nudge: "hello agent"}
	if ConfigFingerprint(a) != ConfigFingerprint(b) {
		t.Error("different Nudge should not produce different hashes")
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

func TestHashPathContentDirectoryIgnoresRuntimeGeneratedArtifacts(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{
			name: "__pycache__",
			write: func(t *testing.T, dir string) {
				t.Helper()
				cacheDir := filepath.Join(dir, "__pycache__")
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cacheDir, "check.cpython-312.pyc"), []byte("cache-a"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: ".pytest_cache",
			write: func(t *testing.T, dir string) {
				t.Helper()
				cacheDir := filepath.Join(dir, ".pytest_cache", "v")
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cacheDir, "cache"), []byte("pytest"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: ".mypy_cache",
			write: func(t *testing.T, dir string) {
				t.Helper()
				cacheDir := filepath.Join(dir, ".mypy_cache", "3.12")
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cacheDir, "module.data.json"), []byte("mypy"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: ".ruff_cache",
			write: func(t *testing.T, dir string) {
				t.Helper()
				cacheDir := filepath.Join(dir, ".ruff_cache")
				if err := os.MkdirAll(cacheDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cacheDir, "CACHEDIR.TAG"), []byte("ruff"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: ".pyc file",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "check.pyc"), []byte("cache-a"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: ".pyo file",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "check.pyo"), []byte("cache-a"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "editor backup suffix",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "check.py~"), []byte("backup"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "vim swap file",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, ".check.py.swp"), []byte("swap"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "vim swap extension file",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, ".check.py.swx"), []byte("swap"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sub := filepath.Join(dir, "scripts")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "check.py"), []byte("print('ok')\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			h1 := HashPathContent(sub)
			if h1 == "" {
				t.Fatal("expected non-empty hash for directory")
			}

			tt.write(t, sub)
			h2 := HashPathContent(sub)
			if h2 != h1 {
				t.Fatalf("%s changed directory hash: %s vs %s", tt.name, h1, h2)
			}
		})
	}
}

func TestHashPathContentDirectoryFingerprintsUserAuthoredTempExtensionFiles(t *testing.T) {
	tests := []string{
		"payload.tmp",
		"fixture.temp",
		"notes.swp",
		"notes.swx",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sub := filepath.Join(dir, "scripts")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "check.py"), []byte("print('ok')\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			h1 := HashPathContent(sub)
			if h1 == "" {
				t.Fatal("expected non-empty hash for directory")
			}

			if err := os.WriteFile(filepath.Join(sub, name), []byte("user-authored"), 0o644); err != nil {
				t.Fatal(err)
			}
			h2 := HashPathContent(sub)
			if h2 == h1 {
				t.Fatalf("user-authored %s should change directory hash", name)
			}
		})
	}
}

func TestHashPathContentDirectoryFingerprintsSourceFileChanges(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "check.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1 := HashPathContent(sub)
	if h1 == "" {
		t.Fatal("expected non-empty hash for directory")
	}

	if err := os.WriteFile(filepath.Join(sub, "check.py"), []byte("print('changed')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2 := HashPathContent(sub)
	if h2 == h1 {
		t.Fatal("source file changes should change directory hash")
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

func TestCoreFingerprintDriftFields(t *testing.T) {
	current := Config{
		Command:   "claude",
		CopyFiles: []CopyEntry{{RelDst: "bar", Probed: true, ContentHash: "newhash"}},
	}
	stored := CoreFingerprintBreakdown(current)
	stored["CopyFiles"] = "oldhash"

	got := CoreFingerprintDriftFields(stored, current)
	if len(got) != 1 || got[0] != "CopyFiles" {
		t.Fatalf("CoreFingerprintDriftFields = %v, want [CopyFiles]", got)
	}

	if got := CoreFingerprintDriftFields(nil, current); len(got) != 0 {
		t.Fatalf("CoreFingerprintDriftFields with missing breakdown = %v, want empty", got)
	}
}
