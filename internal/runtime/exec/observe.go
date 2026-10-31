package exec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the observe.v1 profile (PAR-EXEC): the `observe` and
// `kill-object` clients, their mapping onto the runtime contract, and the
// FreshOnly start. Every method gates on v2Ready, so a script that does not
// declare the profile behaves exactly as before. stdout of these ops can
// carry GC_INSTANCE_TOKEN, so no error built here ever echoes it: parse
// errors name the op and a byte count.

// probeOpTimeout bounds `observe` (and a v2-ready `is-attached`) inside v2's
// 5s fence bound: the script gets SIGTERM at 4s and SIGKILL 2s later
// (execgrace), so an abandoned probe does not outlive the fence by much.
const probeOpTimeout = 4 * time.Second

// observeOutputCap is the most stdout one `observe` answer may print.
const observeOutputCap = 1 << 20

// exitCodeFreshCollision is a fresh_only `start` or `provision` refusing a
// name a box holds (EX_CANTCREAT).
const exitCodeFreshCollision = 73

// absentPollInterval paces Relaunch's wait for its stopped box to go.
var absentPollInterval = 200 * time.Millisecond

// The `observe` states and agent answers.
const (
	stateRunning = "running"
	stateDead    = "dead"
	stateAbsent  = "absent"
	stateUnknown = "unknown"

	agentAlive   = "alive"
	agentDead    = "dead"
	agentUnknown = "unknown"
)

// observeObject is a box's identity on the backend: stable for its life,
// new for every new box under the name.
type observeObject struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// observeWire is one `observe` answer as the script prints it.
type observeWire struct {
	State    string          `json:"state"`
	Object   *observeObject  `json:"object"`
	Agent    string          `json:"agent"`
	AgentPID string          `json:"agent_pid"`
	Identity json.RawMessage `json:"identity"`
}

// observation is a well-formed answer that is not unknown.
type observation struct {
	state    string
	object   observeObject
	agent    string
	agentPID string
	// identity is the box's gc identity env; nil while it is not readable
	// (`"identity": null`), empty for a box with no gc identity.
	identity map[string]string
}

// present reports a box holding the name, live or dead.
func (o observation) present() bool {
	return o.state == stateRunning || o.state == stateDead
}

// parseObserve reads one `observe` answer. Anything but exactly one
// well-formed object whose state is running, dead or absent is an error: an
// unknown state, a present box without its object, a dead box with a live
// agent, or trailing bytes. No error carries the answer's bytes.
func parseObserve(out []byte) (observation, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var w observeWire
	if err := dec.Decode(&w); err != nil {
		return observation{}, fmt.Errorf("malformed answer (%d bytes)", len(out))
	}
	if _, err := dec.Token(); err != io.EOF {
		return observation{}, fmt.Errorf("trailing bytes after the answer (%d bytes)", len(out))
	}
	switch w.State {
	case stateAbsent:
		return observation{state: stateAbsent}, nil
	case stateRunning, stateDead:
	case stateUnknown:
		return observation{}, errors.New("state unknown")
	default:
		return observation{}, fmt.Errorf("unrecognized state (%d bytes)", len(out))
	}
	if w.Object == nil || strings.TrimSpace(w.Object.ID) == "" || strings.TrimSpace(w.Object.Created) == "" {
		return observation{}, errors.New("a present box with no object")
	}
	switch w.Agent {
	case "", agentAlive, agentDead, agentUnknown:
	default:
		return observation{}, fmt.Errorf("unrecognized agent (%d bytes)", len(out))
	}
	if w.State == stateDead && w.Agent == agentAlive {
		return observation{}, errors.New("a dead box with a live agent")
	}
	var identity map[string]string
	if len(w.Identity) > 0 {
		if err := json.Unmarshal(w.Identity, &identity); err != nil {
			return observation{}, fmt.Errorf("malformed identity (%d bytes)", len(out))
		}
	}
	return observation{state: w.State, object: *w.Object, agent: w.Agent, agentPID: w.AgentPID, identity: identity}, nil
}

