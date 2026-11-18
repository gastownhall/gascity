package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// This file is the observe group: the observe.v1 profile reconciler v2
// reads (PAR-EXEC; docs/reference/exec-session-provider.md, "observe.v1").
// Every probe SKIPs unless the handshake declares observe.v1, and a probe
// for a behavior behind a further token (report-attachment,
// conformance-faults, city-scoped, report-activity) SKIPs unless that is
// declared too.

// observeBound is v2's probe bound for one observe (exec probeOpTimeout).
const observeBound = 4 * time.Second

// observeAgentNotRunning is a process name no box runs.
const observeAgentNotRunning = "gc-rpp-no-such-process"

// identityKeys are the identity env keys observe.v1 reports.
var identityKeys = []string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN", "GT_PROCESS_NAMES"}

// observeObject is a box's object as a kill-object request names it.
type observeObject struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// objectOf is an observation's object.
func objectOf(o sessionexec.Observation) observeObject {
	return observeObject{ID: o.ObjectID, Created: o.ObjectCreated}
}

// boxConfig is a start config the observe group sends.
type boxConfig struct {
	WorkDir      string            `json:"work_dir,omitempty"`
	Command      string            `json:"command,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	ProcessNames []string          `json:"process_names,omitempty"`
	FreshOnly    bool              `json:"fresh_only,omitempty"`
}

// agentProcess is the process name of the runner's command (its first
// word, "sleep" by default).
func (r *runner) agentProcess() string {
	if f := strings.Fields(r.opts.Command); len(f) > 0 {
		return f[0]
	}
	return "sleep"
}

// identityFor is the identity env the observe group starts name with.
func (r *runner) identityFor(name, processNames string) map[string]string {
	return map[string]string{
		"GC_SESSION_ID":     name + "-sid",
		"GC_RUNTIME_EPOCH":  "1",
		"GC_INSTANCE_TOKEN": name + "-tok",
		"GT_PROCESS_NAMES":  processNames,
	}
}

// startBox starts name with identity env and processNames under env.
func (r *runner) startBox(ctx context.Context, env []string, name string, processNames []string, freshOnly bool) outcome {
	return r.startWithIdentity(ctx, env, name, r.identityFor(name, strings.Join(processNames, ",")), processNames, freshOnly)
}

// startWithIdentity starts name with the identity env given and
// processNames under env.
func (r *runner) startWithIdentity(ctx context.Context, env []string, name string, identity map[string]string, processNames []string, freshOnly bool) outcome {
	cfg, _ := json.Marshal(boxConfig{
		WorkDir:      r.opts.WorkDir,
		Command:      r.opts.Command,
		Env:          identity,
		ProcessNames: processNames,
		FreshOnly:    freshOnly,
	})
	return r.opEnv(ctx, r.opts.StartTimeout, env, cfg, "start", name)
}

// observe runs observe for name under env with processNames on stdin and
// reads the answer as gc does (exec.ParseObserve): an answer gc would read
// as unknown, state unknown included, is an error.
func (r *runner) observe(ctx context.Context, env []string, name string, processNames ...string) (sessionexec.Observation, outcome) {
	var stdin []byte
	if len(processNames) > 0 {
		stdin = []byte(strings.Join(processNames, "\n") + "\n")
	}
	res := r.opEnv(ctx, r.opts.OpTimeout, env, stdin, "observe", name)
	var o sessionexec.Observation
	if res.ok() {
		var err error
		if o, err = sessionexec.ParseObserve([]byte(res.stdout)); err != nil {
			res.err = fmt.Errorf("observe %s: %w", name, err)
		}
	}
	return o, res
}

// inventory runs inventory under env and reads it as gc does
// (exec.ParseInventory).
func (r *runner) inventory(ctx context.Context, env []string) (sessionexec.Inventory, outcome) {
	res := r.opEnv(ctx, r.opts.OpTimeout, env, nil, "inventory")
	var inv sessionexec.Inventory
	if res.ok() {
		var err error
		if inv, err = sessionexec.ParseInventory([]byte(res.stdout)); err != nil {
			res.err = fmt.Errorf("inventory: %w", err)
		}
	}
	return inv, res
}

// killObject runs kill-object for name's object in mode.
func (r *runner) killObject(ctx context.Context, name string, obj observeObject, mode string) outcome {
	req, _ := json.Marshal(struct {
		Object observeObject `json:"object"`
		Mode   string        `json:"mode"`
	}{obj, mode})
	return r.opTimeout(ctx, r.opts.OpTimeout, req, "kill-object", name)
}

// observeDeclared is the observe group's gate.
func observeDeclared(hs runtime.ProtocolInfo) (Status, string, bool) {
	if !hs.Has(runtime.ProtocolCapabilityObserveV1) {
		return StatusSkip, "observe.v1 not declared", false
	}
	return StatusPass, "", true
}

// presentObserve requires observe of name to answer state, with its object
// when the box is present.
func presentObserve(ctx context.Context, r *runner, name, state string, processNames ...string) (sessionexec.Observation, Status, string) {
	o, res := r.observe(ctx, nil, name, processNames...)
	switch {
	case res.unsupported:
		return o, StatusFail, "observe returned exit 2 although observe.v1 is declared"
	case res.err != nil:
		return o, StatusFail, res.err.Error()
	case o.State != state:
		return o, StatusFail, fmt.Sprintf("observe %s = state %q, want %q", name, o.State, state)
	}
	return o, StatusPass, ""
}

// startOrFail starts name with processNames and returns a failing status if
// it cannot.
func startOrFail(ctx context.Context, r *runner, env []string, name string, processNames ...string) (Status, string) {
	res := r.startBox(ctx, env, name, processNames, false)
	switch {
	case res.unsupported:
		return StatusFail, "start returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start failed: " + res.err.Error()
	}
	return StatusPass, ""
}

// sameIdentity reports whether got carries exactly want's identity keys.
func sameIdentity(got, want map[string]string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	for _, key := range identityKeys {
		if got[key] != want[key] {
			return false
		}
	}
	return true
}

func probeObserveDeclaredOps(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if _, res := r.observe(ctx, nil, name); res.unsupported {
		return StatusFail, "observe returned exit 2 although observe.v1 is declared"
	}
	if _, res := r.inventory(ctx, nil); res.unsupported {
		return StatusFail, "inventory returned exit 2 although observe.v1 is declared"
	}
	if res := r.killObject(ctx, name, observeObject{ID: "gc-rpp-none", Created: "0"}, "corpse"); res.unsupported {
		return StatusFail, "kill-object returned exit 2 although observe.v1 is declared"
	}
	return StatusPass, "observe, inventory and kill-object answer"
}

func probeObserveAbsentNeverStarted(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	if _, s, d := presentObserve(ctx, r, r.nextName(), "absent"); s != StatusPass {
		return s, d
	}
	return StatusPass, "a never-started name is absent"
}

// observePollInterval paces the observes RPP-OBSERVE-003 takes while a
// start runs.
const observePollInterval = 20 * time.Millisecond

// probeObserveRunningWithIdentity starts a box and observes it all the
// while: any answer that shows the box present must carry the start env's
// identity, or null while it is not yet readable, never an empty or partial
// one (which gc would read as an ownerless box); the first observe after
// start returns must carry it.
func probeObserveRunningWithIdentity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	want := r.identityFor(name, r.agentProcess())
	started := make(chan outcome, 1)
	go func() { started <- r.startBox(ctx, nil, name, []string{r.agentProcess()}, false) }()
	defer r.stop(ctx, name)
	tick := time.NewTicker(observePollInterval)
	defer tick.Stop()
	var gap string
	var res outcome
	for running := true; running; {
		select {
		case res = <-started:
			running = false
		case <-tick.C:
		}
		if o, ores := r.observe(ctx, nil, name); gap == "" && ores.ok() && (o.State == "running" || o.State == "dead") && o.Identity != nil && !sameIdentity(o.Identity, want) {
			gap = fmt.Sprintf("an observe during start saw the box with identity %v, not the start env's: identity must be readable before the box is visible (or answer null)", o.Identity)
		}
	}
	switch {
	case res.unsupported:
		return StatusFail, "start returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start failed: " + res.err.Error()
	case gap != "":
		return StatusFail, gap
	}
	o, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	if o.Identity == nil {
		return StatusFail, "the first observe after start answered identity null: identity must be readable before the box is visible"
	}
	if !sameIdentity(o.Identity, want) {
		return StatusFail, fmt.Sprintf("observe identity %v, want the start env's %v", o.Identity, want)
	}
	return StatusPass, "running, with its object and the start env's identity from the first answer"
}

func probeObserveAbsentAfterStop(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	if res := r.stop(ctx, name); res.err != nil || res.unsupported {
		return StatusFail, fmt.Sprintf("stop failed: %v (exit 2: %v)", res.err, res.unsupported)
	}
	if _, s, d := presentObserve(ctx, r, name, "absent"); s != StatusPass {
		return s, d
	}
	return expectRunning(ctx, r, name, false, "after stop")
}

func probeObserveObjectStable(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	first, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		r.stop(ctx, name)
		return s, d
	}
	again, s, d := presentObserve(ctx, r, name, "running")
	r.stop(ctx, name)
	if s != StatusPass {
		return s, d
	}
	if objectOf(again) != objectOf(first) {
		return StatusFail, fmt.Sprintf("the object changed between two observes of one box: %+v then %+v", objectOf(first), objectOf(again))
	}
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	next, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	if next.ObjectID == first.ObjectID {
		return StatusFail, fmt.Sprintf("a new box under the name has the old object id %q", first.ObjectID)
	}
	return StatusPass, "stable for the box, new for a new box"
}

func probeObserveAgentLiveness(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name, r.agentProcess()); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	live, s, d := presentObserve(ctx, r, name, "running", r.agentProcess())
	if s != StatusPass {
		return s, d
	}
	if live.Agent != "alive" {
		return StatusFail, fmt.Sprintf("agent for the running process %q = %q, want alive", r.agentProcess(), live.Agent)
	}
	bogus, s, d := presentObserve(ctx, r, name, "running", observeAgentNotRunning)
	if s != StatusPass {
		return s, d
	}
	if bogus.Agent != "dead" && bogus.Agent != "unknown" {
		return StatusFail, fmt.Sprintf("agent for a process the box does not run = %q, want dead or unknown", bogus.Agent)
	}
	return StatusPass, "alive for the real process, " + bogus.Agent + " for another"
}

func probeObserveInventoryComplete(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	listed := []string{r.nextName(), r.nextName()}
	stopped := r.nextName()
	for _, name := range append(listed, stopped) {
		if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
			return s, d
		}
		defer r.stop(ctx, name)
	}
	r.stop(ctx, stopped)
	want := map[string]sessionexec.Observation{}
	for _, name := range listed {
		o, s, d := presentObserve(ctx, r, name, "running")
		if s != StatusPass {
			return s, d
		}
		want[name] = o
	}
	inv, res := r.inventory(ctx, nil)
	switch {
	case res.unsupported:
		return StatusFail, "inventory returned exit 2 although observe.v1 is declared"
	case res.err != nil:
		return StatusFail, res.err.Error()
	case !inv.Complete:
		return StatusFail, "inventory answered complete:false with every box readable"
	}
	for _, n := range inv.Names {
		if n == stopped {
			return StatusFail, fmt.Sprintf("inventory lists %s after it was stopped", stopped)
		}
	}
	for name, o := range want {
		e, ok := inv.Entries[name]
		switch {
		case !ok:
			return StatusFail, fmt.Sprintf("a complete inventory omits the running %s, or lists it without its state and object", name)
		case e.Incarnation != o.ObjectID+":"+o.ObjectCreated:
			return StatusFail, fmt.Sprintf("inventory object for %s = %q, want observe's %q", name, e.Incarnation, o.ObjectID+":"+o.ObjectCreated)
		case !e.IdentityKnown || !sameIdentity(e.Identity, o.Identity):
			return StatusFail, fmt.Sprintf("inventory identity for %s differs from observe's", name)
		}
	}
	return StatusPass, "lists every started box with observe's object and identity, and no stopped one"
}

func probeObserveMetaIsIdentity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name, r.agentProcess()); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	o, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	for _, key := range identityKeys {
		res := r.op(ctx, "get-meta", name, key)
		if !res.ok() {
			return StatusFail, fmt.Sprintf("get-meta %s failed: %v (exit 2: %v)", key, res.err, res.unsupported)
		}
		if got := strings.TrimSpace(res.stdout); got != o.Identity[key] {
			return StatusFail, fmt.Sprintf("get-meta %s = %q, observe identity says %q", key, got, o.Identity[key])
		}
	}
	const token = "gc-rpp-retoken"
	if res := r.opTimeout(ctx, r.opts.OpTimeout, []byte(token), "set-meta", name, "GC_INSTANCE_TOKEN"); !res.ok() {
		return StatusFail, fmt.Sprintf("set-meta GC_INSTANCE_TOKEN failed: %v (exit 2: %v)", res.err, res.unsupported)
	}
	after, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	if got := after.Identity["GC_INSTANCE_TOKEN"]; got != token {
		return StatusFail, fmt.Sprintf("observe token after set-meta = %q, want %q", got, token)
	}
	return StatusPass, "get-meta and observe report one identity"
}

func probeObserveFreshOnly(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	before, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	// The refused start carries its own attempt's token, as every fresh
	// start gc makes does: a script that seeds it before its held-name
	// check would give the live box to that attempt.
	attempt := r.identityFor(name, "")
	attempt["GC_INSTANCE_TOKEN"] = name + "-tok-refused"
	again := r.startWithIdentity(ctx, nil, name, attempt, nil, true)
	switch {
	case again.ok():
		return StatusFail, "a fresh_only start over a held name succeeded"
	case again.unsupported:
		return StatusFail, "a fresh_only start returned exit 2"
	case again.exitCode != 73 && !collisionPhrase(again.err.Error()):
		return StatusFail, fmt.Sprintf("a fresh_only start over a held name exited %d without the collision phrase; want 73 or already exists/already running", again.exitCode)
	}
	after, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	if objectOf(after) != objectOf(before) {
		return StatusFail, fmt.Sprintf("the refused fresh_only start replaced the box: object %+v then %+v", objectOf(before), objectOf(after))
	}
	if !sameIdentity(after.Identity, before.Identity) {
		return StatusFail, "the refused fresh_only start changed the box's identity"
	}
	return StatusPass, "refused, the box and its identity untouched"
}

func collisionPhrase(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "already running")
}

func probeObserveKillObject(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	kill := func(name string, obj observeObject, mode, want string) (Status, string) {
		res := r.killObject(ctx, name, obj, mode)
		if !res.ok() {
			return StatusFail, fmt.Sprintf("kill-object %s %s failed: %v (exit 2: %v)", mode, name, res.err, res.unsupported)
		}
		if got := strings.TrimSpace(res.stdout); got != want {
			return StatusFail, fmt.Sprintf("kill-object %s on %s = %q, want %q", mode, name, got, want)
		}
		return StatusPass, ""
	}
	held := func(name, after string) (Status, string) {
		if _, s, d := presentObserve(ctx, r, name, "running"); s != StatusPass {
			return StatusFail, after + " took the box: " + d
		}
		return StatusPass, ""
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name, r.agentProcess()); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	old, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	r.stop(ctx, name)
	if s, d := startOrFail(ctx, r, nil, name, r.agentProcess()); s != StatusPass {
		return s, d
	}
	cur, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	for _, c := range []struct {
		obj        observeObject
		mode, want string
		what       string
	}{
		{objectOf(old), "corpse", "gone", "a stale object's kill"},
		{observeObject{ID: cur.ObjectID, Created: "gc-rpp-wrong-created"}, "zombie", "gone", "a kill of the current id created at another time"},
		{objectOf(cur), "corpse", "live", "a corpse kill of a running box"},
		{objectOf(cur), "zombie", "changed", "a zombie kill of a box whose agent lives"},
	} {
		if s, d := kill(name, c.obj, c.mode, c.want); s != StatusPass {
			return s, c.what + ": " + d
		}
		if s, d := held(name, c.what); s != StatusPass {
			return s, d
		}
	}
	zombie := r.nextName()
	if s, d := startOrFail(ctx, r, nil, zombie, observeAgentNotRunning); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, zombie)
	z, s, d := presentObserve(ctx, r, zombie, "running")
	if s != StatusPass {
		return s, d
	}
	if s, d := kill(zombie, objectOf(z), "zombie", "killed"); s != StatusPass {
		return s, d
	}
	if _, s, d := presentObserve(ctx, r, zombie, "absent"); s != StatusPass {
		return StatusFail, "after its kill the zombie's box is still held: " + d
	}
	return StatusPass, "gone, live, changed and killed as the object and its agent warrant"
}

func probeObserveAttachedStrict(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	if !hs.Has(runtime.ProtocolCapabilityReportAttachment) {
		return StatusSkip, "report-attachment not declared"
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	res := r.op(ctx, "is-attached", name)
	if !res.ok() {
		return StatusFail, fmt.Sprintf("is-attached failed on a running box: %v (exit 2: %v)", res.err, res.unsupported)
	}
	if got := strings.TrimSpace(res.stdout); got != "true" && got != "false" {
		return StatusFail, fmt.Sprintf("is-attached = %q, want exactly true or false", got)
	}
	return StatusPass, "true or false"
}

// observeFaultsToken is the handshake token that opts into fault injection.
const observeFaultsToken = "conformance-faults"

func probeObserveFaults(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	if !hs.Has(observeFaultsToken) {
		return StatusSkip, observeFaultsToken + " not declared"
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	fault := []string{"GC_RPP_FAULT=unavailable"}
	if o, res := r.observe(ctx, fault, name); res.ok() {
		return StatusFail, fmt.Sprintf("observe under a backend fault answered %q, want unknown or a failure", o.State)
	}
	if inv, res := r.inventory(ctx, fault); res.ok() && inv.Complete {
		return StatusFail, "inventory under a backend fault answered complete, want complete:false or a failure"
	}
	if hs.Has(runtime.ProtocolCapabilityReportAttachment) {
		if res := r.opEnv(ctx, r.opts.OpTimeout, fault, nil, "is-attached", name); res.ok() {
			return StatusFail, fmt.Sprintf("is-attached under a backend fault answered %q, want a failure", strings.TrimSpace(res.stdout))
		}
	}
	return StatusPass, "a fault reads unknown, never absent or detached"
}

func probeObserveWithinBound(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	for _, n := range []string{name, r.nextName()} {
		start := time.Now()
		_, res := r.observe(ctx, nil, n)
		if took := time.Since(start); took >= observeBound {
			return StatusFail, fmt.Sprintf("observe %s took %v, want under %v (v2 abandons the probe there)", n, took.Round(time.Millisecond), observeBound)
		}
		if res.unsupported {
			return StatusFail, "observe returned exit 2 although observe.v1 is declared"
		}
	}
	return StatusPass, "within " + observeBound.String()
}

// observeCityToken is the handshake token that claims city scoping.
const observeCityToken = "city-scoped"

func probeObserveCityScoped(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	if !hs.Has(observeCityToken) {
		return StatusSkip, observeCityToken + " not declared"
	}
	cityA := []string{"GC_EXEC_CITY_NAME=rpp-a", "GC_EXEC_CITY_PATH=/gc-rpp/city-a"}
	cityB := []string{"GC_EXEC_CITY_NAME=rpp-b", "GC_EXEC_CITY_PATH=/gc-rpp/city-b"}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, cityA, name); s != StatusPass {
		return s, d
	}
	defer r.opEnv(ctx, r.opts.OpTimeout, cityA, nil, "stop", name)
	inv, res := r.inventory(ctx, cityB)
	if !res.ok() {
		return StatusFail, fmt.Sprintf("inventory under another city failed: %v (exit 2: %v)", res.err, res.unsupported)
	}
	for _, n := range inv.Names {
		if n == name {
			return StatusFail, fmt.Sprintf("%s, started under city A, is listed under city B", name)
		}
	}
	mine, res := r.inventory(ctx, cityA)
	if !res.ok() {
		return StatusFail, fmt.Sprintf("inventory under its own city failed: %v", res.err)
	}
	for _, n := range mine.Names {
		if n == name {
			return StatusPass, "listed under its own city only"
		}
	}
	return StatusFail, fmt.Sprintf("%s is not listed under the city it was started in", name)
}

func probeObserveInventoryActivity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	if !hs.Has(runtime.ProtocolCapabilityReportActivity) {
		return StatusSkip, "report-activity not declared"
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	inv, res := r.inventory(ctx, nil)
	if !res.ok() {
		return StatusFail, fmt.Sprintf("inventory failed: %v (exit 2: %v)", res.err, res.unsupported)
	}
	e, ok := inv.Entries[name]
	switch {
	case !ok:
		return StatusFail, fmt.Sprintf("inventory omits the running %s, or lists it without its state and object", name)
	case !e.ActivityKnown:
		return StatusFail, "inventory last_activity is null or not RFC3339 for a running session although report-activity is declared"
	}
	return StatusPass, "last_activity on every running session"
}

// probeObservePeekNoScreen: v2 reads a dead box's screen through `peek` to
// tell why it died before recycling it, so "no screen" must be an answer
// (exit 66, runtime.ExitCodeNoScreen) and never an empty success: a name
// never started, and one stopped, answer it; a running box's peek
// succeeds.
func probeObservePeekNoScreen(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	noScreen := func(when string) (Status, string, bool) {
		res := r.op(ctx, "peek", name, "10")
		if res.unsupported || res.ok() || res.exitCode != runtime.ExitCodeNoScreen {
			return StatusFail, fmt.Sprintf("peek of %s: exit %d (exit 2: %v), want %d, no screen", when, res.exitCode, res.unsupported, runtime.ExitCodeNoScreen), false
		}
		return StatusPass, "", true
	}
	if s, d, ok := noScreen("a never-started name"); !ok {
		return s, d
	}
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	if res := r.op(ctx, "peek", name, "10"); !res.ok() {
		r.stop(ctx, name)
		return StatusFail, fmt.Sprintf("peek of a running box failed: %v (exit 2: %v)", res.err, res.unsupported)
	}
	r.stop(ctx, name)
	if s, d, ok := noScreen("a stopped name"); !ok {
		return s, d
	}
	return StatusPass, "a running box's screen; exit 66 for no box"
}
