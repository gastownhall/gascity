//go:build unix

package gastown_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The export-side tests of the JSONL archive's oversized-delta work: the
// writer's window-memory rule and its telemetry, the settle check, the alarms
// and the lock. The settle script itself is tested in
// jsonl_archive_settle_test.go.

const (
	scanFormat          = "%(objecttype) %(objectsize)"
	pinWarning          = "WARNING=pin_below_4x_largest_blob"
	sizeAlertSubject    = "JSONL archive size over limit"
	settleAlertSubject  = "JSONL archive oversized deltas"
	archiveMaintMarker  = "jsonl-export: archive maintenance:"
	settleCheckMarker   = "jsonl-export: archive settle check:"
	scanBlobBytesLive   = 312682710
	scanBlobBytesFifty  = 52428800
	secondsPerDayAndSix = 90000
)

var settleCheckTelemetry = regexp.MustCompile(`jsonl-export: archive settle check: flagged=(\d+) flagged_kb=(\d+) archive_kb=(\d+) status=(\S+)`)

// writeGitScanStub installs a git that logs each call and answers the writer's
// object-size scan with canned output and exit status; every other call runs
// the real git.
func writeGitScanStub(t *testing.T, binDir, argvLog, scanOutput string, scanExit int) {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "scan.out")
	if err := os.WriteFile(outFile, []byte(scanOutput), 0o644); err != nil {
		t.Fatalf("WriteFile(scan output): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "git"), "#!/bin/sh\n"+
		"printf 'ARGV %s\\n' \"$*\" >> '"+argvLog+"'\n"+
		"case \"$*\" in\n"+
		"*'"+scanFormat+"'*)\n"+
		"    cat '"+outFile+"'\n"+
		"    exit "+strconv.Itoa(scanExit)+"\n"+
		"    ;;\n"+
		"esac\n"+
		"exec '"+lookPathGit(t)+"' \"$@\"\n")
}

// scanReport is what the scan prints for an archive whose largest blob is
// blobBytes: a larger commit and tree, which the rule must skip, and smaller
// blobs.
func scanReport(blobBytes int) string {
	return fmt.Sprintf("blob 12\ncommit 999999999999\nblob %d\ntree 5\nblob 4096\n", blobBytes)
}

// writerRun is one export over a seeded archive whose maintenance is due.
type writerRun struct {
	out      string
	err      error
	argv     string
	scanRuns int
}

func (r writerRun) repackLine() string {
	for _, line := range strings.Split(r.argv, "\n") {
		if strings.Contains(line, " repack ") {
			return line
		}
	}
	return ""
}

func (r writerRun) maintenanceLine() string {
	for _, line := range strings.Split(r.out, "\n") {
		if strings.Contains(line, archiveMaintMarker) {
			return line
		}
	}
	return ""
}

// runWriter runs the export with maintenance due and git's object-size scan
// answering scan (the real git when scan is nil) with exit status scanExit.
func runWriter(t *testing.T, scan *string, scanExit int, env map[string]string) writerRun {
	t.Helper()
	f := newArchiveMaintenanceFixture(t, 3, 5)
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	if scan == nil {
		writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	} else {
		writeGitScanStub(t, f.binDir, argvLog, *scan, scanExit)
	}
	f.limitPackingToSnapshot(t)
	for key, value := range env {
		f.env[key] = value
	}
	out, err := f.run(t)
	data, readErr := os.ReadFile(argvLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadFile(git argv log): %v", readErr)
	}
	return writerRun{out: out, err: err, argv: string(data), scanRuns: strings.Count(string(data), scanFormat)}
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// Rule W, end to end: the repack runs under max(256 MiB, 6 x the largest blob),
// and the maintenance line says what the rule saw and chose.
func TestJsonlExportWindowMemoryFollowsTheLargestBlob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		blob  int
		want  string
		fixed bool
	}{
		{"the live archive's 312,682,710 B blob", scanBlobBytesLive, "1790m", false},
		{"exactly 6 x 50 MiB", scanBlobBytesFifty, "300m", false},
		{"one byte more rounds up", scanBlobBytesFifty + 1, "301m", false},
		{"a small blob keeps the 256m floor", 4096, "256m", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := scanReport(tc.blob)
			run := runWriter(t, &scan, 0, nil)
			if run.err != nil {
				t.Fatalf("export failed: %v\n%s", run.err, run.out)
			}
			wantArgv := "ARGV -C " + "" // the archive path is checked by suffix below
			_ = wantArgv
			line := run.repackLine()
			for _, want := range []string{"-c core.bigFileThreshold=4g", "-c pack.windowMemory=" + tc.want + " ", "-c pack.threads=1", "repack -d -l -q --geometric=2"} {
				if !strings.Contains(line, want) {
					t.Fatalf("the repack %q lacks %q (git calls:\n%s)", line, want, run.argv)
				}
			}
			if run.scanRuns != 1 {
				t.Fatalf("the scan ran %d times, want once:\n%s", run.scanRuns, run.argv)
			}
			want := fmt.Sprintf("largest_blob_kb=%d window_memory=%s", ceilDiv(tc.blob, 1024), tc.want)
			if line := run.maintenanceLine(); !strings.HasSuffix(line, want) {
				t.Fatalf("the maintenance line must end with %q:\n%s", want, line)
			}
		})
	}
}

