package splittest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// StrictEnv turns the split-strict test gate on ("1") for a package whose
	// store seams consult GateEnabled.
	StrictEnv = "GC_TEST_SPLIT_STRICT"
	// StrictPanicEnv makes every gate violation at a production site panic at
	// the offending call ("1"), for bisecting one site with a full stack.
	StrictPanicEnv = "GC_TEST_SPLIT_STRICT_PANIC"
	// StrictReportEnv names an extra file the observed site table is copied
	// to when the run ends.
	StrictReportEnv = "GC_TEST_SPLIT_STRICT_REPORT"
	// StrictDirEnv is the directory the gate writes its files to. It
	// defaults to TEST_UNDECLARED_OUTPUTS_DIR under Bazel, and to a fresh
	// temporary directory otherwise; the first process to start exports it so
	// re-executed helper processes write beside it.
	StrictDirEnv = "GC_TEST_SPLIT_STRICT_DIR"
	// StrictTestTimeoutEnv bounds one top-level test's running time under the
	// gate (a Go duration; default 3m; "0" disables). A test past it is a hang:
	// the gate prints it, dumps every goroutine, writes its report and exits
	// the binary non-zero, so a hung shard still reports.
	StrictTestTimeoutEnv = "GC_TEST_SPLIT_STRICT_TEST_TIMEOUT"

	strictPrimaryEnv      = "GC_TEST_SPLIT_STRICT_PRIMARY_PID"
	eventsFilePattern     = "split-strict-events.*.tsv"
	observedReportName    = "split-strict-observed.tsv"
	relicReportName       = "split-strict-relics.tsv"
	resultReportName      = "split-strict-result.txt"
	defaultStrictTestTime = 3 * time.Minute

	// logTableBegin and logTableEnd fence a table the gate prints into the
	// test log.
	logTableBegin = "SPLIT-STRICT TABLE BEGIN "
	logTableEnd   = "SPLIT-STRICT TABLE END"
)

// gateSink is the process-wide violation sink the gate's views record into.
var gateSink = newGateSink()

func newGateSink() *violationSink {
	s := newViolationSink()
	s.openOnDemand = true
	return s
}

// GateEnabled reports whether GC_TEST_SPLIT_STRICT=1 asked for the gate.
func GateEnabled() bool {
	return os.Getenv(StrictEnv) == "1"
}

// GateWorkView returns the gate's work view over store when the gate is on,
// and store unchanged otherwise. A nil store stays nil.
func GateWorkView(store beads.Store) beads.Store {
	if store == nil || !GateEnabled() {
		return store
	}
	return ProjectionOf(store).WorkViewOf(store)
}

// GateClassView returns the gate's class view over store when the gate is
// on, and store unchanged otherwise. A nil store stays nil.
func GateClassView(store beads.Store) beads.Store {
	if store == nil || !GateEnabled() {
		return store
	}
	return ProjectionOf(store).ClassViewOf(store)
}

// GateSites returns every key this process's gate sink has charged so far.
func GateSites() []SiteSummary {
	return gateSink.summaries()
}

// GateFiles names a package's checked-in gate inputs, relative to the
// package directory the test binary runs in.
type GateFiles struct {
	// Baseline is the known violation keys (ParseBaseline).
	Baseline string
	// KnownFlaky lists tests whose failures are known flakes, each with the
	// bead that owns the flake; a failure of one is reported, not counted.
	KnownFlaky string
	// KnownFailures lists the top-level tests known to fail under the gate,
	// one per line; a test failing that is not listed fails the run.
	KnownFailures string
}

// gateRun is the primary process's run state.
type gateRun struct {
	dir     string
	stdout  *os.File
	pipeW   *os.File
	done    chan struct{}
	stop    chan struct{}
	mu      sync.Mutex
	running map[string]time.Time
	ran     map[string]bool
	failed  map[string]bool
	hungMu  sync.Mutex
	hung    string
	// finishOnce and finishCode make finishGate run once per run.
	finishOnce sync.Once
	finishCode int
}

var (
	gateRunMu sync.Mutex
	activeRun *gateRun
)

