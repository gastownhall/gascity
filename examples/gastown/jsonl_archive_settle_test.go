//go:build unix

package gastown_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file pin the JSONL archive's oversized-delta settle: the
// shared window helper, the detector, jsonl-archive-settle.sh and its lock.
// The export-side tests (the writer's window rule, the detector hook, the
// alarms and the lock) are in jsonl_archive_writer_test.go.

const (
	archiveLockName   = "jsonl-archive.lock"
	zeroObjectID      = "0000000000000000000000000000000000000000"
	settleScriptName  = "jsonl-archive-settle.sh"
	settleCommonName  = "jsonl_archive_common.sh"
	settleHubPath     = "scope/hub.jsonl"
	settleFamilyPath  = "scope/small.jsonl"
	settleChainPath   = "scope/chain.jsonl"
	settleChainLength = 60
)

var settleTelemetry = regexp.MustCompile(`jsonl-archive-settle: mode=(\S+) flagged=(\d+) groups=(\d+) bytes_before=(\d+) bytes_after=(\d+) wall=(\d+)s status=(\S+)`)

// archiveObject is one line of git's object listing for the archive.
type archiveObject struct {
	typ  string
	size int
	disk int
	base string
}

// archiveObjects lists every object of the repository with the fields the
// settle's detector reads.
func archiveObjects(t *testing.T, repo string) map[string]archiveObject {
	t.Helper()
	out := runGitOut(t, repo, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize) %(objectsize:disk) %(deltabase)")
	objects := map[string]archiveObject{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("unexpected object listing line %q", line)
		}
		size, _ := strconv.Atoi(fields[2])
		disk, _ := strconv.Atoi(fields[3])
		base := fields[4]
		if base == zeroObjectID {
			base = ""
		}
		objects[fields[0]] = archiveObject{typ: fields[1], size: size, disk: disk, base: base}
	}
	return objects
}

// deltaDepth is the number of delta links between id and a whole object.
func deltaDepth(objects map[string]archiveObject, id string) int {
	depth := 0
	for objects[id].base != "" {
		id = objects[id].base
		depth++
	}
	return depth
}