// The tiny fixture's real scan gives the 256m floor, and the new fields come
// after every existing field with the existing ones where they were.
func TestJsonlExportMaintenanceTelemetryKeepsFieldOrderAndAppends(t *testing.T) {
	run := runWriter(t, nil, 0, nil)
	if run.err != nil {
		t.Fatalf("export failed: %v\n%s", run.err, run.out)
	}
	line := run.maintenanceLine()
	pattern := regexp.MustCompile(`^jsonl-export: archive maintenance: mode=geometric wall=\d+s loose_before=\d+ loose_after=\d+ packs_before=\d+ packs_after=\d+ pack_kb_after=\d+ largest_blob_kb=\d+ window_memory=256m$`)
	if !pattern.MatchString(line) {
		t.Fatalf("maintenance line = %q, want the old fields in their old order and then largest_blob_kb and window_memory=256m", line)
	}
	if !strings.Contains(run.repackLine(), "-c pack.windowMemory=256m ") {
		t.Fatalf("the unset default is the auto rule, 256m for a small archive:\n%s", run.argv)
	}
	if run.scanRuns != 1 {
		t.Fatalf("the scan ran %d times, want once", run.scanRuns)
	}
}

func TestJsonlExportBigFileThresholdKnob(t *testing.T) {
	run := runWriter(t, nil, 0, map[string]string{"GC_JSONL_BIG_FILE_THRESHOLD": "1g"})
	if run.err != nil {
		t.Fatalf("export failed: %v\n%s", run.err, run.out)
	}
	if !strings.Contains(run.repackLine(), "-c core.bigFileThreshold=1g -c pack.windowMemory=") {
		t.Fatalf("the threshold knob is used verbatim ahead of the window:\n%s", run.argv)
	}
}

// The repack that keeps the largest pack out of the rewrite keeps its flag.
func TestJsonlExportKeepPackRepackCarriesTheNewArguments(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	f.seedLargePackAndBacklog(t)
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"
	// Free space that holds the archive less its largest pack, but not all of it.
	freeKB := dfFreeKB(t, f.archiveRepo)
	stats := readArchiveObjectStats(t, f.archiveRepo)
	f.env["GC_JSONL_GC_MIN_FREE_KB"] = strconv.Itoa(freeKB - (stats.packKB+stats.looseKB)/2)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(argvLog)
	run := writerRun{argv: string(data)}
	line := run.repackLine()
	if !strings.Contains(line, "-c core.bigFileThreshold=4g -c pack.windowMemory=256m -c pack.threads=1 repack -d -l -q --geometric=2 --keep-pack=") {
		t.Fatalf("the keep-pack repack = %q, want the new arguments before --keep-pack=:\n%s", line, run.argv)
	}
}

func dfFreeKB(t *testing.T, dir string) int {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("Statfs(%s): %v", dir, err)
	}
	return int(st.Bavail * uint64(st.Bsize) / 1024)
}

