package maintenance

import (
	"io/fs"
	"strings"
	"testing"

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

// TestMaintenanceOrdersValidate asserts every embedded order TOML parses and
// passes structural validation, so a malformed order can never ship in the gc
// binary's bundled maintenance pack.
func TestMaintenanceOrdersValidate(t *testing.T) {
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
