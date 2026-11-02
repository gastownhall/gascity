//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

// trustFixture is a fake bd serving tree, its call log, and a city dir.
type trustFixture struct {
	city, dir, bin, log string
}

func newTrustFixture(t *testing.T, tree bdflags.HelpTree) trustFixture {
	t.Helper()
	pinBdBuildInfoForTest(t)
	f := trustFixture{city: t.TempDir(), dir: t.TempDir()}
	f.bin = filepath.Join(f.dir, "bd")
	f.log = filepath.Join(f.dir, "log")
	writeFakeTreeBd(t, f.bin, filepath.Join(f.dir, "help"), f.log, tree)
	return f
}

func countVouchUsed(t *testing.T, city string) int {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(city, ".gc", "events.jsonl"))
	return strings.Count(string(data), `"type":"`+bdVouchUsedEvent+`"`)
}

// TestBdTrustReadOnlyCityStillReadsTheCache: when the cache lock cannot be
// opened (a read-only .gc), the cache is still read; only the write is
// skipped.
func TestBdTrustReadOnlyCityStillReadsTheCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	f := newTrustFixture(t, bdflags.InventoryByName("bd-1.3.2").HelpTree())
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || v.Source != "walk" {
		t.Fatalf("first verdict = %+v, %v", v, err)
	}
	walks := bdCountLines(t, f.log)
	cacheDir := filepath.Dir(bdTrustCachePath(f.city))
	for _, p := range []string{bdTrustCachePath(f.city) + ".lock", bdTrustCachePath(f.city)} {
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(cacheDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o755) })
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || v.Source != "cache" || bdCountLines(t, f.log) != walks {
		t.Fatalf("read-only city = %+v, %v (walks %d -> %d); want a cache hit", v, err, walks, bdCountLines(t, f.log))
	}
}

// TestBdVouchIsTiedToTheInventorySet: a vouch written under another set of
// pinned inventories (an older or newer gc) is ignored and the binary is
// walked; vouch_used fires once per binary per process, cache hits
// included.
func TestBdVouchIsTiedToTheInventorySet(t *testing.T) {
	oss := bdflags.InventoryByName("bd-1.3.2")
	f := newTrustFixture(t, oss.HelpTree())
	id, _ := statBdBinary(f.bin)
	sum, _ := fileSHA256(f.bin)
	if err := writeBdVouch(f.city, bdVouch{BinaryPath: id.Path, SHA256: sum, Inventory: oss.Name, InventorySet: "an-older-gc"}); err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || v.Vouched || v.Source != "walk" {
		t.Fatalf("a vouch from another inventory set = %+v, %v; want a walk", v, err)
	}

	if err := writeBdVouch(f.city, bdVouch{BinaryPath: id.Path, SHA256: sum, Inventory: oss.Name, InventorySet: bdflags.InventorySetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bdTrustCachePath(f.city)); err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || !v.Vouched || v.Source != "vouched" || countVouchUsed(t, f.city) != 1 {
		t.Fatalf("vouched = %+v, %v (events %d)", v, err, countVouchUsed(t, f.city))
	}
	// A vouched cache hit in the same process: rate-limited.
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || !v.Vouched || v.Source != "cache" || countVouchUsed(t, f.city) != 1 {
		t.Fatalf("vouched cache hit = %+v, %v (events %d)", v, err, countVouchUsed(t, f.city))
	}
	// A new process (the once-per-process memory cleared): a vouched cache
	// hit records the event.
	bdVouchUsedOnce.Delete(id.Path + "\x00" + sum)
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || v.Source != "cache" || countVouchUsed(t, f.city) != 2 {
		t.Fatalf("vouched cache hit in a new process = %+v, %v (events %d)", v, err, countVouchUsed(t, f.city))
	}
}

// TestBdVouchRunsTheBuildInfoCheck: a vouch does not skip the build-info
// check.
func TestBdVouchRunsTheBuildInfoCheck(t *testing.T) {
	oss := bdflags.InventoryByName("bd-1.3.2")
	f := newTrustFixture(t, oss.HelpTree())
	id, _ := statBdBinary(f.bin)
	sum, _ := fileSHA256(f.bin)
	if err := writeBdVouch(f.city, bdVouch{BinaryPath: id.Path, SHA256: sum, Inventory: oss.Name, InventorySet: bdflags.InventorySetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	bdBuildInfoCheck = func(string, *bdflags.Inventory) error { return errors.New("built with pflag v9") }
	v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{})
	if err != nil || v.Inventory != "" || v.Vouched || !strings.Contains(strings.Join(v.Diffs, ";"), "pflag v9") {
		t.Fatalf("a vouched binary failing the build-info check = %+v, %v; want refused", v, err)
	}
}

// TestBdVouchBdNeverReusesACachedOrForgedVerdict: gc storage vouch-bd walks
// for real, so a forged cache row (or vouch) claiming a match for a binary
// that does not match is refused; gc doctor catches the forged row too.
func TestBdVouchBdNeverReusesACachedOrForgedVerdict(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	oss := bdflags.InventoryByName("bd-1.3.2")
	bin := filepath.Join(c.bin, "bd")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "bad"), filepath.Join(c.scratch, "log"), withoutFlag(oss.HelpTree(), "update", "--claim"))
	id, _ := statBdBinary(bin)
	sum, _ := fileSHA256(bin)
	forged := bdTrustCache{Entries: []bdTrustCacheEntry{{bdBinaryID: id, InventorySet: bdflags.InventorySetFingerprint(), CheckedAt: time.Now(), bdTrustVerdict: bdTrustVerdict{Inventory: oss.Name, SHA256: sum}}}}
	saveBdTrustCache(c.city, forged)
	if v, err := resolveBdInventory(c.city, bin, bdTrustOpts{}); err != nil || v.Inventory != oss.Name {
		t.Fatalf("the fixture's forged row was not served: %+v, %v", v, err)
	}
	if _, err := vouchBdForScope(c.city, bdCityScopeTarget(c.city, cfg)); err == nil || !strings.Contains(err.Error(), "refusing to vouch") {
		t.Fatalf("vouch-bd over a forged cache row: %v", err)
	}
	saveBdTrustCache(c.city, forged)
	r := newBdInventoryCheck(c.city, cfg).Run(&doctor.CheckContext{CityPath: c.city})
	if r.Status != doctor.StatusError || !strings.Contains(strings.Join(r.Details, "\n"), "was cached as "+oss.Name+", but its command tree matches no pinned inventory") {
		t.Fatalf("gc doctor over a forged cache row: %+v", r)
	}
}