// A pin is used verbatim, the scan still runs once, and a pin under 4 x the
// largest blob is called out on the maintenance line.
func TestJsonlExportWindowMemoryPin(t *testing.T) {
	scan := scanReport(scanBlobBytesFifty)
	run := runWriter(t, &scan, 0, map[string]string{"GC_JSONL_GC_WINDOW_MEMORY": "64m"})
	if run.err != nil {
		t.Fatalf("export failed: %v\n%s", run.err, run.out)
	}
	if !strings.Contains(run.repackLine(), "-c pack.windowMemory=64m ") {
		t.Fatalf("a pin goes into the argv verbatim:\n%s", run.argv)
	}
	if run.scanRuns != 1 {
		t.Fatalf("the scan runs once even with a pin, got %d:\n%s", run.scanRuns, run.argv)
	}
	want := fmt.Sprintf("largest_blob_kb=%d window_memory=64m %s", scanBlobBytesFifty/1024, pinWarning)
	if line := run.maintenanceLine(); !strings.HasSuffix(line, want) {
		t.Fatalf("maintenance line must end with %q:\n%s", want, line)
	}

	// A pin the largest blob does not outgrow carries no warning, and keeps
	// its own spelling.
	run = runWriter(t, nil, 0, map[string]string{"GC_JSONL_GC_WINDOW_MEMORY": "64M"})
	if run.err != nil {
		t.Fatalf("export failed: %v\n%s", run.err, run.out)
	}
	if line := run.maintenanceLine(); !strings.HasSuffix(line, "window_memory=64M") || strings.Contains(line, "WARNING") {
		t.Fatalf("a roomy pin must read window_memory=64M with no warning:\n%s", line)
	}
}

// The warning is the 4 x largest blob boundary, in git's own units.
func TestJsonlExportPinWarningBoundary(t *testing.T) {
	for _, tc := range []struct {
		blob int
		pin  string
		warn bool
	}{
		{scanBlobBytesFifty, "199m", true},
		{scanBlobBytesFifty, "200m", false},
		{scanBlobBytesFifty, "204800k", false},
		{scanBlobBytesFifty, "209715199", true},
		{scanBlobBytesFifty, "209715200", false},
		{scanBlobBytesLive, "1192m", true},
		{scanBlobBytesLive, "1193m", false},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.blob, tc.pin), func(t *testing.T) {
			scan := scanReport(tc.blob)
			run := runWriter(t, &scan, 0, map[string]string{"GC_JSONL_GC_WINDOW_MEMORY": tc.pin})
			if run.err != nil {
				t.Fatalf("export failed: %v\n%s", run.err, run.out)
			}
			if got := strings.Contains(run.maintenanceLine(), pinWarning); got != tc.warn {
				t.Fatalf("warning = %v, want %v:\n%s", got, tc.warn, run.maintenanceLine())
			}
		})
	}
}

// A scan that fails, or whose output cannot be read as a size, never falls
// back to a window: there is no repack, the run fails, and the line says so.
func TestJsonlExportScanFailureStopsTheRepack(t *testing.T) {
	for _, tc := range []struct {
		name string
		scan string
		exit int
		pin  string
		want string
	}{
		{"exit status", scanReport(4096), 1, "", "largest_blob_kb=unknown window_memory=unknown"},
		{"exit status with a pin", scanReport(4096), 1, "64m", "largest_blob_kb=unknown window_memory=64m"},
		{"empty output", "", 0, "", "largest_blob_kb=unknown window_memory=unknown"},
		{"empty output with a pin", "", 0, "64m", "largest_blob_kb=unknown window_memory=64m"},
		{"non-numeric size", "blob abc\n", 0, "", "largest_blob_kb=unknown window_memory=unknown"},
		{"no blob line", "commit 100\ntree 5\n", 0, "", "largest_blob_kb=unknown window_memory=unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := tc.scan
			env := map[string]string{}
			if tc.pin != "" {
				env["GC_JSONL_GC_WINDOW_MEMORY"] = tc.pin
			}
			run := runWriter(t, &scan, tc.exit, env)
			if run.err == nil {
				t.Fatalf("a failed scan must fail the run:\n%s", run.out)
			}
			if line := run.repackLine(); line != "" {
				t.Fatalf("a failed scan must not repack, but git ran %q", line)
			}
			line := run.maintenanceLine()
			if !strings.Contains(line, "status=failed") || !strings.HasSuffix(line, tc.want) {
				t.Fatalf("failure line = %q, want status=failed ... %s", line, tc.want)
			}
			if strings.Contains(run.out, "needs git 2.32") {
				t.Fatalf("the git-version hint must not blame a failed scan:\n%s", run.out)
			}
			if !strings.Contains(run.out, "largest blob") {
				t.Fatalf("the failure must say that the scan for the largest blob failed:\n%s", run.out)
			}
		})
	}
}

