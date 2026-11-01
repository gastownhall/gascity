package exec

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	invObject   = `"object":{"id":"c0ffee","created":"t0"}`
	invIdentity = `"identity":{"GC_SESSION_ID":"mc-1","GC_INSTANCE_TOKEN":"` + secretToken + `"}`
)

// Kills (§13.5 6, §13.1): an inventory that is not exactly one well-formed
// object read as a listing; an entry whose name cannot be read listed
// anyway (completeness is then unprovable); malformed attributes dropping
// the name (it would read as gone); and any answer's bytes in an error.
func TestParseInventory(t *testing.T) {
	at := time.Date(2026, 10, 30, 1, 5, 0, 0, time.UTC)
	out := `{"complete":true,"sessions":[
	  {"name":"mc-live","state":"running",` + invObject + `,"agent":"alive","attached":true,"last_activity":"2026-10-30T01:05:00Z","restart_requested":true,` + invIdentity + `},
	  {"name":"mc-zombie","state":"running",` + invObject + `,"agent":"dead","attached":false,"identity":{}},
	  {"name":"mc-quiet","state":"running",` + invObject + `,"agent":"unknown","attached":null,"identity":null},
	  {"name":"mc-dead","state":"dead",` + invObject + `,"agent":"dead"},
	  {"name":"mc-oddattrs","state":"running",` + invObject + `,"agent":"maybe","attached":"yes","last_activity":"soon","restart_requested":"y","identity":"` + secretToken + `"},
	  {"name":"mc-oddstate","state":"zombie",` + invObject + `},
	  {"name":"mc-noobject","state":"running"},
	  {"name":"mc-contradiction","state":"dead",` + invObject + `,"agent":"alive"},
	  {"name":"mc-absent","state":"absent"}
	]}`
	got, err := parseInventory([]byte(out))
	if err != nil {
		t.Fatalf("parseInventory: %v", err)
	}
	wantNames := []string{"mc-live", "mc-zombie", "mc-quiet", "mc-dead", "mc-oddattrs", "mc-oddstate", "mc-noobject", "mc-contradiction", "mc-absent"}
	if !got.complete || !slices.Equal(got.names, wantNames) {
		t.Fatalf("complete=%v names=%v; want complete and every listed name, live, dead or malformed", got.complete, got.names)
	}
	running := runtime.InventoryEntry{Incarnation: "c0ffee@t0", DeadKnown: true}
	want := map[string]runtime.InventoryEntry{}
	e := running
	e.ProcessKnown, e.ProcessAlive, e.AttachedKnown, e.Attached, e.ActivityKnown, e.LastActivity, e.RestartRequested = true, true, true, true, true, at, true
	e.IdentityKnown, e.Identity = true, map[string]string{"GC_SESSION_ID": "mc-1", "GC_INSTANCE_TOKEN": secretToken}
	want["mc-live"] = e
	e = running
	e.ProcessKnown, e.AttachedKnown, e.IdentityKnown, e.Identity = true, true, true, map[string]string{}
	want["mc-zombie"] = e
	want["mc-quiet"] = running
	want["mc-dead"] = runtime.InventoryEntry{Incarnation: "c0ffee@t0", DeadKnown: true, AllPanesDead: true}
	want["mc-oddattrs"] = running
	for name, w := range want {
		if g, ok := got.entries[name]; !ok || !reflect.DeepEqual(g, w) {
			t.Errorf("%s: entry %+v (present %v), want %+v", name, g, ok, w)
		}
	}
	for _, name := range []string{"mc-oddstate", "mc-noobject", "mc-contradiction", "mc-absent"} {
		if g, ok := got.entries[name]; ok {
			t.Errorf("%s: entry %+v, want the name listed with unknown attributes (no entry)", name, g)
		}
	}

	partial, err := parseInventory([]byte(`{"complete":false,"sessions":[{"name":"a","state":"running",` + invObject + `}]}`))
	if err != nil || partial.complete || !slices.Equal(partial.names, []string{"a"}) {
		t.Errorf("partial = %+v, %v; want incomplete, a listed", partial, err)
	}
	if empty, err := parseInventory([]byte(`{"complete":true,"sessions":[]}`)); err != nil || !empty.complete || len(empty.names) != 0 {
		t.Errorf("empty = %+v, %v; want a complete empty listing", empty, err)
	}

	for name, in := range map[string]string{
		"not JSON":             `complete ` + secretToken,
		"empty":                ``,
		"null":                 `null`,
		"array":                `["` + secretToken + `"]`,
		"trailing bytes":       `{"complete":true,"sessions":[]} ` + secretToken,
		"second object":        `{"complete":true,"sessions":[]}{"x":"` + secretToken + `"}`,
		"complete missing":     `{"sessions":[{"name":"` + secretToken + `"}]}`,
		"complete not a bool":  `{"complete":"yes","sessions":[]}`,
		"sessions missing":     `{"complete":true}`,
		"sessions null":        `{"complete":true,"sessions":null}`,
		"sessions not a list":  `{"complete":true,"sessions":{"name":"a"}}`,
		"entry not an object":  `{"complete":true,"sessions":["` + secretToken + `"]}`,
		"name missing":         `{"complete":true,"sessions":[{"state":"running",` + invObject + `,` + invIdentity + `}]}`,
		"name empty":           `{"complete":true,"sessions":[{"name":"","state":"running"}]}`,
		"name not a string":    `{"complete":true,"sessions":[{"name":7,"state":"running"}]}`,
		"name duplicated":      `{"complete":true,"sessions":[{"name":"a","state":"running",` + invObject + `},{"name":"a","state":"dead",` + invObject + `}]}`,
		"name null":            `{"complete":true,"sessions":[{"name":null,` + invIdentity + `}]}`,
		"entries before error": `{"complete":true,"sessions":[{"name":"a"},{"name":""}]}`,
	} {
		if got, err := parseInventory([]byte(in)); err == nil {
			t.Errorf("%s: parseInventory = %+v, want a failure", name, got)
		} else if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error %q echoes the answer", name, err)
		}
	}
}

