// Package reference is the conformance kit's golden reference executable,
// as shell: the one script both conformance suites run (the wire catalog in
// internal/runtime/runtimecontract, and RunProviderTests through the exec
// provider in v2 mode), so they are bound to one implementation. It is a
// test fixture: no production package imports it.
package reference

import (
	"fmt"
	"sort"
	"strings"
)

// The golden reference's observe.v1 op bodies. Every box is the directory
// $box ($B/$name, $B the state of the city GC_EXEC_CITY_PATH names), holding
// its object (id, created), its agent's command (cmd0), the process names
// it was started with (procs) and its identity env as meta.<KEY> files,
// written before the box is renamed into place, so identity is readable
// from the first observe. Every op that reads the backend first calls be,
// the backend, which fails under GC_RPP_FAULT=unavailable: the fault is
// injected at the backend call, so an op that swallows a failed backend
// read is caught.
const (
	faultGuard = `be || exit 1; `

	goldenStart = `cfg=$(cat); fresh=$(printf %s "$cfg" | sed -n 's/.*"fresh_only":true.*/y/p'); ` +
		`if [ -d "$box" ]; then if [ -n "$fresh" ]; then exit 73; fi; exit 1; fi; ` +
		`mkdir -p "$box.tmp"; ` +
		`for k in GC_SESSION_ID GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN GT_PROCESS_NAMES; do v=$(printf %s "$cfg" | sed -n "s/.*\"$k\":\"\([^\"]*\)\".*/\1/p"); if [ -n "$v" ]; then printf %s "$v" > "$box.tmp/meta.$k"; fi; done; ` +
		`printf %s "$cfg" | sed -n 's/.*"process_names":\[\([^]]*\)\].*/\1/p' | tr -d '"' > "$box.tmp/procs"; ` +
		`printf %s "$cfg" | sed -n 's/.*"command":"\([^" ]*\).*/\1/p' > "$box.tmp/cmd0"; ` +
		`n=$(cat "$S/seq" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$S/seq"; printf 'obj-%s' "$n" > "$box.tmp/id"; date +%s > "$box.tmp/created"; ` +
		`mv "$box.tmp" "$box"`

	goldenObserveIdentity = `ident`

	goldenObserve = faultGuard + `names=$(cat); ` +
		`if [ ! -d "$box" ]; then echo '{"state":"absent"}'; exit 0; fi; ` +
		`agent=""; if [ -n "$names" ]; then agent=dead; for p in $names; do if [ "$p" = "$(cat "$box/cmd0")" ]; then agent=alive; fi; done; fi; ` +
		`printf '{"state":"running","object":{"id":"%s","created":"%s"}' "$(cat "$box/id")" "$(cat "$box/created")"; ` +
		`if [ -n "$agent" ]; then printf ',"agent":"%s"' "$agent"; fi; ` +
		goldenObserveAgentPID +
		`printf ',"identity":'; ` + goldenObserveIdentity + `; echo '}'`

	// goldenObserveAgentPID reports the host pid of a box whose command the
	// reference runs (HostFSBodies); other boxes run no process.
	goldenObserveAgentPID = `if [ -f "$box/pid" ]; then printf ',"agent_pid":"%s"' "$(cat "$box/pid")"; fi; `

	goldenInventory = faultGuard + `printf '{"complete":true,"sessions":['; sep=''; ` +
		`for box in "$B"/*; do [ -d "$box" ] || continue; case "$box" in *.tmp) continue ;; esac; ` +
		`if alive "$box"; then agent=alive; else agent=dead; fi; ` +
		`printf '%s{"name":"%s","state":"running","object":{"id":"%s","created":"%s"},"agent":"%s","attached":false,"last_activity":"2026-01-01T00:00:00Z","restart_requested":false,"identity":' "$sep" "$(basename "$box")" "$(cat "$box/id")" "$(cat "$box/created")" "$agent"; ` +
		`ident; printf '}'; sep=','; done; echo ']}'`

	goldenKillObject = `req=$(cat); ` +
		`id=$(printf %s "$req" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p'); cr=$(printf %s "$req" | sed -n 's/.*"created":"\([^"]*\)".*/\1/p'); mode=$(printf %s "$req" | sed -n 's/.*"mode":"\([^"]*\)".*/\1/p'); ` +
		`if [ ! -d "$box" ] || [ "$(cat "$box/id")" != "$id" ] || [ "$(cat "$box/created")" != "$cr" ]; then echo gone; exit 0; fi; ` +
		`case "$mode" in corpse) echo live ;; zombie) if alive "$box"; then echo changed; else rm -rf "$box"; echo killed; fi ;; *) exit 1 ;; esac`
)