// A repack that fails keeps its git-version hint and reports the values it
// ran with.
func TestJsonlExportRepackFailureLineCarriesTheNewFields(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.limitPackingToSnapshot(t)
	f.failGitRepack(t)
	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a failed repack must fail the run:\n%s", out)
	}
	line := writerRun{out: out}.maintenanceLine()
	if !strings.Contains(line, "status=failed") || !regexp.MustCompile(` largest_blob_kb=\d+ window_memory=256m$`).MatchString(line) {
		t.Fatalf("failure line = %q, want largest_blob_kb=<n> window_memory=256m at its end", line)
	}
	if !strings.Contains(out, "needs git 2.32") {
		t.Fatalf("the repack failure keeps its git-version hint:\n%s", out)
	}
}

// The manual-recovery command prints the value the repack would have used.
func TestJsonlExportManualRecoveryLinePrintsTheEffectiveWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  string
		want string
	}{
		{"auto", "", "-c pack.windowMemory=1790m "},
		{"pin", "64m", "-c pack.windowMemory=64m "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := scanReport(scanBlobBytesLive)
			env := map[string]string{"GC_JSONL_GC_MIN_FREE_KB": "1000000000000000"}
			if tc.pin != "" {
				env["GC_JSONL_GC_WINDOW_MEMORY"] = tc.pin
			}
			run := runWriter(t, &scan, 0, env)
			if run.err == nil {
				t.Fatalf("a repack that does not fit must fail the run:\n%s", run.out)
			}
			var manual string
			for _, line := range strings.Split(run.out, "\n") {
				if strings.Contains(line, "to pack the archive once by hand") {
					manual = line
				}
			}
			if !strings.Contains(manual, tc.want) || !strings.Contains(manual, "-c core.bigFileThreshold=4g ") {
				t.Fatalf("the manual-recovery line must print %q and the threshold:\n%s", tc.want, run.out)
			}
		})
	}
}

// Nothing to pack, nothing to scan.
func TestJsonlExportDoesNotScanWhenNoRepackRuns(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(argvLog)
	if strings.Contains(string(data), scanFormat) || strings.Contains(string(data), " repack ") {
		t.Fatalf("the no-repack path must not scan or repack:\n%s", data)
	}
	if strings.Contains(out, archiveMaintMarker) {
		t.Fatalf("no maintenance line is due:\n%s", out)
	}
}

func TestJsonlExportRejectsInvalidSettleAndAlarmSettings(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"GC_JSONL_BIG_FILE_THRESHOLD", "0"},
		{"GC_JSONL_BIG_FILE_THRESHOLD", "08m"},
		{"GC_JSONL_BIG_FILE_THRESHOLD", "4GB"},
		{"GC_JSONL_BIG_FILE_THRESHOLD", "g"},
		{"GC_JSONL_SETTLE_CHECK_INTERVAL", "1d"},
		{"GC_JSONL_SETTLE_CHECK_TIMEOUT_SECS", "-1"},
		{"GC_JSONL_SETTLE_MIN_DISK_KB", "256k"},
		{"GC_JSONL_SETTLE_MIN_RATIO_PCT", "1%"},
		{"GC_JSONL_ARCHIVE_ALERT_KB", "8GiB"},
		{"GC_JSONL_SETTLE_ALERT_KB", "99999999999999999999"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			f := newArchiveMaintenanceFixture(t, 3, 5)
			seedHead := f.head(t)
			f.env[tc.name] = tc.value
			out, err := f.run(t)
			if err == nil {
				t.Fatalf("expected %s=%q to fail the run:\n%s", tc.name, tc.value, out)
			}
			if !strings.Contains(out, "jsonl-export: "+tc.name+" must be") {
				t.Fatalf("expected a %s validation error:\n%s", tc.name, out)
			}
			if head := f.head(t); head != seedHead {
				t.Fatalf("an invalid setting must stop the run before it commits: HEAD %s, want %s", head, seedHead)
			}
		})
	}
}

// --- the settle check --------------------------------------------------------

