package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/session"
)

// The v2 journeys under exec (PAR-EXEC §13.3), on the production effects
// and the production exec provider (NewSeamBackedForCity) over a stateful
// observe.v1 script: the effect test kit's row gc-1 (runtime name s-gc-1,
// generation 3, token tok), with the kit's provider replaced by the exec
// pack. Each op the script runs is logged, so a journey asserts the ops gc
// sent, not only the rows it wrote.

// execJourneyScript is a stateful observe.v1 exec pack. A box is the
// directory $S/b/<name>: its object (id, created), its state (running or
// dead), its agent (alive or dead), its attachment (true, false, or err:
// unreadable), its pane text (its screen, live or dead: `peek` prints it,
// and answers exit 66, no screen, for no box or a box holding noscreen) and
// its metadata (meta.<KEY>, the identity keys seeded from the start's env
// before the box appears). A file
// $S/fault.<op> fails that op (exit 1), as a backend outage does; $S/notready
// drops observe.v1 and report-attachment from the handshake; $S/start.dead
// starts a box that dies at once; $S/start.collide has another creator take
// the name just as the start runs (exit 73); $S/kill.replace replaces the
// box's object just before a kill-object checks it. Every op is appended to
// $S/ops.
const execJourneyScript = `S=%q
op="$1"; name="$2"; box="$S/b/$name"; mkdir -p "$S/b"
in=""; case "$op" in start|observe|kill-object|set-meta) in=$(cat) ;; esac
log() { echo "$op $name${*:+ $*}" >> "$S/ops"; }
field() { printf %%s "$in" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
if [ -f "$S/fault.$op" ]; then log fault; echo "backend unavailable" >&2; exit 1; fi
ident() {
  printf '{'; sep=''
  for k in GC_SESSION_ID GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN GT_PROCESS_NAMES; do
    if [ -f "$box/meta.$k" ]; then printf '%%s"%%s":"%%s"' "$sep" "$k" "$(cat "$box/meta.$k")"; sep=','; fi
  done
  printf '}'
}
case "$op" in
  protocol)
    if [ -f "$S/notready" ]; then echo '{"version":0,"capabilities":[]}'; else echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; fi ;;
  start)
    fresh=$(printf %%s "$in" | sed -n 's/.*"fresh_only":true.*/y/p'); log "fresh=${fresh:-n}"
    if [ -f "$S/start.collide" ] && [ ! -d "$box" ]; then
      mkdir -p "$box"; printf obj-c > "$box/id"; printf tc > "$box/created"; echo running > "$box/state"; echo alive > "$box/agent"; echo false > "$box/attached"
      printf gc-other > "$box/meta.GC_SESSION_ID"; printf tok-other > "$box/meta.GC_INSTANCE_TOKEN"
    fi
    if [ -d "$box" ]; then if [ -n "$fresh" ]; then exit 73; fi; exit 1; fi
    mkdir -p "$box.tmp"
    for k in GC_SESSION_ID GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN; do v=$(field "$k"); [ -n "$v" ] && printf %%s "$v" > "$box.tmp/meta.$k"; done
    printf agent > "$box.tmp/meta.GT_PROCESS_NAMES"
    n=$(cat "$S/seq" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$S/seq"
    printf 'obj-%%s' "$n" > "$box.tmp/id"; printf 't%%s' "$n" > "$box.tmp/created"
    if [ -f "$S/start.dead" ]; then echo dead > "$box.tmp/state"; else echo running > "$box.tmp/state"; fi
    echo alive > "$box.tmp/agent"; echo false > "$box.tmp/attached"; printf '\342\235\257 \n' > "$box.tmp/pane"
    mv "$box.tmp" "$box" ;;
  stop) log; rm -rf "$box" ;;
  observe)
    log
    if [ ! -d "$box" ]; then echo '{"state":"absent"}'; exit 0; fi
    st=$(cat "$box/state")
    printf '{"state":"%%s","object":{"id":"%%s","created":"%%s"}' "$st" "$(cat "$box/id")" "$(cat "$box/created")"
    if [ "$st" = running ] && [ -n "$in" ]; then printf ',"agent":"%%s"' "$(cat "$box/agent")"; fi
    printf ',"identity":'; ident; echo '}' ;;
  inventory)
    log; printf '{"complete":true,"sessions":['; sep=''
    for box in "$S"/b/*; do
      [ -d "$box" ] || continue; case "$box" in *.tmp) continue ;; esac
      st=$(cat "$box/state"); a=$(cat "$box/attached"); [ "$a" = err ] && a=null
      printf '%%s{"name":"%%s","state":"%%s","object":{"id":"%%s","created":"%%s"},"attached":%%s,"identity":' "$sep" "$(basename "$box")" "$st" "$(cat "$box/id")" "$(cat "$box/created")" "$a"
      ident
      if [ "$st" = running ]; then printf ',"agent":"%%s"' "$(cat "$box/agent")"; fi
      printf '}'; sep=','
    done; echo ']}' ;;
  kill-object)
    id=$(field id); cr=$(field created); mode=$(field mode); log "$id $mode"
    [ -f "$S/kill.replace" ] && [ -d "$box" ] && printf obj-9 > "$box/id"
    if [ ! -d "$box" ] || [ "$(cat "$box/id")" != "$id" ] || [ "$(cat "$box/created")" != "$cr" ]; then echo gone; exit 0; fi
    st=$(cat "$box/state")
    case "$mode" in
      corpse) if [ "$st" = dead ]; then rm -rf "$box"; echo killed; else echo live; fi ;;
      zombie) if [ "$st" = running ] && [ "$(cat "$box/agent")" = dead ] && [ "$(cat "$box/attached")" = false ]; then rm -rf "$box"; echo killed; else echo changed; fi ;;
      *) exit 1 ;;
    esac ;;
  is-attached)
    log; [ -d "$box" ] || { echo false; exit 0; }
    a=$(cat "$box/attached"); [ "$a" = err ] && exit 1; echo "$a" ;;
  is-running) [ -d "$box" ] && [ "$(cat "$box/state")" = running ] && echo true || echo false ;;
  get-meta) [ -f "$box/meta.$3" ] && cat "$box/meta.$3"; exit 0 ;;
  set-meta) log "$3"; [ -d "$box" ] && printf %%s "$in" > "$box/meta.$3"; exit 0 ;;
  remove-meta) log "$3"; rm -f "$box/meta.$3" ;;
  peek)
    log; [ -d "$box" ] && [ ! -f "$box/noscreen" ] || exit 66
    cat "$box/pane" ;;
  *) exit 2 ;;
esac
`

