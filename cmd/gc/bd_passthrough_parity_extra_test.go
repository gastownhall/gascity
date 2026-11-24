//go:build !windows

package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// bdParityAssigneeFakeBd is bdParityFakeBd for the claim-identity rows: show
// answers a bead claimed by worker-x, and BD_PARITY_SILENT makes update print
// bd's silent-fallback warning.
const bdParityAssigneeFakeBd = `#!/bin/sh
{
  printf 'argv=%s\n' "$*"
  printf 'pwd=%s\n' "$(pwd -P)"
  printf 'bin=%s\n' "$0"
  for v in GC_STORE_ROOT GC_STORE_SCOPE GC_BEADS_PREFIX BEADS_DIR GC_RIG GC_RIG_ROOT BD_EXPORT_AUTO GC_BIN BD_ACTOR BEADS_ACTOR GC_BD_ALLOW_RELOCATED_CLASS_READ; do
    eval "printf '%s=%s\n' $v \"\${$v}\""
  done
  printf -- '--\n'
} >> "${BD_PARITY_LOG}.$$"
case "$1" in
  show) echo '[{"id":"demo-abc","title":"t","status":"in_progress","issue_type":"task","assignee":"worker-x"}]' ;;
  list|ready) echo '[]' ;;
  update) echo '{"id":"demo-abc"}'; if [ -n "$BD_PARITY_SILENT" ]; then echo 'auto-importing 3 issues into empty database' >&2; fi ;;
  *) : ;;
esac
exit 0
`

func useBdParityAssigneeFake(t *testing.T, c bdParityCity) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.bin, "bd"), []byte(bdParityAssigneeFakeBd), 0o755); err != nil {
		t.Fatal(err)
	}
}

// corruptBundledBdCacheMarker readies the city's builtin caches, forgets that
// this process readied them, and rewrites the bd pack cache's marker to name
// the wrong repository, which composition rejects. It returns the cache dir.
func corruptBundledBdCacheMarker(t *testing.T, cityDir string) string {
	t.Helper()
	materializeBuiltinPacksForTest(t, cityDir)
	resetBuiltinRuntimeReadyCacheFor(cityDir)
	source, ok := builtinpacks.Source("bd")
	if !ok {
		t.Fatal("no bundled source for the bd pack")
	}
	commit := strings.TrimPrefix(config.BundledSourcePinnedVersion(source), "sha:")
	cacheDir := config.GlobalRepoCachePath(os.Getenv("GC_HOME"), source, commit)
	marker := filepath.Join(cacheDir, ".gc-bundled-pack-cache.toml")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), "repository = ", "repository = \"github.com/wrong/repo\"\nunused = ", 1)
	if err := os.WriteFile(marker, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetRemoteCacheValidationCache()
	return cacheDir
}

// makeBundledCacheUnhealable makes cacheDir and its parent read-only, so the
// readiness pass can neither rewrite the cache nor replace it.
func makeBundledCacheUnhealable(t *testing.T, cacheDir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only cache this case depends on")
	}
	chmodTree := func(add, remove fs.FileMode) {
		_ = filepath.Walk(cacheDir, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, (info.Mode()|add)&^remove)
			}
			return nil
		})
	}
	parent := filepath.Dir(cacheDir)
	chmodTree(0, 0o222)
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(parent, 0o755)
		chmodTree(0o200, 0)
	})
}

// unlinkedCacheEntry matches the entry in an "unlinkat <cache>/<entry>: <reason>"
// clause: the cache directory is group 1, the entry is everything up to the
// ": " that starts the reason.
var unlinkedCacheEntry = regexp.MustCompile(`(unlinkat \$GC_HOME/cache/repos/[^/]+)/[^:\n]*: `)

// withoutUnlinkedCacheEntry returns msg with the entry os.RemoveAll named
// first dropped from an "unlinkat <cache>/<entry>: <reason>" clause.
func withoutUnlinkedCacheEntry(msg string) string {
	return unlinkedCacheEntry.ReplaceAllString(msg, "${1}/<entry>: ")
}