// inventoryOps is a ready pack whose `inventory` prints dir/inventory (or
// fails with dir/inventory_exit) and whose `list-running` logs it ran.
var inventoryOps = map[string]string{
	"inventory":    `code=0; [ -f "$dir/inventory_exit" ] && code=$(cat "$dir/inventory_exit"); [ "$code" = 0 ] && cat "$dir/inventory"; exit $code`,
	"list-running": `echo stale-name`,
	"is-running":   `echo true`,
}

const twoBoxes = `{"complete":true,"sessions":[{"name":"mc-live","state":"running",` + invObject + `},{"name":"mc-dead","state":"dead",` + invObject + `},{"name":"other-x","state":"running",` + invObject + `}]}`

// Kills (§13.5 6, design gap 1): a v2-ready listing taken from
// list-running (running names only), a dead box left out of the listing
// (an attested listing would then prove it gone), a partial or failed
// inventory read as complete, and a listing attested for a script that is
// not ready.
func TestReadyListRunningIsTheInventory(t *testing.T) {
	script, dir := observePack(t, readyHandshake, inventoryOps)
	writeFile(t, filepath.Join(dir, "inventory"), twoBoxes)
	sp := NewSeamBacked(script)
	names, err := sp.ListRunning("mc-")
	if err != nil || !slices.Equal(names, []string{"mc-live", "mc-dead"}) {
		t.Fatalf("ListRunning = %v, %v; want the live and the dead box under the prefix", names, err)
	}
	if !runtime.ListRunningAttested(sp) {
		t.Error("a v2-ready listing is not attested")
	}
	if ran := opsRan(t, dir); slices.Contains(ran, "list-running") || !slices.Contains(ran, "inventory") {
		t.Errorf("ops %v: want the listing from inventory alone", ran)
	}
	if all, err := sp.ListRunning(""); err != nil || len(all) != 3 {
		t.Errorf("ListRunning(\"\") = %v, %v; want every listed name", all, err)
	}
	none, err := sp.ListRunning("zz-")
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("ListRunning(zz-) = %#v, %v; want [], nil", none, err)
	}

	writeFile(t, filepath.Join(dir, "inventory"), `{"complete":false,"sessions":[{"name":"mc-live","state":"running",`+invObject+`}]}`)
	names, err = sp.ListRunning("mc-")
	if !runtime.IsPartialListError(err) || !slices.Equal(names, []string{"mc-live"}) {
		t.Errorf("incomplete inventory: ListRunning = %v, %v; want the listed names and a PartialListError", names, err)
	}
	for _, code := range []string{"1", "2"} {
		writeFile(t, filepath.Join(dir, "inventory_exit"), code)
		names, err = sp.ListRunning("mc-")
		if err == nil || runtime.IsPartialListError(err) || names != nil {
			t.Errorf("inventory exit %s: ListRunning = %v, %v; want a failure, never a listing", code, names, err)
		}
	}

	for _, handshake := range []string{"", `{"version":0,"capabilities":["observe.v1"]}`} {
		script, dir := observePack(t, handshake, map[string]string{"inventory": `echo '` + twoBoxes + `'`, "list-running": `exit 2`})
		sp := NewSeamBacked(script)
		if names, err := sp.ListRunning("mc-"); err != nil || names == nil || len(names) != 0 {
			t.Errorf("handshake %q: ListRunning = %#v, %v; want the legacy exit-2 []", handshake, names, err)
		}
		if runtime.ListRunningAttested(sp) {
			t.Errorf("handshake %q: a script that is not v2-ready attests its listing", handshake)
		}
		if _, ok := runtime.InventoryFor(sp); ok {
			t.Errorf("handshake %q: InventoryFor offers an inventory", handshake)
		}
		if _, err := sp.(runtime.InventoryProvider).RuntimeInventory(context.Background()); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Errorf("handshake %q: RuntimeInventory = %v, want ErrRuntimeUnavailable", handshake, err)
		}
		if slices.Contains(opsRan(t, dir), "inventory") {
			t.Errorf("handshake %q: inventory ran for a script that is not v2-ready", handshake)
		}
	}
}

