package citylayout

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// matchExportExclude is the reference implementation of the
// work_dir_exclude pattern grammar documented on [WorkDirExportExcludes]:
// patterns and paths are slash-separated and relative to the work dir, a
// segment of exactly "**" matches zero or more whole segments, every other
// segment is a [path.Match] glob confined to one segment, and a path is
// excluded when it or any directory above it matches.
func matchExportExclude(t *testing.T, pattern, rel string) bool {
	t.Helper()
	pat := strings.Split(pattern, "/")
	segs := strings.Split(rel, "/")
	for n := 1; n <= len(segs); n++ {
		if matchSegments(t, pat, segs[:n]) {
			return true
		}
	}
	return false
}

func matchSegments(t *testing.T, pat, segs []string) bool {
	t.Helper()
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(t, pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil {
		t.Fatalf("pattern segment %q: %v", pat[0], err)
	}
	return ok && matchSegments(t, pat[1:], segs[1:])
}

func excludedByDefault(t *testing.T, rel string) bool {
	t.Helper()
	for _, p := range defaultWorkDirExportExcludes {
		if matchExportExclude(t, p, rel) {
			return true
		}
	}
	return false
}

func TestDefaultWorkDirExportExcludesArePortablePatterns(t *testing.T) {
	if len(defaultWorkDirExportExcludes) == 0 {
		t.Fatal("default export excludes are empty")
	}
	seen := map[string]bool{}
	for _, p := range defaultWorkDirExportExcludes {
		if seen[p] {
			t.Errorf("duplicate pattern %q", p)
		}
		seen[p] = true
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			t.Errorf("pattern %q must be a non-empty slash-separated relative path", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Errorf("pattern %q has invalid segment %q", p, seg)
			}
			if seg != "**" && strings.Contains(seg, "**") {
				t.Errorf("pattern %q: ** must be a whole segment", p)
			}
			if _, err := path.Match(seg, ""); err != nil {
				t.Errorf("pattern %q segment %q: %v", p, seg, err)
			}
		}
	}
}

func TestDefaultWorkDirExportExcludesDenyControllerState(t *testing.T) {
	for _, rel := range []string{
		".gc/services/github-intake/secrets/token",
		".gc/services/a/b/secrets/token",
		".gc/services/.published/web.json",
		".gc/secrets/s-1.key",
		".gc/controller.token",
		".gc/credentials.toml",
		".gc/controller.sock",
		".gc/nudges/wake.sock",
		".gc/controller.lock",
		".gc/scope-ownership.lock",
		".gc/events.jsonl",
		".gc/events.jsonl.lock",
		".gc/usage.jsonl",
		".gc/nudges/state.json",
		".gc/nudges/pollers/a.pid",
		".gc/store/sessions.db",
		".gc/storage/ref/.beads/beads.db",
		".gc/infra/.beads/beads.db",
		".gc/dolt-data/hq/.dolt/noms/x",
		".gc/runtime/suspension-state.json",
		".gc/runtime/packs/dolt/dolt.pid",
		".gc/session-mcp/s-1.json",
		".gc/session-name-locks/a.lock",
		".gc/supervisor/publications.json",
		".gc/worktrees/rig/agent/main.go",
		".gc/storage-served-binding.json",
		".gc/storage-infra-cleared.json",
		".gc/cache/packs/x/child.pid",
		".beads/dolt/hq/.dolt/noms/x",
		".beads/embeddeddolt/hq/.dolt/noms/x",
		".beads/proxieddb/x",
		".beads/beads.db",
		".beads/beads.db-wal",
		".beads/ephemeral.sqlite3",
		".beads/dolt-server.port",
		".beads/dolt-server-config.yaml",
		".beads/proxy.secret",
		".beads/proxy-child.pid",
		".beads/proxied_server_client_info.json",
		".beads/.beads-credential-key",
		".beads/.env",
		".beads/daemon.log",
		".beads/bd.sock",
		".beads/.sync.lock",
		".beads/backup/2026.sql",
		"rigs/app/.beads/dolt/app/.dolt/noms/x",
		"rigs/app/.beads/proxy.secret",
	} {
		if !excludedByDefault(t, rel) {
			t.Errorf("%s should be excluded from the work-dir export", rel)
		}
	}
}