// randomHexLines returns n lines of 96 random hex digits. Random lines keep
// the fixture's blobs incompressible, so their on-disk sizes track their
// logical sizes the way real bead exports do.
func randomHexLines(rng *rand.Rand, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("%016x%016x%016x%016x%016x%016x", rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return lines
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func gitWithStdin(t *testing.T, repo, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = hermeticGitEnv(t, map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
	})
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v\nstderr: %s", strings.Join(args, " "), repo, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// settleObjects names the objects buildOversizedDeltaPacks put in the archive.
type settleObjects struct {
	hub     string
	family  []string // the oversized deltas, each against hub
	chain   []string // a healthy delta chain deeper than 50
	bigPack string   // the pack holding hub, the family and the chain
}

// buildOversizedDeltaPacks adds to the archive repo, which must already have a
// commit, the shape of the incident in git objects: three versions of one
// file that a pack stores as deltas against an unrelated 242,500 B file (about
// 10 KB each on disk where a same-path delta takes a few hundred bytes), and in
// the same pack a healthy 60-version delta chain. A second, small pack holds
// later commits that no settle should touch. The packs are written by hand
// with git pack-objects, whose delta choices the fixture asserts below.
func buildOversizedDeltaPacks(t *testing.T, repo string) settleObjects {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	commit := func(msg string) {
		runGit(t, repo, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "add", "-A")
		runGit(t, repo, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-q", "-m", msg)
	}
	write := func(path string, lines []string) {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", full, err)
		}
		if err := os.WriteFile(full, joinLines(lines), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", full, err)
		}
	}
	blob := func(rev, path string) string { return runGitOut(t, repo, "rev-parse", rev+":"+path) }

	hub := randomHexLines(rng, 2500)
	picked := rng.Perm(len(hub))[:299]
	sort.Ints(picked)
	family := make([]string, 0, 460)
	for _, i := range picked {
		family = append(family, hub[i])
	}
	for _, fresh := range randomHexLines(rng, 161) {
		at := rng.Intn(len(family) + 1)
		family = append(family[:at], append([]string{fresh}, family[at:]...)...)
	}
	write(settleHubPath, hub)
	var familyIDs []string
	for version := 0; version < 3; version++ {
		if version > 0 {
			for _, fresh := range randomHexLines(rng, 3) {
				family[rng.Intn(len(family))] = fresh
			}
		}
		write(settleFamilyPath, family)
		commit(fmt.Sprintf("family %d", version))
		familyIDs = append(familyIDs, blob("HEAD", settleFamilyPath))
	}
	hubID := blob("HEAD", settleHubPath)

	chain := randomHexLines(rng, 400)
	var chainIDs []string
	for version := 0; version < settleChainLength; version++ {
		for _, fresh := range randomHexLines(rng, 3) {
			chain[rng.Intn(len(chain))] = fresh
		}
		at := rng.Intn(len(chain))
		chain = append(chain[:at], chain[at+1:]...)
		write(settleChainPath, chain)
		commit(fmt.Sprintf("chain %d", version))
		chainIDs = append(chainIDs, blob("HEAD", settleChainPath))
	}

	packBase := filepath.Join(repo, ".git", "objects", "pack", "pack")
	for _, id := range familyIDs {
		gitWithStdin(t, repo, hubID+"\n"+id+"\n", "pack-objects", "--window=1", "--depth=50", "--no-reuse-delta", "-q", packBase)
	}
	gitWithStdin(t, repo, strings.Join(chainIDs, "\n")+"\n", "pack-objects", "--window=60", "--depth=250", "--no-reuse-delta", "-q", packBase)
	runGit(t, repo, "prune-packed")
	runGit(t, repo, "repack", "-d", "-q")
	// One pack that holds the oversized deltas, their base and the healthy
	// chain, deltas reused as they are.
	runGit(t, repo, "repack", "-a", "-d", "-q", "--window=0", "--depth=250")
	packs := packsHolding(t, repo, hubID)
	if len(packs) != 1 {
		t.Fatalf("fixture: hub is in %d packs, want 1", len(packs))
	}
	objs := settleObjects{hub: hubID, family: familyIDs, chain: chainIDs, bigPack: packs[0]}

	// Later commits in a pack of their own.
	for i := 0; i < 3; i++ {
		write("scope/other.jsonl", randomHexLines(rng, 5+i))
		commit(fmt.Sprintf("other %d", i))
	}
	runGit(t, repo, "repack", "-d", "-q")
	assertSettleFixtureShape(t, repo, objs)
	return objs
}

// assertSettleFixtureShape fails when git's delta choices no longer give the
// fixture the shape the tests are written against.
func assertSettleFixtureShape(t *testing.T, repo string, objs settleObjects) {
	t.Helper()
	all := archiveObjects(t, repo)
	if all[objs.hub].base != "" || all[objs.hub].size != 242500 {
		t.Fatalf("fixture: hub = %+v, want a whole 242500 B blob", all[objs.hub])
	}
	for _, id := range objs.family {
		o := all[id]
		if o.base != objs.hub || o.disk < 8*1024 || o.disk > 20*1024 {
			t.Fatalf("fixture: family blob %s = %+v, want a delta of 8-20 KB against hub", id, o)
		}
	}
	deepest := 0
	for _, id := range objs.chain {
		if depth := deltaDepth(all, id); depth > deepest {
			deepest = depth
		}
	}
	if deepest <= 50 {
		t.Fatalf("fixture: deepest healthy chain is %d deltas, want more than 50", deepest)
	}
	if got := readArchiveObjectStats(t, repo); got.packs != 2 || got.looseObjects != 0 {
		t.Fatalf("fixture: archive stats = %+v, want 2 packs and no loose objects", got)
	}
}

// packsHolding is the names of the packs whose index lists id.
func packsHolding(t *testing.T, repo, id string) []string {
	t.Helper()
	idxs, err := filepath.Glob(filepath.Join(repo, ".git", "objects", "pack", "pack-*.idx"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	var packs []string
	for _, idx := range idxs {
		data, err := os.ReadFile(idx)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", idx, err)
		}
		out := gitWithStdin(t, repo, string(data), "show-index")
		if strings.Contains(out, " "+id+" ") {
			packs = append(packs, strings.TrimSuffix(filepath.Base(idx), ".idx"))
		}
	}
	return packs
}

// settleFixture is an archive with oversized deltas and the env to run the
// settle over it with thresholds lowered to the fixture's scale.
type settleFixture struct {
	repo    string
	binDir  string
	env     map[string]string
	objects settleObjects
}

func newSettleFixture(t *testing.T) settleFixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "archive")
	initSeedArchive(t, repo, 3)
	objs := buildOversizedDeltaPacks(t, repo)
	binDir := t.TempDir()
	env := map[string]string{
		"GC_PACK_STATE_DIR":                  filepath.Join(t.TempDir(), "state"),
		"GC_JSONL_SETTLE_MIN_DISK_KB":        "8",
		"GC_JSONL_SETTLE_MIN_RATIO_PCT":      "5",
		"GC_JSONL_GC_MIN_FREE_KB":            "1",
		"GC_JSONL_SETTLE_MEMINFO":            filepath.Join(t.TempDir(), "no-meminfo"),
		"GIT_CONFIG_GLOBAL":                  filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM":                "1",
		"PATH":                               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GC_JSONL_ARCHIVE_REPO":              "",
		"GC_JSONL_SETTLE_FAULT":              "",
		"GC_JSONL_BIG_FILE_THRESHOLD":        "",
		"GC_JSONL_GC_WINDOW_MEMORY":          "",
		"GC_JSONL_SETTLE_CHECK_INTERVAL":     "",
		"GC_JSONL_SETTLE_CHECK_TIMEOUT_SECS": "",
	}
	for key, value := range env {
		if value == "" {
			delete(env, key)
		}
	}
	return settleFixture{repo: repo, binDir: binDir, env: env, objects: objs}
}

// settle runs jsonl-archive-settle.sh and returns its combined output and
// exit code (-1 when it did not start).
func (f settleFixture) settle(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return runSettle(t, f.env, args...)
}

func runSettle(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := scriptCmd("", coreScriptPath(settleScriptName), env)
	cmd.Args = append(cmd.Args, args...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		t.Fatalf("starting %s: %v", settleScriptName, err)
		return "", -1
	}
}

func (f settleFixture) apply(t *testing.T) (string, int) {
	t.Helper()
	return f.settle(t, "--archive", f.repo, "--apply")
}

func (f settleFixture) dryRun(t *testing.T) (string, int) {
	t.Helper()
	return f.settle(t, "--archive", f.repo)
}

func settleLine(t *testing.T, out string) []string {
	t.Helper()
	match := settleTelemetry.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no jsonl-archive-settle telemetry line in:\n%s", out)
	}
	return match
}