// execFixture is one journey's exec pack: its state directory and the
// production provider serving it for the city.
type execFixture struct {
	t     *testing.T
	state string
	sp    runtime.Provider
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "pack")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+fmt.Sprintf(execJourneyScript, state)), 0o755); err != nil {
		t.Fatal(err)
	}
	return &execFixture{t: t, state: state, sp: sessionexec.NewSeamBackedForCity(script, "test-city", dir)}
}

// ops is the script's op log, one "<op> <name> [args]" per line, with the
// handshake and the reads no journey asserts left out.
func (f *execFixture) ops() []string {
	data, _ := os.ReadFile(filepath.Join(f.state, "ops"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// count is the number of logged ops starting with prefix.
func (f *execFixture) count(prefix string) int {
	n := 0
	for _, l := range f.ops() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// box puts a box under name carrying the row's identity (session id,
// epoch, token), its state and agent as given.
func (f *execFixture) box(name, id, epoch, token, state, agent string) {
	f.t.Helper()
	dir := filepath.Join(f.state, "b", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for file, v := range map[string]string{
		"id": "obj-0", "created": "t0", "state": state, "agent": agent, "attached": "false", "pane": "",
		"meta.GC_SESSION_ID": id, "meta.GC_RUNTIME_EPOCH": epoch, "meta.GC_INSTANCE_TOKEN": token, "meta.GT_PROCESS_NAMES": "agent",
	} {
		f.write(filepath.Join("b", name, file), v)
	}
}

// set writes one attribute file of s-gc-1's box (state, agent, attached,
// pane, id).
func (f *execFixture) set(attr, v string) { f.write(filepath.Join("b", "s-gc-1", attr), v) }

// read is one attribute file of s-gc-1's box, "" when there is no box.
func (f *execFixture) read(attr string) string {
	data, _ := os.ReadFile(filepath.Join(f.state, "b", "s-gc-1", attr))
	return strings.TrimSpace(string(data))
}

// fault fails op until healed.
func (f *execFixture) fault(op string, on bool) {
	f.t.Helper()
	path := filepath.Join(f.state, "fault."+op)
	if !on {
		_ = os.Remove(path)
		return
	}
	f.write("fault."+op, "")
}

func (f *execFixture) write(rel, v string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, rel), []byte(v), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// execKit is the effect kit running the start on row gc-1 in state, its
// provider the fixture's exec pack.
func execKit(t *testing.T, state string) (*txKit, *execFixture) {
	t.Helper()
	return execKitOn(t, state)
}

// execKitOn is execKit on a row carrying meta too.
func execKitOn(t *testing.T, state string, meta ...string) (*txKit, *execFixture) {
	t.Helper()
	k := startKit(t, intentStart, state, meta...)
	k.it.Deadline = gatherNow.Add(70 * time.Second)
	f := newExecFixture(t)
	k.p.Runtime = f.sp
	return k, f
}

// Journey "start" (A2): exactly one start op per admission, fresh_only,
// and the commit lands on the PreWake token the box carries.
func TestExecJourneyStart(t *testing.T) {
	k, f := execKit(t, "asleep")
	s := k.runKind()
	if s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want the start landed; ops %q", s, f.ops())
	}
	if n, fresh := f.count("start "), f.count("start s-gc-1 fresh=y"); n != 1 || fresh != 1 {
		t.Fatalf("%d start ops (%d fresh_only), want exactly one, fresh_only; ops %q", n, fresh, f.ops())
	}
	if tok := k.meta("instance_token"); tok == "" || tok != f.read("meta.GC_INSTANCE_TOKEN") {
		t.Fatalf("committed token %q, box carries %q; want the PreWake token on both", tok, f.read("meta.GC_INSTANCE_TOKEN"))
	}
}

// execStop runs the stop verb on gc-1 (stopRun) with the fixture's pack as
// the pass's provider, the row's own box (token tok-3, epoch 3) put under
// s-gc-1 by state and agent, and at, when set, run on the box before the
// verb runs.
func execStop(t *testing.T, entry func(*selectionEntry), state, agent string, at func(*execFixture)) (settlement, map[string]string, *execFixture) {
	t.Helper()
	f := newExecFixture(t)
	s, got, _ := stopRun(t, signaled(drainOrphaned), entry, func(p *effectPass, _ *fenceLeaf, _ beads.Store, id string) {
		p.Runtime = f.sp
		f.box("s-gc-1", id, "3", "tok-3", state, agent)
		if at != nil {
			at(f)
		}
	})
	return s, got.Metadata, f
}

// Journey "drain-ack finalize" (A5): a signaled row's live own box is
// stopped, and the row finalizes only once `observe` reads it absent; an
// unknown observe keeps the request open with nothing stopped; a dead box
// counts as stopped, its corpse removed by exact object.
func TestExecJourneyDrainAckFinalize(t *testing.T) {
	s, m, f := execStop(t, undesired(drainOrphaned), "running", "alive", nil)
	if s.Outcome != settledLanded || f.count("stop s-gc-1") != 1 || m["state"] != string(session.StateDrained) || m[session.DrainIntentReasonKey] != "" {
		t.Fatalf("live box: settlement %+v, row %v, ops %q; want one stop, then finalized drained", s, m, f.ops())
	}
	if ops := f.ops(); !slices.ContainsFunc(ops[slices.Index(ops, "stop s-gc-1"):], func(op string) bool { return strings.HasPrefix(op, "observe s-gc-1") }) {
		t.Fatalf("no observe after the stop: ops %q; want the finalize to rest on the box read gone", ops)
	}

	s, m, f = execStop(t, undesired(drainOrphaned), "running", "alive", func(f *execFixture) { f.fault("observe", true) })
	if s.Outcome != settledRefused || f.count("stop ") != 0 || m[session.DrainIntentReasonKey] != drainOrphaned {
		t.Fatalf("unknown observe: settlement %+v, row %v, ops %q; want refused, nothing stopped, the request open", s, m, f.ops())
	}

	s, m, f = execStop(t, undesired(drainOrphaned), "dead", "", nil)
	if s.Outcome != settledLanded || m[session.DrainIntentReasonKey] != "" || f.count("kill-object s-gc-1 obj-0 corpse") != 1 || f.read("id") != "" {
		t.Fatalf("dead box: settlement %+v, row %v, ops %q; want finalized, the corpse killed by its object", s, m, f.ops())
	}
}

// Journey "attached refusal" on the stop path: `is-attached` true, or an
// is-attached that fails, holds the stop: refused, nothing stopped, the row
// still signaled. A dead box under an attached client is finalized, but its
// corpse is left in place.
func TestExecJourneyAttachedRefusesTheStop(t *testing.T) {
	for _, attached := range []string{"true", "err"} {
		s, m, f := execStop(t, undesired(drainOrphaned), "running", "alive", func(f *execFixture) { f.set("attached", attached) })
		if s.Outcome != settledRefused || f.count("stop ") != 0 || m[session.DrainIntentReasonKey] != drainOrphaned {
			t.Errorf("attached %s: settlement %+v, row %v, ops %q; want refused with nothing stopped", attached, s, m, f.ops())
		}
	}
	s, m, f := execStop(t, undesired(drainOrphaned), "dead", "", func(f *execFixture) { f.set("attached", "true") })
	if s.Outcome != settledLanded || m[session.DrainIntentReasonKey] != "" || f.count("kill-object ") != 0 {
		t.Fatalf("attached corpse: settlement %+v, row %v, ops %q; want finalized with the corpse left in place", s, m, f.ops())
	}
}

// Journey "kill-certified" on the recycle path: kill-object names the
// object the read certified (obj-0), never the name; a box replaced under
// the name just before the kill is not that object, so the script kills
// nothing (gone) and the start launches nothing over the new box.
func TestExecJourneyRecycleIsCertified(t *testing.T) {
	for _, state := range []string{"dead", "running"} {
		k, f := execKit(t, "asleep")
		agent := "dead"
		if state == "dead" {
			agent = ""
		}
		f.box("s-gc-1", "gc-1", "3", "tok", state, agent)
		f.write("kill.replace", "")
		s := k.runKind()
		if f.count("kill-object s-gc-1 obj-0 ") != 1 || f.count("start ") != 0 || s.Outcome == settledLanded || f.read("id") != "obj-9" {
			t.Fatalf("%s box: settlement %+v, ops %q; want one kill-object of obj-0, the replacement kept, nothing started", state, s, f.ops())
		}
	}
}

// Journey "kill-certified": the fence's object is the one stopped. A box
// replaced under the name between the fence's read and the Stop (another
// object carrying the row's identity) refuses runtime-moved and stops
// nothing.
func TestExecJourneyStopIsCertified(t *testing.T) {
	f := newExecFixture(t)
	s, m, _ := stopRun(t, signaled(drainOrphaned), undesired(drainOrphaned), func(p *effectPass, _ *fenceLeaf, _ beads.Store, id string) {
		p.Runtime = f.sp
		f.box("s-gc-1", id, "3", "tok-3", "running", "alive")
		p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
			if at == seamBeforeCall && section == 0 {
				f.set("id", "obj-9")
			}
			return nil
		}
	})
	if f.count("stop ") != 0 || s.Cause != fenceRuntimeMoved || m.Metadata[session.DrainIntentReasonKey] != drainOrphaned {
		t.Fatalf("settlement %+v, row %v, ops %q; want refused runtime-moved with nothing stopped", s, m.Metadata, f.ops())
	}
}

// Journey "start", the collision: a box another creator makes under the
// name between gc's vacancy read and its start answers exit 73, which is
// ErrSessionExists: the start refuses occupied rather than failing (no
// failure accrues), nothing is cleaned up (no stop, no kill), the other
// creator's box stays, and the row does not commit.
func TestExecJourneyStartCollisionCleansNothing(t *testing.T) {
	k, f := execKit(t, "asleep")
	f.write("start.collide", "")
	s := k.runKind()
	if s.Outcome != settledRefused || s.Cause != causeOccupied || f.count("start s-gc-1 fresh=y") != 1 || f.count("stop ") != 0 || f.count("kill-object ") != 0 {
		t.Fatalf("settlement %+v, ops %q; want one fresh start refused occupied (no start failure), nothing cleaned up", s, f.ops())
	}
	if f.read("meta.GC_SESSION_ID") != "gc-other" || k.meta("state") == "active" {
		t.Fatalf("box owner %q, row state %q; want the other creator's box kept and no commit", f.read("meta.GC_SESSION_ID"), k.meta("state"))
	}
}

// Journeys "zombie" and "corpse": the row's own dead box (its agent dead
// in a running box, or the box itself dead) is removed by kill-object on
// its exact object, then the row starts once, fresh. Another row's zombie
// is no recycle: occupied, nothing killed or started.
func TestExecJourneyRecycleThenStart(t *testing.T) {
	for _, c := range []struct {
		name, id, state, agent, kill string
		start                        bool
	}{
		{"own zombie", "gc-1", "running", "dead", "kill-object s-gc-1 obj-0 zombie", true},
		{"own corpse", "gc-1", "dead", "", "kill-object s-gc-1 obj-0 corpse", true},
		{"another row's corpse", "gc-9", "dead", "", "kill-object s-gc-1 obj-0 corpse", true},
		{"another row's zombie", "gc-9", "running", "dead", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, f := execKit(t, "asleep")
			tok := "tok"
			if c.id != "gc-1" {
				tok = "theirs"
			}
			f.box("s-gc-1", c.id, "3", tok, c.state, c.agent)
			f.set("pane", "panic: boom")
			s := k.runKind()
			kills := f.count("kill-object ")
			switch {
			case c.kill != "" && (kills != 1 || f.count(c.kill) != 1):
				t.Fatalf("settlement %+v, ops %q; want exactly %q", s, f.ops(), c.kill)
			case c.kill == "" && kills != 0:
				t.Fatalf("settlement %+v, ops %q; want nothing killed", s, f.ops())
			case c.start && (s.Outcome != settledLanded || f.count("start s-gc-1 fresh=y") != 1 || f.count("start ") != 1):
				t.Fatalf("settlement %+v, ops %q; want one fresh start after the kill, landed", s, f.ops())
			case !c.start && (f.count("start ") != 0 || s.Cause != causeOccupied):
				t.Fatalf("settlement %+v, ops %q; want refused occupied, nothing started", s, f.ops())
			}
		})
	}
}

// Journey "attached refusal" on the recycle path: an own zombie whose
// is-attached reads true, or fails, is held: refused attached, nothing
// killed, nothing started.
func TestExecJourneyAttachedRefusesTheRecycle(t *testing.T) {
	for _, attached := range []string{"true", "err"} {
		k, f := execKit(t, "asleep")
		f.box("s-gc-1", "gc-1", "3", "tok", "running", "dead")
		f.set("attached", attached)
		if s := k.runKind(); s.Cause != causeAttached || f.count("kill-object ") != 0 || f.count("start ") != 0 {
			t.Errorf("attached %s: settlement %+v, ops %q; want refused attached, nothing killed or started", attached, s, f.ops())
		}
	}
}

// Journey "stranded repair": an execution-stalled seat with no box closes
// only on an absent `observe`, and its stalled claim is released; a box
// back under the name by the confirmation's read keeps the claim on the
// seat, which stays open.
func TestExecJourneyStrandedRepairReleasesOnAnAbsentRead(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "back by the confirmation"}[back], func(t *testing.T) {
			f := newExecFixture(t)
			var claimID string
			var work beads.Store
			s, got, _ := stopRun(t, signaled(executionStalledDrainReason), undesired(executionStalledDrainReason), func(p *effectPass, _ *fenceLeaf, store beads.Store, id string) {
				work = store
				p.World.Env.Cfg.Daemon.WorkLadder = "off"
				claim, err := store.Create(beads.Bead{Title: "stalled step", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				inProgress := "in_progress"
				if err := store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &id}); err != nil {
					t.Fatal(err)
				}
				claimID = claim.ID
				markStalledClaim(t, store, id, claim.ID)
				rel := newCloseReleases(p.World, nil, io.Discard)
				rel.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, store, nil)
				p.held.release = rel
				p.Runtime = f.sp
				p.seam = func(_ context.Context, at txSeam, _ intent, _, _ int) error {
					if back && at == seamBeforeCall && f.read("id") == "" {
						f.box("s-gc-1", id, "3", "tok-3", "running", "alive")
					}
					return nil
				}
			})
			claim, err := work.Get(claimID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case back && (s.Outcome != settledRefused || claim.Status != "in_progress" || got.Status == "closed"):
				t.Fatalf("settlement %+v, claim %q, row %s, ops %q; want refused, the claim kept, the seat open", s, claim.Status, got.Status, f.ops())
			case !back && (s.Outcome != settledLanded || claim.Status != "open" || got.Status != "closed" || f.count("observe s-gc-1") == 0):
				t.Fatalf("settlement %+v, claim %q, row %s, ops %q; want landed on an absent observe, the claim given back, the seat closed", s, claim.Status, got.Status, f.ops())
			}
		})
	}
}