// GateStart prepares the gate for a test binary's run: it picks the gate
// directory, and in the first (primary) process tees the binary's stdout to
// learn which tests ran and failed, and arms the per-test watchdog. Call it
// from TestMain before m.Run; it does nothing when the gate is off, and only
// prepares the directory in a re-executed helper process.
func GateStart() {
	if !GateEnabled() {
		return
	}
	dir := gateDir()
	if os.Getenv(strictPrimaryEnv) != "" {
		return
	}
	_ = os.Setenv(strictPrimaryEnv, strconv.Itoa(os.Getpid()))
	run := &gateRun{dir: dir, done: make(chan struct{}), stop: make(chan struct{}), running: map[string]time.Time{}, ran: map[string]bool{}, failed: map[string]bool{}}
	r, w, err := os.Pipe()
	if err == nil {
		run.stdout = os.Stdout
		run.pipeW = w
		os.Stdout = w
		go run.tee(r)
	} else {
		close(run.done)
	}
	if limit := strictTestTimeout(); limit > 0 {
		go run.watch(limit)
	}
	gateRunMu.Lock()
	activeRun = run
	gateRunMu.Unlock()
}

// gateDir returns (creating and exporting it on first use) the gate's
// directory.
func gateDir() string {
	if dir := strings.TrimSpace(os.Getenv(StrictDirEnv)); dir != "" {
		return dir
	}
	dir := strings.TrimSpace(os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"))
	if dir == "" {
		tmp, err := os.MkdirTemp("", "split-strict-")
		if err != nil {
			return ""
		}
		dir = tmp
	}
	_ = os.Setenv(StrictDirEnv, dir)
	return dir
}

func strictTestTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(StrictTestTimeoutEnv))
	if raw == "" {
		return defaultStrictTestTime
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultStrictTestTime
	}
	return d
}

// tee copies the binary's stdout through, noting test starts, pauses and
// results (the testing package's -test.v lines).
func (g *gateRun) tee(r io.Reader) {
	defer close(g.done)
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			_, _ = io.WriteString(g.stdout, line)
			g.observe(strings.TrimRight(line, "\n"))
		}
		if err != nil {
			return
		}
	}
}

// soleRunningTest is the one top-level test the primary process's stdout
// tee shows running, or "" when there is no gate run (a re-executed helper
// process) or when none or several (parallel) tests are running.
func soleRunningTest() string {
	gateRunMu.Lock()
	run := activeRun
	gateRunMu.Unlock()
	if run == nil {
		return ""
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.running) != 1 {
		return ""
	}
	for name := range run.running {
		return name
	}
	return ""
}

// observe tracks top-level tests from one output line.
func (g *gateRun) observe(line string) {
	field := func(prefix string) (string, bool) {
		if !strings.HasPrefix(line, prefix) {
			return "", false
		}
		name := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(name) == 0 || strings.Contains(name[0], "/") {
			return "", false
		}
		return name[0], true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if name, ok := field("=== RUN"); ok {
		g.running[name] = time.Now()
		g.ran[name] = true
		return
	}
	if name, ok := field("=== CONT"); ok {
		g.running[name] = time.Now()
		return
	}
	if name, ok := field("=== PAUSE"); ok {
		delete(g.running, name)
		return
	}
	for _, prefix := range []string{"--- PASS:", "--- SKIP:", "--- FAIL:"} {
		if name, ok := field(prefix); ok {
			delete(g.running, name)
			g.ran[name] = true
			if prefix == "--- FAIL:" {
				g.failed[name] = true
			}
			return
		}
	}
}

// watch ends the run when one top-level test runs past limit.
func (g *gateRun) watch(limit time.Duration) {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-g.stop:
			ticker.Stop()
			return
		case now := <-ticker.C:
			g.mu.Lock()
			var hung string
			var since time.Time
			for name, start := range g.running {
				if now.Sub(start) > limit && (hung == "" || start.Before(since)) {
					hung, since = name, start
				}
			}
			g.mu.Unlock()
			if hung == "" {
				continue
			}
			g.hungMu.Lock()
			g.hung = hung
			g.hungMu.Unlock()
			ticker.Stop()
			buf := make([]byte, 8<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "SPLIT-STRICT HUNG TEST: %s ran longer than %s under the gate; goroutines:\n%s\n", hung, limit, buf[:n]) //nolint:errcheck // best-effort test log
			code := finishGate(1, false, g, gateFilesForHang, os.Stderr)
			runHangCleanup()
			os.Exit(code)
		}
	}
}

var (
	hangCleanupMu sync.Mutex
	hangCleanup   func()
)

// SetHangCleanup registers the teardown the hang path runs before it exits
// the binary: the work TestMain would have done after m.Run (temporary
// roots, helper processes), which os.Exit would otherwise skip.
func SetHangCleanup(fn func()) {
	hangCleanupMu.Lock()
	defer hangCleanupMu.Unlock()
	hangCleanup = fn
}