func newOversizedArchiveFixture(t *testing.T) archiveMaintenanceFixture {
	t.Helper()
	f := newArchiveMaintenanceFixture(t, 3, 5)
	buildOversizedDeltaPacks(t, f.archiveRepo)
	f.env["GC_JSONL_SETTLE_MIN_DISK_KB"] = "8"
	f.env["GC_JSONL_SETTLE_MIN_RATIO_PCT"] = "5"
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	return f
}

func settleCheck(t *testing.T, out string) []string {
	t.Helper()
	match := settleCheckTelemetry.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no archive settle check line in:\n%s", out)
	}
	return match
}

// The check finds the oversized deltas, says so, and writes nothing: no
// repack, no new pack, no pack rewritten.
func TestJsonlExportSettleCheckReportsWasteAndRewritesNothing(t *testing.T) {
	f := newOversizedArchiveFixture(t)
	packsBefore := packFileDigests(t, f.archiveRepo)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	line := settleCheck(t, out)
	flaggedKB, _ := strconv.Atoi(line[2])
	if line[1] != "3" || flaggedKB < 29 || flaggedKB > 32 || line[4] != "waste" {
		t.Fatalf("settle check = %v, want flagged=3, flagged_kb about 31 and status=waste:\n%s", line[1:], out)
	}
	if archiveKB, _ := strconv.Atoi(line[3]); archiveKB <= 0 {
		t.Fatalf("archive_kb must be the archive's size, got %s", line[3])
	}
	requireSameDigests(t, "the archive's packs", packsBefore, packFileDigests(t, f.archiveRepo))
	if strings.Contains(out, "packing archive object database") {
		t.Fatalf("the check must not trigger packing:\n%s", out)
	}
}

func TestJsonlExportSettleCheckOnACleanArchive(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	if line := settleCheck(t, out); line[1] != "0" || line[4] != "ok" {
		t.Fatalf("settle check = %v, want flagged=0 status=ok:\n%s", line[1:], out)
	}
}

// The check runs at most once per interval; 0 runs it on every maintenance pass.
func TestJsonlExportSettleCheckInterval(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	out, err := f.run(t)
	if err != nil || !strings.Contains(out, settleCheckMarker) {
		t.Fatalf("the first run must check (err %v):\n%s", err, out)
	}
	if _, ok := f.state(t)["last_settle_check_at"]; !ok {
		t.Fatalf("the check's time must be kept in the state file: %v", f.state(t))
	}
	out, err = f.run(t)
	if err != nil || strings.Contains(out, settleCheckMarker) {
		t.Fatalf("a run inside the interval must not check (err %v):\n%s", err, out)
	}

	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	out, err = f.run(t)
	if err != nil || !strings.Contains(out, settleCheckMarker) {
		t.Fatalf("interval 0 checks on every run (err %v):\n%s", err, out)
	}
}

// The check never fails the export: an error or a timeout is logged and the
// run goes on.
func TestJsonlExportSettleCheckFailureNeverFailsTheExport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		timeout string
		want    string
	}{
		{"error", "echo 'simulated listing failure' >&2\nexit 1\n", "", "archive settle check failed"},
		{"timeout", "exec sleep 30\n", "1", "archive settle check timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveMaintenanceFixture(t, 3, 5)
			writeExecutable(t, filepath.Join(f.binDir, "git"), "#!/bin/sh\ncase \"$*\" in\n*'%(deltabase)'*)\n"+tc.body+"    ;;\nesac\nexec '"+lookPathGit(t)+"' \"$@\"\n")
			f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
			if tc.timeout != "" {
				f.env["GC_JSONL_SETTLE_CHECK_TIMEOUT_SECS"] = tc.timeout
			}
			started := time.Now()
			out, err := f.run(t)
			if err != nil {
				t.Fatalf("a settle check problem must not fail the export: %v\n%s", err, out)
			}
			if time.Since(started) > 25*time.Second {
				t.Fatalf("the check did not stop at its timeout:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("expected %q in:\n%s", tc.want, out)
			}
			if subject := f.headSubject(t); !strings.HasPrefix(subject, "backup ") {
				t.Fatalf("the export must still commit its snapshot, HEAD subject = %q", subject)
			}
		})
	}
}

// --- the alarms --------------------------------------------------------------