// Journey "restart": a controller that starts again over running boxes
// attributes each one through the inventory (one inventory op, no
// per-name read, the identity spent from no budget), and a start of a row
// whose own box is alive with its token adopts it: no start op, nothing
// killed.
func TestExecJourneyRestartAttributesAndAdopts(t *testing.T) {
	f := newExecFixture(t)
	f.box("s-gc-1", "gc-1", "3", "tok", "running", "alive")
	f.box("s-gc-2", "gc-2", "5", "tok-2", "running", "alive")
	listing := listInventoryBackends(f.sp, "exec")
	backends := []BackendPass{classifyInventoryBackend(listing.leaves[0])}
	clk := &clock.Fake{Time: obsTestEpoch}
	l := &runtimeInventoryLane{clock: clk, interval: time.Minute, identity: map[string]inventoryIdentity{}}
	l.envWindowAt, l.envWindowReads = clk.Now(), inventoryAttributionBudget // a restart's budget, spent
	attrs, hosts, _, errs := l.enrich(context.Background(), listing.leaves, backends)
	if len(errs) != 0 || backends[0].Outcome != OutcomeComplete {
		t.Fatalf("enrich errs %v, outcome %v; want the complete inventory", errs, backends[0].Outcome)
	}
	if reads, _ := l.readIdentities(context.Background(), attrs, hosts); reads != 0 {
		t.Fatalf("%d identity reads, want none: the inventory carries every box's", reads)
	}
	for name, want := range map[string][2]string{"s-gc-1": {"gc-1", "tok"}, "s-gc-2": {"gc-2", "tok-2"}} {
		if got := attrs[name].Identity; !got.Known || got.SessionID != want[0] || got.Token != want[1] {
			t.Fatalf("%s identity %+v, want %v from the inventory", name, got, want)
		}
	}
	if n, obs := f.count("inventory"), f.count("observe "); n != 1 || obs != 0 {
		t.Fatalf("%d inventory and %d observe ops for one pass, want one inventory and no per-name read; ops %q", n, obs, f.ops())
	}

	k, f := execKit(t, "active")
	f.box("s-gc-1", "gc-1", "3", "tok", "running", "alive")
	s := k.runKind()
	if s.Outcome == settledRefused || f.count("start ") != 0 || f.count("kill-object ") != 0 || f.count("stop ") != 0 {
		t.Fatalf("settlement %+v, ops %q; want the live own box kept: no start, no kill, no stop", s, f.ops())
	}
}