func runHangCleanup() {
	hangCleanupMu.Lock()
	fn := hangCleanup
	hangCleanupMu.Unlock()
	if fn != nil {
		fn()
	}
}

// gateFilesForHang is the package's GateFiles, recorded by GateMain's caller
// through SetGateFiles so a hang can still run the ratchet.
var gateFilesForHang GateFiles

// SetGateFiles records the package's gate inputs for the hang path. Call it
// before GateStart.
func SetGateFiles(files GateFiles) { gateFilesForHang = files }

// GateChecksBase is the code a TestMain hands the checks it runs around the
// tests (leak guards, the no-city and session-key guards) in place of m.Run's
// code: 0 under the gate, so each check answers only for itself, and code
// unchanged otherwise. A check raises a 0 to 1 but leaves a non-zero code
// alone, so over a known test failure's code its own failure would not show.
func GateChecksBase(code int) int {
	if GateEnabled() {
		return 0
	}
	return code
}

// GateMain finishes a test binary's run under the gate and returns the exit
// code the binary should use. tests is m.Run's code and checks the code the
// checks around it returned when handed GateChecksBase(tests). Off the gate,
// and in a re-executed helper process, it returns what the checks would have
// returned over tests: tests when non-zero, else checks. See finishGate for
// what it reports.
func GateMain(tests, checks int, files GateFiles, out io.Writer) int {
	code := tests
	if code == 0 {
		code = checks
	}
	if !GateEnabled() {
		return code
	}
	gateRunMu.Lock()
	run := activeRun
	gateRunMu.Unlock()
	if run == nil {
		return code
	}
	return finishGate(tests, checks != 0, run, files, out)
}

// finishGate restores stdout, aggregates every process's sightings, writes the
// observed and relic tables and the result file, and prints two separate
// verdicts:
//
//	SPLIT-STRICT RATCHET: PASS|FAIL  — every violation key is a known,
//	    adjudicated baseline row (no new key, no triage row, no function past
//	    its line cap);
//	SPLIT-STRICT TESTS: PASS|FAIL    — every failing test is a known one,
//	    and every check TestMain runs around the tests passed.
//
// code is m.Run's; checksFailed reports a failed check (GateMain). The
// binary's exit code is non-zero when either verdict fails, when a test hung,
// when a check failed, or when the run failed without a failing test the gate
// could see, so a run whose only failures are the known ones passes.
//
// It finishes a run once: when a hung test ends while the watchdog dumps
// stacks, the watchdog and GateMain both call it, and the later call waits
// for the first and returns its code.
func finishGate(code int, checksFailed bool, run *gateRun, files GateFiles, out io.Writer) int {
	run.finishOnce.Do(func() {
		run.finishCode = finishRun(code, checksFailed, run, files, out)
	})
	return run.finishCode
}