// liveness maps an observation onto [runtime.Liveness]. Only a running box
// whose agent the script read as dead, with process names asked, is a dead
// agent; an omitted or unknown agent reads alive, as legacy ProcessAlive.
func (o observation) liveness(processNames []string) runtime.Liveness {
	switch o.state {
	case stateRunning:
		alive := len(agentNames(processNames)) == 0 || o.agent != agentDead
		return runtime.Liveness{Running: true, Alive: alive, ObjectID: o.object.ID, ObjectCreated: o.object.Created, PanePID: o.agentPID}
	case stateDead:
		return runtime.Liveness{Corpse: true, ObjectID: o.object.ID, ObjectCreated: o.object.Created}
	default:
		return runtime.Liveness{}
	}
}

// agentNames are the non-empty process names, trimmed.
func agentNames(processNames []string) []string {
	var out []string
	for _, n := range processNames {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// observe runs `observe <name>` under probeOpTimeout with processNames on
// stdin. Any failure, or an answer of unknown, wraps
// [runtime.ErrRuntimeUnavailable]: unknown is never absent.
func (p *Provider) observe(ctx context.Context, name string, processNames []string) (observation, error) {
	var stdin []byte
	if names := agentNames(processNames); len(names) > 0 {
		stdin = []byte(strings.Join(names, "\n") + "\n")
	}
	out, err := p.runCapped(ctx, probeOpTimeout, observeOutputCap, stdin, "observe", name)
	if err != nil {
		return observation{}, fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	o, err := parseObserve([]byte(out))
	if err != nil {
		return observation{}, fmt.Errorf("%w: exec provider %s observe %s: %w", runtime.ErrRuntimeUnavailable, p.script, name, err)
	}
	return o, nil
}

// ObserveLivenessWithError implements [runtime.LivenessObserverWithError].
// On a v2-ready pack it is one fresh `observe`; otherwise it is the
// IsRunning and ProcessAlive read runtime.ObserveLivenessWithError makes for
// a provider without the capability, with a nil error.
func (p *Provider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if !p.v2Ready() {
		return runtime.ObserveLiveness(p, name, processNames), nil
	}
	o, err := p.observe(context.Background(), name, processNames)
	if err != nil {
		return runtime.Liveness{}, err
	}
	return o.liveness(processNames), nil
}

// LivenessReadsFresh implements [runtime.FreshByConstruction]: a v2-ready
// script's `observe` reads its backend on every call. Safe on a nil
// provider.
func (p *Provider) LivenessReadsFresh() bool {
	return p.v2Ready()
}

// IdentityObservable implements [runtime.IdentityObserver]: true on a
// v2-ready pack. Safe on a nil provider.
func (p *Provider) IdentityObservable() bool {
	return p.v2Ready()
}

// ObserveIdentity implements [runtime.IdentityObserver] with one `observe`
// and no process names: absent is [runtime.ErrSessionNotFound]; unknown,
// or a box whose identity is not yet readable, wraps
// [runtime.ErrRuntimeUnavailable], never an empty identity.
func (p *Provider) ObserveIdentity(name string) (map[string]string, error) {
	if !p.v2Ready() {
		return nil, fmt.Errorf("%w: exec provider %s: identity reads need %s", runtime.ErrRuntimeUnavailable, p.script, runtime.ProtocolCapabilityObserveV1)
	}
	o, err := p.observe(context.Background(), name, nil)
	if err != nil {
		return nil, err
	}
	if !o.present() {
		return nil, fmt.Errorf("exec provider %s: %w: %s", p.script, runtime.ErrSessionNotFound, name)
	}
	if o.identity == nil {
		return nil, fmt.Errorf("%w: exec provider %s observe %s: identity not yet readable", runtime.ErrRuntimeUnavailable, p.script, name)
	}
	return o.identity, nil
}

// killObjectRequest is `kill-object`'s stdin.
type killObjectRequest struct {
	Object   observeObject `json:"object"`
	Mode     string        `json:"mode"`
	AgentPID string        `json:"agent_pid,omitempty"`
}

// killObjectResults are `kill-object`'s answers.
var killObjectResults = map[string]runtime.SessionObjectKillResult{
	"killed":  runtime.SessionObjectKilled,
	"gone":    runtime.SessionObjectGone,
	"renamed": runtime.SessionObjectRenamed,
	"live":    runtime.SessionObjectLive,
	"changed": runtime.SessionObjectChanged,
}

// KillCorpseObject implements [runtime.SessionObjectKiller] with
// `kill-object` in corpse mode.
func (p *Provider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	return p.killObject(name, killObjectRequest{Object: observeObject{ID: objectID, Created: created}, Mode: "corpse"})
}

// KillZombieObject implements [runtime.SessionObjectKiller] with
// `kill-object` in zombie mode. An exec box may not know its agent's pid,
// so an empty panePID is allowed.
func (p *Provider) KillZombieObject(name, objectID, created, panePID string) (runtime.SessionObjectKillResult, error) {
	return p.killObject(name, killObjectRequest{Object: observeObject{ID: objectID, Created: created}, Mode: "zombie", AgentPID: panePID})
}

// killObject runs `kill-object <name>`, which re-checks the exact object
// and kills it, never by name. A request with no name, object id or
// creation time is refused before anything runs; a pack that is not
// v2-ready kills nothing.
func (p *Provider) killObject(name string, req killObjectRequest) (runtime.SessionObjectKillResult, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(req.Object.ID) == "" || strings.TrimSpace(req.Object.Created) == "" {
		return runtime.SessionObjectNotKilled, fmt.Errorf("%w: exec kill-object %q needs a name, an object id and a creation time", runtime.ErrInvalidSessionObject, name)
	}
	if !p.v2Ready() {
		return runtime.SessionObjectNotKilled, fmt.Errorf("%w: exec provider %s: object kills need %s", runtime.ErrSessionObjectKillUnsupported, p.script, runtime.ProtocolCapabilityObserveV1)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return runtime.SessionObjectNotKilled, fmt.Errorf("exec provider: marshaling kill-object request: %w", err)
	}
	out, err := p.run(data, "kill-object", name)
	if err != nil {
		return runtime.SessionObjectNotKilled, err
	}
	if res, ok := killObjectResults[strings.TrimSpace(out)]; ok {
		return res, nil
	}
	return runtime.SessionObjectNotKilled, fmt.Errorf("exec provider %s kill-object %s: unrecognized result (%d bytes)", p.script, name, len(out))
}

// startFresh is a [runtime.Config.FreshOnly] start: a new box or
// [runtime.ErrSessionExists], never a recycled, killed, reused or
// respawned one. It needs observe.v1, so a pack that is not v2-ready fails
// closed. A fresh `observe` must read the name absent (unknown defers with
// [runtime.ErrRuntimeUnavailable], nothing run); the `start` (or
// `provision`) op then carries fresh_only, and the script refuses a name
// held since with exit 73 or its collision phrase. No input fence is
// cleared: the name was vacant.
func (p *Provider) startFresh(ctx context.Context, name string, cfg runtime.Config) error {
	if !p.v2Ready() {
		return fmt.Errorf("%w: exec provider %s: a fresh-only start needs %s", runtime.ErrRuntimeUnavailable, p.script, runtime.ProtocolCapabilityObserveV1)
	}
	o, err := p.observe(ctx, name, nil)
	if err != nil {
		return err
	}
	if o.present() {
		return fmt.Errorf("%w: exec provider %s: a box holds %q", runtime.ErrSessionExists, p.script, name)
	}
	cfg, err = freshStartConfig(cfg)
	if err != nil {
		return fmt.Errorf("exec provider: minting an instance token: %w", err)
	}
	p.rememberReadyPrompt(name, cfg)
	if err := p.createFresh(ctx, name, cfg); err != nil {
		return p.cleanupAfterFreshStartFailure(name, cfg, err)
	}
	return nil
}

// createFresh creates the box and launches the agent: `provision` then a
// fresh launchAgent for a separable pack, otherwise the welded `start` op
// followed by finishStart, as Start runs it.
func (p *Provider) createFresh(ctx context.Context, name string, cfg runtime.Config) error {
	data, err := marshalStartConfig(cfg)
	if err != nil {
		return fmt.Errorf("exec provider: marshaling start config: %w", err)
	}
	if p.supportsSeparableLaunch() {
		if _, err := p.runWithContext(ctx, p.startTimeout, data, "provision", name); err != nil {
			return err
		}
		return p.launchAgent(ctx, name, cfg)
	}
	if _, err := p.runWithContext(ctx, p.startTimeout, data, "start", name); err != nil {
		return err
	}
	_, err = p.finishStart(ctx, name, cfg)
	return err
}

// freshStartConfig is cfg as a fresh start sends it: fresh_only set, and
// the identity the box must carry complete. GT_PROCESS_NAMES is added from
// ProcessNames when unset, and a GC_INSTANCE_TOKEN is minted when unset (as
// tmux does), so the failed-start cleanup can tell this attempt's box.
func freshStartConfig(cfg runtime.Config) (runtime.Config, error) {
	env := make(map[string]string, len(cfg.Env)+3)
	for k, v := range cfg.Env {
		env[k] = v
	}
	if names := strings.Join(agentNames(cfg.ProcessNames), ","); names != "" && strings.TrimSpace(env["GT_PROCESS_NAMES"]) == "" {
		env["GT_PROCESS_NAMES"] = names
	}
	if strings.TrimSpace(env["GC_INSTANCE_TOKEN"]) == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return cfg, err
		}
		env["GC_INSTANCE_TOKEN"] = hex.EncodeToString(b)
		// A minted token has no holder token to match; keep them aligned,
		// as tmux's backstop does.
		env["BEADS_HOLDER_TOKEN"] = env["GC_INSTANCE_TOKEN"]
	}
	cfg.Env = env
	cfg.FreshOnly = true
	return cfg, nil
}

// freshCollision reports a fresh start the script refused because a box
// holds the name: a `start` or `provision` op's exit 73, or its existing
// collision phrases. Nothing else is a collision, however it is worded or
// wrapped: a refused launch into the box this attempt provisioned is a
// start failure, whose cleanup stops that box when it carries our token.
func freshCollision(err error) bool {
	if exitCode(err) == exitCodeFreshCollision {
		return true
	}
	var e *opExitError
	return errors.As(err, &e) && isStartCollision(e.msg)
}

// cleanupAfterFreshStartFailure returns the error a failed fresh start
// reports, after stopping the box it may have left. A collision touches
// nothing. Otherwise the box is stopped only when a fresh `observe` shows it
// carrying this attempt's GC_INSTANCE_TOKEN, the proof it is this attempt's
// and not one created under the name since; anything else is left for v2's
// commit and abandon, which decide on their own fresh read. kill-object
// cannot serve here: its modes refuse a box whose agent lives.
func (p *Provider) cleanupAfterFreshStartFailure(name string, cfg runtime.Config, startErr error) error {
	if freshCollision(startErr) {
		if errors.Is(startErr, runtime.ErrSessionExists) {
			return startErr
		}
		return fmt.Errorf("%w: %w", runtime.ErrSessionExists, startErr)
	}
	if !p.carriesToken(name, cfg.Env["GC_INSTANCE_TOKEN"]) {
		return startErr
	}
	if stopErr := p.Stop(name); stopErr != nil {
		return errors.Join(startErr, fmt.Errorf("exec provider: cleanup after startup failure: %w", stopErr))
	}
	return startErr
}

// carriesToken reports a box holding name, read fresh, whose identity
// carries token.
func (p *Provider) carriesToken(name, token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	o, err := p.observe(context.Background(), name, nil)
	return err == nil && o.present() && strings.TrimSpace(o.identity["GC_INSTANCE_TOKEN"]) == token
}

// awaitAbsent polls `observe` until it reads name absent, within ctx and
// the provider's op timeout.
func (p *Provider) awaitAbsent(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	for {
		o, err := p.observe(ctx, name, nil)
		if err == nil && !o.present() {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("%w: a box still holds %q", runtime.ErrSessionExists, name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("exec provider %s: relaunch %q: not confirmed absent after stop: %w", p.script, name, err)
		case <-time.After(absentPollInterval):
		}
	}
}