// Journey "provider swap": a swap onto an exec pack that is not v2-ready
// (its handshake lacks observe.v1 and report-attachment) gets no effect:
// a start refuses liveness-unsupported and the stop verb refuses, with no
// start, stop or kill op sent.
func TestExecJourneyNotReadyPackGetsNoEffect(t *testing.T) {
	k, f := execKit(t, "asleep")
	f.write("notready", "")
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeLivenessUnsupported || f.count("start ") != 0 {
		t.Fatalf("start: settlement %+v, ops %q; want refused liveness-unsupported, no start op", s, f.ops())
	}
	s, m, f := execStop(t, undesired(drainOrphaned), "running", "alive", func(f *execFixture) { f.write("notready", "") })
	if s.Outcome != settledRefused || f.count("stop ") != 0 || f.count("kill-object ") != 0 || m[session.DrainIntentReasonKey] != drainOrphaned {
		t.Fatalf("stop: settlement %+v, row %v, ops %q; want refused with nothing stopped", s, m, f.ops())
	}
}

// Journey "backend outage" on the effects: while `observe` fails, a start
// refuses (its vacancy read is unknown) and sends no start op; the stop
// verb's case is TestExecJourneyDrainAckFinalize's unknown observe, and the
// planner's is TestSimExecOutageMakesNoEffect.
func TestExecJourneyOutageStartsNothing(t *testing.T) {
	k, f := execKit(t, "asleep")
	f.fault("observe", true)
	f.fault("inventory", true)
	if s := k.runKind(); s.Outcome != settledRefused || f.count("start ") != 0 || f.count("kill-object ") != 0 {
		t.Fatalf("settlement %+v, ops %q; want refused with no start or kill op", s, f.ops())
	}
}

