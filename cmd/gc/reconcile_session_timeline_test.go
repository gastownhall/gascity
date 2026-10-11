package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
)

// The timeline differential (ARCH-RESTRUCTURE R6.1). The session
// differential compares legacy and v2 at one frozen instant, which hid every
// rule that reads the clock (the idle latch, detached_at, the stability
// clears). A timeline fixture is a parity fixture plus steps, each on a
// fixed schedule: tick through the step's clock advance at patrol cadence,
// apply one outside event to both copies, then tick a fixed number of
// patrols more. A tick is one legacy controller tick (its real phases, with
// the immediate follow-up ticks it asks for) and one v2 pass (with K1's
// steps). The schedule depends on neither copy, so legacy's golden cannot
// move with v2; each copy must be quiet at its step's last tick. Legacy's
// drain tracker and async stops, the simulator's caches and both providers
// carry across steps, as a controller's do across ticks.
//
// After every tick the copies compare on every aspect of their outcome:
// each row's and work bead's status, type, title, labels and metadata (both
// legs, config hashes raw: one city path serves both copies), each runtime
// and its meta, and the tick's provider calls and events with their payload
// values. Times are offsets from parityNow; a time outside the fixture's
// window is the wall clock (<wall>). A difference is accounted as in the
// session differential, but per step: every Explain key reads "@<step>
// <aspect>", and one no tick of its step shows fails. The values of the
// explained differences at each step's end are pinned too
// (testdata/session_differential/timeline_explained.golden), so an explained
// aspect whose v2 value moves fails as well.
//
// Every fixture seeds a census-only row on the rig leg (C5D2-1), keeps
// titles, session names and aliases distinct (E3B-S1), and seeds
// last_woke_at (C5D2-2).
//
// The legacy golden (owner ruling O7): what legacy changed in each step,
// its clock advance's ticks then (after "~ op") the ticks after the step's
// own outside write, is checked in as
// testdata/session_differential/timeline.golden; config hashes show as
// <changed> and core_hash_breakdown per field, with Env masked. The test
// fails when legacy differs from it; `go test -run
// TestSessionTimelineDifferential -update-golden` rewrites both goldens, and
// the test then fails until timelineGoldenRulings admits the fixture's new
// section, by digest, under a ruling (timelineRulingRefs, a "§12.2 row N",
// or a merged fix, "fix #NNNN (mc-xxxx)").

var updateTimelineGolden = flag.Bool("update-golden", false, "rewrite the timeline differential's goldens from the copies' outcomes")

const (
	timelineGoldenPath    = "testdata/session_differential/timeline.golden"
	timelineExplainedPath = "testdata/session_differential/timeline_explained.golden"
	// timelineSettleTicks is a step's ticks after its outside write.
	timelineSettleTicks = 3
)

// timelineFixture is a parity fixture run as a sequence of steps after the
// seed step. Setup, when set, prepares each copy before the seed.
type timelineFixture struct {
	parityFixture
	Setup func(tw twin)
	Steps []parityStep
}

// steps is f's steps after the seed step.
func (f timelineFixture) steps() []parityStep {
	if len(f.Steps) > 0 && f.Steps[0].Name == "seed" {
		return f.Steps
	}
	return append([]parityStep{{Name: "seed"}}, f.Steps...)
}

// parityStep ticks both copies through a clock advance of Advance, or to At
// after parityNow, at patrol cadence (the last tick lands on the target),
// applies Op to each copy, then ticks Ticks patrols more (timelineSettleTicks
// when 0, none when negative), the first at once after an Op. Each copy must
// be quiet at the step's last tick unless Restless. A fixture's first step
// named "seed" replaces the default seed step.
type parityStep struct {
	Name     string
	Advance  time.Duration
	At       time.Duration
	Ticks    int
	Restless bool
	Op       func(tw twin)
}

// settleTicks is st's ticks after its Op.
func (st parityStep) settleTicks() int {
	switch {
	case st.Ticks < 0:
		return 0
	case st.Ticks == 0:
		return timelineSettleTicks
	}
	return st.Ticks
}

// schedule is st's tick times from at: its advance's, ending on its target,
// then its settle ticks', the first at the advance's end when st writes or
// ticked nothing yet. It is all the clock a step reads.
func (st parityStep) schedule(at time.Time) (advance, settle []time.Time) {
	target := at.Add(st.Advance)
	if st.At > 0 {
		target = parityNow.Add(st.At)
	}
	for now := at; now.Before(target); {
		now = minTime(now.Add(simPatrol), target)
		advance = append(advance, now)
	}
	now := target
	for i := range st.settleTicks() {
		if i > 0 || st.Op == nil && len(advance) > 0 {
			now = now.Add(simPatrol)
		}
		settle = append(settle, now)
	}
	return advance, settle
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// twin is one copy as an outside writer sees it: its config and reload,
// provider, stores (the city leg, then the rig leg) and recorder.
type twin struct {
	t      *testing.T
	cfg    *config.City
	reload func(*config.City)
	sp     *simProvider
	stores []beads.Store
	rec    events.Recorder
	now    time.Time
	city   string // the copy's city path: an outside writer's Manager takes the runtime lease there
}

// row finds id on either leg.
func (tw twin) row(id string) (beads.Store, beads.Bead) {
	for _, st := range tw.stores {
		if b, err := st.Get(id); err == nil {
			return st, b
		}
	}
	tw.t.Fatalf("timeline: no row %s", id)
	return nil, beads.Bead{}
}

// runtime changes the runtime under row id's session name; a nil result
// removes it.
func (tw twin) runtime(id string, fn func(rt simRuntime) *simRuntime) {
	_, b := tw.row(id)
	name := b.Metadata["session_name"]
	tw.sp.mu.Lock()
	defer tw.sp.mu.Unlock()
	rt := tw.sp.rts[name]
	if rt == nil {
		tw.t.Fatalf("timeline: no runtime under %s", name)
	}
	if next := fn(*rt); next == nil {
		tw.sp.drop(name)
	} else {
		*rt = *next
		tw.sp.touch(name)
	}
}

// twin is copy A's view; a reload swaps the config its next tick reads.
func (w *legacyWorld) twin(t *testing.T) twin {
	stores := []beads.Store{w.store}
	for _, rig := range slices.Sorted(maps.Keys(w.rigs)) {
		stores = append(stores, w.rigs[rig])
	}
	return twin{t: t, cfg: w.cfg, reload: func(c *config.City) { w.cfg = c }, sp: w.sp.simProvider, stores: stores, rec: w.rec, now: w.clk.Now(), city: w.cityPath}
}

// twin is copy B's view; it writes the backings, and a reload publishes a
// new environment generation.
func (w *v2World) twin(t *testing.T) twin {
	var stores []beads.Store
	for _, l := range w.s.legs {
		stores = append(stores, l.backing)
	}
	reload := func(c *config.City) {
		env := w.s.env.Env()
		next := *env
		next.Gen, next.Cfg = env.Gen+1, c
		*env, w.s.cfg = next, c
	}
	return twin{t: t, cfg: w.s.cfg, reload: reload, sp: w.s.sp, stores: stores, rec: w.rec, now: w.s.clk.Now(), city: w.s.env.CityPath}
}

// delivered queues the outside writes' events and delivers each to its
// leg's cache, as a quiet controller's event stream would.
func (w *v2World) delivered() {
	w.s.audit("timeline")
	for _, l := range w.s.legs {
		for _, ev := range l.events {
			l.cache.ApplyEvent("bead.updated", ev)
		}
		l.events = nil
		l.cache.ReconcileNowForTest()
	}
}

// runtimesOf is each runtime by name, its identity and shape.
func runtimesOf(p *simProvider) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for name, rt := range p.rts {
		shape := "alive"
		switch {
		case rt.corpse:
			shape = "corpse"
		case rt.zombie:
			shape = "zombie"
		}
		token := rt.token
		if token != "" && !strings.HasPrefix(token, "tok-") {
			token = "<set>"
		}
		out[name] = fmt.Sprintf("id=%s epoch=%s token=%s %s attached=%t", rt.id, rt.epoch, token, shape, rt.attached)
	}
	return out
}

