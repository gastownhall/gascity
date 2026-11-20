package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the provision group's proc.launch half (RUNTIME-RPP-029): a
// script declaring proc.launch starts the agent in the box provision
// prepared through the `launch` op, so gc can un-weld start for a pack
// without tmux in its box. launch replaces a running agent unless the config
// is fresh_only (then exit 73), and types nothing: gc delivers the first
// turn itself. Every probe SKIPs unless the handshake declares proc.launch.

// launchProbeTurn is the first turn the probes put in launch's config, which
// launch must never type.
const launchProbeTurn = "gc-rpp-launch-must-not-type-this"

// launchConfig is the start config the launch probes send.
type launchConfig struct {
	WorkDir   string            `json:"work_dir,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Nudge     string            `json:"nudge,omitempty"`
	FreshOnly bool              `json:"fresh_only,omitempty"`
}

// launch sends cfg to name's launch op and returns the raw outcome.
func (r *runner) launch(ctx context.Context, name string, cfg launchConfig) outcome {
	cfg.WorkDir, cfg.Command = r.opts.WorkDir, r.opts.Command
	data, _ := json.Marshal(cfg) // a struct of strings, a map of strings and a bool always encodes
	return r.opTimeout(ctx, r.opts.StartTimeout, data, "launch", name)
}

// launchDeclared is the proc.launch requirements' gate.
func launchDeclared(hs runtime.ProtocolInfo) (Status, string, bool) {
	if !hs.Has(runtime.ProtocolCapabilityLaunch) {
		return StatusSkip, "proc.launch not declared", false
	}
	return StatusPass, "", true
}

// provisionAndLaunch provisions name, then launches its agent with cfg,
// returning a failing status when either op does not succeed. A fresh_only
// cfg is provisioned fresh_only too, the sequence gc's fresh start
// (createFresh) runs.
func provisionAndLaunch(ctx context.Context, r *runner, name string, cfg launchConfig) (Status, string) {
	switch prov := r.provision(ctx, name, cfg.FreshOnly); {
	case prov.unsupported:
		return StatusFail, "provision returned exit 2; proc.launch launches into the box provision prepares, so it needs provision"
	case prov.err != nil:
		return StatusFail, "provision failed: " + prov.err.Error()
	}
	switch res := r.launch(ctx, name, cfg); {
	case res.unsupported:
		return StatusFail, "launch returned exit 2 although the handshake declares proc.launch; a declared op cannot be unknown"
	case res.err != nil:
		return StatusFail, "launch after provision failed: " + res.err.Error()
	}
	return StatusPass, ""
}

// probeLaunchStartsAgent: after provision, launch starts the agent
// (is-running true) and types nothing, not even the nudge its config carries.
func probeLaunchStartsAgent(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := launchDeclared(hs); !ok {
		return status, detail
	}
	name := r.nextName()
	defer r.stop(ctx, name)
	if status, detail := provisionAndLaunch(ctx, r, name, launchConfig{Nudge: launchProbeTurn}); status != StatusPass {
		return status, detail
	}
	if res := r.isRunning(ctx, name); !res.ok() || strings.TrimSpace(res.stdout) != "true" {
		return StatusFail, fmt.Sprintf("is-running after launch = %q (%v), want \"true\": launch starts the agent", strings.TrimSpace(res.stdout), res.err)
	}
	// Typing is asynchronous in a real box: read the screen after a beat.
	select {
	case <-ctx.Done():
		return StatusFail, ctx.Err().Error()
	case <-time.After(200 * time.Millisecond):
	}
	switch res := r.peek(ctx, name, 120); {
	case res.unsupported:
		return StatusPass, "launch started the agent (no peek to read what it typed)"
	case res.err != nil:
		return StatusFail, "peek after launch failed: " + res.err.Error()
	case strings.Contains(res.stdout, launchProbeTurn):
		return StatusFail, "the screen after launch shows the config's nudge: launch types nothing, gc delivers the first turn"
	}
	return StatusPass, "launch started the agent and typed nothing"
}

// probeLaunchReplaces: a launch without fresh_only on a running agent
// replaces it. The replacement is read from observe's identity, which the
// launch's env seeds, so the requirement needs observe.v1.
func probeLaunchReplaces(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := launchDeclared(hs); !ok {
		return status, detail
	}
	if !hs.Has(runtime.ProtocolCapabilityObserveV1) {
		return StatusSkip, "observe.v1 not declared: the replacement is read from observe's identity"
	}
	name := r.nextName()
	defer r.stop(ctx, name)
	first := launchConfig{Env: map[string]string{runtime.InstanceTokenEnv: "gc-rpp-launch-first"}}
	if status, detail := provisionAndLaunch(ctx, r, name, first); status != StatusPass {
		return status, detail
	}
	const second = "gc-rpp-launch-second"
	if res := r.launch(ctx, name, launchConfig{Env: map[string]string{runtime.InstanceTokenEnv: second}}); !res.ok() {
		return StatusFail, fmt.Sprintf("a second launch on a running agent failed (exit %d, %v): without fresh_only launch replaces the agent", res.exitCode, res.err)
	}
	o, res := r.observe(ctx, nil, name)
	switch {
	case !res.ok():
		return StatusFail, fmt.Sprintf("observe after the second launch failed: %v", res.err)
	case o.State != "running":
		return StatusFail, fmt.Sprintf("observe after the second launch = %q, want running", o.State)
	case o.Identity[runtime.InstanceTokenEnv] != second:
		return StatusFail, fmt.Sprintf("observe's %s after the second launch = %q, want %q: launch kept the old agent", runtime.InstanceTokenEnv, o.Identity[runtime.InstanceTokenEnv], second)
	}
	return StatusPass, "a launch on a running agent replaced it"
}

// probeLaunchFreshOnly: a fresh_only launch into the box provision just
// prepared starts the agent (gc launches every fresh start this way), and a
// second fresh_only launch, on the now-running agent, exits 73.
func probeLaunchFreshOnly(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, ok := launchDeclared(hs); !ok {
		return status, detail
	}
	name := r.nextName()
	defer r.stop(ctx, name)
	if status, detail := provisionAndLaunch(ctx, r, name, launchConfig{FreshOnly: true}); status != StatusPass {
		return status, detail + " (a fresh_only launch into a freshly provisioned box, with no agent yet, starts it)"
	}
	if res := r.isRunning(ctx, name); !res.ok() || strings.TrimSpace(res.stdout) != "true" {
		return StatusFail, fmt.Sprintf("is-running after a fresh_only launch into a provisioned box = %q (%v), want \"true\"", strings.TrimSpace(res.stdout), res.err)
	}
	if res := r.launch(ctx, name, launchConfig{FreshOnly: true}); res.exitCode != runtime.ExitCodeFreshCollision {
		return StatusFail, fmt.Sprintf("a fresh_only launch on a running agent exited %d (%v), want %d", res.exitCode, res.err, runtime.ExitCodeFreshCollision)
	}
	return StatusPass, fmt.Sprintf("a fresh_only launch starts an agent in a provisioned box and exits %d on a running one", runtime.ExitCodeFreshCollision)
}

// launchIdentity is the identity the identity probe writes as gc writes a
// launch's (exec's publishLaunchIdentity), counters before the token.
var launchIdentity = []struct{ key, value string }{
	{runtime.RuntimeEpochEnv, "7"},
	{runtime.ContinuationEpochEnv, "3"},
	{runtime.InstanceTokenEnv, "gc-rpp-launch-identity"},
}

// probeLaunchIdentity: a pack that un-welds start keeps the launch identity
// gc writes after the launch. Its first turn is queued bound to that
// identity and judged and fenced by get-meta reads of it, so after
// provision (and launch, under proc.launch, whose own env carries another
// token) get-meta answers each key as set-meta stored it.
func probeLaunchIdentity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !hs.SeparableLaunch() {
		return StatusSkip, "start is welded: the handshake does not un-weld it (proc.provision with proc.launch, or with proc.exec under the default carrier)"
	}
	name := r.nextName()
	defer r.stop(ctx, name)
	if hs.Has(runtime.ProtocolCapabilityLaunch) {
		cfg := launchConfig{Env: map[string]string{runtime.InstanceTokenEnv: "gc-rpp-launch-own-token"}}
		if status, detail := provisionAndLaunch(ctx, r, name, cfg); status != StatusPass {
			return status, detail
		}
	} else {
		switch prov := r.provision(ctx, name, false); {
		case prov.unsupported:
			return StatusFail, "provision returned exit 2 although the handshake declares proc.provision"
		case prov.err != nil:
			return StatusFail, "provision failed: " + prov.err.Error()
		}
	}
	for _, id := range launchIdentity {
		if err := r.setMeta(ctx, name, id.key, id.value); err != nil {
			return StatusFail, fmt.Sprintf("set-meta %s after the launch: %v; a pack that un-welds start keeps the identity gc writes", id.key, err)
		}
	}
	for _, id := range launchIdentity {
		got, err := r.getMeta(ctx, name, id.key)
		switch {
		case err != nil:
			return StatusFail, fmt.Sprintf("get-meta %s after the launch: %v; gc judges the queued first turn by it", id.key, err)
		case got != id.value:
			return StatusFail, fmt.Sprintf("get-meta %s after the launch = %q, want %q as set-meta stored it", id.key, got, id.value)
		}
	}
	return StatusPass, "get-meta answers the launch identity set-meta stored"
}