// TestUnlinkedCacheEntryIsNotPartOfTheErrorIdentity pins the comparison the
// two unhealable-cache parity tests rely on. os.RemoveAll reports the first
// entry it cannot unlink, in the filesystem's readdir order, so two runs over
// identical caches name different files (spawn-storm-detect.sh in one,
// orphan-sweep.sh in the other). The error's identity is everything but that
// entry: the cache directory it names stays, the entry does not.
func TestUnlinkedCacheEntryIsNotPartOfTheErrorIdentity(t *testing.T) {
	const head = `gc bd: loading config: preparing builtin pack caches: removing stale bundled pack cache "$GC_HOME/cache/repos/b03dd4cd": `
	a := head + "unlinkat $GC_HOME/cache/repos/b03dd4cd/internal/bootstrap/packs/core/assets/scripts/spawn-storm-detect.sh: permission denied\n"
	b := head + "unlinkat $GC_HOME/cache/repos/b03dd4cd/internal/bootstrap/packs/core/assets/scripts/orphan-sweep.sh: permission denied\n"
	if a == b {
		t.Fatal("fixture error: the two messages must differ before normalising")
	}
	if got, want := withoutUnlinkedCacheEntry(a), withoutUnlinkedCacheEntry(b); got != want {
		t.Fatalf("messages differing only in the unlinked entry compare unequal:\n %q\n %q", got, want)
	}
	if got := withoutUnlinkedCacheEntry(a); !strings.Contains(got, "unlinkat $GC_HOME/cache/repos/b03dd4cd/") || !strings.HasSuffix(got, ": permission denied\n") {
		t.Fatalf("normalising dropped the cache directory or the error class: %q", got)
	}
	other := strings.Replace(a, "b03dd4cd/internal", "c0ffee00/internal", 1)
	if withoutUnlinkedCacheEntry(a) == withoutUnlinkedCacheEntry(other) {
		t.Fatal("a different cache directory must stay a different error")
	}
	if got := withoutUnlinkedCacheEntry(strings.Replace(a, "permission denied", "no such file or directory", 1)); got == withoutUnlinkedCacheEntry(a) {
		t.Fatal("a different error class must stay a different error")
	}
	if msg := "gc bd: loading config: syntax error\n"; withoutUnlinkedCacheEntry(msg) != msg {
		t.Fatal("a message without an unlinked entry must pass through unchanged")
	}
}

// TestBdPassthroughConfigReportsAReadinessPassThatCannotHeal pins the
// fallback's failure path: when the cache composition rejects cannot be
// healed, gc bd reports the readiness pass's own error, as the full load does,
// not the composition error the pass was run to repair.
func TestBdPassthroughConfigReportsAReadinessPassThatCannotHeal(t *testing.T) {
	run := func(legacy bool) (int, string, bdTrimCounters) {
		cityDir := newBdTrimsTestCity(t)
		makeBundledCacheUnhealable(t, corruptBundledBdCacheMarker(t, cityDir))
		if legacy {
			defer useLegacyBdPassthroughSeams()()
		}
		before := snapBdTrimCounters()
		var stdout, stderr bytes.Buffer
		code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr)
		got := snapBdTrimCounters().since(before)
		out := strings.ReplaceAll(stderr.String(), os.Getenv("GC_HOME"), "$GC_HOME")
		return code, strings.ReplaceAll(out, cityDir, "$CITY"), got
	}
	legacyCode, legacyStderr, _ := run(true)
	code, stderr, got := run(false)

	const want = "gc bd: loading config: preparing builtin pack caches: "
	if code != 1 || !strings.HasPrefix(stderr, want) {
		t.Fatalf("doBd over an unhealable cache = %d, stderr %q; want exit 1 and %q...", code, stderr, want)
	}
	if !strings.Contains(stderr, "$GC_HOME/cache/repos/") {
		t.Fatalf("the error does not name the cache under GC_HOME: %q", stderr)
	}
	if code != legacyCode || withoutUnlinkedCacheEntry(stderr) != withoutUnlinkedCacheEntry(legacyStderr) {
		t.Fatalf("unhealable cache differs from the full load:\n legacy:  %d %q\n trimmed: %d %q", legacyCode, legacyStderr, code, stderr)
	}
	if got.fallbacks != 1 || got.passes != 1 {
		t.Fatalf("fallbacks = %d, passes = %d; want 1 and 1", got.fallbacks, got.passes)
	}
}

