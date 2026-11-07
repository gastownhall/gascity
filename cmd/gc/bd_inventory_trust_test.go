//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// renderBdHelp renders one HelpView the way cobra's default help template
// lays it out, for a fake bd that serves a tree's help.
func renderBdHelp(path string, v bdflags.HelpView) string {
	name := "bd"
	if path != "" {
		fields := strings.Fields(path)
		name = fields[len(fields)-1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Some command.\n\nUsage:\n  bd %s [flags]\n", path)
	if len(v.Aliases) > 0 {
		fmt.Fprintf(&b, "\nAliases:\n  %s, %s\n", name, strings.Join(v.Aliases, ", "))
	}
	if len(v.Subcommands) > 0 {
		b.WriteString("\nAvailable Commands:\n")
		for _, s := range v.Subcommands {
			fmt.Fprintf(&b, "  %-12s does a thing\n", s)
		}
	}
	if v.Flags != "" {
		fmt.Fprintf(&b, "\nFlags:\n%s\n", v.Flags)
	}
	if v.GlobalFlags != "" {
		fmt.Fprintf(&b, "\nGlobal Flags:\n%s\n", v.GlobalFlags)
	}
	b.WriteString("\nUse \"bd [command] --help\" for more information about a command.\n")
	return b.String()
}

// writeFakeTreeBd writes a fake bd at binPath that serves tree's help from
// files in helpDir and logs every call to logPath.
func writeFakeTreeBd(t *testing.T, binPath, helpDir, logPath string, tree bdflags.HelpTree) {
	t.Helper()
	if err := os.MkdirAll(helpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, v := range tree {
		key := "_root"
		if path != "" {
			key = strings.ReplaceAll(path, " ", "_")
		}
		if err := os.WriteFile(filepath.Join(helpDir, key+".txt"), []byte(renderBdHelp(path, v)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
key=""
for a in "$@"; do
  [ "$a" = "--help" ] && continue
  key="${key}${key:+_}$a"
done
[ -z "$key" ] && key=_root
cat %q/"$key".txt
`, logPath, helpDir)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// resolveScopeBdInventory resolves the verdict for target's bd binary.
func resolveScopeBdInventory(cityPath string, target execStoreTarget) (bdTrustVerdict, error) {
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		return bdTrustVerdict{}, err
	}
	return resolveBdInventory(cityPath, bdPath, bdTrustOpts{})
}

// bumpMtime moves a rewritten binary's mtime forward, as any replacement on
// a real filesystem does, without sleeping.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func bdCountLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// withoutFlag returns tree with one flag line removed from one command: a
// binary whose tree differs from the pinned one in one visible flag.
func withoutFlag(tree bdflags.HelpTree, path, long string) bdflags.HelpTree {
	out := bdflags.HelpTree{}
	for p, v := range tree {
		if p == path {
			var kept []string
			for _, l := range strings.Split(v.Flags, "\n") {
				if !strings.Contains(l, long+" ") {
					kept = append(kept, l)
				}
			}
			v.Flags = strings.Join(kept, "\n")
		}
		out[p] = v
	}
	return out
}

// TestBdTrustMatchesTheCommandTreeNotTheVersion is the owner's trust policy
// with maintainer-city's config shape: city.toml's [workspace.env] PATH pins a
// bd that is NOT the pinned build's file — a different path and binary — but
// has the pinned Enterprise tree, and it is accepted. A binary with OSS main's
// kind of difference (one visible flag changed) is refused with the
// difference named. The verdict is cached per binary and recomputed when the
// binary changes.
func TestBdTrustMatchesTheCommandTreeNotTheVersion(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	enterprise := bdflags.InventoryByName("bd-1.3.0-enterprise-77612097cec0")
	logPath := filepath.Join(c.scratch, "walk.log")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help"), logPath, enterprise.HelpTree())
	target := bdCityScopeTarget(c.city, cfg)

	resolvedBin, err := resolveBdBinaryForScope(c.city, target.ScopeRoot)
	if err != nil || resolvedBin != bin {
		t.Fatalf("the [workspace.env] PATH pin resolved %q (%v), want %q", resolvedBin, err, bin)
	}
	v, err := resolveScopeBdInventory(c.city, target)
	if err != nil || v.Inventory != enterprise.Name || v.Source != "walk" {
		t.Fatalf("verdict = %+v, %v; want %s by walk", v, err, enterprise.Name)
	}
	walks := bdCountLines(t, logPath)
	if walks < 100 {
		t.Fatalf("the walk ran bd %d times; the tree looks unwalked", walks)
	}

	// Cached: no bd call at all.
	v, err = resolveScopeBdInventory(c.city, target)
	if err != nil || v.Inventory != enterprise.Name || v.Source != "cache" || bdCountLines(t, logPath) != walks {
		t.Fatalf("second verdict = %+v, %v (bd calls %d → %d); want a cache hit", v, err, walks, bdCountLines(t, logPath))
	}

	// The binary changes (OSS main's kind of drift): the cache key misses and
	// the new tree is refused.
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help2"), logPath, withoutFlag(enterprise.HelpTree(), "update", "--claim"))
	bumpMtime(t, bin)
	v, err = resolveScopeBdInventory(c.city, target)
	if err != nil || v.Inventory != "" || v.Source != "walk" || len(v.Diffs) == 0 {
		t.Fatalf("changed binary verdict = %+v, %v; want a refusal by walk", v, err)
	}
	if !slices.ContainsFunc(v.Diffs, func(d string) bool { return strings.Contains(d, `"update" flags differ`) }) {
		t.Errorf("the refusal does not name the difference: %v", v.Diffs)
	}
	if _, err := bdInventoryForScope(c.city, cfg, target); err == nil || !strings.Contains(err.Error(), "matches none") {
		t.Fatalf("bdInventoryForScope on a refused tree: %v", err)
	}
}

// TestBdTrustVouchAndShaCache: a city's vouch for a binary's sha256 is taken
// without a walk and recorded as an event; the same bytes under another path
// reuse the cached verdict; a vouch for different bytes is ignored.
func TestBdTrustVouchAndShaCache(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	oss := bdflags.InventoryByName("bd-1.3.2")
	logPath := filepath.Join(c.scratch, "walk.log")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "help"), logPath, oss.HelpTree())
	sum, err := fileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	binID, _ := statBdBinary(bin)
	if err := writeBdVouch(c.city, bdVouch{BinaryPath: binID.Path, SHA256: sum, Inventory: oss.Name, InventorySet: bdflags.InventorySetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	v, err := resolveBdInventory(c.city, bin, bdTrustOpts{})
	if err != nil || v.Inventory != oss.Name || v.Source != "vouched" || !v.Vouched || bdCountLines(t, logPath) != 0 {
		t.Fatalf("vouched verdict = %+v, %v (bd calls %d); want vouched, no walk", v, err, bdCountLines(t, logPath))
	}
	if data, _ := os.ReadFile(filepath.Join(c.city, ".gc", "events.jsonl")); !strings.Contains(string(data), `"type":"`+bdVouchUsedEvent+`"`) {
		t.Fatalf("no %s event for the vouch:\n%s", bdVouchUsedEvent, data)
	}
	// A copy at another path: same sha256, cached verdict, no walk.
	copyPath := filepath.Join(c.scratch, "bd-copy")
	data, _ := os.ReadFile(bin)
	if err := os.WriteFile(copyPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	v, err = resolveBdInventory(c.city, copyPath, bdTrustOpts{})
	if err != nil || v.Inventory != oss.Name || v.Source != "cache" || bdCountLines(t, logPath) != 0 {
		t.Fatalf("same-bytes verdict = %+v, %v (bd calls %d); want a sha cache hit", v, err, bdCountLines(t, logPath))
	}
	// A vouch for other bytes is not this binary's.
	if err := writeBdVouch(c.city, bdVouch{BinaryPath: binID.Path, SHA256: strings.Repeat("0", 64), Inventory: oss.Name}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bdTrustCachePath(c.city), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err = resolveBdInventory(c.city, bin, bdTrustOpts{})
	if err != nil || v.Source != "walk" || v.Inventory != oss.Name || v.Vouched {
		t.Fatalf("mismatched vouch verdict = %+v, %v; want a walk", v, err)
	}
}

// pinBdBuildInfoForTest accepts a fake bd (a shell script, which has no Go
// build info) as a Go build of bd's cmd/bd.
func pinBdBuildInfoForTest(t *testing.T) {
	t.Helper()
	prev := bdBuildInfoCheck
	bdBuildInfoCheck = func(string, *bdflags.Inventory) error { return nil }
	t.Cleanup(func() { bdBuildInfoCheck = prev })
}

// writeScriptBd writes a fake bd whose body is script.
func writeScriptBd(t *testing.T, binPath, script string) {
	t.Helper()
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestBdTrustWalkIgnoresTheCallersEnvironment is round-3 must-fix 1: the
// walk runs with a fixed minimal environment, so a caller's CLICOLOR_FORCE
// (which makes bd color its help, so the root parses with no commands)
// cannot reach it.
func TestBdTrustWalkIgnoresTheCallersEnvironment(t *testing.T) {
	city := t.TempDir()
	pinBdBuildInfoForTest(t)
	oss := bdflags.InventoryByName("bd-1.3.2")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help"), filepath.Join(dir, "log"), oss.HelpTree())
	data, _ := os.ReadFile(bin)
	// The colorized root: what CLICOLOR_FORCE does to bd's help.
	script := strings.Replace(string(data), "key=\"\"\n", "if [ -n \"$CLICOLOR_FORCE$BD_NAME$BEADS_DIR\" ]; then printf '\\033[1mUsage\\033[0m\\n'; exit 0; fi\nkey=\"\"\n", 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("BD_NAME", "bd2")
	t.Setenv("BEADS_DIR", dir)
	v, err := resolveBdInventory(city, bin, bdTrustOpts{})
	if err != nil || v.Inventory != oss.Name {
		t.Fatalf("verdict under a hostile caller env = %+v, %v; want %s", v, err, oss.Name)
	}
}

// TestBdTrustNeverCachesAnInconclusiveWalk: a walk whose root lists no
// commands, or that fails, is an error remembered for bdWalkErrorTTL, never a
// cached refusal; a later good walk of the same binary is accepted.
func TestBdTrustNeverCachesAnInconclusiveWalk(t *testing.T) {
	city := t.TempDir()
	pinBdBuildInfoForTest(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	logPath := filepath.Join(dir, "log")
	writeScriptBd(t, bin, fmt.Sprintf("printf '%%s\\n' \"$*\" >> %q\nprintf 'Usage:\\n  bd [flags]\\n'\n", logPath))
	if _, err := resolveBdInventory(city, bin, bdTrustOpts{}); err == nil || !strings.Contains(err.Error(), "listed no commands") {
		t.Fatalf("an empty root: %v; want an inconclusive-walk error", err)
	}
	for _, e := range loadBdTrustCache(city).Entries {
		if e.WalkError == "" {
			t.Fatalf("an inconclusive walk was cached as a verdict: %+v", e)
		}
	}
	walks := bdCountLines(t, logPath)
	if _, err := resolveBdInventory(city, bin, bdTrustOpts{}); err == nil || !strings.Contains(err.Error(), "failed") || bdCountLines(t, logPath) != walks {
		t.Fatalf("within the TTL: %v (walks %d -> %d); want the remembered error, no walk", err, walks, bdCountLines(t, logPath))
	}
	cache := loadBdTrustCache(city)
	for i := range cache.Entries {
		cache.Entries[i].ErrorUntil = time.Now().Add(-time.Second)
	}
	saveBdTrustCache(city, cache)
	if _, err := resolveBdInventory(city, bin, bdTrustOpts{}); err == nil || bdCountLines(t, logPath) == walks {
		t.Fatalf("after the TTL: %v; want a fresh walk", err)
	}

	// A bd that fails outright: an error, not a refusal.
	writeScriptBd(t, bin, "exit 3\n")
	bumpMtime(t, bin)
	if v, err := resolveBdInventory(city, bin, bdTrustOpts{}); err == nil || v.Inventory != "" || len(v.Diffs) != 0 {
		t.Fatalf("a failing bd = %+v, %v; want an error", v, err)
	}

	// The same binary, repaired: accepted.
	oss := bdflags.InventoryByName("bd-1.3.2")
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help"), logPath, oss.HelpTree())
	bumpMtime(t, bin)
	if v, err := resolveBdInventory(city, bin, bdTrustOpts{}); err != nil || v.Inventory != oss.Name {
		t.Fatalf("the repaired bd = %+v, %v", v, err)
	}
}

// TestBdTrustCacheKey: the cache key is path, device, inode, size, mtime and
// ctime, under one embedded inventory set. A replacement that keeps size and
// restores mtime (cp -p, install -p, rsync -t, tar) still changes ctime; a
// rename into place changes the inode. An entry from another inventory set,
// or naming an inventory this gc does not embed, is not used.
func TestBdTrustCacheKey(t *testing.T) {
	city := t.TempDir()
	pinBdBuildInfoForTest(t)
	oss := bdflags.InventoryByName("bd-1.3.2")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	logPath := filepath.Join(dir, "log")
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help1"), logPath, oss.HelpTree())
	before, err := statBdBinary(bin)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(city, bin, bdTrustOpts{}); err != nil || v.Source != "walk" || v.Inventory != oss.Name {
		t.Fatalf("first verdict = %+v, %v", v, err)
	}
	// Same size, mtime restored: only ctime moved.
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help2"), logPath, withoutFlag(oss.HelpTree(), "update", "--claim"))
	mtime := time.Unix(0, before.MTime)
	if err := os.Chtimes(bin, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	after, _ := statBdBinary(bin)
	if after.Size != before.Size || after.MTime != before.MTime || after.Ino != before.Ino {
		t.Fatalf("the fixture did not keep size, mtime and inode: %+v vs %+v", before, after)
	}
	v, err := resolveBdInventory(city, bin, bdTrustOpts{})
	if err != nil || v.Source != "walk" || v.Inventory != "" {
		t.Fatalf("an mtime-preserving replacement = %+v, %v; want a fresh walk that refuses", v, err)
	}

	// Rename into place: a new inode.
	next := filepath.Join(dir, "bd.new")
	writeFakeTreeBd(t, next, filepath.Join(dir, "help1"), logPath, oss.HelpTree())
	if err := os.Rename(next, bin); err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(city, bin, bdTrustOpts{}); err != nil || v.Inventory != oss.Name {
		t.Fatalf("after a rename into place = %+v, %v", v, err)
	}
	walks := bdCountLines(t, logPath)

	// Another inventory set, or a vanished inventory: re-judged, never
	// "refused: <nil>".
	for _, mutate := range []func(*bdTrustCacheEntry){
		func(e *bdTrustCacheEntry) { e.InventorySet = "another-gc" },
		func(e *bdTrustCacheEntry) { e.Inventory = "bd-0.0.0-removed" },
	} {
		cache := loadBdTrustCache(city)
		for i := range cache.Entries {
			mutate(&cache.Entries[i])
		}
		saveBdTrustCache(city, cache)
		v, err := resolveBdInventory(city, bin, bdTrustOpts{})
		if err != nil || v.Source != "walk" || v.Inventory != oss.Name || bdCountLines(t, logPath) == walks {
			t.Fatalf("a stale cache entry = %+v, %v; want a re-walk", v, err)
		}
		walks = bdCountLines(t, logPath)
	}
}

// TestBdTrustBuildInfo: the binary must be a Go build of bd's cmd/bd with
// the matched inventory's cobra and pflag.
func TestBdTrustBuildInfo(t *testing.T) {
	inv := bdflags.InventoryByName("bd-1.3.2")
	good := func() *debug.BuildInfo {
		return &debug.BuildInfo{Path: "github.com/steveyegge/beads/cmd/bd", Deps: []*debug.Module{
			{Path: "github.com/spf13/cobra", Version: inv.Provenance.CobraVersion},
			{Path: "github.com/spf13/pflag", Version: inv.Provenance.PflagVersion},
		}}
	}
	if err := checkBdBuildInfo(good(), inv); err != nil {
		t.Fatalf("bd's own build info: %v", err)
	}
	shim := good()
	shim.Path = "github.com/jdx/mise"
	pflag := good()
	pflag.Deps[1].Version = "v1.0.9"
	replaced := good()
	replaced.Deps[0].Replace = &debug.Module{Path: "example.com/cobra", Version: "v0.0.1"}
	missing := good()
	missing.Deps = missing.Deps[:1]
	for name, bi := range map[string]*debug.BuildInfo{"a shim": shim, "pflag drift": pflag, "a replaced cobra": replaced, "no pflag": missing} {
		if err := checkBdBuildInfo(bi, inv); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	script := filepath.Join(t.TempDir(), "bd")
	writeScriptBd(t, script, "exec /usr/local/bin/bd \"$@\"\n")
	if err := bdBuildInfoCheck(script, inv); err == nil || !strings.Contains(err.Error(), "could not read Go build info") {
		t.Fatalf("a wrapper script: %v", err)
	}
}

// TestBdTrustWalkKillsAHungBd: a bd that hangs, and leaves a child holding
// its output, is killed with its whole process group (the child included),
// and the walk errors promptly.
func TestBdTrustWalkKillsAHungBd(t *testing.T) {
	prev := bdWalkCommandTimeout
	bdWalkCommandTimeout = 500 * time.Millisecond
	t.Cleanup(func() { bdWalkCommandTimeout = prev })
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	pidFile := filepath.Join(dir, "child.pid")
	writeScriptBd(t, bin, fmt.Sprintf("sleep 30 &\necho $! > %q\nsleep 30\n", pidFile))
	start := time.Now()
	_, err := resolveBdInventory(t.TempDir(), bin, bdTrustOpts{})
	if err == nil {
		t.Fatal("a hung bd produced a verdict")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the hung walk took %s", took)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fake bd never started its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// The kill is asynchronous; poll until the child is gone.
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		select {
		case <-deadline:
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hung bd's child %d outlived the walk", pid)
		case <-poll.C:
		}
	}
}

// TestBdTrustCacheLockIsExclusive: the verdict-cache lock serializes gc
// processes, so a cold cache costs one walk.
func TestBdTrustCacheLockIsExclusive(t *testing.T) {
	city := t.TempDir()
	unlock, err := lockBdTrustCache(city)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		second, err := lockBdTrustCache(city)
		if err == nil {
			second()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second holder took the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the lock")
	}
}

// TestBdVouchCommand: `gc storage vouch-bd` walks the scope's bd for real,
// refuses (and writes nothing) on a mismatch, and on a match records the
// binary in .gc/bd-inventory-vouch.json. city.toml is never touched, so a
// gc that predates the vouch loads the city as before. The doctor check has
// no fix: the doctor framework only fixes failing checks, and vouching is an
// explicit operator act.
func TestBdVouchCommand(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	cityTOML, _ := os.ReadFile(filepath.Join(c.city, "city.toml"))
	enterprise := bdflags.InventoryByName("bd-1.3.0-enterprise-77612097cec0")
	bin := filepath.Join(c.bin, "bd")
	logPath := filepath.Join(c.scratch, "walk.log")
	target := bdCityScopeTarget(c.city, cfg)

	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "bad"), logPath, withoutFlag(enterprise.HelpTree(), "close", "--reason"))
	check := newBdInventoryCheck(c.city, cfg)
	if check.CanFix() {
		t.Fatal("the check must not offer a fix: vouching is the explicit gc storage vouch-bd")
	}
	if r := check.Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusError || !strings.Contains(strings.Join(r.Details, "\n"), "matches no pinned") {
		t.Fatalf("un-inventoried bd: %+v", r)
	}
	if _, err := vouchBdForScope(c.city, target); err == nil || !strings.Contains(err.Error(), "refusing to vouch") || !strings.Contains(err.Error(), `"close" flags differ`) {
		t.Fatalf("vouching a mismatched tree: %v", err)
	}
	if _, err := os.Stat(bdVouchPath(c.city)); !os.IsNotExist(err) {
		t.Fatalf("a refused vouch wrote %s (%v)", bdVouchPath(c.city), err)
	}
	// A starting controller records the finding as a gc_bd.uninventoried
	// event.
	ev := bdInventoryStartFinding(check)
	if ev == nil || ev.Type != bdUninventoriedEvent || !strings.Contains(string(ev.Payload), "matches no pinned") {
		t.Fatalf("controller-start finding = %+v", ev)
	}
	if bdInventoryStartFinding(newBdInventoryCheck(c.city, &config.City{})) != nil {
		t.Fatal("a single-store city produced a controller-start finding")
	}

	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "good"), logPath, enterprise.HelpTree())
	bumpMtime(t, bin)
	walks := bdCountLines(t, logPath)
	vouch, err := vouchBdForScope(c.city, target)
	if err != nil {
		t.Fatalf("vouching a matching tree: %v", err)
	}
	if bdCountLines(t, logPath) == walks {
		t.Fatal("the vouch did not walk the binary")
	}
	sum, _ := fileSHA256(bin)
	vouches, err := loadBdVouches(c.city)
	if err != nil || len(vouches) != 1 || vouches[0].SHA256 != sum || vouches[0].Inventory != enterprise.Name || vouches[0].BinaryPath != vouch.BinaryPath || vouches[0].WrittenAt.IsZero() {
		t.Fatalf("vouch file = %+v, %v", vouches, err)
	}
	if after, _ := os.ReadFile(filepath.Join(c.city, "city.toml")); string(after) != string(cityTOML) {
		t.Fatalf("vouching edited city.toml:\n%s", after)
	}
	if _, err := loadBdPassthroughConfig(c.city, os.Stderr); err != nil {
		t.Fatalf("the city does not load with a vouch present: %v", err)
	}
	// The vouch admits the binary without a walk, with an event.
	if err := os.Remove(bdTrustCachePath(c.city)); err != nil {
		t.Fatal(err)
	}
	walks = bdCountLines(t, logPath)
	v, err := resolveBdInventory(c.city, bin, bdTrustOpts{})
	if err != nil || !v.Vouched || v.Inventory != enterprise.Name || bdCountLines(t, logPath) != walks {
		t.Fatalf("vouched verdict = %+v, %v (walks %d -> %d)", v, err, walks, bdCountLines(t, logPath))
	}
	if data, _ := os.ReadFile(filepath.Join(c.city, ".gc", "events.jsonl")); !strings.Contains(string(data), `"type":"`+bdVouchUsedEvent+`"`) {
		t.Fatalf("no %s event:\n%s", bdVouchUsedEvent, data)
	}
	if r := newBdInventoryCheck(c.city, cfg).Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusOK {
		t.Fatalf("after the vouch: %+v", r)
	}
	if ev := bdInventoryStartFinding(newBdInventoryCheck(c.city, cfg)); ev != nil {
		t.Fatalf("a matching bd produced a controller-start finding: %+v", ev)
	}
	if !check.WarmupEligible() {
		t.Fatal("the check must run in gc start's warm-up, which is the start warning")
	}
	if _, err := vouchBdTarget(c.city, cfg, "no-such-rig"); err == nil {
		t.Fatal("vouching an unknown rig was accepted")
	}
}

// TestBdInventoryDoctorRewalksVouchedBinaries: the vouch is a convenience,
// not a security boundary, so the doctor walks a vouched binary and reports
// a mismatch; an unreadable vouch file is a warning and is ignored.
func TestBdInventoryDoctorRewalksVouchedBinaries(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	enterprise := bdflags.InventoryByName("bd-1.3.0-enterprise-77612097cec0")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "bad"), filepath.Join(c.scratch, "log"), withoutFlag(enterprise.HelpTree(), "close", "--reason"))
	sum, _ := fileSHA256(bin)
	// A vouch someone wrote by hand for a binary that does not match.
	binID, _ := statBdBinary(bin)
	if err := writeBdVouch(c.city, bdVouch{BinaryPath: binID.Path, SHA256: sum, Inventory: enterprise.Name, InventorySet: bdflags.InventorySetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	r := newBdInventoryCheck(c.city, cfg).Run(&doctor.CheckContext{CityPath: c.city})
	if r.Status != doctor.StatusError || !strings.Contains(strings.Join(r.Details, "\n"), "was vouched for as "+enterprise.Name+", but its command tree matches no pinned inventory") {
		t.Fatalf("a vouched binary whose tree does not match: %+v", r)
	}

	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "good"), filepath.Join(c.scratch, "log"), enterprise.HelpTree())
	bumpMtime(t, bin)
	if err := os.WriteFile(bdVouchPath(c.city), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(c.city, bin, bdTrustOpts{}); err != nil || v.Inventory != enterprise.Name || v.Vouched {
		t.Fatalf("with a malformed vouch file = %+v, %v; want a walked match", v, err)
	}
	r = newBdInventoryCheck(c.city, cfg).Run(&doctor.CheckContext{CityPath: c.city})
	if r.Status != doctor.StatusWarning || !strings.Contains(strings.Join(r.Details, "\n"), "is ignored") {
		t.Fatalf("a malformed vouch file: %+v", r)
	}
	// gc bd works there: a starting controller records no
	// gc_bd.uninventoried, the event that says it does not.
	if ev := bdInventoryStartFinding(newBdInventoryCheck(c.city, cfg)); ev != nil {
		t.Fatalf("a malformed vouch file with every bd matching produced a controller-start finding: %+v", ev)
	}
}

// TestBdInventoryDoctorWalksEachBinaryOnce: gc doctor checks what the cache or
// a vouch claims with a real walk, but a claim that was itself a walk (a cache
// miss) is not walked again.
func TestBdInventoryDoctorWalksEachBinaryOnce(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"walk", "cache", "vouched"} {
		forced := 0
		check := newBdInventoryCheck(c.city, cfg)
		check.resolve = func(_, _ string, opts bdTrustOpts) (bdTrustVerdict, error) {
			if opts.forceWalk {
				forced++
				return bdTrustVerdict{Inventory: "bd-1.3.2", Source: "walk"}, nil
			}
			return bdTrustVerdict{Inventory: "bd-1.3.2", Source: source}, nil
		}
		if r := check.Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusOK {
			t.Fatalf("claim from %s: %+v", source, r)
		}
		want := 1
		if source == "walk" {
			want = 0
		}
		if forced != want {
			t.Errorf("claim from %s: the doctor forced %d walk(s), want %d", source, forced, want)
		}
	}
}

// TestBdTrustWalkScratchAndReadOnlyCity: bd's HOME, cwd and TMPDIR are a
// scratch directory under the city's .gc/cache/tmp, never the caller's
// TMPDIR; on a city whose .gc cannot be written the binary is walked
// without the cache instead of refused.
func TestBdTrustWalkScratchAndReadOnlyCity(t *testing.T) {
	pinBdBuildInfoForTest(t)
	oss := bdflags.InventoryByName("bd-1.3.2")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	envLog := filepath.Join(dir, "env")
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help"), filepath.Join(dir, "log"), oss.HelpTree())
	data, _ := os.ReadFile(bin)
	script := strings.Replace(string(data), "key=\"\"\n", fmt.Sprintf("printf '%%s %%s\\n' \"$TMPDIR\" \"$HOME\" >> %q\nkey=\"\"\n", envLog), 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(dir, "callers-tmp"))
	city := t.TempDir()
	if v, err := resolveBdInventory(city, bin, bdTrustOpts{}); err != nil || v.Inventory != oss.Name {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
	logged, _ := os.ReadFile(envLog)
	want := filepath.Join(city, ".gc", "cache", "tmp") + string(filepath.Separator)
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[0], want) || f[0] != f[1] {
			t.Fatalf("bd ran with TMPDIR/HOME %q; want one scratch dir under %s", line, want)
		}
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	t.Setenv("TMPDIR", dir)
	if v, err := resolveBdInventory(ro, bin, bdTrustOpts{}); err != nil || v.Inventory != oss.Name || v.Source != "walk" {
		t.Fatalf("a read-only city = %+v, %v; want an uncached walked match", v, err)
	}
}

// TestBdTrustWalkStopsAtTheFirstMismatchedLevel: a root that matches no
// inventory ends the walk there, so no subcommand (including one no
// inventory knows, which bd may not even parse --help for) is run.
func TestBdTrustWalkStopsAtTheFirstMismatchedLevel(t *testing.T) {
	pinBdBuildInfoForTest(t)
	tree := bdflags.HelpTree{}
	for p, v := range bdflags.InventoryByName("bd-1.3.2").HelpTree() {
		tree[p] = v
	}
	root := tree[""]
	root.Subcommands = append(append([]string(nil), root.Subcommands...), "zz-unknown")
	tree[""] = root
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	logPath := filepath.Join(dir, "log")
	writeFakeTreeBd(t, bin, filepath.Join(dir, "help"), logPath, tree)
	v, err := resolveBdInventory(t.TempDir(), bin, bdTrustOpts{})
	if err != nil || v.Inventory != "" || len(v.Diffs) == 0 {
		t.Fatalf("verdict = %+v, %v; want a refusal", v, err)
	}
	if n := bdCountLines(t, logPath); n != 1 {
		t.Fatalf("the walk ran bd %d times; want only the root", n)
	}
	if !bdflags.SkipUnpinnedOrUnparsed("zz-unknown") || bdflags.SkipUnpinnedOrUnparsed("show") || !bdflags.SkipUnpinnedOrUnparsed("mail") {
		t.Fatal("the walk's skip must refuse unknown and unparsed commands and allow pinned ones")
	}
	// The refusal carries the difference itself, not just a headline.
	if !strings.Contains(strings.Join(v.Diffs, "\n"), "zz-unknown") {
		t.Fatalf("diffs do not show the difference: %v", v.Diffs)
	}
}

// TestGcBdRefusalRecordsAnEvent is review must-fix 3: a split-city refusal
// writes a gc_bd.refused event, so a script's `|| true` cannot hide it.
func TestGcBdRefusalRecordsAnEvent(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	t.Setenv("GC_AGENT", "deacon")
	var stdout, stderr strings.Builder
	if code := doBd([]string{"serve"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(c.city, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("no events log: %v", err)
	}
	var found bool
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"type":"gc_bd.refused"`) && strings.Contains(line, `"caller":"deacon"`) && strings.Contains(line, `"verb":"serve"`) && strings.Contains(line, `"kind":"allowlist"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no gc_bd.refused event for the refusal:\n%s", data)
	}
}