// goldenPeek prints a box's screen, its agent's command, and answers exit
// 66 (no screen) when no box holds the name.
const goldenPeek = faultGuard + `if [ ! -d "$box" ]; then exit 66; fi; cat "$box/cmd0"; echo`

// goldenExec runs the piped command in the box. Its tmux is the box's own:
// the session `main` exists while the box does (has-session) and its pane
// shows the agent's command (capture-pane), as the idle-boundary declaration
// promises; any other tmux command goes to the host's tmux unchanged. eval,
// not sh -c, so the command sees the box's tmux. The golden does not declare
// proc.exec, so gc's own idle reads (tmux -u set-environment and
// show-environment, whose first argument is -u) never reach this model;
// declaring proc.exec here would need them modeled too.
const goldenExec = `cmd=$(cat); ` +
	`tmux() { case "$1" in has-session) [ -d "$box" ] ;; capture-pane) [ -d "$box" ] && cat "$box/cmd0" ;; *) command tmux "$@" ;; esac; }; ` +
	`eval "$cmd"`

// goldenListRunning lists the city's boxes whose name starts with the
// prefix ($name holds it), and fails, never lists nothing, when the backend
// cannot be read.
const goldenListRunning = faultGuard + `for b in "$B"/*; do [ -d "$b" ] || continue; case "$b" in *.tmp) continue ;; esac; ` +
	`n=$(basename "$b"); case "$n" in "$name"*) echo "$n" ;; esac; done`

// goldenServerStop ends the city's server: every box of the city (the city's
// state, $B) goes with it, whether or not any is left. It is behind the
// backend fault, like every op that reaches the backend.
const goldenServerStop = faultGuard + `rm -rf "$B"`

// goldenPrelude computes the city's state and the name's box, and the
// helpers the observe.v1 bodies share: ident prints $box's identity, and
// alive reports whether a box's agent (cmd0) is among its process names.
const goldenPrelude = `C=$(printf %s "${GC_EXEC_CITY_PATH:-}" | tr '/' '_'); B="$S/city$C"; mkdir -p "$B"; box="$B/$name"
be() { if [ "${GC_RPP_FAULT:-}" = unavailable ]; then echo "backend unavailable" >&2; return 1; fi; }
ident() { printf '{'; sep=''; for k in GC_SESSION_ID GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN GT_PROCESS_NAMES; do if [ -f "$box/meta.$k" ]; then printf '%s"%s":"%s"' "$sep" "$k" "$(cat "$box/meta.$k")"; sep=','; fi; done; printf '}'; }
alive() { for p in $(tr ',' ' ' < "$1/procs"); do if [ "$p" = "$(cat "$1/cmd0")" ]; then return 0; fi; done; return 1; }
reap() { if [ -f "$1/pid" ]; then p=$(cat "$1/pid"); pkill -P "$p" 2>/dev/null; kill "$p" 2>/dev/null; fi; return 0; }
`

// Bodies returns the op bodies of the golden reference: a
// fully-conformant, stateful RPP executable that passes every catalog
// requirement, the observe.v1 profile and server-lifecycle included, and the
// in-tree provider contract through the exec provider (runtimetest.RunProviderTests). The
// bodies reference the shell state ($S, $B, $box) Script injects.
// The negative-gating tests override one body to violate exactly one
// behavior.
func Bodies() map[string]string {
	return map[string]string{
		"protocol":          `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped","idle-boundary","server-lifecycle"]}'`,
		"start":             goldenStart,
		"stop":              `rm -rf "$box"`,
		"is-running":        `if [ -d "$box" ]; then echo true; else echo false; fi`,
		"is-attached":       faultGuard + `echo false`,
		"process-alive":     `names=$(cat); if [ ! -d "$box" ]; then echo false; elif [ -z "$names" ]; then echo true; else a=false; for p in $names; do if [ "$p" = "$(cat "$box/cmd0")" ]; then a=true; fi; done; echo "$a"; fi`,
		"get-last-activity": `echo 2026-01-01T00:00:00Z`,
		"get-meta":          `if [ -f "$box/meta.$3" ]; then cat "$box/meta.$3"; fi`,
		"set-meta":          `v=$(cat); if [ -d "$box" ]; then printf %s "$v" > "$box/meta.$3"; fi`,
		"remove-meta":       `rm -f "$box/meta.$3"`,
		"list-running":      goldenListRunning,
		"observe":           goldenObserve,
		"inventory":         goldenInventory,
		"kill-object":       goldenKillObject,
		"peek":              goldenPeek,
		"exec":              goldenExec,
		// provision = the box half: consume the config and succeed WITHOUT
		// creating the box, so is-running stays false; the box is exec-able
		// (exec runs regardless).
		"provision": `cat >/dev/null`,
		// server-stop = the city's server going down: every box of the
		// city ends with it, and with no box left it has nothing to do.
		"server-stop": goldenServerStop,
	}
}