// TestGcBdPassthroughTrimsKeepRoutingMore is TestGcBdPassthroughTrimsKeepRouting's
// second table: the remaining resolution env spellings, nested and redirected
// working directories, claim identity, the silent-fallback exit code, and the
// readiness edge cases (healed and unhealable caches, the legacy packs tree).
func TestGcBdPassthroughTrimsKeepRoutingMore(t *testing.T) {
	plain := bdParityOpts{}
	registered := bdParityOpts{registered: true}
	redirectCwd := func(sub, target func(c bdParityCity) string, viaGCDir bool, args ...string) func(*testing.T, bdParityCity) []string {
		return func(t *testing.T, c bdParityCity) []string {
			wt := filepath.Join(c.city, ".gc", "worktrees", sub(c))
			if err := os.MkdirAll(filepath.Join(wt, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, ".beads", "redirect"), []byte(target(c)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if viaGCDir {
				t.Setenv("GC_DIR", wt)
			} else {
				setCwd(t, wt)
			}
			return args
		}
	}
	rigBeads := func(c bdParityCity) string { return filepath.Join(c.rig, ".beads") }
	cases := []struct {
		name  string
		opts  bdParityOpts
		setup func(t *testing.T, c bdParityCity) []string
	}{
		{"GC_CITY env", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY", c.city)
			setCwd(t, c.rig)
			return []string{"list", "--json"}
		}},
		{"GC_CITY_ROOT env", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY_ROOT", c.city)
			return []string{"show", "demo-abc"}
		}},
		{"GC_RIG only", registered, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_RIG", "frontend")
			return []string{"list", "--json"}
		}},
		{"GC_DIR legacy misfire", registered, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			if err := os.MkdirAll(filepath.Join(c.rig, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_DIR", c.rig)
			setCwd(t, c.scratch)
			return []string{"list", "--json"}
		}},
		{"registered nested rig subdir cwd", registered, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			sub := filepath.Join(c.rig, "a", "b")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			setCwd(t, sub)
			return []string{"list", "--json"}
		}},
		{"nested rig subdir cwd write", plain, func(t *testing.T, c bdParityCity) []string {
			sub := filepath.Join(c.rig, "a")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			setCwd(t, sub)
			return []string{"update", "fe-abc", "--status", "in_progress"}
		}},
		{"gc --rig on an unregistered city", plain, func(*testing.T, bdParityCity) []string {
			rigFlag = "frontend"
			return []string{"list", "--json"}
		}},
		{"gc --city from a rig cwd", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			cityFlag = c.city
			setCwd(t, c.rig)
			return []string{"list", "--json"}
		}},
		{"bd --city with GC_RIG", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_RIG", "frontend")
			return []string{"--city", c.city, "show", "demo-abc"}
		}},
		{"-C a non-rig dir", plain, func(_ *testing.T, c bdParityCity) []string {
			return []string{"-C", c.scratch, "list", "--json"}
		}},
		{"explicit --include-infra", plain, func(*testing.T, bdParityCity) []string {
			return []string{"list", "--include-infra", "--json"}
		}},
		{"list --type=wisp", plain, func(*testing.T, bdParityCity) []string {
			return []string{"list", "--type=wisp"}
		}},
		{"close own claim", plain, func(t *testing.T, c bdParityCity) []string {
			useBdParityAssigneeFake(t, c)
			t.Setenv("GC_SESSION_ID", "worker-x")
			t.Setenv("BEADS_ACTOR", "someone-else")
			return []string{"close", "demo-abc"}
		}},
		{"release-if-current match", plain, func(t *testing.T, c bdParityCity) []string {
			useBdParityAssigneeFake(t, c)
			return []string{"release-if-current", "demo-abc", "worker-x"}
		}},
		{"release-if-current mismatch", plain, func(t *testing.T, c bdParityCity) []string {
			useBdParityAssigneeFake(t, c)
			return []string{"release-if-current", "demo-abc", "other"}
		}},
		{"silent fallback exit code", plain, func(t *testing.T, c bdParityCity) []string {
			useBdParityAssigneeFake(t, c)
			t.Setenv("BD_PARITY_SILENT", "1")
			return []string{"update", "demo-abc", "--status", "in_progress"}
		}},
		{"unreadable city.toml", plain, func(t *testing.T, c bdParityCity) []string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-0 file")
			}
			p := filepath.Join(c.city, "city.toml")
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
			return []string{"show", "demo-abc"}
		}},
		{"syntax error with builtin imports", plain, func(t *testing.T, c bdParityCity) []string {
			appendFile(t, filepath.Join(c.city, "city.toml"), "\n[[rigs]\nname=\n")
			return []string{"show", "demo-abc"}
		}},
		{"syntax error with builtin imports and GC_RIG", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_RIG", "frontend")
			appendFile(t, filepath.Join(c.city, "city.toml"), "\n[[rigs]\nname=\n")
			return []string{"show", "demo-abc"}
		}},
		{"fallback heals a rejected marker", plain, func(t *testing.T, c bdParityCity) []string {
			corruptBundledBdCacheMarker(t, c.city)
			return []string{"show", "demo-abc"}
		}},
		{"fallback heals a rejected marker, GC_RIG write", plain, func(t *testing.T, c bdParityCity) []string {
			corruptBundledBdCacheMarker(t, c.city)
			t.Setenv("GC_RIG", "frontend")
			return []string{"update", "fe-abc", "--status", "in_progress"}
		}},
		{"fallback cannot heal", plain, func(t *testing.T, c bdParityCity) []string {
			makeBundledCacheUnhealable(t, corruptBundledBdCacheMarker(t, c.city))
			return []string{"show", "demo-abc"}
		}},
		{"rig worktree redirect cwd", plain, redirectCwd(
			func(bdParityCity) string { return filepath.Join("frontend", "polecat") }, rigBeads, false,
			"update", "fe-abc", "--status", "in_progress")},
		{"city worktree redirect cwd", plain, redirectCwd(
			func(bdParityCity) string { return filepath.Join("demo", "mayor") },
			func(c bdParityCity) string { return filepath.Join(c.city, ".beads") }, false,
			"list", "--json")},
		{"foreign redirect cwd", plain, redirectCwd(
			func(bdParityCity) string { return filepath.Join("x", "y") },
			func(c bdParityCity) string { return filepath.Join(c.scratch, ".beads") }, false,
			"list", "--json")},
		{"rig worktree redirect GC_DIR", plain, redirectCwd(
			func(bdParityCity) string { return filepath.Join("frontend", "p2") }, rigBeads, true,
			"list", "--json")},
		{"legacy system packs tree with GC_RIG", plain, func(t *testing.T, c bdParityCity) []string {
			if err := os.MkdirAll(filepath.Join(c.city, ".gc", "system", "packs", "core"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_RIG", "frontend")
			return []string{"show", "demo-abc"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := runBdParityArm(t, true, tc.opts, tc.setup)
			trimmed := runBdParityArm(t, false, tc.opts, tc.setup)
			legacy.stderr = withoutUnlinkedCacheEntry(legacy.stderr)
			trimmed.stderr = withoutUnlinkedCacheEntry(trimmed.stderr)
			if legacy != trimmed {
				t.Fatalf("routing changed:\nlegacy:  code=%d\n stdout=%q\n stderr=%q\n bd=%s\ntrimmed: code=%d\n stdout=%q\n stderr=%q\n bd=%s",
					legacy.code, legacy.stdout, legacy.stderr, legacy.bd,
					trimmed.code, trimmed.stdout, trimmed.stderr, trimmed.bd)
			}
		})
	}
}

// TestGcBdPassthroughTrimsKeepMigratedCopyRouting covers the migrated-copy
// census (relic refusal, the copy manifest) on the legacy and trimmed loaders.
func TestGcBdPassthroughTrimsKeepMigratedCopyRouting(t *testing.T) {
	type result struct {
		code                  int
		stdout, stderr, calls string
	}
	runIn := func(t *testing.T, cityPath string, args ...string) result {
		log := passthroughFakeBd(t, cityPath)
		var stdout, stderr bytes.Buffer
		code := doBd(args, &stdout, &stderr)
		calls, _ := os.ReadFile(log)
		norm := func(s string) string { return strings.ReplaceAll(s, cityPath, "$CITY") }
		return result{code, norm(stdout.String()), norm(stderr.String()), norm(string(calls))}
	}
	cases := []struct {
		name string
		run  func(t *testing.T) result
	}{
		{"relic refused", func(t *testing.T) result {
			cityPath, _ := foreignProviderCity(t)
			relic, _ := classResidentWorkShapedBead(t, cityPath, "gc-relic1", "carried across by the migration")
			refuseTheseCities(t, theRefusalARefusedCityCarries(), cityPath)
			return runIn(t, cityPath, "show", relic.ID, "--json")
		}},
		{"manifest-delivered collected id refused", func(t *testing.T) result {
			cityPath, _ := refusedSQLiteBindingCity(t, []string{"gc-relic1"}, []string{"gc-relic1", "gc-collected1"})
			return runIn(t, cityPath, "show", "gc-collected1")
		}},
		{"work id passes through", func(t *testing.T) result {
			cityPath, _ := refusedSQLiteBindingCity(t, []string{"gc-relic1"}, []string{"gc-relic1"})
			return runIn(t, cityPath, "show", "gc-work1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var legacy, trimmed result
			t.Run("legacy", func(t *testing.T) {
				defer useLegacyBdPassthroughSeams()()
				legacy = tc.run(t)
			})
			t.Run("trimmed", func(t *testing.T) { trimmed = tc.run(t) })
			// The one intended difference: the legacy full load also printed
			// the missing-builtin-import hint for these import-less fixtures.
			legacy.stderr = dropMissingBuiltinImportHint(legacy.stderr)
			if legacy != trimmed {
				t.Fatalf("migrated-copy routing changed:\nlegacy=%+v\ntrimmed=%+v", legacy, trimmed)
			}
		})
	}
}