// screenScriptPath is contrib/session-scripts/gc-session-screen: under
// Bazel in the test's runfiles (//contrib:all_files), otherwise beside
// this source file.
func screenScriptPath(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "session-scripts", "gc-session-screen")
	}
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "contrib", "session-scripts", "gc-session-screen"))
}

// screenKit is the start kit over row gc-1 (state, generation gen, token
// tok) on the shipped gc-session-screen pack: its agent is a shell that
// runs sleep (the process name gc watches) and then waits on its terminal,
// so killing sleep leaves the box running with its agent dead.
func screenKit(t *testing.T, sp runtime.Provider, state, gen, tok string) *txKit {
	t.Helper()
	k := startKit(t, intentStart, state, "generation", gen, "instance_token", tok)
	k.it.Deadline = gatherNow.Add(70 * time.Second)
	row := k.p.World.Census.Rows[k.it.Key]
	accept := false
	tp := TemplateParams{TemplateName: "worker", Command: `sh -c "sleep 300; while :; do read x; done"`}
	tp.Hints.ProcessNames, tp.Hints.AcceptStartupDialogs = []string{"sleep"}, &accept
	k.p.World.Templates.entries[templateMemoKeyOf(row.Info)] = templateResolution{TP: tp}
	k.p.Runtime = sp
	return k
}

