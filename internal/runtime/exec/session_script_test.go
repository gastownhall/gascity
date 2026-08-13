package exec

import (
	"bytes"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

type screenSessionCommandFactory func(t *testing.T, args ...string) *osexec.Cmd

func TestScreenSessionScriptOwnedPathContract(t *testing.T) {
	run := func(t *testing.T, args ...string) *osexec.Cmd {
		t.Helper()
		return osexec.Command(screenSessionScriptPath(t), args...)
	}
	t.Run("declares capability", func(t *testing.T) {
		testScreenSessionScriptDeclaresOwnedPathCapability(t, run)
	})
	t.Run("preserves owned hook and stages siblings", func(t *testing.T) {
		testScreenSessionScriptPreservesOwnedHookAndStagesSiblings(t, run)
	})
	t.Run("fails closed when owned overlay filter fails", func(t *testing.T) {
		testScreenSessionScriptFailsClosedWhenOwnedOverlayFilterFails(t, run)
	})
	t.Run("fails closed when owned self-copy is missing", func(t *testing.T) {
		testScreenSessionScriptFailsClosedWhenOwnedSelfCopyIsMissing(t, run)
	})
	t.Run("rejects invalid owned copy contract", func(t *testing.T) {
		testScreenSessionScriptRejectsInvalidOwnedCopyContract(t, run)
	})
}

func testScreenSessionScriptDeclaresOwnedPathCapability(t *testing.T, run screenSessionCommandFactory) {
	cmd := run(t, "protocol")
	cmd.Env = append(os.Environ(), "GC_EXEC_STATE_DIR="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gc-session-screen protocol: %v", err)
	}
	var handshake struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(out, &handshake); err != nil {
		t.Fatalf("decode protocol response: %v\n%s", err, out)
	}
	for _, capability := range handshake.Capabilities {
		if capability == gcruntime.ProtocolCapabilityReconcilerOwnedMergeablePaths {
			return
		}
	}
	t.Fatalf("protocol capabilities = %v, missing %q", handshake.Capabilities, gcruntime.ProtocolCapabilityReconcilerOwnedMergeablePaths)
}

func testScreenSessionScriptPreservesOwnedHookAndStagesSiblings(t *testing.T, run screenSessionCommandFactory) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	canonical := []byte(`{"hooks":{"SessionStart":[{"matcher":"startup"}]}}`)
	if err := os.WriteFile(canonicalPath, canonical, 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}

	overlayDir := t.TempDir()
	for rel, contents := range map[string]string{
		filepath.Join(".codex", "hooks.json"):     `{"hooks":{"SessionStart":[{"matcher":""}]}}`,
		"AGENTS.codex.md":                         "ordinary sibling",
		filepath.Join(".gemini", "settings.json"): `{"hooks":{"BeforeAgent":[]}}`,
	} {
		path := filepath.Join(overlayDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir overlay %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("write overlay %s: %v", rel, err)
		}
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "screen"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake screen: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"command":                          "echo hi",
		"work_dir":                         workDir,
		"overlay_dir":                      overlayDir,
		"reconciler_owned_mergeable_paths": []string{".codex/hooks.json"},
		"copy_files": []map[string]any{{
			"src": canonicalPath, "rel_dst": ".codex/hooks.json", "probed": true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	cmd := run(t, "start", "mayor")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_EXEC_STATE_DIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gc-session-screen start: %v\n%s", err, out)
	}

	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical hook: %v", err)
	}
	if !bytes.Equal(got, canonical) {
		t.Fatalf("owned Codex hook changed:\ngot:  %s\nwant: %s", got, canonical)
	}
	for rel, want := range map[string]string{
		"AGENTS.codex.md":                         "ordinary sibling",
		filepath.Join(".gemini", "settings.json"): `{"hooks":{"BeforeAgent":[]}}`,
	} {
		data, err := os.ReadFile(filepath.Join(workDir, rel))
		if err != nil {
			t.Fatalf("read staged %s: %v", rel, err)
		}
		if string(data) != want {
			t.Fatalf("staged %s = %q, want %q", rel, data, want)
		}
	}
}

func testScreenSessionScriptFailsClosedWhenOwnedOverlayFilterFails(t *testing.T, run screenSessionCommandFactory) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}
	overlayDir := t.TempDir()
	hookPath := filepath.Join(overlayDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook dir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	binDir := t.TempDir()
	for name, script := range map[string]string{
		"screen": "#!/bin/sh\nexit 0\n",
		"tar":    "#!/bin/sh\nexit 42\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"command":                          "echo hi",
		"work_dir":                         workDir,
		"overlay_dir":                      overlayDir,
		"reconciler_owned_mergeable_paths": []string{".codex/hooks.json"},
		"copy_files": []map[string]any{{
			"src": canonicalPath, "rel_dst": ".codex/hooks.json", "probed": true,
			"content_hash": gcruntime.HashPathContent(canonicalPath),
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cmd := run(t, "start", "mayor")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_EXEC_STATE_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-session-screen unexpectedly started after tar failure; output:\n%s", out)
	}
	if !bytes.Contains(out, []byte("failed to filter reconciler-owned overlay paths")) {
		t.Fatalf("error output = %q, want owned-overlay filter failure", out)
	}
}

func testScreenSessionScriptFailsClosedWhenOwnedSelfCopyIsMissing(t *testing.T, run screenSessionCommandFactory) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	payload, err := json.Marshal(map[string]any{
		"command":                          "echo hi",
		"work_dir":                         workDir,
		"reconciler_owned_mergeable_paths": []string{".codex/hooks.json"},
		"copy_files": []map[string]any{{
			"src": canonicalPath, "rel_dst": ".codex/hooks.json", "probed": true,
			"content_hash": strings.Repeat("0", 64),
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cmd := run(t, "start", "mayor")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(), "GC_EXEC_STATE_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-session-screen unexpectedly started with missing owned self-copy; output:\n%s", out)
	}
	if !bytes.Contains(out, []byte("missing or unsafe reconciler-owned copy_file")) {
		t.Fatalf("error output = %q, want missing/unsafe owned copy_file failure", out)
	}
}

func testScreenSessionScriptRejectsInvalidOwnedCopyContract(t *testing.T, run screenSessionCommandFactory) {
	workDir := t.TempDir()
	canonicalPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hook dir: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write canonical hook: %v", err)
	}
	validHash := gcruntime.HashPathContent(canonicalPath)
	validEntry := func() map[string]any {
		return map[string]any{
			"src": canonicalPath, "rel_dst": ".codex/hooks.json",
			"probed": true, "content_hash": validHash,
		}
	}
	tests := []struct {
		name    string
		entries func() []map[string]any
		want    string
	}{
		{
			name: "duplicate destination",
			entries: func() []map[string]any {
				return []map[string]any{validEntry(), validEntry()}
			},
			want: "requires exactly one copy_file",
		},
		{
			name: "not probed",
			entries: func() []map[string]any {
				entry := validEntry()
				entry["probed"] = false
				return []map[string]any{entry}
			},
			want: "must be probed",
		},
		{
			name: "missing digest",
			entries: func() []map[string]any {
				entry := validEntry()
				delete(entry, "content_hash")
				return []map[string]any{entry}
			},
			want: "requires a SHA-256 content_hash",
		},
		{
			name: "stale digest",
			entries: func() []map[string]any {
				entry := validEntry()
				entry["content_hash"] = strings.Repeat("f", 64)
				return []map[string]any{entry}
			},
			want: "changed after reconciliation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"command":                          "echo hi",
				"work_dir":                         workDir,
				"reconciler_owned_mergeable_paths": []string{".codex/hooks.json"},
				"copy_files":                       tc.entries(),
			})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			cmd := run(t, "start", "mayor")
			cmd.Stdin = bytes.NewReader(payload)
			cmd.Env = append(os.Environ(), "GC_EXEC_STATE_DIR="+t.TempDir())
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("gc-session-screen accepted invalid ownership contract; output:\n%s", out)
			}
			if !bytes.Contains(out, []byte(tc.want)) {
				t.Fatalf("error output = %q, want %q", out, tc.want)
			}
		})
	}
}

func screenSessionScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-screen"))
}