// Kills: the batched inventory not served through the production seam, a
// failure or an oversized answer read as an inventory, and an inventory
// that outlives its caller's bound.
func TestRuntimeInventoryThroughTheSeam(t *testing.T) {
	script, dir := observePack(t, readyHandshake, inventoryOps)
	writeFile(t, filepath.Join(dir, "inventory"), twoBoxes)
	sp := NewSeamBacked(script)
	inv, ok := runtime.InventoryFor(sp)
	if !ok {
		t.Fatal("InventoryFor(ready seam) offers no inventory")
	}
	entries, err := inv.RuntimeInventory(context.Background())
	if err != nil || len(entries) != 3 || !entries["mc-dead"].AllPanesDead || entries["mc-live"].Incarnation != "c0ffee@t0" {
		t.Fatalf("RuntimeInventory = %+v, %v", entries, err)
	}

	writeFile(t, filepath.Join(dir, "inventory_exit"), "1")
	if got, err := inv.RuntimeInventory(context.Background()); err == nil || got != nil {
		t.Errorf("failed inventory = %v, %v; want an error", got, err)
	}

	big, _ := observePack(t, readyHandshake, map[string]string{
		"inventory": `printf '{"complete":true,"sessions":[],"pad":"'; head -c 70000000 /dev/zero | tr '\0' a; printf '"}'`,
	})
	if got, err := NewSeamBacked(big).(runtime.InventoryProvider).RuntimeInventory(context.Background()); err == nil || strings.Contains(err.Error(), "aaaa") {
		t.Errorf("oversized inventory = %v, %v; want a failure that echoes nothing", len(got), err)
	}

	slow, _ := observePack(t, readyHandshake, map[string]string{"inventory": `sleep 10 & wait $!; echo '{"complete":true,"sessions":[]}'`})
	p := NewProvider(slow)
	if _, err := p.V2Readiness(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.RuntimeInventory(ctx); err == nil {
		t.Error("an inventory past its caller's bound succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("RuntimeInventory took %v, want it bounded by ctx", elapsed)
	}
}