// runtimeMeta is each runtime name's meta, replayed from the provider's
// recorded SetMeta and RemoveMeta calls.
func runtimeMeta(p *simProvider) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range p.SnapshotCalls() {
		switch c.Method {
		case "SetMeta":
			if out[c.Name] == nil {
				out[c.Name] = map[string]string{}
			}
			out[c.Name][c.Key] = c.Value
		case "RemoveMeta":
			delete(out[c.Name], c.Key)
		}
	}
	return out
}

var timelineTimes = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)

// normalizer renders a value for comparison and the goldens: paths elided,
// and times inside the fixture's window [lo, hi] as offsets from parityNow,
// others as <wall> (the wall clock: the window ends an hour past the
// fixture's schedule, and the wall clock is days past parityNow).
type normalizer struct {
	paths  *strings.Replacer
	lo, hi time.Time
}

func (n normalizer) value(v string) string {
	return timelineTimes.ReplaceAllStringFunc(n.paths.Replace(v), func(s string) string {
		at, err := time.Parse(time.RFC3339Nano, s)
		d := at.Sub(parityNow)
		switch {
		case err != nil || at.Before(n.lo) || at.After(n.hi):
			return "<wall>"
		case d < 0:
			return "T-" + (-d).String()
		}
		return "T+" + d.String()
	})
}

// view flattens o, its calls and events counted from prev on, into
// aspect → value: each row's and work bead's presence, status, type, title,
// labels, assignee and metadata keys (core_hash_breakdown per field), each
// runtime and its meta, and each call and event.
func (n normalizer) view(o, prev parityOutcome, sp *simProvider) map[string]string {
	out := map[string]string{}
	bead := func(kind string, b beads.Bead) {
		p := kind + ":" + b.ID
		out[p], out[p+":status"], out[p+":type"], out[p+":title"] = "present", b.Status, b.Type, b.Title
		if len(b.Labels) > 0 {
			out[p+":labels"] = strings.Join(slices.Sorted(slices.Values(b.Labels)), ",")
		}
		if b.Assignee != "" {
			out[p+":assignee"] = n.value(b.Assignee)
		}
		for k, v := range b.Metadata {
			var hb struct {
				Version string            `json:"version"`
				Fields  map[string]string `json:"fields"`
			}
			switch {
			case v == "":
			case k == "core_hash_breakdown" && json.Unmarshal([]byte(v), &hb) == nil:
				out[p+":"+k+".version"] = hb.Version
				for f, h := range hb.Fields {
					out[p+":"+k+"."+f] = h
				}
			default:
				out[p+":"+k] = n.value(v)
			}
		}
	}
	for _, b := range o.rows {
		bead("row", b)
	}
	for _, b := range o.work {
		bead("work", b)
	}
	for name, rt := range runtimesOf(sp) {
		out["runtime:"+name] = rt
	}
	for name, meta := range runtimeMeta(sp) {
		for k, v := range meta {
			out["runtime:"+name+":meta:"+k] = n.value(v)
		}
	}
	for a, c := range o.actions {
		if d := c - prev.actions[a]; d != 0 {
			out["provider:"+a] = fmt.Sprint(d)
		}
	}
	for e, c := range o.payloads {
		if d := c - prev.payloads[e]; d != 0 {
			out["event:"+n.value(e)] = fmt.Sprint(d)
		}
	}
	return out
}

// goldenShown renders an aspect's value for the goldens: a config hash, which
// digests the paths, as <changed>, and core_hash_breakdown's Env field
// masked; the other breakdown fields as they are.
func goldenShown(aspect, v string) string {
	key := aspect[strings.LastIndex(aspect, ":")+1:]
	if v != "" && (strings.HasSuffix(key, "_hash") || key == "core_hash_breakdown.Env") {
		return "<changed>"
	}
	return v
}

// counted reports whether aspect is a step's call or event, not state.
func counted(aspect string) bool {
	return strings.HasPrefix(aspect, "provider:") || strings.HasPrefix(aspect, "event:")
}

// goldenLines renders what changed from prev to cur: each state aspect that
// differs ("aspect=" clears it), then each call and event.
func goldenLines(cur, prev map[string]string) string {
	var b strings.Builder
	for _, a := range unionKeys(cur, prev) {
		if counted(a) {
			if cur[a] != "" {
				fmt.Fprintf(&b, "%s=%s\n", a, cur[a])
			}
		} else if cur[a] != prev[a] {
			fmt.Fprintf(&b, "%s=%s\n", a, goldenShown(a, cur[a]))
		}
	}
	return b.String()
}

// timelineRun is one fixture's run: the copies, the normalizer, and the
// accounting carried across ticks.
type timelineRun struct {
	t      *testing.T
	f      timelineFixture
	lw     *legacyWorld
	vw     *v2World
	n      normalizer
	quiet  bool            // report nothing: a probe of the legacy golden alone
	shown  map[string]bool // the Explain keys a tick showed
	diffs  map[string]string
	lprev  parityOutcome // each copy's outcome at its last tick, for the next one's calls and events
	vprev  parityOutcome
	failed bool
}

func (r *timelineRun) errorf(format string, args ...any) {
	r.failed = true
	if !r.quiet {
		r.t.Errorf(format, args...)
	}
}

// tick runs one tick of each copy at now, accounts every difference
// between them under step's Explain keys, and returns legacy's view.
func (r *timelineRun) tick(step string, now time.Time) (lchanged, vchanged bool, legacy map[string]string) {
	r.lw.clk.Time = now
	r.vw.s.advance(now.Sub(r.vw.s.clk.Now()))
	lchanged, vchanged = r.lw.once(r.t), r.vw.once(r.t)
	lout, vout := r.lw.outcome(r.t), r.vw.outcome(r.t)
	l, v := r.n.view(lout, r.lprev, r.lw.sp.simProvider), r.n.view(vout, r.vprev, r.vw.s.sp)
	r.lprev, r.vprev = lout, vout
	r.diffs = map[string]string{}
	for _, aspect := range unionKeys(l, v) {
		if l[aspect] == v[aspect] {
			continue
		}
		key, ok := r.f.explains("@" + step + " " + aspect)
		if !ok {
			r.errorf("%s, step %s at +%s: unexplained difference %s (legacy %q, v2 %q)", r.f.Name, step, now.Sub(parityNow), aspect, l[aspect], v[aspect])
			continue
		}
		r.shown[key] = true
		r.diffs[aspect] = fmt.Sprintf("legacy=%q v2=%q [%s]", goldenShown(aspect, l[aspect]), goldenShown(aspect, v[aspect]), r.f.Explain[key])
		if _, _, ok := parityEntryOf(r.f.Explain[key]); !ok {
			r.errorf("%s: %s accounted to %q, which no table lists", r.f.Name, aspect, r.f.Explain[key])
		}
	}
	return lchanged, vchanged, l
}