// Journeys "start" and "zombie" on real screen, through the shipped
// gc-session-screen: the start lands on its PreWake token with the box
// observed alive; with the agent killed in its running box, the next start
// recycles that exact object by kill-object and starts a new box. Skipped
// where screen, jq, flock or cksum is missing, or where there is no procfs
// to find the agent in (screenAgentPID), as on macOS.
func TestExecJourneyOnScreen(t *testing.T) {
	for _, tool := range []string{"screen", "jq", "flock", "cksum", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skipf("no procfs: %v", err)
	}
	state, err := os.MkdirTemp("", "gcj") // short: a screen socket path must fit 107 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_EXEC_STATE_DIR", state)
	sp := sessionexec.NewSeamBackedForCity(screenScriptPath(t), "test-city", t.TempDir())
	t.Cleanup(func() {
		_ = sp.Stop("s-gc-1")
		_ = os.RemoveAll(state)
	})
	// On a loaded host a real `observe` can run past v2's 4s bound, which
	// reads unknown: v2 retries such a read on a later pass, and so do these
	// reads and runs, a few times, a second apart. Only a timeout is retried:
	// an observe error must be its deadline, and a run refused
	// liveness-unknown must have taken the probe bound.
	pause := func() {
		tm := time.NewTimer(time.Second)
		defer tm.Stop()
		<-tm.C
	}
	observe := func() runtime.Liveness {
		var err error
		for range 5 {
			var l runtime.Liveness
			if l, err = sp.(runtime.LivenessObserverWithError).ObserveLivenessWithError("s-gc-1", []string{"sleep"}); err == nil {
				return l
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("observe: %v, not a timeout", err)
			}
			pause()
		}
		t.Fatalf("observe: %v", err)
		return runtime.Liveness{}
	}
	run := func(k *txKit) settlement {
		var s settlement
		for range 5 {
			began := time.Now()
			if s = k.runKind(); s.Outcome != settledRefused || s.Cause != causeLivenessUnknown {
				break
			}
			if took := time.Since(began); took < 4*time.Second {
				t.Fatalf("settlement %+v after %s: an unknown read within the probe bound is no timeout", s, took)
			}
			pause()
		}
		return s
	}

	k := screenKit(t, sp, "asleep", "3", "tok")
	if s := run(k); s.Outcome != settledLanded {
		t.Fatalf("start: settlement %+v, want landed", s)
	}
	first := observe()
	tok, err := sp.GetMeta("s-gc-1", "GC_INSTANCE_TOKEN")
	if err != nil || !first.Running || !first.Alive || tok == "" || tok != k.meta("instance_token") {
		t.Fatalf("after the start: %+v, box token %q (%v), row token %q; want alive on the committed token", first, tok, err, k.meta("instance_token"))
	}

	// Kill the agent (sleep, the box shell's child): the box keeps running.
	agent := screenAgentPID(t, state)
	if err := syscall.Kill(agent, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline, tick := time.Now().Add(10*time.Second), time.NewTicker(100*time.Millisecond)
	defer tick.Stop()
	for l := observe(); l.Alive || !l.Running; l = observe() {
		if time.Now().After(deadline) {
			t.Fatalf("after killing the agent: %+v, want a running box with its agent dead", l)
		}
		<-tick.C
	}

	k2 := screenKit(t, sp, "asleep", k.meta("generation"), k.meta("instance_token"))
	if s := run(k2); s.Outcome != settledLanded {
		t.Fatalf("recycle: settlement %+v, want landed", s)
	}
	second := observe()
	if !second.Running || !second.Alive || second.ObjectID == first.ObjectID {
		t.Fatalf("after the recycle: %+v, first object %q; want a new box alive", second, first.ObjectID)
	}
}

// screenAgentPID is the pid of the sleep the box's window shell runs: the
// window pid comes from the script's own record (<city key>.meta/
// s-gc-1.window), and
// its child is found in /proc.
func screenAgentPID(t *testing.T, state string) int {
	t.Helper()
	recs, _ := filepath.Glob(filepath.Join(state, "*.meta", "s-gc-1.window"))
	if len(recs) != 1 {
		t.Fatalf("window records %v, want one", recs)
	}
	data, err := os.ReadFile(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data)) // "<boot id> <pid> <start>"
	if len(fields) < 2 {
		t.Fatalf("window record %q", data)
	}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range stats {
		stat, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || !strings.Contains(s[:i], "(sleep") {
			continue
		}
		if rest := strings.Fields(s[i+1:]); len(rest) > 1 && rest[1] == fields[1] {
			pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p)))
			if err == nil {
				return pid
			}
		}
	}
	t.Fatalf("no sleep under the window process %s", fields[1])
	return 0
}

