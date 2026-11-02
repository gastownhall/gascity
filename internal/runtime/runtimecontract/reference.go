package runtimecontract

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the golden reference executable, as shell: the one script
// both conformance suites run (TestGoldenReferenceIsFullyConformant here,
// and the exec provider's v2 contract run in internal/runtime/exec), so the
// wire catalog and RunProviderTests are bound to one implementation.

// The golden reference's observe.v1 op bodies. Every box is the directory
// $box ($B/$name, $B the state of the city GC_EXEC_CITY_PATH names), holding
// its object (id, created), its agent's command (cmd0), the process names
// it was started with (procs) and its identity env as meta.<KEY> files,
// written before the box is renamed into place, so identity is readable
// from the first observe. faultGuard is each fault-aware op's first line.
const (
	faultGuard = `if [ "${GC_RPP_FAULT:-}" = unavailable ]; then exit 1; fi; `

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
		`printf ',"identity":'; ` + goldenObserveIdentity + `; echo '}'`

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

// goldenPrelude computes the city's state and the name's box, and the
// helpers the observe.v1 bodies share: ident prints $box's identity, and
// alive reports whether a box's agent (cmd0) is among its process names.
const goldenPrelude = `C=$(printf %s "${GC_EXEC_CITY_PATH:-}" | tr '/' '_'); B="$S/city$C"; mkdir -p "$B"; box="$B/$name"
ident() { printf '{'; sep=''; for k in GC_SESSION_ID GC_RUNTIME_EPOCH GC_INSTANCE_TOKEN GT_PROCESS_NAMES; do if [ -f "$box/meta.$k" ]; then printf '%s"%s":"%s"' "$sep" "$k" "$(cat "$box/meta.$k")"; sep=','; fi; done; printf '}'; }
alive() { for p in $(tr ',' ' ' < "$1/procs"); do if [ "$p" = "$(cat "$1/cmd0")" ]; then return 0; fi; done; return 1; }
`

// ReferenceBodies returns the op bodies of the golden reference: a
// fully-conformant, stateful RPP executable that passes every catalog
// requirement, the observe.v1 profile included, and the in-tree provider
// contract through the exec provider (runtimetest.RunProviderTests). The
// bodies reference the shell state ($S, $B, $box) ReferenceScript injects.
// The negative-gating tests override one body to violate exactly one
// behavior.
func ReferenceBodies() map[string]string {
	return map[string]string{
		"protocol":          `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped"]}'`,
		"start":             goldenStart,
		"stop":              `rm -rf "$box"`,
		"is-running":        `if [ -d "$box" ]; then echo true; else echo false; fi`,
		"is-attached":       faultGuard + `echo false`,
		"process-alive":     `names=$(cat); if [ ! -d "$box" ]; then echo false; elif [ -z "$names" ]; then echo true; else a=false; for p in $names; do if [ "$p" = "$(cat "$box/cmd0")" ]; then a=true; fi; done; echo "$a"; fi`,
		"get-last-activity": `echo 2026-01-01T00:00:00Z`,
		"get-meta":          `if [ -f "$box/meta.$3" ]; then cat "$box/meta.$3"; fi`,
		"set-meta":          `v=$(cat); if [ -d "$box" ]; then printf %s "$v" > "$box/meta.$3"; fi`,
		"remove-meta":       `rm -f "$box/meta.$3"`,
		"observe":           goldenObserve,
		"inventory":         goldenInventory,
		"kill-object":       goldenKillObject,
		"exec":              `sh -c "$(cat)"`,
		// provision = the box half: consume the config and succeed WITHOUT
		// creating the box, so is-running stays false; the box is exec-able
		// (exec runs regardless).
		"provision": `cat >/dev/null`,
	}
}

// ReferenceScript renders bodies (ReferenceBodies, or a variant) as an
// executable RPP shell script keeping its state under stateDir. Unlisted
// ops exit 2.
func ReferenceScript(stateDir string, bodies map[string]string) string {
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