// runTimeline runs f's steps on both copies, accounts every difference
// after every tick, and returns legacy's golden section and the explained
// differences' section. arms, when set, is a v2-only mutant; quiet reports
// nothing.
func runTimeline(t *testing.T, f timelineFixture, arms func([]rowArm) []rowArm, quiet bool) (legacy, explained string) {
	t.Helper()
	for key := range f.Explain {
		if !strings.HasPrefix(key, "@") && !quiet {
			t.Errorf("%s: Explain key %q names no step (\"@<step> <aspect>\")", f.Name, key)
		}
	}
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", simRigLeg)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := f.converged(t, cityPath)
	lw := newLegacyWorld(t, f.parityFixture, cityPath, cloneDiffBeads(rows))
	lw.cfg.Rigs = []config.Rig{{Name: simRigLeg, Path: rigPath}}
	lw.rigs = map[string]beads.Store{simRigLeg: beads.NewMemStoreFrom(0, cloneDiffBeads(f.Rig), nil)}
	lw.pokes = countDrainAckPokes(t, cityPath)
	vw := newV2World(t, f.parityFixture, cityPath, rows, append([]beads.Bead{}, f.Rig...), rigPath, arms)
	if f.Setup != nil {
		f.Setup(lw.twin(t))
		f.Setup(vw.twin(t))
		vw.delivered()
	}
	end := parityNow
	for _, st := range f.steps() {
		advance, settle := st.schedule(end)
		if ticks := slices.Concat(advance, settle); len(ticks) > 0 {
			end = ticks[len(ticks)-1]
		}
	}
	r := &timelineRun{
		t: t, f: f, lw: lw, vw: vw, quiet: quiet, shown: map[string]bool{},
		n: normalizer{paths: strings.NewReplacer(rigPath, "<rig>", cityPath, "<city>"), lo: parityNow.Add(-30 * 24 * time.Hour), hi: end.Add(time.Hour)},
	}
	// Both goldens show each tick that changed something, under "@ +T":
	// legacy's golden what legacy changed in it (the seed's first tick from
	// the fixture's rows, its session-bead sync's convergence included; a
	// tick after an outside write from that write), the explained golden
	// each explained difference that appeared, moved or resolved.
	raw := newParityOutcome(cloneDiffBeads(append(f.beads(), f.Rig...)), lw.sp, &memRecorder{})
	state := r.n.view(raw, raw, lw.sp.simProvider)
	r.lprev, r.vprev = lw.outcome(t), vw.outcome(t)
	var lsec, xsec strings.Builder
	pinned := map[string]string{}
	now := parityNow
	for i, st := range f.steps() {
		advance, settle := st.schedule(now)
		var ltick, xtick strings.Builder
		var lchanged, vchanged bool
		tick := func(at time.Time) {
			var l map[string]string
			lchanged, vchanged, l = r.tick(st.Name, at)
			if lines := goldenLines(l, state); lines != "" {
				fmt.Fprintf(&ltick, "@ +%s\n%s", at.Sub(parityNow), lines)
			}
			var x strings.Builder
			for _, aspect := range unionKeys(r.diffs, pinned) {
				if r.diffs[aspect] == "" {
					fmt.Fprintf(&x, "%s resolved\n", aspect)
				} else if r.diffs[aspect] != pinned[aspect] {
					fmt.Fprintf(&x, "%s %s\n", aspect, r.diffs[aspect])
				}
			}
			if x.Len() > 0 {
				fmt.Fprintf(&xtick, "@ +%s\n%s", at.Sub(parityNow), x.String())
			}
			state, pinned, now = l, r.diffs, at
		}
		for _, at := range advance {
			tick(at)
		}
		if st.Op != nil {
			st.Op(lw.twin(t))
			st.Op(vw.twin(t))
			vw.delivered()
			r.lprev, r.vprev = lw.outcome(t), vw.outcome(t)
			state = r.n.view(r.lprev, r.lprev, lw.sp.simProvider)
			ltick.WriteString("~ op\n")
		}
		for _, at := range settle {
			tick(at)
		}
		if !st.Restless && (lchanged || vchanged) {
			r.errorf("%s, step %s: still acting at its last tick (+%s): legacy %t, v2 %t; give it more Ticks", f.Name, st.Name, now.Sub(parityNow), lchanged, vchanged)
		}
		header := fmt.Sprintf("## %d %s (+%s)\n", i, st.Name, now.Sub(parityNow))
		lsec.WriteString(header + ltick.String())
		xsec.WriteString(header + xtick.String())
	}
	for key, id := range f.Explain {
		if !r.shown[key] {
			r.errorf("%s: %s, accounted to %s, shows at no tick: legacy and v2 agree", f.Name, key, id)
		}
	}
	return lsec.String(), xsec.String()
}

// countDrainAckPokes counts the drain-ack async stops' completion pokes
// from cityPath's legacy copy, the controller's cue for a follow-up tick.
func countDrainAckPokes(t *testing.T, cityPath string) *atomic.Int64 {
	pokes := &atomic.Int64{}
	prev := drainAckAsyncStopPokeController
	drainAckAsyncStopPokeController = func(city string, _ reconcilekey.Key) error {
		if city == cityPath {
			pokes.Add(1)
		}
		return nil
	}
	t.Cleanup(func() { drainAckAsyncStopPokeController = prev })
	return pokes
}

// goldenRuling admits one golden section: the fixture, the ruling that
// admits it, and the section's digest.
type goldenRuling struct{ fixture, ruling, digest string }

// timelineGoldenBaseline admits legacy's outcomes as this PR found them; it
// may only be a fixture's first entry.
const timelineGoldenBaseline = "O7 baseline: legacy at 72115d84b4 (TL)"

// timelineRulingRefs are the rulings a later entry may name: the owner's
// O-rulings (O7 itself admits no change), CONTRACT §12.2 rows in the form
// "§12.2 row N" (timelineSection122), and a legacy behavior change a
// reviewed, merged PR made on purpose, named with its bead as "fix #NNNN
// (mc-xxxx)" (timelineMergedFix).
var (
	timelineRulingRefs = []string{"O1", "O2", "O3", "O4", "O5", "O6", "O8", "O9", "O10"}
	timelineSection122 = regexp.MustCompile(`§12\.2 row \d+\b`)
	timelineMergedFix  = regexp.MustCompile(`\bfix #[1-9]\d* \(mc-[a-z0-9]{4,}\)`)
)

