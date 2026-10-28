package splittest

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// SplitViolation is one read or write that reached a wrong-class row through
// the wrong store: on a split city the row would not be there, so the read
// would have come back silently empty (or the write would have landed in a
// store no reader of its class looks at).
type SplitViolation struct {
	// View is the side of the split the caller held: "work" or "class".
	View string
	// Op is the store operation: "get", "list", "children", "ready",
	// "update", "create", "dep-add", ...
	Op string
	// Class is the coordination class of the offending row.
	Class string
	// ID is the offending row's id.
	ID string
	// Site is the production call site the violation is charged to.
	Site Site
	// Via is the production frame that called Site, for triage.
	Via Site
	// Pkg is the nearest frame in the package under test that led to Site.
	Pkg Site
	// Test is the top-level test the call ran under ("" off the test's
	// goroutine).
	Test string
}

// String renders the violation for a failure message.
func (v SplitViolation) String() string {
	test := v.Test
	if test == "" {
		test = "?"
	}
	return fmt.Sprintf("%s %s: %s of %s row %s through the %s store [test %s]", v.Site, v.Site.Func, v.Op, v.Class, v.ID, v.View, test)
}

// SiteKey is what the ratchet matches a violation on: where it happened (file
// and function, not line, so an edit above it does not move it), what it did
// (operation, view, row class), and which function of the package under test
// led there.
type SiteKey struct {
	File, Func, Op, View, Class, Caller string
}

func (k SiteKey) String() string {
	return fmt.Sprintf("%s %s %s/%s/%s via %s", k.File, k.Func, k.Op, k.View, k.Class, k.Caller)
}

func keyOf(v SplitViolation) SiteKey {
	caller := v.Pkg.Func
	if caller == "" {
		caller = "-"
	}
	return SiteKey{File: v.Site.File, Func: v.Site.Func, Op: v.Op, View: v.View, Class: v.Class, Caller: caller}
}

// SiteSummary aggregates every violation charged to one SiteKey.
type SiteSummary struct {
	Key SiteKey
	// Lines are the distinct source lines in Key.File the key was seen at.
	Lines []int
	// Tests is the number of distinct tests that reached the key: the count
	// the baseline records (deterministic for a test set, additive across
	// shards and processes, each test running in exactly one).
	Tests int
	// Events counts distinct (line, test) sightings, for information.
	Events int
	// TestNames are the distinct tests that reached the key ("?" for a
	// goroutine off any test's stack), sorted. A fixture row of the baseline
	// is bound to them: the same key reached from any other test is new.
	TestNames []string
	// CallerLines are the distinct lines of Key.Caller the key was reached
	// from.
	CallerLines []int
	// SampleTest and SampleVia name one test and one caller, for triage.
	SampleTest, SampleVia string
}

// Site returns the key's first line as a Site.
func (s SiteSummary) Site() Site {
	line := 0
	if len(s.Lines) > 0 {
		line = s.Lines[0]
	}
	return Site{File: s.Key.File, Line: line, Func: s.Key.Func}
}

// eventKind separates a violation from a relic read.
type eventKind string

const (
	kindViolation eventKind = "violation"
	kindRelic     eventKind = "relic"
)

// sighting is one distinct (kind, key, line, test) the sink has seen.
type sighting struct {
	kind       eventKind
	key        SiteKey
	line       int
	callerLine int
	test       string
	via        string
}

// violationSink collects sightings. The gate's process-wide sink also appends
// each new sighting to an events file the moment it is seen (see gate.go), so
// a hung or crashed test binary still leaves its sites behind, and re-executed
// helper processes add theirs to the same directory.
type violationSink struct {
	mu       sync.Mutex
	seen     map[sighting]bool
	order    []sighting
	testOnly int
	// file, when set, receives one line per new sighting.
	file *os.File
	// openOnDemand opens file in the gate directory (StrictDirEnv) on the
	// first sighting: the gate's own sink, in the primary process and in
	// every re-executed helper process alike.
	openOnDemand bool
	fileTried    bool
}

func newViolationSink() *violationSink {
	return &violationSink{seen: map[sighting]bool{}}
}

// record adds one violation, panicking first at a production site when
// GC_TEST_SPLIT_STRICT_PANIC=1 asks for a stack at the offending call (for
// bisecting one site).
func (s *violationSink) record(v SplitViolation) {
	if v.Site.File != "" && os.Getenv(StrictPanicEnv) == "1" {
		panic("split-strict violation: " + v.String())
	}
	s.add(kindViolation, v)
}

// recordRelic counts a read under beads.RelicRead: declared debt, not a
// failure.
func (s *violationSink) recordRelic(v SplitViolation) {
	s.add(kindRelic, v)
}

func (s *violationSink) add(kind eventKind, v SplitViolation) {
	test := v.Test
	if test == "" && v.Site.File != "" {
		// Off the test's goroutine (a controller loop, a handler): charge
		// the sighting to the one top-level test running, so a fixture row's
		// scope binds it too. With none or several running it stays "?".
		test = soleRunningTest()
	}
	if test == "" {
		test = "?"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.Site.File == "" {
		if kind == kindViolation {
			s.testOnly++
		}
		return
	}
	via := ""
	if v.Via.File != "" {
		via = v.Via.String() + " " + v.Via.Func
	}
	key := keyOf(v)
	if kind == kindRelic {
		// Relic reads are ratcheted too, in a key space of their own.
		key.View = relicViewPrefix + key.View
	}
	sg := sighting{kind: kind, key: key, line: v.Site.Line, callerLine: v.Pkg.Line, test: test}
	if s.seen[sg] {
		return
	}
	s.seen[sg] = true
	sg.via = via
	s.order = append(s.order, sg)
	if s.openOnDemand && !s.fileTried {
		s.fileTried = true
		if dir := os.Getenv(StrictDirEnv); dir != "" {
			name := filepath.Join(dir, strings.Replace(eventsFilePattern, "*", strconv.Itoa(os.Getpid()), 1))
			if f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				s.file = f
				for _, earlier := range s.order[:len(s.order)-1] {
					fmt.Fprintln(f, formatSighting(earlier)) //nolint:errcheck // best-effort test report
				}
			}
		}
	}
	if s.file != nil {
		fmt.Fprintln(s.file, formatSighting(sg)) //nolint:errcheck // best-effort test report
	}
}