// packFileDigests maps each file under objects/pack to its content hash.
func packFileDigests(t *testing.T, repo string) map[string][32]byte {
	t.Helper()
	digests := map[string][32]byte{}
	entries, err := os.ReadDir(filepath.Join(repo, ".git", "objects", "pack"))
	if err != nil {
		t.Fatalf("ReadDir(pack): %v", err)
	}
	for _, entry := range entries {
		path := filepath.Join(repo, ".git", "objects", "pack", entry.Name())
		digests[entry.Name()] = fileSHA256(t, path)
	}
	return digests
}

// gitTreeDigest fingerprints every file under .git, so a read-only command
// can be shown to have written nothing. The lock file may be created.
func gitTreeDigest(t *testing.T, repo string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	root := filepath.Join(repo, ".git")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Base(path) == archiveLockName {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		digests[rel] = fmt.Sprintf("%x %d", fileSHA256(t, path), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(.git): %v", err)
	}
	return digests
}

func requireSameDigests[V comparable](t *testing.T, what string, before, after map[string]V) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s changed: %d entries before, %d after", what, len(before), len(after))
	}
	for name, digest := range before {
		if got, ok := after[name]; !ok || got != digest {
			t.Fatalf("%s changed at %s", what, name)
		}
	}
}

func objectIDs(objects map[string]archiveObject) []string {
	ids := make([]string, 0, len(objects))
	for id := range objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func requireFsckClean(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "fsck", "--full", "--strict")
}

func requireAllObjectsPresent(t *testing.T, repo string, want map[string]archiveObject) {
	t.Helper()
	have := archiveObjects(t, repo)
	for _, id := range objectIDs(want) {
		if _, ok := have[id]; !ok {
			t.Fatalf("object %s (%s) is gone from the archive", id, want[id].typ)
		}
	}
}

// holdArchiveLock takes the archive's lock the way a running settle does and
// returns the function that releases it.
func holdArchiveLock(t *testing.T, repo string) func() {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(repo, ".git", archiveLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(lock): %v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("Flock(lock): %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			_ = file.Close()
		}
	}
	t.Cleanup(release)
	return release
}

func requireEmptyLockFile(t *testing.T, repo string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(repo, ".git", archiveLockName))
	if err != nil {
		t.Fatalf("the lock file must outlive the run (it is never removed): %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("the lock file must stay zero bytes, got %d", info.Size())
	}
}

// writeLoggingShim installs name, a shim that logs its arguments to logFile
// and runs the command that follows its own leading skip arguments.
func writeLoggingShim(t *testing.T, binDir, name string, skip int, logFile string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, name), "#!/bin/sh\nprintf '"+name+" %s\\n' \"$*\" >> '"+logFile+"'\nshift "+strconv.Itoa(skip)+"\nexec \"$@\"\n")
}

// --- the shared helper ------------------------------------------------------

func runCommonFunction(t *testing.T, call string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", `. "$1" && shift && "$@"`, "common", coreScriptPath(settleCommonName))
	cmd.Args = append(cmd.Args, strings.Fields(call)...)
	cmd.Env = mergeTestEnv(map[string]string{})
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)), 0
	case errors.As(err, &exit):
		return strings.TrimSpace(string(out)), exit.ExitCode()
	default:
		t.Fatalf("running %q: %v", call, err)
		return "", -1
	}
}

// Rule W: window memory is max(256 MiB, 6 x the largest blob), in whole MiB.
func TestArchiveWindowMemoryRule(t *testing.T) {
	for _, tc := range []struct {
		largest int
		want    string
	}{
		{0, "256m"},
		{5, "256m"},
		{44739242, "256m"},   // 6 x S is just under 256 MiB
		{44739243, "257m"},   // ... and just over
		{52428800, "300m"},   // exactly 6 x 50 MiB
		{52428801, "301m"},   // one byte more rounds up
		{312682710, "1790m"}, // the live archive's largest blob
		{262500000, "1503m"}, // 6 x S = 1,575,000,000 B
	} {
		out, code := runCommonFunction(t, "archive_window_memory "+strconv.Itoa(tc.largest))
		if code != 0 || out != tc.want {
			t.Errorf("archive_window_memory %d = %q (exit %d), want %q", tc.largest, out, code, tc.want)
		}
	}
}