// timelineGoldenRulings admit each fixture's golden section. A change to
// legacy's outcome on a fixture appends an entry naming its ruling and the
// new digest; the last entry per fixture is the one in force.
var timelineGoldenRulings = []goldenRuling{
	{"a crash 29s after wake", timelineGoldenBaseline, "55c4df6af9c5"},
	{"a crash 31s after wake", timelineGoldenBaseline, "b799cd2aa091"},
	{"a crash 5m0s after wake", timelineGoldenBaseline, "21983abfeb7d"},
	{"a live quarantined row 29s after wake", timelineGoldenBaseline, "f559e3fcf255"},
	{"a live quarantined row 31s after wake", timelineGoldenBaseline, "25811fbce399"},
	{"a managed suspend drains, then wake", timelineGoldenBaseline, "48df08bf8396"},
	{"a managed suspend drains, then wake", "fix #7417 (mc-esqo7)", "61f2ce80feb0"},
	{"a provider swap stops the runtime", timelineGoldenBaseline, "68a5d9d9b0ef"},
	{"a quarantine, then a kill fence, expires", timelineGoldenBaseline, "e6f3872c765e"},
	{"a reload turns idle sleep on for a detached row", timelineGoldenBaseline, "787037012e13"},
	{"an idle on-demand named session sleeps", timelineGoldenBaseline, "2752bd8f4cc4"},
	{"an unwanted row inside, then past, the wake grace", timelineGoldenBaseline, "c4d3c5eb08a0"},
	{"an unwanted row of a suspended agent", timelineGoldenBaseline, "90405aa55ba7"},
	{"an unwanted row woken by a clock ten minutes ahead", timelineGoldenBaseline, "27e15af2efe9"},
	{"an unwanted row woken by a clock two minutes ahead", timelineGoldenBaseline, "7863a5af3820"},
	{"attach, then detach, then idle", timelineGoldenBaseline, "a8a76fb85cf8"},
	{"idle, then sleep, then the next pass", timelineGoldenBaseline, "df4154567a92"},
	{"kill a seat holding a dep-blocked claim", timelineGoldenBaseline, "44f501ec9412"},
	{"kill a seat holding a ready claim", timelineGoldenBaseline, "a6a3283ef592"},
	{"suspend, then wake", timelineGoldenBaseline, "14a789f280e4"},
	{"suspend, then wake", "fix #7417 (mc-esqo7)", "ff88e0c7b075"},
}

func goldenDigest(section string) string {
	sum := sha256.Sum256([]byte(section))
	return hex.EncodeToString(sum[:6])
}

// namesRuling reports whether a later ruling names one of the fixed refs.
func namesRuling(ruling string) bool {
	for _, w := range strings.FieldsFunc(ruling, func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == '(' || r == ')' }) {
		if slices.Contains(timelineRulingRefs, w) {
			return true
		}
	}
	return timelineSection122.MatchString(ruling) || timelineMergedFix.MatchString(ruling)
}

// unruledGolden lists the sections no ruling admits and the rulings out of
// form: one for no fixture, a baseline that is not its fixture's first
// entry, or a later entry that names no ruling.
func unruledGolden(sections map[string]string, rulings []goldenRuling) []string {
	var out []string
	inForce := map[string]goldenRuling{}
	for _, r := range rulings {
		_, first := inForce[r.fixture]
		first = !first
		switch {
		case sections[r.fixture] == "":
			out = append(out, fmt.Sprintf("ruling %q names no fixture %q", r.ruling, r.fixture))
		case first && r.ruling != timelineGoldenBaseline:
			out = append(out, fmt.Sprintf("%q's first ruling %q is not the baseline", r.fixture, r.ruling))
		case !first && (r.ruling == timelineGoldenBaseline || !namesRuling(r.ruling)):
			out = append(out, fmt.Sprintf("ruling %q for %q names none of %v, a \"§12.2 row N\" nor a \"fix #NNNN (mc-xxxx)\"", r.ruling, r.fixture, timelineRulingRefs))
		}
		inForce[r.fixture] = r
	}
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		if d := goldenDigest(sections[name]); inForce[name].digest != d {
			out = append(out, fmt.Sprintf("golden section %q (digest %s) is admitted by no ruling: append {%q, \"<ruling>\", %q}", name, d, name, d))
		}
	}
	return out
}

// parseGolden splits a golden file into sections by fixture.
func parseGolden(text string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(text, "\n=== ")[1:] {
		name, body, _ := strings.Cut(part, "\n")
		out[name] = strings.TrimSuffix(body, "\n") + "\n"
	}
	return out
}

func renderGolden(header string, sections map[string]string) string {
	var b strings.Builder
	b.WriteString(header)
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		fmt.Fprintf(&b, "\n=== %s\n%s", name, strings.TrimSuffix(sections[name], "\n"))
	}
	b.WriteString("\n")
	return b.String()
}