func TestDefaultWorkDirExportExcludesKeepDeploymentArtifacts(t *testing.T) {
	for _, rel := range []string{
		"city.toml",
		"pack.toml",
		"pack.lock",
		"packs.lock",
		".gc/settings.json",
		".gc/site.toml",
		".gc/cache/packs/gastown/pack.toml",
		".gc/cache/packs/gastown/packs.lock",
		".gc/cache/includes/x/pack.toml",
		".gc/system/packs/core/pack.toml",
		".gc/scripts/check.sh",
		".gc/tmp/skill-catalog-rig_agent.b64",
		".gc/tmp/agent-prompt.md",
		".gc/mcp-managed/claude.json",
		".gc/molecules/root/artifacts/b-1/out.txt",
		".beads/metadata.json",
		".beads/config.yaml",
		".beads/http_target.json",
		".beads/redirect",
		"rigs/app/.beads/metadata.json",
		"src/main.go",
	} {
		if excludedByDefault(t, rel) {
			t.Errorf("%s is needed by sessions and must not be excluded", rel)
		}
	}
}

func TestWorkDirExportExcludes(t *testing.T) {
	t.Run("empty work dir", func(t *testing.T) {
		if got := WorkDirExportExcludes(""); got != nil {
			t.Fatalf("WorkDirExportExcludes(\"\") = %v, want nil", got)
		}
	})
	t.Run("unrelated work dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := WorkDirExportExcludes(dir); got != nil {
			t.Fatalf("WorkDirExportExcludes(unrelated) = %v, want nil", got)
		}
	})
	t.Run(".gc file is not a runtime root", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, RuntimeRoot), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := WorkDirExportExcludes(dir); got != nil {
			t.Fatalf("WorkDirExportExcludes(.gc file) = %v, want nil", got)
		}
	})
	for _, marker := range []string{RuntimeRoot, BeadsRoot} {
		t.Run("work dir with "+marker, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, marker), 0o755); err != nil {
				t.Fatal(err)
			}
			got := WorkDirExportExcludes(dir)
			if len(got) != len(defaultWorkDirExportExcludes) {
				t.Fatalf("WorkDirExportExcludes = %v, want the default list", got)
			}
			for i := range got {
				if got[i] != defaultWorkDirExportExcludes[i] {
					t.Fatalf("WorkDirExportExcludes[%d] = %q, want %q", i, got[i], defaultWorkDirExportExcludes[i])
				}
			}
			got[0] = "mutated"
			if defaultWorkDirExportExcludes[0] == "mutated" {
				t.Fatal("WorkDirExportExcludes returned the shared default slice")
			}
		})
	}
}

func TestMatchExportExcludeGrammar(t *testing.T) {
	for _, tc := range []struct {
		pattern, rel string
		want         bool
	}{
		{".gc/nudges/**", ".gc/nudges/state.json", true},
		{".gc/nudges/**", ".gc/nudges", true},
		{".gc/nudges/**", ".gc/nudgesx/state.json", false},
		{".gc/*.lock", ".gc/controller.lock", true},
		{".gc/*.lock", ".gc/cache/packs.lock", false},
		{".gc/**/*.sock", ".gc/controller.sock", true},
		{".gc/**/*.sock", ".gc/nudges/wake.sock", true},
		{"**/.beads/proxy*", ".beads/proxy.secret", true},
		{"**/.beads/proxy*", "rigs/a/.beads/proxy.secret", true},
		{"**/.beads/proxied*", ".beads/proxieddb/x/y", true},
		{"**/.beads/*.lock", ".beads/.sync.lock", true},
	} {
		if got := matchExportExclude(t, tc.pattern, tc.rel); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}