func TestArchiveWindowBytesFollowsGitsUnitRule(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"64m", "67108864"},
		{"64M", "67108864"},
		{"204800k", "209715200"},
		{"1g", "1073741824"},
		{"1G", "1073741824"},
		{"209715199", "209715199"},
	} {
		out, code := runCommonFunction(t, "archive_window_bytes "+tc.value)
		if code != 0 || out != tc.want {
			t.Errorf("archive_window_bytes %s = %q (exit %d), want %q", tc.value, out, code, tc.want)
		}
	}
}

// The guard is the settle's refusal: a value under 4 x the largest blob makes
// the delta window collapse, so nothing may be written with it.
func TestArchiveWindowGuardRefusesValueUnderFourTimesLargestBlob(t *testing.T) {
	const largest = "52428800" // 4 x S is exactly 200 MiB
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"199m", false},
		{"200m", true},
		{"204800k", true},
		{"209715199", false},
		{"209715200", true},
		{"1m", false},
	} {
		out, code := runCommonFunction(t, "archive_window_guard "+tc.value+" "+largest)
		if tc.ok && code != 0 {
			t.Errorf("archive_window_guard %s %s refused (exit %d): %s", tc.value, largest, code, out)
		}
		if !tc.ok && code == 0 {
			t.Errorf("archive_window_guard %s %s accepted a value under 4 x S", tc.value, largest)
		}
	}
	out, code := runCommonFunction(t, "archive_window_guard 1m 312682710")
	if code == 0 || !strings.Contains(out, "1m") {
		t.Errorf("archive_window_guard 1m 312682710 = exit %d, %q; want a refusal that names the value", code, out)
	}
}

// --- the fixture -----------------------------------------------------------

func TestSettleFixtureHoldsTheIncidentShape(t *testing.T) {
	f := newSettleFixture(t)
	all := archiveObjects(t, f.repo)
	if got := len(f.objects.family); got != 3 || len(f.objects.chain) != settleChainLength {
		t.Fatalf("fixture has %d family blobs and %d chain blobs", got, len(f.objects.chain))
	}
	if _, ok := all[f.objects.hub]; !ok {
		t.Fatalf("hub %s is missing", f.objects.hub)
	}
}

// --- dry run ---------------------------------------------------------------

func TestSettleDryRunReportsTheWasteAndWritesNothing(t *testing.T) {
	f := newSettleFixture(t)
	before := gitTreeDigest(t, f.repo)
	all := archiveObjects(t, f.repo)

	out, code := f.dryRun(t)
	if code != 0 {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	requireSameDigests(t, "the .git tree", before, gitTreeDigest(t, f.repo))

	line := settleLine(t, out)
	if line[1] != "dry-run" || line[2] != "3" || line[3] != "1" || line[7] != "ok" {
		t.Fatalf("telemetry = %v, want mode=dry-run flagged=3 groups=1 status=ok:\n%s", line[1:], out)
	}
	if line[4] != line[5] {
		t.Fatalf("a dry run changes nothing: bytes_before=%s bytes_after=%s", line[4], line[5])
	}
	var familyDisk int
	for _, id := range f.objects.family {
		familyDisk += all[id].disk
	}
	if want := fmt.Sprintf("group %s objects=3 bytes=%d", settleFamilyPath, familyDisk); !strings.Contains(out, want) {
		t.Fatalf("expected the flagged group %q in:\n%s", want, out)
	}
	if strings.Contains(out, "group "+settleHubPath) || strings.Contains(out, "group "+settleChainPath) {
		t.Fatalf("only the oversized deltas are groups, not their base or healthy chains:\n%s", out)
	}
	largestKB := (242500 + 1023) / 1024
	for _, want := range []string{
		"closure_objects=4",
		"packs_to_retire=1",
		"largest_blob_kb=" + strconv.Itoa(largestKB),
		"window_memory=256m",
		"expected_peak_rss_kb=" + strconv.Itoa((9*242500+1023)/1024),
		"mem_available_kb=unknown",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run report is missing %q:\n%s", want, out)
		}
	}
	for _, label := range []string{"estimated_reclaim_kb=", "transient_need_kb="} {
		if !regexp.MustCompile(label + `\d+`).MatchString(out) {
			t.Fatalf("dry-run report is missing a number for %s:\n%s", label, out)
		}
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("no warning is due when MemAvailable is unknown:\n%s", out)
	}
}

func TestSettleDryRunWarnsWhenPeakMemoryExceedsMemAvailable(t *testing.T) {
	f := newSettleFixture(t)
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:       16000000 kB\nMemFree:          100000 kB\nMemAvailable:       1000 kB\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(meminfo): %v", err)
	}
	f.env["GC_JSONL_SETTLE_MEMINFO"] = meminfo
	out, code := f.dryRun(t)
	if code != 0 {
		t.Fatalf("a memory warning must not refuse the dry run (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "mem_available_kb=1000") || !strings.Contains(out, "WARNING") {
		t.Fatalf("expected mem_available_kb=1000 and a WARNING:\n%s", out)
	}

	if err := os.WriteFile(meminfo, []byte("MemAvailable:  999999999 kB\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(meminfo): %v", err)
	}
	out, _ = f.dryRun(t)
	if strings.Contains(out, "WARNING") {
		t.Fatalf("ample MemAvailable must not warn:\n%s", out)
	}
}