// TestBdInventoryWarmupNeverWalks: gc start's warm-up reports from the cache
// only; a binary with no verdict is pending (OK, so nothing is mailed).
func TestBdInventoryWarmupNeverWalks(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	oss := bdflags.InventoryByName("bd-1.3.2")
	bin := filepath.Join(c.bin, "bd")
	logPath := filepath.Join(c.scratch, "log")
	writeFakeTreeBd(t, bin, filepath.Join(c.scratch, "bad"), logPath, withoutFlag(oss.HelpTree(), "update", "--claim"))
	warm := newWarmupBdInventoryCheck(c.city, cfg)
	r := warm.Run(&doctor.CheckContext{CityPath: c.city})
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "pending") || bdCountLines(t, logPath) != 0 {
		t.Fatalf("warm-up with no verdict = %+v (bd calls %d); want pending and no walk", r, bdCountLines(t, logPath))
	}
	if _, err := resolveBdInventory(c.city, bin, bdTrustOpts{}); err != nil {
		t.Fatal(err)
	}
	walks := bdCountLines(t, logPath)
	if r := warm.Run(&doctor.CheckContext{CityPath: c.city}); r.Status != doctor.StatusError || bdCountLines(t, logPath) != walks {
		t.Fatalf("warm-up with a cached refusal = %+v (walks %d -> %d)", r, walks, bdCountLines(t, logPath))
	}
	names := doctorCheckNames(buildDoctorChecks(c.city, cfg, nil, buildDoctorChecksOpts{Warmup: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipStorePreflight: true}))
	if doctorCheckIndex(names, "bd-command-inventory") < 0 {
		t.Fatal("the warm-up build lost the bd-command-inventory check")
	}
}

// chanRecorder hands recorded events to a channel.
type chanRecorder struct {
	mu sync.Mutex
	ch chan events.Event
}

func (r *chanRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ch <- e
}

// TestBdInventoryControllerStartDoesNotBlock: the controller-start check
// returns at once, however slow the walk, and records its event later.
func TestBdInventoryControllerStartDoesNotBlock(t *testing.T) {
	c := newBdParityCity(t, bdParityOpts{storage: bdSQLRefusalSplitStorage})
	pinBdBuildInfoForTest(t)
	cfg, err := loadBdPassthroughConfig(c.city, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(c.bin, "bd")
	writeScriptBd(t, bin, "sleep 1\nprintf 'Usage:\\n  bd [flags]\\n\\nAvailable Commands:\\n  zz-unknown  x\\n'\n")
	rec := &chanRecorder{ch: make(chan events.Event, 4)}
	start := time.Now()
	warnBdInventoryAtControllerStart(c.city, cfg, rec)
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("the controller-start check blocked for %s", took)
	}
	select {
	case ev := <-rec.ch:
		if ev.Type != bdUninventoriedEvent {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the background check never recorded its finding")
	}
	warnBdInventoryAtControllerStart(c.city, &config.City{}, rec) // single-store: nothing
}

// TestBdTrustSetupErrorsAreNotRemembered: a failure of gc's own setup (no
// scratch directory) is reported but never cached as a walk error, so the
// next call walks once the setup works.
func TestBdTrustSetupErrorsAreNotRemembered(t *testing.T) {
	f := newTrustFixture(t, bdflags.InventoryByName("bd-1.3.2").HelpTree())
	tmp := filepath.Join(f.city, ".gc", "cache", "tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(f.dir, "no-such-dir"))
	if _, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err == nil || !strings.Contains(err.Error(), "preparing to check") {
		t.Fatalf("with no scratch dir: %v", err)
	}
	for _, e := range loadBdTrustCache(f.city).Entries {
		t.Fatalf("a setup error was cached: %+v", e)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{}); err != nil || v.Source != "walk" {
		t.Fatalf("after the setup recovered = %+v, %v", v, err)
	}
}

// TestBdTrustWalkStopsAtDepthOne: a tree that matches at the root but not at
// depth 1 is refused without running any depth-2 command.
func TestBdTrustWalkStopsAtDepthOne(t *testing.T) {
	f := newTrustFixture(t, withoutFlag(bdflags.InventoryByName("bd-1.3.2").HelpTree(), "update", "--claim"))
	v, err := resolveBdInventory(f.city, f.bin, bdTrustOpts{})
	if err != nil || v.Inventory != "" {
		t.Fatalf("verdict = %+v, %v; want a refusal", v, err)
	}
	data, _ := os.ReadFile(f.log)
	depth1 := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch n := len(strings.Fields(line)); {
		case n == 2:
			depth1++
		case n > 2:
			t.Fatalf("the walk ran a depth-2 command after depth 1 mismatched: %q", line)
		}
	}
	if depth1 == 0 {
		t.Fatal("the walk never reached depth 1")
	}
	if !strings.Contains(strings.Join(v.Diffs, "\n"), `"update" flags differ`) {
		t.Fatalf("diffs = %v", v.Diffs)
	}
}