// Journeys on a dead box's screen (F-1): v2 reads an exec corpse's screen
// through the script's `peek`, even though the box is not running. A rate
// limit screen quarantines the row and a terminal provider error parks it:
// neither is recycled (no kill-object, no start). "No screen" (exit 66)
// is a known-empty screen: the corpse is recycled. Any other peek failure
// holds the recycle (refused peek-error, the paced hold the tmux pane
// errors take): nothing killed, nothing started; the retry after the peek
// recovers reads the screen and decides on it.
func TestExecJourneyDeadBoxScreen(t *testing.T) {
	recent := []string{"last_woke_at", gatherNow.Add(-5 * time.Second).Format(time.RFC3339)}
	for _, c := range []struct {
		name, pane, reason string
		meta               []string
		noScreen, fault    bool
		recycled           bool
		cause              string
	}{
		{name: "rate limit quarantines", pane: "Usage limit reached", reason: string(session.SleepReasonRateLimit), meta: recent},
		{name: "terminal error parks", pane: "Error: model_not_found", reason: string(session.SleepReasonProviderTerminalError)},
		{name: "no screen recycles", pane: "Usage limit reached", meta: recent, noScreen: true, recycled: true},
		{name: "a peek failure holds", pane: "Usage limit reached", meta: recent, fault: true, cause: causePeekError},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, f := execKitOn(t, "asleep", c.meta...)
			f.box("s-gc-1", "gc-1", "3", "tok", "dead", "")
			f.set("pane", c.pane)
			if c.noScreen {
				f.set("noscreen", "")
			}
			f.fault("peek", c.fault)
			s := k.runKind()
			if f.count("peek s-gc-1") == 0 {
				t.Fatalf("settlement %+v, ops %q; want the dead box's screen read by peek", s, f.ops())
			}
			kills, starts := f.count("kill-object "), f.count("start ")
			switch {
			case c.recycled && (kills != 1 || starts != 1 || s.Outcome != settledLanded):
				t.Fatalf("settlement %+v, ops %q; want the corpse killed and the start landed", s, f.ops())
			case !c.recycled && (kills != 0 || starts != 0):
				t.Fatalf("settlement %+v, ops %q; want nothing killed or started", s, f.ops())
			case c.reason != "" && (k.meta("sleep_reason") != c.reason || s.Outcome != settledLanded):
				t.Fatalf("settlement %+v, sleep_reason %q; want %q", s, k.meta("sleep_reason"), c.reason)
			case c.reason == string(session.SleepReasonRateLimit) && k.meta("quarantined_until") == "":
				t.Fatal("a rate-limit screen did not quarantine the row")
			case c.cause != "" && (s.Outcome != settledRefused || s.Cause != c.cause || k.meta("sleep_reason") != ""):
				t.Fatalf("settlement %+v, sleep_reason %q; want refused %s with the row untouched", s, k.meta("sleep_reason"), c.cause)
			}
			if !c.fault {
				return
			}
			// The hold is no wedge: the retry reads the screen again, and once
			// the peek answers, the row's screen decides (here a rate limit:
			// quarantined, still not recycled).
			f.fault("peek", false)
			if s := k.runKind(); s.Outcome != settledLanded || k.meta("sleep_reason") != string(session.SleepReasonRateLimit) || f.count("kill-object ") != 0 {
				t.Fatalf("retry after the peek recovered: settlement %+v, sleep_reason %q, ops %q; want quarantined", s, k.meta("sleep_reason"), f.ops())
			}
		})
	}
}
