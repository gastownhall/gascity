package citylayout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func excludedByDefault(t *testing.T, rel string) bool {
	t.Helper()
	got, err := WorkDirExportExcluded(defaultWorkDirExportExcludes, rel)
	if err != nil {
		t.Fatalf("WorkDirExportExcluded(%q): %v", rel, err)
	}
	return got
}

func matchExportExclude(t *testing.T, pattern, rel string) bool {
	t.Helper()
	got, err := MatchWorkDirExclude(pattern, rel)
	if err != nil {
		t.Fatalf("MatchWorkDirExclude(%q, %q): %v", pattern, rel, err)
	}
	return got
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
		".gc/agents/dog/src/main.go",
		".gc/scope-ownership.json",
		".gc/mcp-locks/abc.lock",
		".gc/dispatch-wake",
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
		".beads/doltlite/hq.db",
		".beads/doltlite/hq.db-wal",
		".beads/doltlite/hq.db-shm",
		"rigs/app/.beads/doltlite/app.db",
		".gc/sessions/s-1/start-stderr.log",
		".gc/sessions/s-1/nudge-unconfirmed.log",
		".gc/transcripts/acp/s-1/1.jsonl",
		".gc/gate-slots/slot-1",
		".beads/dolt.20260101T000000.corrupt.backup/hq/.dolt/noms/x",
		".beads/dolt-backups/success/1/x",
		".gc/beads.json",
		"rigs/app/.gc/beads.json",
		".gc/beads.json.bak",
		".gc/credentials.json",
		".gc/jsonl-archive/2026-01.jsonl",
		".gc/jsonl-export-state.json",
		".gc/mcp-adopted/claude/settings.json",
		".gc/emergency/spool/1.json",
		".beads/ownership-handoff.json",
		".beads/dolt-handoff-L1.yaml",
		".beads/export-state/x.json",
		".beads/export-state.json",
		".beads/dolt-pprof/cpu.pprof",
		".beads/sync-state.json",
		".beads/push-state.json",
		".beads/last-touched",
		".beads/.local_version",
		".beads/last_pull",
		".beads/.exclusive-lock",
		".beads/db.sqlite",
		".gc/beads.sqlite",
		".gc/beads.sqlite-shm",
		".gc/backups/2026-10-01/beads.sqlite",
		"var/store/graph/beads.sqlite-wal",
		"rigs/app/var/store/beads.sqlite",
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
		".gc/artifacts/b-1/iter-1/out.txt",
		".gc/synth/role.meta.md",
		".gc/file-beads-layout",
		".beads/metadata.json",
		".beads/config.yaml",
		".beads/http_target.json",
		".beads/redirect",
		"rigs/app/.beads/metadata.json",
		"rigs/app/.beads/config.yaml",
		"rigs/app/.gc/settings.json",
		"src/main.go",
		"src/.gc/beads.json.go",
	} {
		if excludedByDefault(t, rel) {
			t.Errorf("%s is needed by sessions and must not be excluded", rel)
		}
	}
}