// checkGolden compares the sections a run produced with path's, rewriting
// them under -update-golden; sections a -run filter skipped are kept, and a
// section for no fixture fails (or, updating, goes). It returns the file's
// sections.
func checkGolden(t *testing.T, path, header string, got map[string]string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !*updateTimelineGolden {
		t.Fatal(err)
	}
	want := parseGolden(string(raw))
	names := map[string]bool{}
	for _, f := range timelineFixtures() {
		names[f.Name] = true
	}
	if *updateTimelineGolden {
		next := map[string]string{}
		for name := range names {
			if section, ok := got[name]; ok {
				next[name] = section
			} else if section, ok := want[name]; ok {
				next[name] = section
			}
		}
		if err := os.WriteFile(path, []byte(renderGolden(header, next)), 0o644); err != nil {
			t.Fatal(err)
		}
		want = next
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !names[name] {
			t.Errorf("%s: section %q is for no fixture: rerun with -update-golden to drop it", path, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if got[name] != want[name] {
			t.Errorf("%s: %q differs: if a ruling explains it, rerun with -update-golden (and for legacy, add the ruling to timelineGoldenRulings)\n--- golden\n%s\n--- got\n%s", path, name, want[name], got[name])
		}
	}
	return want
}

const (
	timelineLegacyHeader = "# What legacy changed in each step of each timeline fixture (reconcile_session_timeline_test.go).\n" +
		"# Owner ruling O7: a change here needs an entry in timelineGoldenRulings. Regenerate with -update-golden.\n"
	timelineExplainedHeader = "# The explained legacy/v2 differences at each step's end, with their values.\n" +
		"# Regenerate with -update-golden when an arm's port moves them.\n"
)

// Kills a clock-dependent divergence between legacy and v2 that a frozen
// instant cannot show, a moved v2 value under an explained difference, and
// a silent change to legacy's timeline behavior: every difference after
// every tick is accounted and its values at each step's end pinned,
// legacy's changes equal the golden, and a ruling admits every golden
// section.
func TestSessionTimelineDifferential(t *testing.T) {
	legacy, explained := map[string]string{}, map[string]string{}
	for _, f := range timelineFixtures() {
		t.Run(f.Name, func(t *testing.T) { legacy[f.Name], explained[f.Name] = runTimeline(t, f, nil, false) })
	}
	want := checkGolden(t, timelineGoldenPath, timelineLegacyHeader, legacy)
	checkGolden(t, timelineExplainedPath, timelineExplainedHeader, explained)
	for _, msg := range unruledGolden(want, timelineGoldenRulings) {
		t.Error(msg)
	}
}

// Kills a legacy golden that moves with v2: every fixture's legacy section
// is byte-identical with v2's row arms all removed.
func TestTimelineLegacyGoldenIgnoresV2(t *testing.T) {
	for _, f := range timelineFixtures() {
		t.Run(f.Name, func(t *testing.T) {
			want, _ := runTimeline(t, f, nil, true)
			got, _ := runTimeline(t, f, func([]rowArm) []rowArm { return nil }, true)
			if got != want {
				t.Errorf("legacy's section moved with a v2-only mutant\n--- v2 intact\n%s\n--- v2 armless\n%s", want, got)
			}
		})
	}
}

// Kills a golden that changes with no ruling, or under a ruling out of form:
// the baseline only as a fixture's first entry, a later entry naming a fixed
// ruling or a §12.2 row, and the last entry carrying the section's digest.
func TestTimelineGoldenNeedsARuling(t *testing.T) {
	sections := map[string]string{"a": "## 0 seed\nrow:gc-1=present\n", "b": "## 0 seed\n"}
	da, db := goldenDigest(sections["a"]), goldenDigest(sections["b"])
	base := timelineGoldenBaseline
	for _, tc := range []struct {
		name    string
		rulings []goldenRuling
		want    int
	}{
		{"every section ruled", []goldenRuling{{"a", base, da}, {"b", base, db}}, 0},
		{"a section unruled", []goldenRuling{{"a", base, da}}, 1},
		{"a changed section under its old ruling", []goldenRuling{{"a", base, "000000000000"}, {"b", base, db}}, 1},
		{"a later O-ruling supersedes", []goldenRuling{{"a", base, "000000000000"}, {"a", "O4 (polarity)", da}, {"b", base, db}}, 0},
		{"a later §12.2 row supersedes", []goldenRuling{{"a", base, "000000000000"}, {"a", "CONTRACT §12.2 row 25", da}, {"b", base, db}}, 0},
		{"a later merged fix supersedes", []goldenRuling{{"a", base, "000000000000"}, {"a", "fix #7417 (mc-esqo7)", da}, {"b", base, db}}, 0},
		{"a fix with no bead", []goldenRuling{{"a", base, "000000000000"}, {"a", "fix #7417", da}, {"b", base, db}}, 1},
		{"a fix with no PR number", []goldenRuling{{"a", base, "000000000000"}, {"a", "fix 7417 (mc-esqo7)", da}, {"b", base, db}}, 1},
		{"a fix with a malformed bead", []goldenRuling{{"a", base, "000000000000"}, {"a", "fix #7417 (esqo7)", da}, {"b", base, db}}, 1},
		{"a fix as the first entry", []goldenRuling{{"a", "fix #7417 (mc-esqo7)", da}, {"b", base, db}}, 1},
		{"a later entry naming O7", []goldenRuling{{"a", base, "000000000000"}, {"a", "O7", da}, {"b", base, db}}, 1},
		{"a later baseline", []goldenRuling{{"a", base, "000000000000"}, {"a", base, da}, {"b", base, db}}, 1},
		{"a later entry naming nothing", []goldenRuling{{"a", base, "000000000000"}, {"a", "looks fine", da}, {"b", base, db}}, 1},
		{"a first entry that is not the baseline", []goldenRuling{{"a", "O4", da}, {"b", base, db}}, 1},
		{"a ruling for no fixture", []goldenRuling{{"a", base, da}, {"b", base, db}, {"c", base, db}}, 1},
	} {
		if got := unruledGolden(sections, tc.rulings); len(got) != tc.want {
			t.Errorf("%s: %d complaints %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
	if got := parseGolden(renderGolden("# h\n", sections)); !maps.Equal(got, sections) {
		t.Errorf("golden round trip: %v, want %v", got, sections)
	}
}

// timelineFixtures are the clock-dependent families of ARCH-RESTRUCTURE
// §3.6, each group's own: the stability clears and accrual, then the idle
// and drain timers, then the operator verbs.
func timelineFixtures() []timelineFixture {
	return slices.Concat(stabilityTimeline(), idleTimeline(), operatorTimeline())
}

func tlAt(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }

// tlRow is an awake pool row woken woke ago, whose title, session name and alias
// differ; the transient pool manages no alias, so legacy clears it.
func tlRow(id string, slot int, woke time.Duration, meta ...string) parityRow {
	b := poolRow(id, "worker", slot, "awake", append([]string{
		"instance_token", "tok-" + id, "last_woke_at", tlAt(-woke), "alias", "alias-" + id,
	}, meta...)...)
	b.Title = "Display " + id
	return b
}

func tlLive(id string) simRuntime { return simRuntime{id: id, epoch: "1", token: "tok-" + id} }

func tlClaim(status, assignee string) []parityRow {
	return []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: status, Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}}
}

// tlCity names its tmux sessions apart from its agents.
func tlCity() *config.City {
	cfg := workerCity(3)
	cfg.Workspace.SessionTemplate = "{{.City}}--{{.Agent}}"
	return cfg
}

// tlFixture builds a timeline fixture over gc-1's city leg and the
// census-only rig-leg row rg-1 every fixture seeds. Its Explain is
// standing, scoped to every step, plus per, scoped to the step it is keyed
// by.
func tlFixture(name string, cfg func() *config.City, rows, work []parityRow, rts []simRuntime, steps []parityStep, standing map[string]string, per map[string]map[string]string) timelineFixture {
	explain := map[string]string{}
	for _, st := range (timelineFixture{Steps: steps}).steps() {
		for k, v := range standing {
			explain["@"+st.Name+" "+k] = v
		}
		for k, v := range per[st.Name] {
			explain["@"+st.Name+" "+k] = v
		}
	}
	rig := []parityRow{tlRow("rg-1", 11, time.Hour)}
	return timelineFixture{parityFixture: parityFixture{Name: name, City: cfg, Rows: rows, Work: work, Rig: rig, Runtimes: append(rts, tlLive("rg-1")), Explain: explain}, Steps: steps}
}

// tlMerged2 is per with step's map added.
func tlMerged2(per map[string]map[string]string, step string, m map[string]string) map[string]map[string]string {
	out := maps.Clone(per)
	out[step] = m
	return out
}

func tlMerged(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

// The standing differences: legacy's sync converges row id's sleep-policy
// keys and identity.
func tlPol(id string) map[string]string {
	return map[string]string{"row:" + id + ":*policy": "sleep-policy-keys"}
}

func tlIdent() map[string]string { return map[string]string{"row:gc-1:*identity": "A7 identity"} }

// tlStartState is the row and runtime legacy's start of row id under name
// leaves, and tlStarted that start in its own step; v2 leaves both to A18.
func tlStartState(id, name string) map[string]string {
	return map[string]string{"row:" + id + ":*start": "A18 start", "runtime:" + name: "A18 start"}
}

func tlStarted(id, name, subject string) map[string]string {
	return tlMerged(tlStartState(id, name), map[string]string{"provider:Start " + name: "A18 start", "provider:RemoveMeta " + name: "A18 start", "event:session.woke " + subject + " null": "A18 start"})
}

func tlAccrued() map[string]string {
	return map[string]string{"row:gc-1:*accrual": "stability-accrual"}
}

// stabilityTimeline is SESS-537..540 through time, on a pool of one whose
// canonical alias "worker" legacy keeps and whose work is assigned by that
// alias.
func stabilityTimeline() []timelineFixture {
	stable := func() *config.City {
		cfg := tlCity()
		cfg.Agents[0].MaxActiveSessions, cfg.Agents[0].MinActiveSessions = intPtr(1), intPtr(0)
		return cfg
	}
	row := func(meta ...string) parityRow {
		b := tlRow("gc-1", 1, 0, meta...)
		b.Metadata["alias"], b.Metadata["agent_name"] = "worker", "worker"
		return b
	}
	standing := tlMerged(tlPol("gc-1"), tlIdent())
	// crashAfter is a crash d after the wake of a row with prior wake
	// failures and churn: inside the 30s stability threshold legacy counts a
	// wake failure and restarts; past it a death before the productivity
	// threshold is churn.
	crashAfter := func(d time.Duration, crashed map[string]string) timelineFixture {
		crash := func(tw twin) { tw.runtime("gc-1", func(simRuntime) *simRuntime { return nil }) }
		return tlFixture(fmt.Sprintf("a crash %s after wake", d), stable,
			[]parityRow{row("wake_attempts", "2", "churn_count", "2")}, tlClaim("in_progress", "worker"), []simRuntime{tlLive("gc-1")},
			[]parityStep{{Name: "crash", At: d, Op: crash, Ticks: 6}}, standing, map[string]map[string]string{"crash": crashed})
	}
	// quarantinedAt is a live, quarantined row ticked through to d after its
	// wake, then seen a tick past it: past the 30s stability threshold both
	// copies clear the wake failures and the quarantine (SESS-539).
	// Meanwhile its quarantine keeps it from waking for its work, so legacy
	// drains it each tick, and its work vetoes the drain.
	quarantinedAt := func(d time.Duration, per map[string]map[string]string) timelineFixture {
		return tlFixture(fmt.Sprintf("a live quarantined row %s after wake", d), stable,
			[]parityRow{row("wake_attempts", "2", "quarantined_until", tlAt(time.Hour))}, tlClaim("in_progress", "worker"), []simRuntime{tlLive("gc-1")},
			[]parityStep{{Name: "seed", Ticks: 1, Restless: true}, {Name: "alive", At: d, Ticks: -1, Restless: true}, {Name: "settled", Ticks: 6}}, standing, per)
	}
	// drained is legacy's drain of the quarantined row, begun and vetoed in
	// a tick and its follow-up; lagged, v2's current-bead stamp a pass after
	// the stability clear.
	drained := map[string]string{"provider:SetMeta s-gc-1": "A20 drain-begin", "provider:RemoveMeta s-gc-1": "A20 drain-begin"}
	lagged := map[string]string{"row:gc-1:currently_processing_bead_id": "§12.2#37 current-bead-lag"}
	restarted := tlStarted("gc-1", "s-gc-1", "worker")
	return []timelineFixture{
		crashAfter(29*time.Second, tlMerged(restarted, tlAccrued())),
		crashAfter(31*time.Second, tlMerged(tlAccrued(), map[string]string{"row:gc-1:last_woke_at": "stability-accrual"})),
		crashAfter(5*time.Minute, restarted),
		quarantinedAt(29*time.Second, map[string]map[string]string{
			"seed": drained, "alive": drained, "settled": tlMerged(drained, lagged),
		}),
		quarantinedAt(31*time.Second, map[string]map[string]string{
			"seed": drained, "alive": tlMerged(drained, lagged),
		}),
	}
}

// reconfigure reloads the copy with a changed copy of its config.
func (tw twin) reconfigure(change func(*config.City)) {
	next := *tw.cfg
	next.Agents = make([]config.Agent, len(tw.cfg.Agents))
	for i, a := range tw.cfg.Agents {
		next.Agents[i] = a.Clone()
	}
	next.NamedSessions, next.Rigs = slices.Clone(tw.cfg.NamedSessions), slices.Clone(tw.cfg.Rigs)
	change(&next)
	tw.reload(&next)
}

// tlAliasSeed adds legacy's seed-tick clear of gc-1's unmanaged alias on
// its live runtime to the identity difference.
func tlAliasSeed() map[string]string {
	return tlMerged(tlIdent(), map[string]string{"provider:SetMeta s-gc-1": "A7 identity", "provider:RemoveMeta s-gc-1": "A7 identity"})
}

// tlAliasMeta is the identity legacy's alias clear leaves on gc-1's runtime,
// which the fake keeps past the runtime.
func tlAliasMeta() map[string]string {
	return map[string]string{"runtime:s-gc-1:meta:GC_AGENT": "A7 identity", "runtime:s-gc-1:meta:BEADS_ACTOR": "A7 identity"}
}

// idleTimeline is the idle and drain timers: the idle drain and its latch,
// detached_at, and the INC-003 wake grace.
func idleTimeline() []timelineFixture {
	napping := func() *config.City {
		cfg := tlCity()
		cfg.Agents[0].SleepAfterIdle = "5m"
		return cfg
	}
	// chatNapping adds an always named session "chat" that idle-sleeps after
	// five minutes; named is its row, under the city's session name for it.
	chatNapping := func() *config.City {
		cfg := tlCity()
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true", SleepAfterIdle: "5m"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
		return cfg
	}
	chatName := agent.SessionNameFor("test-city", "chat", tlCity().Workspace.SessionTemplate)
	named := chatRow("gc-c", "1", "state", "awake", "session_name", chatName, "instance_token", "tok-gc-c", "last_woke_at", tlAt(-time.Minute), "alias", "chat")
	named.Title = "Display gc-c"
	// slept is legacy's idle sleep of gc-c, its runtime stopped, which v2
	// leaves to A20.
	slept := map[string]string{
		"row:gc-c:*idle": "A20 idle-sleep", "row:gc-c:state": "A20 idle-sleep", "row:gc-c:last_woke_at": "A20 idle-sleep", "runtime:" + chatName: "A20 idle-sleep",
	}
	// sleeping is slept with the drain's calls and event.
	sleeping := tlMerged(slept, map[string]string{
		"provider:SetMeta " + chatName: "A20 idle-sleep", "provider:RemoveMeta " + chatName: "A20 idle-sleep", "provider:Stop " + chatName: "A20 idle-sleep",
		`event:session.stopped chat {"reason":"drain acknowledged","session_id":"gc-c","template":"chat"}`: "A20 idle-sleep",
	})
	chatStanding := tlMerged(tlPol("gc-c"), map[string]string{"row:gc-c:*sync": "A7 row-metadata"})
	// Legacy's idle probe succeeds (WaitForIdle answers idle), so it begins
	// and finishes the idle drain itself: the idle sleep, its latch over the
	// always-named demand, and the latch's release once a reload changes the
	// policy (its fingerprint).
	idle := tlFixture("idle, then sleep, then the next pass", chatNapping, []parityRow{named}, nil, []simRuntime{tlLive("gc-c")},
		[]parityStep{
			{Name: "idle past sleep_after_idle", At: 6 * time.Minute},
			{Name: "the next pass", Advance: time.Minute},
			{Name: "the sleep policy changes", Advance: time.Minute, Op: func(tw twin) {
				tw.reconfigure(func(c *config.City) { c.Agents[1].SleepAfterIdle = "10m" })
			}},
		}, chatStanding, map[string]map[string]string{
			"idle past sleep_after_idle": sleeping,
			"the next pass":              slept,
			"the sleep policy changes":   tlMerged(tlStarted("gc-c", chatName, "chat"), map[string]string{"row:gc-c:*idle": "A20 idle-sleep"}),
		})
	idle.Setup = func(tw twin) {
		tw.sp.mu.Lock()
		tw.sp.WaitForIdleErrors[chatName] = nil
		tw.sp.mu.Unlock()
	}
	attach := tlFixture("attach, then detach, then idle", napping,
		[]parityRow{tlRow("gc-1", 1, time.Minute)}, []parityRow{routedDemandBead("gw-1")}, []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", attached: true}},
		[]parityStep{
			{Name: "detach", Advance: time.Minute, Op: func(tw twin) {
				tw.sp.SetActivity("s-gc-1", tw.now)
				tw.runtime("gc-1", func(rt simRuntime) *simRuntime { rt.attached = false; return &rt })
			}},
			{Name: "idle past sleep_after_idle", Advance: 6 * time.Minute},
		}, tlMerged(tlPol("gc-1"), tlIdent(), tlAliasMeta()), map[string]map[string]string{"seed": tlAliasSeed()})
	// retired is legacy's drain of gc-1, acknowledged and stopped, and the
	// pool slot's close; v2 leaves the drain to A20 and the close to A21.
	retired := map[string]string{
		"row:gc-1:*drain": "A20 drain-begin", "runtime:s-gc-1": "A20 drain-begin", "row:gc-1:*close": "A21 close", "row:gc-1:*sync": "A7 row-metadata",
	}
	retiring := map[string]string{
		"provider:SetMeta s-gc-1": "A20 drain-begin", "provider:RemoveMeta s-gc-1": "A20 drain-begin", "provider:Stop s-gc-1": "A20 drain-begin",
		`event:session.stopped worker {"reason":"drain acknowledged","session_id":"gc-1","template":"worker"}`: "A20 drain-begin",
	}
	// An unwanted row woken a minute ago: legacy keeps it for the 5m wake
	// grace (INC-003), then drains it and retires its slot.
	grace := tlFixture("an unwanted row inside, then past, the wake grace", tlCity,
		[]parityRow{tlRow("gc-1", 1, time.Minute)}, nil, []simRuntime{tlLive("gc-1")},
		[]parityStep{
			{Name: "inside the grace", At: 2 * time.Minute},
			{Name: "past the grace", At: 5 * time.Minute},
			{Name: "past the drain timeout", Advance: 6 * time.Minute},
		}, nil, map[string]map[string]string{
			"past the grace":         tlMerged(retired, retiring),
			"past the drain timeout": retired,
		})
	// drainedAtOnce is retired in the seed tick, with its calls and event.
	drainedAtOnce := map[string]map[string]string{"seed": tlMerged(retired, retiring)}
	// A row whose last_woke_at a skewed clock put ten minutes ahead: legacy
	// grants it no INC-003 grace and retires it at once, by design (a skew
	// must never pin a row).
	skewed := tlFixture("an unwanted row woken by a clock ten minutes ahead", tlCity,
		[]parityRow{tlRow("gc-1", 1, -10*time.Minute)}, nil, []simRuntime{tlLive("gc-1")},
		[]parityStep{{Name: "past the skewed wake's grace", At: 16 * time.Minute}}, nil, tlMerged2(drainedAtOnce, "past the skewed wake's grace", retired))
	// An agent suspended in config (E2b): its rows skip the INC-003 grace.
	suspendedAgent := tlFixture("an unwanted row of a suspended agent", func() *config.City {
		cfg := tlCity()
		cfg.Agents[0].Suspended = true
		return cfg
	}, []parityRow{tlRow("gc-1", 1, time.Minute)}, nil, []simRuntime{tlLive("gc-1")},
		[]parityStep{{Name: "a minute on", Advance: time.Minute}}, nil, tlMerged2(drainedAtOnce, "a minute on", retired))
	// A reload that turns idle sleep on reaches a detached row on both
	// copies: legacy's next tick and v2's next env generation stamp
	// detached_at.
	reload := tlFixture("a reload turns idle sleep on for a detached row", tlCity,
		[]parityRow{tlRow("gc-1", 1, time.Minute)}, []parityRow{routedDemandBead("gw-1")}, []simRuntime{tlLive("gc-1")},
		[]parityStep{{Name: "the reload", Advance: time.Minute, Op: func(tw twin) {
			tw.reconfigure(func(c *config.City) { c.Agents[0].SleepAfterIdle = "5m" })
		}}}, tlMerged(tlPol("gc-1"), tlIdent(), tlAliasMeta()), map[string]map[string]string{"seed": tlAliasSeed()})
	// An on_demand named session, running and detached with no work: the
	// awake set puts it to idle sleep (ComputeAwakeSet's idle-sleep) once
	// it has been detached past sleep_after_idle, and its probe answers
	// idle. Its agent was last active at +2m, which holds the config
	// suppression (idle since its last activity) to +7m, so the awake set's
	// threshold alone decides the drain.
	onDemand := tlFixture("an idle on-demand named session sleeps", func() *config.City {
		cfg := chatNapping()
		cfg.NamedSessions[0].Mode = "on_demand"
		return cfg
	}, []parityRow{func() parityRow {
		b := chatRow("gc-c", "1", "state", "awake", "session_name", chatName, "instance_token", "tok-gc-c", "last_woke_at", tlAt(-time.Minute), "alias", "chat", "configured_named_mode", "on_demand")
		b.Title = "Display gc-c"
		return b
	}()}, nil, []simRuntime{tlLive("gc-c")},
		[]parityStep{{Name: "idle past sleep_after_idle", At: 6 * time.Minute}}, chatStanding, map[string]map[string]string{"idle past sleep_after_idle": sleeping})
	onDemand.Setup = func(tw twin) {
		idle.Setup(tw)
		tw.sp.SetActivity(chatName, parityNow.Add(2*time.Minute))
	}
	// A row whose last_woke_at a clock put two minutes ahead: the tolerated
	// side of the skew, which keeps its INC-003 grace.
	tolerated := tlFixture("an unwanted row woken by a clock two minutes ahead", tlCity,
		[]parityRow{tlRow("gc-1", 1, -2*time.Minute)}, nil, []simRuntime{tlLive("gc-1")},
		[]parityStep{{Name: "past the skewed wake's grace", At: 8 * time.Minute}}, nil, map[string]map[string]string{"past the skewed wake's grace": tlMerged(retired, retiring)})
	return []timelineFixture{idle, attach, grace, skewed, tolerated, suspendedAgent, reload, onDemand}
}

// set writes kv to row id's metadata, as the CLI does.
func (tw twin) set(id string, kv ...string) {
	st, _ := tw.row(id)
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	if err := st.SetMetadataBatch(id, m); err != nil {
		tw.t.Fatal(err)
	}
}

// patch writes p to row id's metadata.
func (tw twin) patch(id string, p session.MetadataPatch) {
	var kv []string
	for k, v := range p {
		kv = append(kv, k, v)
	}
	tw.set(id, kv...)
}

// operatorTimeline is the operator verbs through time: suspend (with and
// without a controller to own it), kill on a ready and a blocked claim, a
// provider swap, and a quarantine expiring under a kill fence.
func operatorTimeline() []timelineFixture {
	standing := tlMerged(tlPol("gc-1"), tlAliasMeta())
	wake := func(tw twin) {
		st, _ := tw.row("gc-1")
		if _, err := session.NewStore(beads.SessionStore{Store: st}).WakeSession("gc-1", tw.now, session.WakeOpts{}); err != nil {
			tw.t.Fatal(err)
		}
	}
	restarted := tlMerged(tlIdent(), tlStarted("gc-1", "s-gc-1", "worker-1"))
	// woken is the woken row: legacy restarts it, consuming the wake request
	// (D7) in its start, which v2 leaves to A18; settled, the row it leaves.
	wakeRequest := map[string]string{"row:gc-1:wake_request": "A18 start", "row:gc-1:wake_requested_at": "A18 start"}
	woken := tlMerged(restarted, wakeRequest)
	settled := tlMerged(tlIdent(), tlStartState("gc-1", "s-gc-1"), wakeRequest)
	// healed is legacy's state heal of the suspended row whose runtime is
	// gone: asleep, under the hold; v2 keeps it suspended.
	healed := map[string]string{"row:gc-1:state": "suspended-row-heal"}
	// restamped is the suspend stamp a legacy start keeps and v2's heal of
	// the woken, still-dead row replaces.
	restamped := map[string]string{"row:gc-1:suspended_at": "A18 start", "row:gc-1:slept_at": "A18 start"}
	// gc session suspend with no controller to own it (Manager.Suspend): the
	// runtime stops and the row takes the operator hold
	// (session.OperatorSuspendPatch) in one step, so its work, assigned by
	// session name, no longer restarts it (mc-esqo7, fixed by #7417). gc
	// session wake 20s later restarts it.
	suspend := tlFixture("suspend, then wake", tlCity,
		[]parityRow{tlRow("gc-1", 1, time.Hour)}, tlClaim("in_progress", "s-gc-1"), []simRuntime{tlLive("gc-1")},
		[]parityStep{
			{Name: "suspend", Op: func(tw twin) {
				st, _ := tw.row("gc-1")
				if err := session.NewManagerWithOptions(st, tw.sp, session.WithClock(&clock.Fake{Time: tw.now}), session.WithCityPath(tw.city)).Suspend("gc-1"); err != nil {
					tw.t.Fatal(err)
				}
			}},
			{Name: "wake", Advance: 20 * time.Second, Op: wake},
			{Name: "the next pass", Advance: time.Minute},
		}, standing, map[string]map[string]string{
			"seed": tlAliasSeed(), "suspend": tlMerged(tlIdent(), healed), "wake": tlMerged(woken, restamped), "the next pass": tlMerged(settled, restamped),
		})
	// gc session suspend under a controller writes the same operator hold
	// (session.Store.OperatorSuspend) on a live runtime, and legacy drains
	// it. Its drain completion keeps the user-hold intent, so the hold stays
	// an operator's and the work no longer restarts the row (mc-esqo7, fixed
	// by #7417); the completion still writes sleep_reason=idle and clears
	// suspended_at. Then gc session wake restarts it.
	drainedRow := map[string]string{
		"row:gc-1:*drain": "A20 drain-begin", "row:gc-1:sleep_reason": "A20 drain-begin", "row:gc-1:suspended_at": "A20 drain-begin", "row:gc-1:last_woke_at": "A20 drain-begin",
	}
	managed := tlFixture("a managed suspend drains, then wake", tlCity,
		[]parityRow{tlRow("gc-1", 1, time.Hour)}, tlClaim("in_progress", "s-gc-1"), []simRuntime{tlLive("gc-1")},
		[]parityStep{
			{Name: "suspend", Op: func(tw twin) {
				st, _ := tw.row("gc-1")
				if err := session.NewStore(beads.SessionStore{Store: st}).OperatorSuspend("gc-1", tw.now); err != nil {
					tw.t.Fatal(err)
				}
			}},
			{Name: "past the drain timeout", Advance: 6 * time.Minute},
			{Name: "wake", Advance: time.Minute, Op: wake},
			{Name: "the next pass", Advance: time.Minute},
		}, standing, map[string]map[string]string{
			"seed": tlAliasSeed(),
			"suspend": tlMerged(tlIdent(), drainedRow, healed, map[string]string{
				"runtime:s-gc-1": "A20 drain-begin", "provider:SetMeta s-gc-1": "A20 drain-begin", "provider:RemoveMeta s-gc-1": "A20 drain-begin", "provider:Stop s-gc-1": "A20 drain-begin",
				`event:session.drain_acked_with_assigned_work worker {"bead_id":"gw-1","bead_status":"in_progress","reason":"drain_acked_with_assigned_work","session_id":"gc-1","template":"worker"}`: "A20 drain-begin",
				`event:session.stopped worker {"reason":"drain acknowledged","session_id":"gc-1","template":"worker"}`:                                                                                 "A20 drain-begin",
			}),
			"past the drain timeout": tlMerged(tlIdent(), drainedRow, healed, map[string]string{"runtime:s-gc-1": "A20 drain-begin"}),
			"wake":                   tlMerged(woken, drainedRow),
			"the next pass":          tlMerged(settled, map[string]string{"row:gc-1:*drain": "A20 drain-begin"}),
		})
	// gc session kill (the fence, then the stop) on a seat holding only an
	// open claim. Ready, the claim restarts the seat in place past the
	// grace; blocked by an open dependency, it is unexecuted, so legacy
	// releases it and closes the seat with no fresh seat (owner ruling B1).
	kill := func(blocked bool) timelineFixture {
		name, work := "kill a seat holding a ready claim", tlClaim("open", "gc-1")
		graced := tlMerged(restarted, map[string]string{"row:gc-1:sleep_reason": "A18 start"})
		if blocked {
			name = "kill a seat holding a dep-blocked claim"
			work = append(work, parityRow{ID: "gw-2", Title: "gw-2", Type: "task", Status: "open", Metadata: map[string]string{}})
			graced = tlMerged(tlIdent(), map[string]string{"row:gc-1:*close": "A21 killed-seat", "row:gc-1:*sync": "A7 row-metadata", "work:gw-1:assignee": "A21 killed-seat"})
		}
		f := tlFixture(name, tlCity, []parityRow{tlRow("gc-1", 1, time.Hour)}, work, []simRuntime{tlLive("gc-1")},
			[]parityStep{
				{Name: "kill", Op: func(tw twin) {
					tw.patch("gc-1", session.KillPendingPatch(tw.now))
					_ = tw.sp.Stop("s-gc-1")
				}},
				{Name: "past the kill grace", Advance: session.KillPendingGrace + time.Minute},
			}, tlMerged(tlPol("gc-1"), tlAliasMeta()), map[string]map[string]string{"seed": tlAliasSeed(), "kill": tlIdent(), "past the kill grace": graced})
		if blocked {
			f.Setup = func(tw twin) {
				st, _ := tw.row("gw-1")
				if err := st.DepAdd("gw-1", "gw-2", "blocks"); err != nil {
					tw.t.Fatal(err)
				}
			}
		}
		return f
	}
	// The swap's stop is shared code (P7b, both modes) and writes no row;
	// 10s after the wake, legacy counts it as a wake failure (P7-M3).
	swap := tlFixture("a provider swap stops the runtime", tlCity,
		[]parityRow{tlRow("gc-1", 1, 10*time.Second)}, tlClaim("in_progress", "gc-1"), []simRuntime{tlLive("gc-1")},
		[]parityStep{
			{Name: "swap", Ticks: 6, Op: func(tw twin) {
				if err := stopProviderSwapRuntimes([]swapStop{{name: "s-gc-1", backend: tw.sp}}, tw.cfg, tw.stores[0], tw.rec, io.Discard, io.Discard); err != nil {
					tw.t.Fatal(err)
				}
			}},
			{Name: "the next pass", Advance: time.Minute},
		}, standing, map[string]map[string]string{
			"seed": tlAliasSeed(), "swap": tlMerged(restarted, tlAccrued()), "the next pass": tlMerged(tlIdent(), tlStartState("gc-1", "s-gc-1"), tlAccrued()),
		})
	// A kill fence and a quarantine, each holding the row from waking for its
	// work until it expires. Legacy heals the expired quarantine under the
	// live fence (mc-uyclc).
	fenced := tlRow("gc-1", 1, time.Hour, "state", "asleep", "quarantined_until", tlAt(2*time.Minute))
	maps.Copy(fenced.Metadata, session.KillPendingPatch(parityNow))
	fence := tlFixture("a quarantine, then a kill fence, expires", tlCity, []parityRow{fenced}, tlClaim("in_progress", "gc-1"), nil,
		[]parityStep{
			{Name: "the quarantine expires", At: 3 * time.Minute},
			{Name: "the kill fence expires", At: 6 * time.Minute},
		}, tlIdent(), map[string]map[string]string{
			"the quarantine expires": {"row:gc-1:*fenced": "§12.2#35 fenced-timer-heal"},
			"the kill fence expires": tlMerged(tlPol("gc-1"), restarted, map[string]string{
				"row:gc-1:sleep_reason": "A18 start", "row:gc-1:currently_processing_bead_id": "A18 start", "row:gc-1:*fenced": "§12.2#35 fenced-timer-heal",
			}),
		})
	return []timelineFixture{suspend, managed, kill(false), kill(true), swap, fence}
}