// mutateState edits the state file and its backup alike.
func mutateState(t *testing.T, f archiveMaintenanceFixture, edit func(state map[string]any)) {
	t.Helper()
	for _, path := range []string{f.stateFile(), f.stateFile() + ".bak"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		state := readStateObject(t, path)
		edit(state)
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", path, err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
}

func alertSentAt(t *testing.T, f archiveMaintenanceFixture, condition string) (float64, bool) {
	t.Helper()
	alerts, _ := f.state(t)["archive_alerts"].(map[string]any)
	entry, _ := alerts[condition].(map[string]any)
	sentAt, ok := entry["sent_at"].(float64)
	return sentAt, ok
}

// One alert per condition per 24 hours, none below the limit or when the limit
// is 0, a silent re-arm once the condition clears.
func TestJsonlExportArchiveSizeAlarm(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "1"

	f.exportOnce(t)
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 1 {
		t.Fatalf("the first run over the limit sends one alert, got %d:\n%s", got, f.mail(t))
	}
	for _, want := range []string{"Archive: " + f.archiveRepo, "GC_JSONL_ARCHIVE_ALERT_KB"} {
		if !strings.Contains(f.mail(t), want) {
			t.Fatalf("the alert must say %q:\n%s", want, f.mail(t))
		}
	}
	if _, ok := alertSentAt(t, f, "size"); !ok {
		t.Fatalf("the alert time is kept in the state file: %v", f.state(t))
	}

	if out, err := f.run(t); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 1 {
		t.Fatalf("the condition still holds inside 24 hours, so no second alert; got %d", got)
	}

	// A day later the standing condition alerts again.
	mutateState(t, f, func(state map[string]any) {
		state["archive_alerts"] = map[string]any{"size": map[string]any{"sent_at": float64(time.Now().Unix() - secondsPerDayAndSix)}}
	})
	if out, err := f.run(t); err != nil {
		t.Fatalf("third run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 2 {
		t.Fatalf("a standing condition alerts again after 24 hours, got %d", got)
	}

	// The condition clears: no mail, and the marker is gone.
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "999999999"
	if out, err := f.run(t); err != nil {
		t.Fatalf("fourth run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 2 {
		t.Fatalf("clearing the condition sends nothing, got %d alerts", got)
	}
	if _, ok := alertSentAt(t, f, "size"); ok {
		t.Fatalf("a cleared condition must re-arm, but the state still holds it: %v", f.state(t))
	}
	// ... and it alerts at once when it comes back.
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "1"
	if out, err := f.run(t); err != nil {
		t.Fatalf("fifth run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 3 {
		t.Fatalf("a re-armed alarm alerts when the condition returns, got %d", got)
	}
}

func TestJsonlExportArchiveSizeAlarmIsOffAtZero(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "0"
	f.env["GC_JSONL_SETTLE_ALERT_KB"] = "0"
	f.exportOnce(t)
	if strings.Contains(f.mail(t), "JSONL archive") {
		t.Fatalf("a limit of 0 disables the alarm:\n%s", f.mail(t))
	}
}

func TestJsonlExportSettleAlarm(t *testing.T) {
	f := newOversizedArchiveFixture(t)
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "0"
	f.env["GC_JSONL_SETTLE_ALERT_KB"] = "1"

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	if line := settleCheck(t, out); line[1] != "3" || line[4] != "alerted" {
		t.Fatalf("settle check = %v, want flagged=3 status=alerted:\n%s", line[1:], out)
	}
	if got := strings.Count(f.mail(t), settleAlertSubject); got != 1 {
		t.Fatalf("expected one oversized-deltas alert, got %d:\n%s", got, f.mail(t))
	}
	if strings.Contains(f.mail(t), sizeAlertSubject) {
		t.Fatalf("the size alarm is off here:\n%s", f.mail(t))
	}
	for _, want := range []string{"Archive: " + f.archiveRepo, "jsonl-archive-settle.sh", "flagged=3"} {
		if !strings.Contains(f.mail(t), want) {
			t.Fatalf("the alert must say %q:\n%s", want, f.mail(t))
		}
	}
	if out, err := f.run(t); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), settleAlertSubject); got != 1 {
		t.Fatalf("at most one alert per 24 hours, got %d", got)
	}
}

// An alert that cannot be delivered stays pending, survives a corrupted state
// file through its backup, and goes out once on the next run.
func TestJsonlExportRetriesPendingArchiveAlertFromBackupAfterPrimaryCorruption(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "1"
	writeJsonlExportGCStubWithMailExitCode(t, f.binDir, 1)

	f.exportOnce(t)
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 1 {
		t.Fatalf("the failed delivery is attempted once, got %d", got)
	}
	for _, path := range []string{f.stateFile(), f.stateFile() + ".bak"} {
		pending, _ := readStateObject(t, path)["pending_archive_alerts"].(map[string]any)
		if _, ok := pending["size"]; !ok {
			t.Fatalf("%s must hold the undelivered alert: %v", path, readStateObject(t, path))
		}
	}
	if err := os.WriteFile(f.stateFile(), []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}
	writeJsonlExportGCStub(t, f.binDir)

	if out, err := f.run(t); err != nil {
		t.Fatalf("retry run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 2 {
		t.Fatalf("expected the failed attempt plus one backup-backed retry, got %d:\n%s", got, f.mail(t))
	}
	state := f.state(t)
	if _, ok := state["pending_archive_alerts"]; ok {
		t.Fatalf("a delivered alert must leave the pending set: %v", state)
	}
	if _, ok := alertSentAt(t, f, "size"); !ok {
		t.Fatalf("a delivered alert records its time: %v", state)
	}
	if out, err := f.run(t); err != nil {
		t.Fatalf("third run failed: %v\n%s", err, out)
	}
	if got := strings.Count(f.mail(t), sizeAlertSubject); got != 2 {
		t.Fatalf("no further alert inside 24 hours, got %d", got)
	}
}

// --- the lock ---------------------------------------------------------------

// While a settle holds the lock the export still exports, commits and pushes;
// it leaves the packing and the check alone and reports success, so it adds
// nothing to a failure streak.
func TestJsonlExportSkipsArchiveMaintenanceWhileTheSettleHoldsTheLock(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remote := f.addOrigin(t)
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	f.limitPackingToSnapshot(t)
	f.env["GC_JSONL_SETTLE_CHECK_INTERVAL"] = "0"
	f.env["GC_JSONL_ARCHIVE_ALERT_KB"] = "1"
	release := holdArchiveLock(t, f.archiveRepo)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("the export must succeed while the settle holds the lock: %v\n%s", err, out)
	}
	if !strings.Contains(out, "archive maintenance skipped: settle in progress") {
		t.Fatalf("expected the skip line:\n%s", out)
	}
	data, _ := os.ReadFile(argvLog)
	if strings.Contains(string(data), " repack ") || strings.Contains(out, settleCheckMarker) || strings.Contains(out, "packing archive object database") {
		t.Fatalf("a held lock means no repack and no detector:\n%s\n%s", out, data)
	}
	if strings.Contains(f.mail(t), "JSONL archive") {
		t.Fatalf("a skipped pass raises no alarm:\n%s", f.mail(t))
	}
	if subject := f.headSubject(t); !strings.HasPrefix(subject, "backup ") {
		t.Fatalf("the export must still commit its snapshot, HEAD subject = %q", subject)
	}
	if got := f.originMain(t, remote); got != f.head(t) {
		t.Fatalf("the export must still push its snapshot: origin %s, HEAD %s", got, f.head(t))
	}
	requireEmptyLockFile(t, f.archiveRepo)

	// Once the lock is free the same backlog is packed.
	release()
	out, err = f.run(t)
	if err != nil || !strings.Contains(out, "packing archive object database") {
		t.Fatalf("a run with the lock free must pack the backlog (err %v):\n%s", err, out)
	}
	requireEmptyLockFile(t, f.archiveRepo)
}

// The legacy copies stay until an operator removes them: nothing ages them out.
func TestJsonlExportLegacyCopiesCommentDoesNotPromiseRetention(t *testing.T) {
	data, err := os.ReadFile(coreScriptPath("jsonl-export.sh"))
	if err != nil {
		t.Fatalf("ReadFile(jsonl-export.sh): %v", err)
	}
	if strings.Contains(string(data), "normal archive retention ages the legacy copies out") {
		t.Fatal("jsonl-export.sh still says archive retention ages the legacy copies out; no mechanism does")
	}
}