// The settle derives its window from the closure's largest blob and takes no
// pin: a pin that is too small is the failure it exists to repair.
func TestSettleIgnoresTheWindowMemoryPin(t *testing.T) {
	f := newSettleFixture(t)
	f.env["GC_JSONL_GC_WINDOW_MEMORY"] = "1m"
	out, code := f.dryRun(t)
	if code != 0 || !strings.Contains(out, "window_memory=256m") || strings.Contains(out, "window_memory=1m") {
		t.Fatalf("the dry run must print the derived 256m, not the 1m pin (exit %d):\n%s", code, out)
	}
}

func TestSettleDetectorThresholds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		minDiskKB   string
		minRatioPct string
		flagged     string
	}{
		{"fixture scale", "8", "5", "3"},
		{"floor of 1 KiB", "1", "1", "3"},
		{"disk floor above every delta", "64", "5", "0"},
		{"ratio above every delta", "8", "60", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSettleFixture(t)
			f.env["GC_JSONL_SETTLE_MIN_DISK_KB"] = tc.minDiskKB
			f.env["GC_JSONL_SETTLE_MIN_RATIO_PCT"] = tc.minRatioPct
			out, code := f.dryRun(t)
			if code != 0 {
				t.Fatalf("dry run exited %d:\n%s", code, out)
			}
			if line := settleLine(t, out); line[2] != tc.flagged {
				t.Fatalf("flagged = %s, want %s:\n%s", line[2], tc.flagged, out)
			}
		})
	}
}

func TestSettleDryRunWorksWhileTheLockIsHeld(t *testing.T) {
	f := newSettleFixture(t)
	holdArchiveLock(t, f.repo)
	out, code := f.dryRun(t)
	if code != 0 || settleLine(t, out)[7] != "ok" {
		t.Fatalf("a dry run is read-only and needs no lock (exit %d):\n%s", code, out)
	}
}

func TestSettleArchiveSelection(t *testing.T) {
	f := newSettleFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")

	// --archive wins over the environment.
	f.env["GC_JSONL_ARCHIVE_REPO"] = elsewhere
	if out, code := f.dryRun(t); code != 0 {
		t.Fatalf("--archive must win over GC_JSONL_ARCHIVE_REPO (exit %d):\n%s", code, out)
	}
	// Without --archive the environment names the repository.
	f.env["GC_JSONL_ARCHIVE_REPO"] = f.repo
	if out, code := f.settle(t); code != 0 || settleLine(t, out)[2] != "3" {
		t.Fatalf("GC_JSONL_ARCHIVE_REPO must select the archive (exit %d):\n%s", code, out)
	}
	// ... and the pack state directory holds the default.
	delete(f.env, "GC_JSONL_ARCHIVE_REPO")
	stateDir := f.env["GC_PACK_STATE_DIR"]
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(f.repo, filepath.Join(stateDir, "jsonl-archive")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if out, code := f.settle(t); code != 0 || settleLine(t, out)[2] != "3" {
		t.Fatalf("the default archive is $GC_PACK_STATE_DIR/jsonl-archive (exit %d):\n%s", code, out)
	}
}

func TestSettleRejectsBadInvocationsWithExitTwo(t *testing.T) {
	f := newSettleFixture(t)
	notGit := t.TempDir()
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"not a git repository", nil, []string{"--archive", notGit}},
		{"missing path", nil, []string{"--archive", filepath.Join(notGit, "absent")}},
		{"unknown flag", nil, []string{"--archive", f.repo, "--force"}},
		{"bad min disk", map[string]string{"GC_JSONL_SETTLE_MIN_DISK_KB": "8k"}, []string{"--archive", f.repo}},
		{"bad ratio", map[string]string{"GC_JSONL_SETTLE_MIN_RATIO_PCT": "5%"}, []string{"--archive", f.repo}},
		{"bad free-space floor", map[string]string{"GC_JSONL_GC_MIN_FREE_KB": "500MiB"}, []string{"--archive", f.repo}},
		{"bad threshold", map[string]string{"GC_JSONL_BIG_FILE_THRESHOLD": "0"}, []string{"--archive", f.repo}},
		{"unknown fault", map[string]string{"GC_JSONL_SETTLE_FAULT": "never"}, []string{"--archive", f.repo, "--apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := packFileDigests(t, f.repo)
			env := map[string]string{}
			for k, v := range f.env {
				env[k] = v
			}
			for k, v := range tc.env {
				env[k] = v
			}
			out, code := runSettle(t, env, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2:\n%s", code, out)
			}
			requireSameDigests(t, "the packs", before, packFileDigests(t, f.repo))
		})
	}
}