// The host-filesystem additions to the golden start (HostFSBodies). Each
// reads one field of the start config with sed, which holds for the configs
// the conformance kits send (no escaped quotes in these fields, and at most
// one pre_start entry); a real pack decodes the JSON.
const (
	// goldenHostFSStage keeps the config the start received, stages its
	// overlay_dir into its work_dir and runs its pre_start there, failing the
	// start, with no box, when the pre_start fails.
	goldenHostFSStage = `printf %s "$cfg" > "$box.tmp/start.json"; ` +
		`wd=$(printf %s "$cfg" | sed -n 's/.*"work_dir":"\([^"]*\)".*/\1/p'); ` +
		`od=$(printf %s "$cfg" | sed -n 's/.*"overlay_dir":"\([^"]*\)".*/\1/p'); ` +
		`if [ -n "$od" ] && [ -n "$wd" ]; then cp -R "$od"/. "$wd"/ || { rm -rf "$box.tmp"; exit 1; }; fi; ` +
		`ps=$(printf %s "$cfg" | sed -n 's/.*"pre_start":\["\([^"]*\)"\].*/\1/p'); ` +
		`if [ -n "$ps" ]; then (cd "${wd:-.}" && sh -c "$ps") </dev/null >/dev/null 2>&1 || { rm -rf "$box.tmp"; echo "pre_start failed" >&2; exit 1; }; fi; `

	// goldenHostFSLaunch runs the start's command in its work_dir as a
	// background host process of the caller's user, with the session's
	// identity env from the config's env (goldenLaunchEnv), and records its
	// pid.
	goldenHostFSLaunch = `cmd=$(printf %s "$cfg" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p'); ` +
		`if [ -n "$cmd" ]; then (cd "${wd:-.}" && ` +
		`for k in ` + goldenLaunchEnv + `; do v=$(printf %s "$cfg" | sed -n "s/.*\"$k\":\"\([^\"]*\)\".*/\1/p"); if [ -n "$v" ]; then export "$k=$v"; fi; done; ` +
		`exec sh -c "$cmd") </dev/null >/dev/null 2>&1 & echo $! > "$box.tmp/pid"; fi; `

	// goldenLaunchEnv are the env keys of the start config the host
	// command runs with: the session's identity and city, so its processes
	// carry them as an agent's do.
	goldenLaunchEnv = `GC_SESSION_ID GC_SESSION_NAME GC_CITY_PATH GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN`
)

// HostFSBodies returns Bodies for a reference whose boxes share the
// controller's host filesystem and user: it declares locality local and
// fs.host, stages the overlay_dir and runs the pre_start its start config
// carries (exactly what the config carries: gc leaves out what it did
// itself), runs the start's command in work_dir as a host process carrying
// the session's identity env, whose pid observe reports as agent_pid, and
// stops that process with the box. Each box keeps the start config it
// received as start.json.
func HostFSBodies() map[string]string {
	b := Bodies()
	b["protocol"] = `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped","idle-boundary","server-lifecycle","fs.host"],"locality":"local"}'`
	b["start"] = strings.Replace(strings.Replace(b["start"],
		`mkdir -p "$box.tmp"; `, `mkdir -p "$box.tmp"; `+goldenHostFSStage, 1),
		`mv "$box.tmp" "$box"`, goldenHostFSLaunch+`mv "$box.tmp" "$box"`, 1)
	b["stop"] = `reap "$box"; ` + b["stop"]
	b["kill-object"] = strings.Replace(b["kill-object"], `rm -rf "$box"; echo killed`, `reap "$box"; rm -rf "$box"; echo killed`, 1)
	b["server-stop"] = strings.Replace(b["server-stop"], `rm -rf "$B"`, `for x in "$B"/*; do reap "$x"; done; rm -rf "$B"`, 1)
	return b
}

// Script renders bodies (Bodies, or a variant) as an
// executable RPP shell script keeping its state under stateDir. Unlisted
// ops exit 2.
func Script(stateDir string, bodies map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\nS='%s'\nop=\"$1\"\nname=\"$2\"\n%scase \"$op\" in\n", stateDir, goldenPrelude)
	ops := make([]string, 0, len(bodies))
	for op := range bodies {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		fmt.Fprintf(&b, "  %s) %s ;;\n", op, bodies[op])
	}
	b.WriteString("  *) exit 2 ;;\nesac\n")
	return b.String()
}