// finishRun is finishGate's one run.
func finishRun(code int, checksFailed bool, run *gateRun, files GateFiles, out io.Writer) int {
	close(run.stop)
	if run.pipeW != nil {
		os.Stdout = run.stdout
		_ = run.pipeW.Close()
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
		}
	}
	run.hungMu.Lock()
	hung := run.hung
	run.hungMu.Unlock()

	var lines []string
	say := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	failed := false

	all, err := readSightingFiles(run.dir)
	if err != nil {
		say("split-strict: reading the gate's event files in %s: %v", run.dir, err)
		failed = true
	}
	all = append(all, gateSink.sightings()...)
	all = dedupeSightings(all)
	relics := summarize(all, kindRelic)
	// Relic keys are ratcheted with the violations (their views carry the
	// relic: prefix), so a new relic read is new debt the run reports.
	observed := append(summarize(all, kindViolation), relics...)
	writeGateFile(run.dir, observedReportName, FormatSiteTable(observed), out)
	writeGateFile(run.dir, relicReportName, FormatSiteTable(relics), out)
	if path := strings.TrimSpace(os.Getenv(StrictReportEnv)); path != "" {
		writeGateFile(filepath.Dir(path), filepath.Base(path), FormatSiteTable(observed), out)
	}

	ratchet := "PASS"
	if failed {
		// Sightings it could not read are keys it could not judge.
		ratchet = "FAIL"
	}
	report := ""
	baseline, err := readBaselineFile(files.Baseline)
	var result RatchetResult
	if err != nil {
		say("split-strict: %v", err)
		ratchet = "FAIL"
	} else {
		result = CompareToBaseline(observed, baseline)
		report = result.Report(files.Baseline)
		if result.Failed() {
			ratchet = "FAIL"
		}
	}
	if n := gateSink.testOnlyCount(); n > 0 {
		say("split-strict: %d violation(s) came from test code reading through a view directly (fixture artifacts, not keys).", n)
	}
	if len(relics) > 0 {
		say("split-strict: %d relic key(s) read under beads.RelicRead (declared debt; see %s).", len(relics), relicReportName)
	}

	tests := "PASS"
	known, err := readKnownFailures(files.KnownFailures)
	if err != nil {
		say("split-strict: %v", err)
		tests = "FAIL"
	}
	flaky, err := readKnownFlaky(files.KnownFlaky)
	if err != nil {
		say("split-strict: %v", err)
		tests = "FAIL"
	}
	sorted := run.sortFailing(known, flaky)
	failing := sorted.failing
	if len(sorted.newFailing) > 0 || len(sorted.untriaged) > 0 || hung != "" {
		tests = "FAIL"
	}
	if code != 0 && len(failing) == 0 {
		// The binary failed outside every test (a panic in TestMain or an
		// init, a crashed helper): nothing here says it was a known failure.
		say("split-strict: exit %d with no failing test (the binary failed outside every test)", code)
		tests = "FAIL"
	}
	if checksFailed {
		// A leak guard, the no-city guard or the session-key guard failed;
		// its report is above. Known test failures do not excuse it.
		say("split-strict: a check TestMain runs around the tests failed (its report is above)")
		tests = "FAIL"
	}

	fmt.Fprint(out, strings.Join(lines, "\n")) //nolint:errcheck // best-effort test log
	if len(lines) > 0 {
		fmt.Fprintln(out) //nolint:errcheck // best-effort test log
	}
	fmt.Fprint(out, report)                                                                                                         //nolint:errcheck // best-effort test log
	fmt.Fprintf(out, "SPLIT-STRICT RATCHET: %s (%d key(s) observed, %d new, %d triage, %d past their line cap, %d out of scope)\n", //nolint:errcheck // best-effort test log
		ratchet, len(observed), len(result.New), len(result.Triage), len(result.LineCap), len(result.OutOfScope))
	fmt.Fprintf(out, "SPLIT-STRICT TESTS: %s (%d failing, %d not in %s%s%s%s)\n", tests, len(failing), len(sorted.newFailing), files.KnownFailures, untriagedNote(len(sorted.untriaged)), hungNote(hung), checksNote(checksFailed)) //nolint:errcheck // best-effort test log
	sorted.report(out, files.KnownFailures, flaky)
	// The tables also go to the log, which every runner keeps (a remote run
	// does not bring test.outputs back): scripts/split-strict-baseline reads
	// them from either place.
	fmt.Fprintf(out, "%s\n%s%s\n", logTableBegin+observedReportName, FormatSiteTable(observed), logTableEnd) //nolint:errcheck // best-effort test log
	fmt.Fprintf(out, "%s\n%s%s\n", logTableBegin+relicReportName, FormatSiteTable(relics), logTableEnd)      //nolint:errcheck // best-effort test log
	writeGateFile(run.dir, resultReportName, fmt.Sprintf("ratchet=%s\ntests=%s\nhung=%s\nnew_failing=%s\n", ratchet, tests, hung, strings.Join(sorted.newFailing, ",")), out)

	if failed || ratchet == "FAIL" || tests == "FAIL" {
		if code != 0 && len(failing) == 0 {
			return code
		}
		return 1
	}
	return 0
}

// failingTests are a run's failing top-level tests sorted against the
// package's known failures and known flakes.
type failingTests struct {
	failing    []string // every failing test
	newFailing []string // listed nowhere
	untriaged  []string // known failures listed with an UNTRIAGED reason
	flaked     []string // known flakes
	fixed      []string // known failures that ran and passed
}

// sortFailing sorts the run's failing tests against known and flaky.
func (g *gateRun) sortFailing(known, flaky map[string]string) failingTests {
	g.mu.Lock()
	defer g.mu.Unlock()
	sorted := failingTests{failing: sortedKeys(g.failed)}
	for _, name := range sorted.failing {
		switch {
		case known[name] != "":
			if Untriaged(known[name]) {
				sorted.untriaged = append(sorted.untriaged, name)
			}
		case flaky[name] != "":
			sorted.flaked = append(sorted.flaked, name)
		default:
			sorted.newFailing = append(sorted.newFailing, name)
		}
	}
	for name := range known {
		if g.ran[name] && !g.failed[name] {
			sorted.fixed = append(sorted.fixed, name)
		}
	}
	sort.Strings(sorted.fixed)
	return sorted
}