// --- apply -----------------------------------------------------------------

func TestSettleApplyRemovesTheWasteAndLosesNothing(t *testing.T) {
	f := newSettleFixture(t)
	before := archiveObjects(t, f.repo)
	head := runGitOut(t, f.repo, "rev-parse", "HEAD")
	refs := runGitOut(t, f.repo, "for-each-ref")
	config := runGitOut(t, f.repo, "config", "--local", "--list")
	otherPack := packsHolding(t, f.repo, runGitOut(t, f.repo, "rev-parse", "HEAD:scope/other.jsonl"))
	if len(otherPack) != 1 {
		t.Fatalf("fixture: later commits are in %d packs, want 1", len(otherPack))
	}
	otherDigests := map[string][32]byte{}
	for _, ext := range []string{".pack", ".idx"} {
		path := filepath.Join(f.repo, ".git", "objects", "pack", otherPack[0]+ext)
		otherDigests[ext] = fileSHA256(t, path)
	}

	out, code := f.apply(t)
	if code != 0 {
		t.Fatalf("--apply exited %d:\n%s", code, out)
	}
	line := settleLine(t, out)
	if line[1] != "apply" || line[2] != "3" || line[3] != "1" || line[7] != "ok" {
		t.Fatalf("telemetry = %v, want mode=apply flagged=3 groups=1 status=ok:\n%s", line[1:], out)
	}

	requireFsckClean(t, f.repo)
	requireAllObjectsPresent(t, f.repo, before)
	if got := runGitOut(t, f.repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved from %s to %s", head, got)
	}
	if got := runGitOut(t, f.repo, "for-each-ref"); got != refs {
		t.Fatalf("refs changed:\nbefore:\n%s\nafter:\n%s", refs, got)
	}
	if got := runGitOut(t, f.repo, "config", "--local", "--list"); got != config {
		t.Fatalf("the settle must not touch the archive config:\nbefore:\n%s\nafter:\n%s", config, got)
	}
	for ext, digest := range otherDigests {
		if got := fileSHA256(t, filepath.Join(f.repo, ".git", "objects", "pack", otherPack[0]+ext)); got != digest {
			t.Fatalf("the pack of later commits (%s) was rewritten; the settle touches only packs holding the waste", ext)
		}
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "jsonl-archive-settle.stage")); !os.IsNotExist(err) {
		t.Fatalf("the staging directory must be gone after a successful apply (stat: %v)", err)
	}

	after := archiveObjects(t, f.repo)
	for _, id := range f.objects.family {
		if after[id].base != "" && after[id].disk >= 8*1024 {
			t.Fatalf("family blob %s is still an oversized delta: %+v", id, after[id])
		}
	}
	// Whatever the settle did not have to touch keeps its delta base and its
	// size to within 4 bytes, the chain deeper than 50 included.
	inClosure := map[string]bool{f.objects.hub: true}
	for _, id := range f.objects.family {
		inClosure[id] = true
	}
	for id, was := range before {
		if inClosure[id] {
			continue
		}
		if now := after[id]; now.base != was.base || now.disk < was.disk-4 || now.disk > was.disk+4 {
			t.Fatalf("object %s (%s) was %+v before the settle and %+v after", id, was.typ, was, now)
		}
	}
	deepest := 0
	for _, id := range f.objects.chain {
		if depth := deltaDepth(after, id); depth > deepest {
			deepest = depth
		}
	}
	if deepest <= 50 {
		t.Fatalf("the healthy chain was reshaped: deepest is now %d", deepest)
	}

	// The detector now finds nothing, and a second apply is a no-op.
	out, code = f.dryRun(t)
	if code != 0 || settleLine(t, out)[2] != "0" {
		t.Fatalf("detector after the apply: exit %d:\n%s", code, out)
	}
	again, code := f.apply(t)
	if code != 0 || settleLine(t, again)[2] != "0" || settleLine(t, again)[7] != "ok" {
		t.Fatalf("a second apply must find flagged=0 (exit %d):\n%s", code, again)
	}
	if size := readArchiveObjectStats(t, f.repo); size.looseObjects != 0 {
		t.Fatalf("the settle must not leave loose objects: %+v", size)
	}
}

