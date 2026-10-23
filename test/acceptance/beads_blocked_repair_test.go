//go:build acceptance_a

// gc start's one-shot is_blocked repair (beads#7037) on the default topology:
// a proxied city and a proxied rig, each served by its own bd proxy and Dolt
// child. Migration 0059's damage is inflicted through bd's own front door
// (`bd sql`), the scopes are marked as last repaired under an older bd, and a
// restart must correct exactly the damaged rows in both scopes, record the
// event, return the hidden beads to `bd ready`, and do nothing on the restart
// after that.
package acceptance_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// blockedRepairMarkerKey mirrors cmd/gc's marker key: the bd config key, in
// the scope's own database, naming the bd version that last repaired it.
const blockedRepairMarkerKey = "custom.gascity.blocked_repair_bd_version"

type blockedRepairEvent struct {
	Subject string `json:"subject"`
	Payload struct {
		Scope         string `json:"scope"`
		RowsCorrected int    `json:"rows_corrected"`
		BDVersion     string `json:"bd_version"`
	} `json:"payload"`
}

// blockedRepairEvents reads every beads.blocked.recomputed event the city has
// recorded, in log order.
func blockedRepairEvents(t *testing.T, cityDir string) []blockedRepairEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []blockedRepairEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"beads.blocked.recomputed"`) {
			continue
		}
		var ev blockedRepairEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("parse event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read events log: %v", err)
	}
	return out
}

// blockedRepairScopeBD runs `gc bd` against one scope: the city when rig is
// "", else the named rig.
func blockedRepairScopeBD(t *testing.T, run *helpers.TopologyRun, rig string, args ...string) string {
	t.Helper()
	full := []string{"bd"}
	if rig != "" {
		full = append(full, "--rig", rig)
	}
	full = append(full, args...)
	out, err := run.City.GCStdout(full...)
	if err != nil {
		t.Fatalf("gc %s: %v\n%s", strings.Join(full, " "), err, out)
	}
	return out
}

func blockedRepairCreate(t *testing.T, run *helpers.TopologyRun, rig, title string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	lastJSONLine(t, blockedRepairScopeBD(t, run, rig, "create", title, "--json"), &created)
	if created.ID == "" {
		t.Fatalf("bd create %q returned no id", title)
	}
	return created.ID
}

func blockedRepairReady(t *testing.T, run *helpers.TopologyRun, rig string) []string {
	t.Helper()
	out := blockedRepairScopeBD(t, run, rig, "ready", "--json", "--limit", "0")
	start := strings.Index(out, "[")
	if start < 0 {
		t.Fatalf("bd ready returned no JSON array:\n%s", out)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &rows); err != nil {
		t.Fatalf("parse bd ready: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// overBlockScope seeds B <- A (blocks), A <- C (relates-to), A <- F
// (discovered-from), then flags C and F blocked the way migration 0059 does,
// and returns C and F.
func overBlockScope(t *testing.T, run *helpers.TopologyRun, rig string) []string {
	t.Helper()
	b := blockedRepairCreate(t, run, rig, "blocker")
	a := blockedRepairCreate(t, run, rig, "blocked")
	c := blockedRepairCreate(t, run, rig, "relates to blocked")
	f := blockedRepairCreate(t, run, rig, "discovered from blocked")
	blockedRepairScopeBD(t, run, rig, "dep", "add", a, b)
	blockedRepairScopeBD(t, run, rig, "dep", "add", c, a, "--type", "relates-to")
	blockedRepairScopeBD(t, run, rig, "dep", "add", f, a, "--type", "discovered-from")
	hidden := []string{c, f}
	ready := blockedRepairReady(t, run, rig)
	for _, id := range hidden {
		if !slices.Contains(ready, id) {
			t.Fatalf("scope %q: %s not ready before the damage: %v", rig, id, ready)
		}
	}
	blockedRepairScopeBD(t, run, rig, "sql", "UPDATE issues SET is_blocked = 1 WHERE id IN ('"+c+"', '"+f+"')")
	ready = blockedRepairReady(t, run, rig)
	for _, id := range hidden {
		if slices.Contains(ready, id) {
			t.Fatalf("scope %q: the simulated 0059 damage did not hide %s: %v", rig, id, ready)
		}
	}
	// The scope was last repaired under an older bd: the next start is the
	// first one after a bd upgrade.
	blockedRepairScopeBD(t, run, rig, "config", "set", blockedRepairMarkerKey, "1.3.0")
	return hidden
}

var blockedRepairFinishedLine = regexp.MustCompile(`blocked-flag repair finished for (\d+) of (\d+) scope\(s\) in (\S+)`)

func TestBlockedRepairOnProxiedCityAndRig(t *testing.T) {
	// Three proxied city restarts (about 4 minutes) do not fit the Tier A smoke
	// budget; Bazel's acceptance lane opts in (test/acceptance/BUILD.bazel).
	helpers.RequireTopologyMatrix(t)
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	helpers.RequireBDAtLeast(t, bdPath, "v1.1.0", "bd recompute-blocked")
	var topo helpers.BeadsTopology
	for _, candidate := range helpers.BeadsTopologies() {
		if candidate.Name == "M1-proxied-local" {
			topo = candidate
		}
	}
	if topo.Name == "" {
		t.Fatal("no M1-proxied-local topology")
	}
	run := helpers.StartTopology(t, testEnv, topo, bdPath, doltPath)
	const rig = "blockedrig"
	if out, err := run.GC("rig", "add", run.RigWorkspace(t, rig)); err != nil {
		t.Fatalf("gc rig add: %v\n%s", err, out)
	}
	scopeID := map[string]string{"": "city", rig: "rig/" + rig}

	// `gc rig add` repairs the rig it added, on its own: an existing store
	// adopted as a rig must not wait for the next start.
	if added := blockedRepairEvents(t, run.City.Dir); len(added) != 1 || added[0].Payload.Scope != "rig/"+rig {
		t.Fatalf("gc rig add recorded repair events %+v, want one for rig/%s", added, rig)
	}

	// First start of the fresh city: the city scope is repaired once (nothing
	// to correct) and stamped; the rig, already stamped by rig add, is not.
	run.Start(t)
	first := blockedRepairEvents(t, run.City.Dir)
	if len(first) != 2 || first[1].Payload.Scope != "city" {
		t.Fatalf("after the first start, repair events = %+v, want rig add's then the city's", first)
	}
	bdVersion := first[0].Payload.BDVersion
	for _, ev := range first {
		if ev.Payload.RowsCorrected != 0 || ev.Payload.BDVersion != bdVersion {
			t.Fatalf("fresh-city repair event = %+v, want 0 rows under bd %s", ev, bdVersion)
		}
	}

	hidden := map[string][]string{}
	for r := range scopeID {
		hidden[r] = overBlockScope(t, run, r)
	}

	// The upgrade start: both scopes are repaired.
	logPath := filepath.Join(run.Env.Get("GC_HOME"), "supervisor.log")
	run.Start(t)
	repaired := blockedRepairEvents(t, run.City.Dir)[len(first):]
	got := map[string]int{}
	for _, ev := range repaired {
		if ev.Subject != ev.Payload.Scope || ev.Payload.BDVersion != bdVersion {
			t.Errorf("repair event = %+v, want subject = scope and bd %s", ev, bdVersion)
		}
		got[ev.Payload.Scope] = ev.Payload.RowsCorrected
	}
	for r, id := range scopeID {
		if got[id] != 2 {
			t.Errorf("%s: rows corrected = %d (events %+v), want 2", id, got[id], repaired)
		}
		ready := blockedRepairReady(t, run, r)
		for _, bead := range hidden[r] {
			if !slices.Contains(ready, bead) {
				t.Errorf("%s: %s is still hidden from bd ready after the repair: %v", id, bead, ready)
			}
		}
		if marker := strings.TrimSpace(blockedRepairScopeBD(t, run, r, "config", "get", blockedRepairMarkerKey)); !strings.Contains(marker, bdVersion) {
			t.Errorf("%s: marker = %q, want %s", id, marker, bdVersion)
		}
	}
	if log, err := os.ReadFile(logPath); err == nil {
		if !strings.Contains(string(log), "repairing blocked flags in 2 scope(s)") {
			t.Errorf("supervisor.log does not announce the repair:\n%s", log)
		}
		for _, m := range blockedRepairFinishedLine.FindAllStringSubmatch(string(log), -1) {
			t.Logf("repair: %s of %s scope(s) in %s", m[1], m[2], m[3])
		}
	}

	// The next start under the same bd does nothing.
	run.Start(t)
	if after := blockedRepairEvents(t, run.City.Dir); len(after) != len(first)+len(repaired) {
		t.Fatalf("a start under the same bd repaired again: %+v", after[len(first)+len(repaired):])
	}
}