func TestWorkDirExportExcludesIsAFreshCopyOfTheDefaults(t *testing.T) {
	got := WorkDirExportExcludes()
	if strings.Join(got, "\n") != strings.Join(defaultWorkDirExportExcludes, "\n") {
		t.Fatalf("WorkDirExportExcludes = %v, want the default list", got)
	}
	got[0] = "mutated"
	if defaultWorkDirExportExcludes[0] == "mutated" {
		t.Fatal("WorkDirExportExcludes returned the shared default slice")
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

// soleMatchProbes pins every default pattern to a path that pattern alone
// excludes, so deleting or narrowing any single pattern fails a test even when
// broader patterns overlap it. A pattern starting with "**/" must be pinned
// by a nested path, so pinning it to the work-dir root also fails.
var soleMatchProbes = map[string]string{
	".gc/runtime/**":                   ".gc/runtime/suspension-state.json",
	".gc/services/**":                  ".gc/services/github-intake/secrets/token",
	".gc/sessions/**":                  ".gc/sessions/s-1/start-stderr.log",
	".gc/transcripts/**":               ".gc/transcripts/acp/s-1/1.jsonl",
	".gc/session-name-locks/**":        ".gc/session-name-locks/a",
	".gc/gate-slots/**":                ".gc/gate-slots/slot-1",
	".gc/secrets/**":                   ".gc/secrets/s-1.key",
	".gc/controller.token":             ".gc/controller.token",
	".gc/credentials.toml":             ".gc/credentials.toml",
	".gc/credentials.json":             ".gc/credentials.json",
	".gc/**/*.sock":                    ".gc/controller.sock",
	".gc/**/*.pid":                     ".gc/controller.pid",
	".gc/*.lock":                       ".gc/controller.lock",
	".gc/events.jsonl*":                ".gc/events.jsonl",
	".gc/usage.jsonl*":                 ".gc/usage.jsonl",
	".gc/nudges/**":                    ".gc/nudges/state.json",
	".gc/store/**":                     ".gc/store/sessions.db",
	".gc/storage/**":                   ".gc/storage/ref/.beads/metadata.json",
	".gc/infra/**":                     ".gc/infra/x/config.yaml",
	".gc/dolt-data/**":                 ".gc/dolt-data/hq/.dolt/noms/x",
	".gc/session-mcp/**":               ".gc/session-mcp/s-1.json",
	".gc/supervisor/**":                ".gc/supervisor/publications.json",
	".gc/worktrees/**":                 ".gc/worktrees/rig/agent/main.go",
	".gc/agents/**":                    ".gc/agents/dog/src/main.go",
	".gc/scope-ownership.json":         ".gc/scope-ownership.json",
	".gc/mcp-locks/**":                 ".gc/mcp-locks/abc",
	".gc/dispatch-wake":                ".gc/dispatch-wake",
	".gc/storage-served-binding.json":  ".gc/storage-served-binding.json",
	".gc/storage-infra-cleared.json":   ".gc/storage-infra-cleared.json",
	".gc/mcp-adopted/**":               ".gc/mcp-adopted/claude/settings.json",
	".gc/emergency/**":                 ".gc/emergency/spool/1.json",
	".gc/jsonl-archive/**":             ".gc/jsonl-archive/2026-01.jsonl",
	".gc/jsonl-export-state.json":      ".gc/jsonl-export-state.json",
	"**/beads.sqlite*":                 ".gc/backups/2026-10-01/beads.sqlite-wal",
	"**/.gc/beads.json":                "rigs/app/.gc/beads.json",
	"**/.gc/beads.json.bak":            "rigs/app/.gc/beads.json.bak",
	"**/.beads/dolt/**":                "rigs/app/.beads/dolt/app/.dolt/noms/x",
	"**/.beads/doltlite/**":            "rigs/app/.beads/doltlite/app.db",
	"**/.beads/embeddeddolt/**":        "rigs/app/.beads/embeddeddolt/app/.dolt/noms/x",
	"**/.beads/dolt-backups/**":        "rigs/app/.beads/dolt-backups/current/x",
	"**/.beads/*.corrupt.backup/**":    "rigs/app/.beads/dolt.1.corrupt.backup/x",
	"**/.beads/backup/**":              "rigs/app/.beads/backup/2026.sql",
	"**/.beads/export-state*":          "rigs/app/.beads/export-state/x.json",
	"**/.beads/dolt-pprof/**":          "rigs/app/.beads/dolt-pprof/cpu.pprof",
	"**/.beads/*.db*":                  "rigs/app/.beads/beads.db",
	"**/.beads/db.sqlite":              "rigs/app/.beads/db.sqlite",
	"**/.beads/*.sqlite3*":             "rigs/app/.beads/ephemeral.sqlite3",
	"**/.beads/*.lock":                 "rigs/app/.beads/.sync.lock",
	"**/.beads/.exclusive-lock":        "rigs/app/.beads/.exclusive-lock",
	"**/.beads/dolt-server*":           "rigs/app/.beads/dolt-server.port",
	"**/.beads/proxy*":                 "rigs/app/.beads/proxy.secret",
	"**/.beads/proxied*":               "rigs/app/.beads/proxieddb/x",
	"**/.beads/daemon.*":               "rigs/app/.beads/daemon.log",
	"**/.beads/bd.sock*":               "rigs/app/.beads/bd.sock",
	"**/.beads/.env":                   "rigs/app/.beads/.env",
	"**/.beads/.beads-credential-key":  "rigs/app/.beads/.beads-credential-key",
	"**/.beads/sync-state.json":        "rigs/app/.beads/sync-state.json",
	"**/.beads/push-state.json":        "rigs/app/.beads/push-state.json",
	"**/.beads/last-touched":           "rigs/app/.beads/last-touched",
	"**/.beads/.local_version":         "rigs/app/.beads/.local_version",
	"**/.beads/last_pull":              "rigs/app/.beads/last_pull",
	"**/.beads/ownership-handoff.json": "rigs/app/.beads/ownership-handoff.json",
	"**/.beads/dolt-handoff-*":         "rigs/app/.beads/dolt-handoff-L1.yaml",
}

func TestEveryDefaultExcludeHasASoleMatchProbe(t *testing.T) {
	inDefault := map[string]bool{}
	for _, p := range defaultWorkDirExportExcludes {
		inDefault[p] = true
		probe, ok := soleMatchProbes[p]
		if !ok {
			t.Errorf("pattern %q has no sole-match probe in soleMatchProbes", p)
			continue
		}
		for _, other := range defaultWorkDirExportExcludes {
			if got := matchExportExclude(t, other, probe); got != (other == p) {
				t.Errorf("probe %q: match(%q) = %v, want %v", probe, other, got, other == p)
			}
		}
		if rooted, ok := strings.CutPrefix(p, "**/"); ok && matchExportExclude(t, rooted, probe) {
			t.Errorf("probe %q for %q is not nested, so pinning the pattern to the work-dir root would go unnoticed", probe, p)
		}
	}
	for p := range soleMatchProbes {
		if !inDefault[p] {
			t.Errorf("soleMatchProbes names %q, which is not a default pattern", p)
		}
	}
}

// exportKeptRuntimeRoots lists the .gc/ roots citylayout names that sessions
// read at start, so they ship with the work dir.
var exportKeptRuntimeRoots = map[string]string{
	SystemRoot:        "bundled system packs a session's gc loads with city.toml",
	SystemPacksRoot:   "bundled system packs a session's gc loads with city.toml",
	CacheRoot:         "fetched pack sources a session's gc loads with city.toml",
	CachePacksRoot:    "fetched pack sources a session's gc loads with city.toml",
	CacheIncludesRoot: "fetched include sources a session's gc loads with city.toml",
}

// TestEveryCitylayoutRuntimeRootIsClassified scans this package's source for
// every .gc/ path it names, as a constant or as a RuntimePath(...) call with
// literal segments, and requires each to be excluded from the work-dir export
// or kept on purpose in exportKeptRuntimeRoots. New orchestrator state added
// here has to be classified instead of shipping to remote sandboxes silently.
func TestEveryCitylayoutRuntimeRootIsClassified(t *testing.T) {
	roots := citylayoutRuntimeRoots(t)
	for _, want := range []string{".gc/sessions", ".gc/transcripts/acp", ".gc/services", ".gc/cache"} {
		if !roots[want] {
			t.Fatalf("source scan found %v, missing %q; the scanner no longer sees citylayout's roots", roots, want)
		}
	}
	for root := range roots {
		if _, kept := exportKeptRuntimeRoots[root]; kept {
			if excludedByDefault(t, root+"/x") {
				t.Errorf("%s is on the keep list but the default excludes drop it", root)
			}
			continue
		}
		if !excludedByDefault(t, root) {
			t.Errorf("%s is neither excluded from the work-dir export nor on exportKeptRuntimeRoots", root)
		}
	}
}

func citylayoutRuntimeRoots(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, f)
	}
	if len(parsed) == 0 {
		t.Fatal("no citylayout sources found to scan")
	}
	consts := map[string]ast.Expr{}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						consts[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	var eval func(ast.Expr) (string, bool)
	eval = func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		case *ast.Ident:
			if c, ok := consts[v.Name]; ok {
				return eval(c)
			}
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				l, lok := eval(v.X)
				r, rok := eval(v.Y)
				return l + r, lok && rok
			}
		}
		return "", false
	}
	roots := map[string]bool{}
	for _, c := range consts {
		if s, ok := eval(c); ok && strings.HasPrefix(s, RuntimeRoot+"/") {
			roots[s] = true
		}
	}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "RuntimePath" || len(call.Args) < 2 || call.Ellipsis.IsValid() {
				return true
			}
			segs := []string{RuntimeRoot}
			for _, a := range call.Args[1:] {
				s, ok := eval(a)
				if !ok {
					return true
				}
				segs = append(segs, s)
			}
			roots[path.Join(segs...)] = true
			return true
		})
	}
	return roots
}