// The settle only ever packs. It never runs gc or a full repack, never
// commits, pushes, fetches or moves a ref, and never writes config; it runs
// its packing at the lowest priority.
func TestSettleApplyCommandsAreExactlyThePackingTheDesignNames(t *testing.T) {
	f := newSettleFixture(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	writeLoggingShim(t, f.binDir, "nice", 2, argvLog)
	writeLoggingShim(t, f.binDir, "ionice", 3, argvLog)
	f.env["GC_JSONL_GC_WINDOW_MEMORY"] = "1m"
	configBefore, err := os.ReadFile(filepath.Join(f.repo, ".git", "config"))
	if err != nil {
		t.Fatalf("ReadFile(config): %v", err)
	}

	out, code := f.apply(t)
	if code != 0 {
		t.Fatalf("--apply exited %d:\n%s", code, out)
	}
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("ReadFile(argv log): %v", err)
	}
	log := string(data)
	forbidden := regexp.MustCompile(`(?m)^ARGV (?:.* )?(gc|repack|commit|push|fetch|pull|update-ref|symbolic-ref|config|reflog|prune|remote|checkout|reset)(?: |$)`)
	if m := forbidden.FindStringSubmatch(log); m != nil {
		t.Fatalf("the settle ran git %s:\n%s", m[1], log)
	}
	var n1, r []string
	for _, line := range strings.Split(log, "\n") {
		if !regexp.MustCompile(`^ARGV (?:.* )?pack-objects `).MatchString(line) {
			continue
		}
		if strings.Contains(line, "--no-reuse-delta") {
			n1 = append(n1, line)
		} else {
			r = append(r, line)
		}
	}
	if len(n1) != 1 || len(r) != 1 {
		t.Fatalf("want one re-chaining pack-objects and one remainder pack-objects, got %d and %d:\n%s", len(n1), len(r), log)
	}
	for _, want := range []string{"-c core.bigFileThreshold=4g", "-c pack.windowMemory=256m", "-c pack.threads=1", "--window=3", "--depth=250", "--delta-base-offset"} {
		if !strings.Contains(n1[0], want) {
			t.Errorf("the re-chaining pack-objects lacks %q: %s", want, n1[0])
		}
	}
	if strings.Contains(n1[0], "pack.windowMemory=1m") {
		t.Errorf("the settle must ignore GC_JSONL_GC_WINDOW_MEMORY: %s", n1[0])
	}
	for _, want := range []string{"--window=0", "--depth=4095", "--delta-base-offset"} {
		if !strings.Contains(r[0], want) {
			t.Errorf("the remainder pack-objects lacks %q: %s", want, r[0])
		}
	}
	for _, shim := range []string{"nice -n 19 git", "ionice -t -c 3"} {
		if !strings.Contains(log, shim) {
			t.Errorf("the packing must run under %q:\n%s", shim, log)
		}
	}
	configAfter, err := os.ReadFile(filepath.Join(f.repo, ".git", "config"))
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatalf(".git/config changed (read err %v)", err)
	}
}

func TestSettleApplyRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *settleFixture)
		want  string
	}{
		{"a pack to retire has a .keep", func(t *testing.T, f *settleFixture) {
			path := filepath.Join(f.repo, ".git", "objects", "pack", f.objects.bigPack+".keep")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(.keep): %v", err)
			}
		}, ".keep"},
		{"free space is short", func(_ *testing.T, f *settleFixture) {
			f.env["GC_JSONL_GC_MIN_FREE_KB"] = "1000000000000000"
		}, "free"},
		{"the lock is held", func(t *testing.T, f *settleFixture) {
			holdArchiveLock(t, f.repo)
		}, "lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSettleFixture(t)
			tc.setup(t, &f)
			before := gitTreeDigest(t, f.repo)

			out, code := f.apply(t)
			if code != 1 {
				t.Fatalf("exit = %d, want 1:\n%s", code, out)
			}
			if line := settleLine(t, out); line[1] != "apply" || line[7] != "refused" {
				t.Fatalf("telemetry = %v, want mode=apply status=refused:\n%s", line[1:], out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("the refusal must say why (%q):\n%s", tc.want, out)
			}
			requireSameDigests(t, "the .git tree", before, gitTreeDigest(t, f.repo))
			if _, err := os.Stat(filepath.Join(f.repo, ".git", "jsonl-archive-settle.stage")); !os.IsNotExist(err) {
				t.Fatalf("a refused apply must leave no staging directory (stat: %v)", err)
			}
		})
	}
}

// Without flock there is no lock, so there is no apply; the dry run still works.
func TestSettleApplyRefusesWithoutFlock(t *testing.T) {
	f := newSettleFixture(t)
	bin := t.TempDir()
	for _, tool := range []string{"bash", "env", "git", "awk", "sed", "sort", "uniq", "df", "wc", "ls", "mktemp", "mv", "rm", "mkdir", "cat", "tr", "head", "tail", "date", "basename", "dirname", "comm", "grep", "cut", "stat", "find", "nice", "ionice", "timeout", "id", "chmod", "touch", "sleep", "ln", "cp", "rmdir", "readlink", "expr", "du", "dd", "tee", "xargs", "join", "paste", "uname", "true", "false", "test"} {
		if _, err := exec.LookPath(tool); err == nil {
			linkTestPathTool(t, bin, tool)
		}
	}
	if _, err := os.Stat(filepath.Join(bin, "flock")); err == nil {
		t.Fatal("test setup linked flock")
	}
	f.env["PATH"] = bin
	before := gitTreeDigest(t, f.repo)

	out, code := f.apply(t)
	if code != 1 || !strings.Contains(out, "flock") || settleLine(t, out)[7] != "refused" {
		t.Fatalf("--apply without flock must refuse and name flock (exit %d):\n%s", code, out)
	}
	requireSameDigests(t, "the .git tree", before, gitTreeDigest(t, f.repo))

	out, code = f.dryRun(t)
	if code != 0 || settleLine(t, out)[2] != "3" {
		t.Fatalf("the dry run needs no flock (exit %d):\n%s", code, out)
	}
}

