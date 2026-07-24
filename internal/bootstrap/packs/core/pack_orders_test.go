package core

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// readOrder parses an order TOML from the embedded pack FS and restores the
// Name the scanner would normally derive from the filename (Parse leaves it
// blank because Name is not a TOML field).
func readOrder(t *testing.T, file string) orders.Order {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "orders/"+file)
	if err != nil {
		t.Fatalf("reading orders/%s: %v", file, err)
	}
	o, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parsing orders/%s: %v", file, err)
	}
	o.Name = strings.TrimSuffix(file, ".toml")
	return o
}

// TestCoreOrdersValidate asserts every embedded order TOML parses and
// passes structural validation, so a malformed order can never ship in the gc
// binary's bundled core pack.
func TestCoreOrdersValidate(t *testing.T) {
	entries, err := fs.ReadDir(PackFS, "orders")
	if err != nil {
		t.Fatalf("reading orders dir: %v", err)
	}
	saw := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		saw = true
		o := readOrder(t, e.Name())
		if err := orders.Validate(o); err != nil {
			t.Errorf("order %s failed validation: %v", e.Name(), err)
		}
	}
	if !saw {
		t.Fatal("no order TOML files found in embedded pack")
	}
}

// assertEventExecOrder checks an event-triggered exec order: it must validate,
// listen for the expected event type, dispatch via exec (not a formula/pool),
// and point at a script that is actually embedded in the pack.
func assertEventExecOrder(t *testing.T, orderFile, eventType, scriptBase string) {
	t.Helper()
	o := readOrder(t, orderFile)
	if err := orders.Validate(o); err != nil {
		t.Fatalf("%s failed validation: %v", orderFile, err)
	}
	if o.Trigger != "event" {
		t.Errorf("%s: trigger = %q, want %q", orderFile, o.Trigger, "event")
	}
	if o.On != eventType {
		t.Errorf("%s: on = %q, want %q", orderFile, o.On, eventType)
	}
	if !o.IsExec() {
		t.Errorf("%s: want exec dispatch, got formula %q", orderFile, o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("%s: exec orders must not set a pool, got %q", orderFile, o.Pool)
	}
	wantSuffix := "assets/scripts/" + scriptBase
	if !strings.HasSuffix(o.Exec, wantSuffix) {
		t.Errorf("%s: exec = %q, want suffix %q", orderFile, o.Exec, wantSuffix)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("%s: referenced script not embedded: %v", orderFile, err)
	}
}

// TestNudgeOnRouteOrder pins the nudge-on-route order's event contract: it wakes
// on bead.updated and runs the nudge-on-route script.
func TestNudgeOnRouteOrder(t *testing.T) {
	assertEventExecOrder(t, "nudge-on-route.toml", "bead.updated", "nudge-on-route.sh")
}

// TestCascadeNudgeOnBlockerCloseOrder pins the cascade-nudge order's event
// contract: it wakes on bead.closed — the event the close transition actually
// emits — and runs the cascade-nudge script.
func TestCascadeNudgeOnBlockerCloseOrder(t *testing.T) {
	assertEventExecOrder(t, "cascade-nudge-on-blocker-close.toml", "bead.closed", "cascade-nudge-on-blocker-close.sh")
}

// TestCascadeNudgeRoutesCrossRig guards the cascade order's cross-rig
// routing. Two properties must hold or cross-rig cascades break silently
// (failures are soft-skipped via `|| continue`, so a regression is invisible
// at runtime): (1) the dependent lookup runs through the `gc bd` wrapper, not
// bare `bd` — `--rig` is a gc flag, not a bd flag, and the wrapper runs bd in
// the owning rig's directory; (2) the prefix->rig lookup excludes the HQ entry
// (`gc rig list` reports the city root as an hq=true pseudo-rig that
// `gc --rig <cityName>` cannot resolve), matching orphan-sweep.sh's
// `select(.hq == false)` convention.
func TestCascadeNudgeRoutesCrossRig(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/cascade-nudge-on-blocker-close.sh")
	if err != nil {
		t.Fatalf("reading cascade-nudge-on-blocker-close.sh: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "gc bd dep list") {
		t.Error("cascade-nudge script must route the dep lookup through `gc bd dep list`; missing")
	}
	if strings.Contains(body, "$(bd dep list") {
		t.Error("cascade-nudge script must not run bare `bd dep list` (--rig is a gc flag, not a bd flag)")
	}
	if !strings.Contains(body, ".hq != true") {
		t.Error("cascade-nudge script must exclude the HQ entry from the prefix->rig lookup; missing `.hq != true`")
	}
}

// TestNudgeOnRouteResolvesPoolMembers guards the pool-base fan-out: a
// multi-session pool routes to the pool BASE (sling's NormalizePoolRouteTarget
// collapses slot -> base), which is the members' template, not a session name
// `gc session nudge` can resolve. The script must therefore enumerate pool
// members by template before nudging — a naive `gc session nudge "$routed_to"`
// silently no-ops for exactly the warm-idle pool workers this order targets.
func TestNudgeOnRouteResolvesPoolMembers(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/nudge-on-route.sh")
	if err != nil {
		t.Fatalf("reading nudge-on-route.sh: %v", err)
	}
	body := string(data)
	for _, want := range []string{"gc session list", "--template"} {
		if !strings.Contains(body, want) {
			t.Errorf("nudge-on-route.sh must resolve pool members; missing %q", want)
		}
	}
}

// TestNotifyOnHumanGateCreationOrder pins the notify-on-human-gate-creation
// order's event contract: it wakes on bead.created — the event synthesized for
// any newly-appeared bead — and runs the notify-on-human-gate-creation script.
func TestNotifyOnHumanGateCreationOrder(t *testing.T) {
	assertEventExecOrder(t, "notify-on-human-gate-creation.toml", "bead.created", "notify-on-human-gate-creation.sh")
}

// TestNotifyOnHumanGateCreationScriptContract guards the load-bearing behaviors
// of the notify script. Each property, if it regresses, breaks the order
// silently (failures are best-effort and swallowed at runtime), so they are
// pinned here:
//
//   - The bead.created payload does NOT carry await_type, so a human gate is
//     indistinguishable from a timer/gh gate at the event alone. The script
//     must re-fetch the bead via `gc bd show` and gate on await_type == "human"
//     AND status == "open" — otherwise it would notify on every gate creation
//     (or none).
//   - Addressee resolution must consult gc.deferred_assignee: formula/molecule
//     gates strip the assignee to that metadata key at create time, so a naive
//     `.assignee`-only lookup finds an empty addressee and misroutes to the
//     human fallback for exactly the automated gates that name a real one.
//   - Notification must ride `gc mail send --notify`, the one primitive that
//     mails AND nudges a real session while natively skipping the tmux-nudge
//     for the sessionless "human" recipient (cmd_mail.go `to != "human"`). A
//     hand-rolled `gc session nudge` would fail on the human channel.
//   - The prefix->rig lookup must exclude the HQ entry (`gc rig list` reports
//     the city root as an hq=true pseudo-rig `gc --rig <cityName>` cannot
//     resolve), matching the cross-rig convention in the sibling scripts.
//   - Event-shape robustness: the API envelope wraps the bead under
//     .payload.bead, but the `gc events` local fallback (API down) emits the
//     bead fields directly under .payload. The filter must read both via
//     `(.payload.bead // .payload)` or it silently finds no gates in fallback
//     mode — exactly when notifications matter most.
//   - Loud-fail: an undeliverable send must surface and NOT be recorded as
//     done. Surfacing requires a NON-ZERO exit — the controller logs an exec
//     order's captured output only on a non-zero exit — so the script must
//     exit non-zero when any send failed (gastownhall/gascity#4543).
func TestNotifyOnHumanGateCreationScriptContract(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/notify-on-human-gate-creation.sh")
	if err != nil {
		t.Fatalf("reading notify-on-human-gate-creation.sh: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"(.payload.bead // .payload)", // normalize API-envelope vs local-fallback event shape
		`$b.issue_type == "gate"`,     // filter events to gate creations
		"gc bd show",                  // re-fetch (event lacks await_type)
		`"$AWAIT_TYPE" = "human"`,     // human gates only
		`"$STATUS" = "open"`,          // skip already-resolved gates
		`gc.deferred_assignee`,        // formula/molecule addressee
		"--notify",                    // mail + nudge, human-safe primitive
		".hq != true",                 // exclude HQ from prefix->rig lookup
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notify-on-human-gate-creation.sh missing load-bearing element %q", want)
		}
	}

	// Loud-fail: the send must be conditional (retry on failure), and the
	// failure path must surface to stderr rather than silently record the gate
	// as notified. The dedup record must live on the SUCCESS branch only.
	if !strings.Contains(body, "if gc mail send") {
		t.Error("notify-on-human-gate-creation.sh must branch on the mail-send result (loud-fail retry), not fire-and-forget")
	}
	if !strings.Contains(body, "will retry next sweep") {
		t.Error("notify-on-human-gate-creation.sh must surface an undeliverable send to stderr (loud-fail #4543)")
	}
	// The controller captures an exec order's combined output but logs it only
	// on a NON-ZERO exit (order_dispatch.go), so a fire-and-forget exit 0 would
	// swallow the failure lines above. The script must exit non-zero when any
	// send failed — after writing state, so recorded successes are not lost.
	if !strings.Contains(body, `"$FAILED" -gt 0`) {
		t.Error("notify-on-human-gate-creation.sh must exit non-zero when a send failed, or the loud-fail message is never logged (#4543)")
	}
}

// assertCooldownExecOrder checks a cooldown-triggered exec order: it must
// validate, run on a cooldown trigger with a parseable interval, dispatch via
// exec (not a formula/pool), and point at a script embedded in the pack.
func assertCooldownExecOrder(t *testing.T, orderFile, scriptBase string) {
	t.Helper()
	o := readOrder(t, orderFile)
	if err := orders.Validate(o); err != nil {
		t.Fatalf("%s failed validation: %v", orderFile, err)
	}
	if o.Trigger != "cooldown" {
		t.Errorf("%s: trigger = %q, want %q", orderFile, o.Trigger, "cooldown")
	}
	if _, err := time.ParseDuration(o.Interval); err != nil {
		t.Errorf("%s: interval %q is not a valid duration: %v", orderFile, o.Interval, err)
	}
	if !o.IsExec() {
		t.Errorf("%s: want exec dispatch, got formula %q", orderFile, o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("%s: exec orders must not set a pool, got %q", orderFile, o.Pool)
	}
	wantSuffix := "assets/scripts/" + scriptBase
	if !strings.HasSuffix(o.Exec, wantSuffix) {
		t.Errorf("%s: exec = %q, want suffix %q", orderFile, o.Exec, wantSuffix)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("%s: referenced script not embedded: %v", orderFile, err)
	}
}

// TestRenudgeStaleHumanGatesOrder pins the staleness-sweep order's contract: it
// is a cooldown-triggered exec order running the renudge-stale-human-gates
// script. It is the repeating companion to notify-on-human-gate-creation (which
// fires once, on bead.created); this one re-fires on a cooldown for gates left
// open.
func TestRenudgeStaleHumanGatesOrder(t *testing.T) {
	assertCooldownExecOrder(t, "renudge-stale-human-gates.toml", "renudge-stale-human-gates.sh")
}

// TestRenudgeStaleHumanGatesScriptContract guards the load-bearing behaviors of
// the staleness re-nudge script. Like the creation-notify script its failures
// are best-effort and swallowed at runtime, so the contract is pinned here:
//
//   - Enumeration is over OPEN gates (`gc bd gate list`, open-only by default)
//     with `--limit 0` so a rig past the default 50-gate page is not silently
//     truncated — a truncated page would drop stale gates from the sweep.
//   - It re-nudges ONLY open human gates: await_type == "human" AND
//     status == "open". The live town carries dozens of legacy await_type=null
//     workflow gates that must never be mailed about.
//   - Both the staleness threshold and the repeat interval are configurable
//     (GC_STALE_GATE_THRESHOLD / GC_STALE_GATE_RENUDGE_INTERVAL) — the order's
//     purpose is "open past a configurable threshold, repeating on the
//     interval".
//   - Addressee resolution consults gc.deferred_assignee (formula/molecule
//     gates strip the assignee there), matching the creation notify so a gate
//     is re-nudged at the same address it was first notified.
//   - The list projection omits assignee/metadata, so the script must re-fetch
//     via `gc bd show` to resolve the addressee.
//   - Notification rides `gc mail send --notify`, the one primitive that mails
//     AND nudges a real session while natively skipping the tmux-nudge for the
//     sessionless "human" recipient (cmd_mail.go `to != "human"`).
//   - The prefix->rig enumeration excludes the HQ pseudo-rig (`.hq != true`),
//     matching the sibling scripts' cross-rig convention.
//   - Timestamp parsing is portable: GNU-only `date -d` returns empty on
//     BSD/macOS, skipping every gate and silently disabling the sweep, so the
//     BSD `date -ju -f` fallback (matching wisp-compact.sh) is required.
//   - Loud-fail: an undeliverable send must surface and NOT be recorded. As
//     with the creation notify, surfacing requires a NON-ZERO exit (the
//     controller logs an exec order's output only on a non-zero exit), so the
//     script must exit non-zero when any re-nudge failed (#4543).
func TestRenudgeStaleHumanGatesScriptContract(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/renudge-stale-human-gates.sh")
	if err != nil {
		t.Fatalf("reading renudge-stale-human-gates.sh: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"gc bd gate list",                // enumerate OPEN gates (not events)
		"--limit 0",                      // no silent 50-gate truncation
		`.await_type == "human"`,         // human gates only
		`.status == "open"`,              // skip already-resolved gates
		"GC_STALE_GATE_THRESHOLD",        // configurable staleness threshold
		"GC_STALE_GATE_RENUDGE_INTERVAL", // configurable repeat interval
		"gc bd show",                     // re-fetch (list omits assignee)
		"gc.deferred_assignee",           // formula/molecule addressee
		"--notify",                       // mail + nudge, human-safe primitive
		".hq != true",                    // exclude HQ from prefix->rig lookup
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renudge-stale-human-gates.sh missing load-bearing element %q", want)
		}
	}

	// Loud-fail: the send must be conditional (retry on failure), and the
	// failure path must surface to stderr rather than silently record the gate
	// as re-nudged. The dedup record must live on the SUCCESS branch only.
	if !strings.Contains(body, "if gc mail send") {
		t.Error("renudge-stale-human-gates.sh must branch on the mail-send result (loud-fail retry), not fire-and-forget")
	}
	if !strings.Contains(body, "will retry next sweep") {
		t.Error("renudge-stale-human-gates.sh must surface an undeliverable send to stderr (loud-fail #4543)")
	}
	// Timestamp parsing must be portable: GNU-only `date -d` returns empty on
	// BSD/macOS, which skips every gate at the age check and silently disables
	// the whole sweep. The BSD `date -ju -f` fallback (matching wisp-compact.sh)
	// is load-bearing.
	if !strings.Contains(body, "date -ju -f") {
		t.Error("renudge-stale-human-gates.sh must parse timestamps portably via the BSD `date -ju -f` fallback; GNU-only `date -d` disables the sweep on macOS")
	}
	// Same loud-fail exit contract as the creation notify: the controller logs
	// an exec order's output only on a non-zero exit.
	if !strings.Contains(body, `"$FAILED" -gt 0`) {
		t.Error("renudge-stale-human-gates.sh must exit non-zero when a re-nudge failed, or the loud-fail message is never logged (#4543)")
	}
}
