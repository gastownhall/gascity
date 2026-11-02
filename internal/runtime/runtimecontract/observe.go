package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
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

// observeObject is a box's object as observe and inventory report it.
type observeObject struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// observeAnswer is one observe answer, or one inventory entry.
type observeAnswer struct {
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Object       *observeObject  `json:"object"`
	Agent        string          `json:"agent"`
	Identity     json.RawMessage `json:"identity"`
	LastActivity *string         `json:"last_activity"`
}

// identity is the answer's identity, nil when it is null or absent.
func (a observeAnswer) identity() map[string]string {
	var id map[string]string
	if len(a.Identity) > 0 {
		_ = json.Unmarshal(a.Identity, &id)
	}
	return id
}

// inventoryAnswer is one inventory answer.
type inventoryAnswer struct {
	Complete *bool           `json:"complete"`
	Sessions []observeAnswer `json:"sessions"`
}

// decodeOne decodes exactly one JSON value from out into v.
func decodeOne(out string, v any) error {
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("malformed answer: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing bytes after the answer")
	}
	return nil
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
	cfg, _ := json.Marshal(boxConfig{
		WorkDir:      r.opts.WorkDir,
		Command:      r.opts.Command,
		Env:          r.identityFor(name, strings.Join(processNames, ",")),
		ProcessNames: processNames,
		FreshOnly:    freshOnly,
	})
	return r.opEnv(ctx, r.opts.StartTimeout, env, cfg, "start", name)
}

// observe runs observe for name under env with processNames on stdin.
func (r *runner) observe(ctx context.Context, env []string, name string, processNames ...string) (observeAnswer, outcome) {
	var stdin []byte
	if len(processNames) > 0 {
		stdin = []byte(strings.Join(processNames, "\n") + "\n")
	}
	res := r.opEnv(ctx, r.opts.OpTimeout, env, stdin, "observe", name)
	var a observeAnswer
	if res.ok() {
		if err := decodeOne(res.stdout, &a); err != nil {
			res.err = fmt.Errorf("observe %s: %w", name, err)
		}
	}
	return a, res
}