// An interruption at any boundary leaves an archive that passes fsck with all
// its objects, and a rerun finishes the job.
func TestSettleApplyFaultInjection(t *testing.T) {
	for _, point := range []string{"after-stage", "after-verify", "after-promote", "mid-retire"} {
		t.Run(point, func(t *testing.T) {
			f := newSettleFixture(t)
			before := archiveObjects(t, f.repo)
			f.env["GC_JSONL_SETTLE_FAULT"] = point

			out, code := f.apply(t)
			if code != 1 {
				t.Fatalf("a fault at %s must exit 1, got %d:\n%s", point, code, out)
			}
			requireFsckClean(t, f.repo)
			requireAllObjectsPresent(t, f.repo, before)
			requireEmptyLockFile(t, f.repo)

			delete(f.env, "GC_JSONL_SETTLE_FAULT")
			out, code = f.apply(t)
			if code != 0 || settleLine(t, out)[7] != "ok" {
				t.Fatalf("the rerun after a fault at %s must complete (exit %d):\n%s", point, code, out)
			}
			requireFsckClean(t, f.repo)
			requireAllObjectsPresent(t, f.repo, before)
			out, code = f.dryRun(t)
			if code != 0 || settleLine(t, out)[2] != "0" {
				t.Fatalf("flagged must be 0 after the rerun (exit %d):\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(f.repo, ".git", "jsonl-archive-settle.stage")); !os.IsNotExist(err) {
				t.Fatalf("the rerun must clear a stale staging directory (stat: %v)", err)
			}
		})
	}
}

// Anything that fails before the promote deletes the staging directory and
// changes nothing.
func TestSettleApplyFailureBeforePromoteChangesNothing(t *testing.T) {
	f := newSettleFixture(t)
	writeGitSubcommandFailureStub(t, f.binDir, lookPathGit(t), "verify-pack")
	before := gitTreeDigest(t, f.repo)

	out, code := f.apply(t)
	if code != 1 || settleLine(t, out)[7] != "failed" {
		t.Fatalf("a failed verify must fail the apply (exit %d):\n%s", code, out)
	}
	requireSameDigests(t, "the .git tree", before, gitTreeDigest(t, f.repo))
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "jsonl-archive-settle.stage")); !os.IsNotExist(err) {
		t.Fatalf("a failure before the promote must delete the staging directory (stat: %v)", err)
	}
}

// An export that runs while a settle holds the lock still exports, commits and
// pushes; it only leaves the archive's packing alone.
func TestSettleHoldsTheLockAgainstAConcurrentExport(t *testing.T) {
	f := newSettleFixture(t)
	slowLog := filepath.Join(t.TempDir(), "slow.log")
	started := filepath.Join(t.TempDir(), "started")
	realGit := lookPathGit(t)
	// The first pack-objects waits, so the apply holds the lock long enough
	// for the export to run beside it.
	writeExecutable(t, filepath.Join(f.binDir, "git"), `#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = pack-objects ] && [ ! -e '`+started+`' ]; then
        : > '`+started+`'
        printf 'waiting\n' >> '`+slowLog+`'
        sleep 6
    fi
done
exec '`+realGit+`' "$@"
`)
	cmd := scriptCmd("", coreScriptPath(settleScriptName), f.env)
	cmd.Args = append(cmd.Args, "--archive", f.repo, "--apply")
	var settleOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &settleOut, &settleOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the settle: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the settle never reached its packing:\n%s", settleOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	exp := newArchiveMaintenanceFixture(t, 3, 5)
	// Run the export over the settle's archive.
	exp.env["GC_JSONL_ARCHIVE_REPO"] = f.repo
	exp.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, f.repo, "init", "--bare", "-q", remote)
	runGit(t, f.repo, "remote", "add", "origin", remote)
	runGit(t, f.repo, "push", "-q", "-u", "origin", "main")
	out, err := exp.run(t)
	if err != nil {
		t.Fatalf("the export beside a running settle must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "archive maintenance skipped: settle in progress") {
		t.Fatalf("expected the skip line:\n%s", out)
	}
	if subject := runGitOut(t, f.repo, "log", "-1", "--format=%s"); !strings.HasPrefix(subject, "backup ") {
		t.Fatalf("the export must still commit its snapshot, HEAD subject = %q", subject)
	}
	if got, want := runGitOut(t, f.repo, "--git-dir", remote, "rev-parse", "refs/heads/main"), runGitOut(t, f.repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("the export must still push its snapshot: origin %s, HEAD %s", got, want)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the settle failed: %v\n%s", err, settleOut.String())
	}
	if line := settleLine(t, settleOut.String()); line[7] != "ok" {
		t.Fatalf("the settle beside an export must end ok:\n%s", settleOut.String())
	}
	requireFsckClean(t, f.repo)
	requireEmptyLockFile(t, f.repo)
}