func TestWorkDirExportExcludedRejectsBadPattern(t *testing.T) {
	if _, err := WorkDirExportExcluded([]string{"[unclosed"}, "x"); err == nil {
		t.Fatal("WorkDirExportExcluded accepted a malformed pattern")
	}
	if _, err := WorkDirExportExcluded([]string{".gc/**", "a/[bad"}, ".gc/x"); err == nil {
		t.Fatal("WorkDirExportExcluded hid a malformed pattern behind an earlier match")
	}
	for _, rel := range []string{"a", "b", "a/x", "a/x/y"} {
		if _, err := MatchWorkDirExclude("a/[bad", rel); err == nil {
			t.Errorf("MatchWorkDirExclude(%q, %q) accepted a malformed segment", "a/[bad", rel)
		}
	}
}

func writeExportTree(t *testing.T, dir string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func exportEntries(t *testing.T, dir string) []string {
	t.Helper()
	got, err := WorkDirExportEntries(dir)
	if err != nil {
		t.Fatalf("WorkDirExportEntries: %v", err)
	}
	return got
}

func assertExportEntries(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("WorkDirExportEntries =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestWorkDirExportEntries(t *testing.T) {
	dir := t.TempDir()
	writeExportTree(t, dir,
		".gc/controller.token",
		".gc/settings.json",
		".gc/sessions/s-1/start-stderr.log",
		".beads/doltlite/hq.db",
		".beads/metadata.json",
		"a/b",
		"a-c",
		"src/main.go",
	)
	if err := os.MkdirAll(filepath.Join(dir, "empty", "nested-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, ".gc"), filepath.Join(dir, "gc-link")); err != nil {
		t.Fatal(err)
	}
	assertExportEntries(t, exportEntries(t, dir),
		".beads/metadata.json",
		".gc/settings.json",
		"a/b",
		"a-c",
		"empty/nested-empty",
		"gc-link",
		"src/main.go",
	)
}

// TestWorkDirExportEntriesSurviveARecursiveConsumer expands every listed
// entry the way tar, rsync -r, or cp -r would, and requires the expansion to
// add nothing excluded.
func TestWorkDirExportEntriesSurviveARecursiveConsumer(t *testing.T) {
	dir := t.TempDir()
	writeExportTree(t, dir,
		".gc/controller.token",
		".gc/secrets/s-1.key",
		".gc/settings.json",
		".beads/.env",
		".beads/metadata.json",
		"rigs/app/.beads/doltlite/app.db",
		"rigs/app/main.go",
	)
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range exportEntries(t, dir) {
		err := filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(entry)), func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			if excludedByDefault(t, filepath.ToSlash(rel)) {
				t.Errorf("recursing into listed entry %q ships excluded %q", entry, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkDirExportEntriesExcludeNestedScopesWithoutTopLevelMarker(t *testing.T) {
	dir := t.TempDir()
	writeExportTree(t, dir,
		"appA/.beads/.env",
		"appA/.beads/config.yaml",
		"appA/main.go",
		"appB/.beads/doltlite/b.db",
		"appB/.gc/beads.json",
		"appB/main.go",
	)
	assertExportEntries(t, exportEntries(t, dir),
		"appA/.beads/config.yaml",
		"appA/main.go",
		"appB/main.go",
	)
}

func TestWorkDirExportEntriesResolveASymlinkedWorkDir(t *testing.T) {
	parent := t.TempDir()
	cityDir := filepath.Join(parent, "city")
	writeExportTree(t, cityDir, ".gc/controller.token", ".gc/settings.json", "src/a")
	link := filepath.Join(parent, "linkcity")
	if err := os.Symlink(cityDir, link); err != nil {
		t.Fatal(err)
	}
	assertExportEntries(t, exportEntries(t, link), ".gc/settings.json", "src/a")
}

func TestWorkDirExportEntriesWithoutRuntimeStateListsEverything(t *testing.T) {
	dir := t.TempDir()
	writeExportTree(t, dir, "events.jsonl")
	assertExportEntries(t, exportEntries(t, dir), "events.jsonl")
}

func TestWorkDirExportEntriesErrors(t *testing.T) {
	if _, err := WorkDirExportEntries(""); err == nil {
		t.Error("empty work dir: want error")
	}
	if _, err := WorkDirExportEntries(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing work dir: want error")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WorkDirExportEntries(file); err == nil {
		t.Error("file work dir: want error")
	}
}