// report prints one line per failing test that is not a plain known
// failure, and per known failure that now passes.
func (f failingTests) report(out io.Writer, knownFailures string, flaky map[string]string) {
	for _, name := range f.newFailing {
		fmt.Fprintf(out, "  NEW FAILING %s\n", name) //nolint:errcheck // best-effort test log
	}
	for _, name := range f.untriaged {
		fmt.Fprintf(out, "  UNTRIAGED %s: %s lists it without a ruling; replace its reason with why it fails under the gate\n", name, knownFailures) //nolint:errcheck // best-effort test log
	}
	for _, name := range f.flaked {
		fmt.Fprintf(out, "  KNOWN FLAKY %s failed (%s)\n", name, flaky[name]) //nolint:errcheck // best-effort test log
	}
	for _, name := range f.fixed {
		fmt.Fprintf(out, "  NOW PASSING %s (remove it from %s)\n", name, knownFailures) //nolint:errcheck // best-effort test log
	}
}

func hungNote(hung string) string {
	if hung == "" {
		return ""
	}
	return "; HUNG: " + hung
}

func untriagedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d listed UNTRIAGED", n)
}

func checksNote(failed bool) string {
	if !failed {
		return ""
	}
	return "; a TestMain check failed"
}

func dedupeSightings(all []sighting) []sighting {
	seen := map[sighting]bool{}
	out := all[:0:0]
	for _, sg := range all {
		key := sg
		key.via = ""
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sg)
	}
	return out
}

func writeGateFile(dir, name, content string, out io.Writer) {
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		fmt.Fprintf(out, "split-strict: writing %s: %v\n", name, err) //nolint:errcheck // best-effort test log
	}
}

func readBaselineFile(path string) ([]BaselineEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the split-strict baseline: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	entries, err := ParseBaseline(f)
	if err != nil {
		return nil, fmt.Errorf("parsing the split-strict baseline %s: %w", path, err)
	}
	return entries, nil
}

// readKnownFailures reads a known-failures list (ParseKnownFailures).
func readKnownFailures(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the split-strict known failures: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	return ParseKnownFailures(f)
}

// readKnownFlaky reads a known-flaky list (ParseKnownFlaky); a package
// without one has none.
func readKnownFlaky(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the split-strict known flaky tests: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	return ParseKnownFlaky(f)
}

// ParseKnownFailures reads a known-failures list: one top-level test per
// line, tab, the reason it fails under the gate; '#' comments. Every entry
// needs a reason, so the list can only shrink by someone reading it.
func ParseKnownFailures(r io.Reader) (map[string]string, error) {
	return parseTestList(r, "known failures", 2)
}

// UntriagedReason opens the reason scripts/split-strict-baseline writes for a
// test it adds to a known-failures list: the test's first logged message, not
// yet a ruling. A failing test listed with it fails the TESTS verdict until
// someone replaces the reason with why the test fails under the gate (a gate
// artifact, a misroute and its owner bead, a known violation).
const UntriagedReason = "UNTRIAGED:"

// Untriaged reports a known-failures reason that is not yet a ruling.
func Untriaged(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), UntriagedReason)
}

// ParseKnownFlaky reads a known-flaky list: test, tab, the owner bead that
// tracks the flake, tab, the reason. A listed test's failure is reported but
// does not fail the TESTS verdict; it is not a known failure, so it is never
// baked into the known-failures list either.
func ParseKnownFlaky(r io.Reader) (map[string]string, error) {
	return parseTestList(r, "known flaky", 3)
}

func parseTestList(r io.Reader, what string, fields int) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != fields {
			return nil, fmt.Errorf("%s line %d: want %d tab-separated fields, got %d", what, lineNo, fields, len(f))
		}
		name := f[0]
		if !strings.HasPrefix(name, "Test") || strings.ContainsAny(name, " /") {
			return nil, fmt.Errorf("%s line %d: %q is not a top-level test name", what, lineNo, name)
		}
		for _, field := range f[1:] {
			if strings.TrimSpace(field) == "" {
				return nil, fmt.Errorf("%s line %d: %s has an empty field; every entry says why", what, lineNo, name)
			}
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s line %d: %s repeats", what, lineNo, name)
		}
		out[name] = strings.Join(f[1:], ": ")
	}
	return out, scanner.Err()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