// inventory runs inventory under env.
func (r *runner) inventory(ctx context.Context, env []string) (inventoryAnswer, outcome) {
	res := r.opEnv(ctx, r.opts.OpTimeout, env, nil, "inventory")
	var inv inventoryAnswer
	if res.ok() {
		if err := decodeOne(res.stdout, &inv); err != nil {
			res.err = fmt.Errorf("inventory: %w", err)
		} else if inv.Complete == nil {
			res.err = fmt.Errorf("inventory: no complete flag")
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

// presentObserve requires observe of name to answer state with an object.
func presentObserve(ctx context.Context, r *runner, name, state string, processNames ...string) (observeAnswer, Status, string) {
	a, res := r.observe(ctx, nil, name, processNames...)
	switch {
	case res.unsupported:
		return a, StatusFail, "observe returned exit 2 although observe.v1 is declared"
	case res.err != nil:
		return a, StatusFail, res.err.Error()
	case a.State != state:
		return a, StatusFail, fmt.Sprintf("observe %s = state %q, want %q", name, a.State, state)
	case state != "absent" && (a.Object == nil || a.Object.ID == "" || a.Object.Created == ""):
		return a, StatusFail, fmt.Sprintf("observe %s answered %s with no object id and created time", name, state)
	}
	return a, StatusPass, ""
}

// startOrFail starts name and returns a failing status if it cannot.
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

func probeObserveRunningWithIdentity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if s, d, ok := observeDeclared(hs); !ok {
		return s, d
	}
	name := r.nextName()
	if s, d := startOrFail(ctx, r, nil, name, r.agentProcess()); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	a, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	got := a.identity()
	if got == nil {
		return StatusFail, "the first observe after start answered identity null: identity must be readable before the box is visible"
	}
	for key, want := range r.identityFor(name, r.agentProcess()) {
		if got[key] != want {
			return StatusFail, fmt.Sprintf("observe identity %s = %q, want the start env's %q", key, got[key], want)
		}
	}
	return StatusPass, "running, with its object and the start env's identity"
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
	if *again.Object != *first.Object {
		return StatusFail, fmt.Sprintf("the object changed between two observes of one box: %+v then %+v", *first.Object, *again.Object)
	}
	if s, d := startOrFail(ctx, r, nil, name); s != StatusPass {
		return s, d
	}
	defer r.stop(ctx, name)
	next, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	if next.Object.ID == first.Object.ID {
		return StatusFail, fmt.Sprintf("a new box under the name has the old object id %q", first.Object.ID)
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
	if bogus.Agent == "alive" || bogus.Agent == "" {
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
	want := map[string]observeAnswer{}
	for _, name := range listed {
		a, s, d := presentObserve(ctx, r, name, "running")
		if s != StatusPass {
			return s, d
		}
		want[name] = a
	}
	inv, res := r.inventory(ctx, nil)
	switch {
	case res.unsupported:
		return StatusFail, "inventory returned exit 2 although observe.v1 is declared"
	case res.err != nil:
		return StatusFail, res.err.Error()
	case !*inv.Complete:
		return StatusFail, "inventory answered complete:false with every box readable"
	}
	got := map[string]observeAnswer{}
	for _, e := range inv.Sessions {
		got[e.Name] = e
	}
	if _, ok := got[stopped]; ok {
		return StatusFail, fmt.Sprintf("inventory lists %s after it was stopped", stopped)
	}
	for name, a := range want {
		e, ok := got[name]
		switch {
		case !ok:
			return StatusFail, fmt.Sprintf("a complete inventory omits the running %s", name)
		case e.Object == nil || *e.Object != *a.Object:
			return StatusFail, fmt.Sprintf("inventory object for %s = %+v, want observe's %+v", name, e.Object, *a.Object)
		case !sameIdentity(e.identity(), a.identity()):
			return StatusFail, fmt.Sprintf("inventory identity for %s differs from observe's", name)
		}
	}
	return StatusPass, "lists every started box with observe's object and identity, and no stopped one"
}

func sameIdentity(a, b map[string]string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	for _, key := range identityKeys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
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
	a, s, d := presentObserve(ctx, r, name, "running")
	if s != StatusPass {
		return s, d
	}
	id := a.identity()
	for _, key := range identityKeys {
		res := r.op(ctx, "get-meta", name, key)
		if !res.ok() {
			return StatusFail, fmt.Sprintf("get-meta %s failed: %v (exit 2: %v)", key, res.err, res.unsupported)
		}
		if got := strings.TrimSpace(res.stdout); got != id[key] {
			return StatusFail, fmt.Sprintf("get-meta %s = %q, observe identity says %q", key, got, id[key])
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
	if got := after.identity()["GC_INSTANCE_TOKEN"]; got != token {
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
	again := r.startBox(ctx, nil, name, nil, true)
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
	if *after.Object != *before.Object {
		return StatusFail, fmt.Sprintf("the refused fresh_only start replaced the box: object %+v then %+v", *before.Object, *after.Object)
	}
	return StatusPass, "refused, the box untouched"
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
	if s, d := kill(name, *old.Object, "corpse", "gone"); s != StatusPass {
		return s, d
	}
	if _, s, d := presentObserve(ctx, r, name, "running"); s != StatusPass {
		return StatusFail, "a stale object's kill took the new box: " + d
	}
	if s, d := kill(name, *cur.Object, "corpse", "live"); s != StatusPass {
		return s, d
	}
	if s, d := kill(name, *cur.Object, "zombie", "changed"); s != StatusPass {
		return s, d
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
	if s, d := kill(zombie, *z.Object, "zombie", "killed"); s != StatusPass {
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
	if a, res := r.observe(ctx, fault, name); res.ok() && (a.State == "absent" || a.State == "running" || a.State == "dead") {
		return StatusFail, fmt.Sprintf("observe under a backend fault answered %q, want unknown or a failure", a.State)
	}
	if inv, res := r.inventory(ctx, fault); res.ok() && *inv.Complete {
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
	for _, e := range inv.Sessions {
		if e.Name == name {
			return StatusFail, fmt.Sprintf("%s, started under city A, is listed under city B", name)
		}
	}
	mine, res := r.inventory(ctx, cityA)
	if !res.ok() {
		return StatusFail, fmt.Sprintf("inventory under its own city failed: %v", res.err)
	}
	for _, e := range mine.Sessions {
		if e.Name == name {
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
	for _, e := range inv.Sessions {
		if e.Name != name {
			continue
		}
		if e.LastActivity == nil {
			return StatusFail, "inventory last_activity is null for a running session although report-activity is declared"
		}
		if _, err := time.Parse(time.RFC3339, *e.LastActivity); err != nil {
			return StatusFail, fmt.Sprintf("inventory last_activity %q is not RFC3339", *e.LastActivity)
		}
		return StatusPass, "last_activity on every running session"
	}
	return StatusFail, fmt.Sprintf("inventory omits the running %s", name)
}