// formatSighting renders one events-file line.
func formatSighting(sg sighting) string {
	k := sg.key
	return strings.Join([]string{
		string(sg.kind), k.File, k.Func, k.Op, k.View, k.Class, k.Caller,
		strconv.Itoa(sg.line), strconv.Itoa(sg.callerLine), sg.test, sg.via,
	}, "\t")
}

// parseSighting reads one events-file line.
func parseSighting(line string) (sighting, bool) {
	f := strings.Split(line, "\t")
	if len(f) != 11 {
		return sighting{}, false
	}
	n, err := strconv.Atoi(f[7])
	if err != nil {
		return sighting{}, false
	}
	c, err := strconv.Atoi(f[8])
	if err != nil {
		return sighting{}, false
	}
	return sighting{
		kind: eventKind(f[0]), key: SiteKey{File: f[1], Func: f[2], Op: f[3], View: f[4], Class: f[5], Caller: f[6]},
		line: n, callerLine: c, test: f[9], via: f[10],
	}, true
}

// sightings returns what this sink has seen, in order.
func (s *violationSink) sightings() []sighting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sighting(nil), s.order...)
}

// summaries aggregates this sink's violations (not relics).
func (s *violationSink) summaries() []SiteSummary {
	return summarize(s.sightings(), kindViolation)
}

// testOnlyCount returns the number of violations no production frame owned.
func (s *violationSink) testOnlyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.testOnly
}

// reset drops everything recorded so far.
func (s *violationSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = map[sighting]bool{}
	s.order = nil
	s.testOnly = 0
}

// summarize folds sightings of one kind into per-key summaries, sorted.
func summarize(all []sighting, kind eventKind) []SiteSummary {
	type agg struct {
		lines, clines map[int]bool
		tests         map[string]bool
		events        map[[2]string]bool
		sample, via   string
	}
	byKey := map[SiteKey]*agg{}
	for _, sg := range all {
		if sg.kind != kind {
			continue
		}
		a, ok := byKey[sg.key]
		if !ok {
			a = &agg{lines: map[int]bool{}, clines: map[int]bool{}, tests: map[string]bool{}, events: map[[2]string]bool{}}
			byKey[sg.key] = a
		}
		a.lines[sg.line] = true
		if sg.callerLine > 0 {
			a.clines[sg.callerLine] = true
		}
		a.tests[sg.test] = true
		a.events[[2]string{strconv.Itoa(sg.line), sg.test}] = true
		if a.sample == "" || (a.sample == "?" && sg.test != "?") {
			a.sample = sg.test
		}
		if a.via == "" {
			a.via = sg.via
		}
	}
	out := make([]SiteSummary, 0, len(byKey))
	for k, a := range byKey {
		out = append(out, SiteSummary{
			Key: k, Lines: sortedInts(a.lines), CallerLines: sortedInts(a.clines),
			Tests: len(a.tests), TestNames: sortedKeys(a.tests), Events: len(a.events),
			SampleTest: a.sample, SampleVia: a.via,
		})
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// relicViewPrefix marks a relic read's key: "relic:work" is a work-view read
// under beads.RelicRead.
const relicViewPrefix = "relic:"

func lessKey(a, b SiteKey) bool {
	for _, pair := range [][2]string{{a.File, b.File}, {a.Func, b.Func}, {a.Op, b.Op}, {a.View, b.View}, {a.Class, b.Class}, {a.Caller, b.Caller}} {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

// readSightingFiles reads every events file in dir (this process's and any
// re-executed helper's).
func readSightingFiles(dir string) ([]sighting, error) {
	paths, err := filepath.Glob(filepath.Join(dir, eventsFilePattern))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []sighting
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if sg, ok := parseSighting(scanner.Text()); ok {
				out = append(out, sg)
			}
		}
		_ = f.Close()
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// violationLog collects one strict leaf's split violations for its test's
// cleanup check, the read-side twin of residenceLog.
type violationLog struct {
	mu         sync.Mutex
	violations []SplitViolation
}

func (l *violationLog) record(v SplitViolation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.violations = append(l.violations, v)
}

func (l *violationLog) take() []SplitViolation {
	l.mu.Lock()
	defer l.mu.Unlock()
	taken := l.violations
	l.violations = nil
	return taken
}

// unclaimedSplitViolationsMessage renders a strict leaf's cleanup failure, or ""
// when the fixture left nothing behind.
func unclaimedSplitViolationsMessage(store string, violations []SplitViolation) string {
	if len(violations) == 0 {
		return ""
	}
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  - "+v.String())
	}
	return fmt.Sprintf("%d split violation(s) on the strict %s store:\n%s\n"+
		"On a split city each class lives in its own store, so these reads would come back empty and these writes would land where no reader of their class looks. "+
		"Route the call to the store that owns the class, wrap a deliberate federation leg or residence probe in beads.FederatedRead, "+
		"or call splittest.TakeSplitViolations if asserting the misroute is the point of this test.",
		len(violations), store, strings.Join(lines, "\n"))
}
