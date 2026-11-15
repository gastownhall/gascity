package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

var stuckNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func stuckView(complete bool, rows ...api.StatusStuckCreatingSessionView) *api.StatusStuckCreatingView {
	return &api.StatusStuckCreatingView{Sessions: rows, Complete: complete}
}

func renderStuck(v *api.StatusStuckCreatingView) string {
	var buf bytes.Buffer
	renderStuckCreatingBlock(&buf, v, stuckNow)
	return buf.String()
}

func assertNoStarvation(t *testing.T, label, out string) {
	t.Helper()
	if strings.Contains(strings.ToLower(out), "starv") {
		t.Errorf("%s output names starvation, which status does not measure:\n%s", label, out)
	}
}

// TestRenderStuckCreatingBlockThreeStates pins the three measured states of
// the block. Silence means a measured zero and nothing else: an unmeasured
// block says so in one line.
func TestRenderStuckCreatingBlockThreeStates(t *testing.T) {
	t.Run("measured zero is silent", func(t *testing.T) {
		if out := renderStuck(stuckView(true)); out != "" {
			t.Fatalf("output = %q, want nothing for a measured zero", out)
		}
	})
	t.Run("not measured is one explicit line", func(t *testing.T) {
		out := renderStuck(nil)
		if !strings.Contains(out, "Stuck creating: unknown (not measured)") {
			t.Fatalf("output = %q, want the unknown line", out)
		}
		assertNoStarvation(t, "unknown", out)
	})
	t.Run("list names every session with its age and the remedy", func(t *testing.T) {
		out := renderStuck(stuckView(true,
			api.StatusStuckCreatingSessionView{ID: "gc-12", Template: "myrig/worker", State: "creating", Since: stuckNow.Add(-72 * time.Hour)},
			api.StatusStuckCreatingSessionView{ID: "gc-15", Template: "myrig/builder", State: "start-pending", Since: stuckNow.Add(-5 * time.Minute)},
		))
		for _, want := range []string{"Stuck creating: 2 sessions", "gc-12", "myrig/worker", "creating", "3d", "gc-15", "myrig/builder", "start-pending", "5m", "gc doctor", "gc session close"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "lower bound") {
			t.Errorf("a complete read must not claim a lower bound:\n%s", out)
		}
		assertNoStarvation(t, "list", out)
	})
	t.Run("one session is singular", func(t *testing.T) {
		out := renderStuck(stuckView(true, api.StatusStuckCreatingSessionView{ID: "gc-1", State: "creating", Since: stuckNow.Add(-time.Hour)}))
		if !strings.Contains(out, "Stuck creating: 1 session") || strings.Contains(out, "1 sessions") {
			t.Fatalf("output = %q, want a singular count", out)
		}
	})
}

// TestRenderStuckCreatingBlockUnanchoredRow: a row with no anchor has no age,
// and says so instead of printing 0s or a zero-time date.
func TestRenderStuckCreatingBlockUnanchoredRow(t *testing.T) {
	out := renderStuck(stuckView(true, api.StatusStuckCreatingSessionView{ID: "gc-9", Template: "myrig/worker", State: "creating"}))
	if !strings.Contains(out, "gc-9") || !strings.Contains(out, "age unknown") {
		t.Fatalf("output = %q, want the row with \"age unknown\"", out)
	}
	for _, bad := range []string{"0s", "0001", "1970"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q for an unanchored row:\n%s", bad, out)
		}
	}
}

// TestRenderStuckCreatingBlockPartialReadIsALowerBound: a partial read never
// claims absence, with or without found rows.
func TestRenderStuckCreatingBlockPartialReadIsALowerBound(t *testing.T) {
	withRows := renderStuck(stuckView(false, api.StatusStuckCreatingSessionView{ID: "gc-1", State: "creating", Since: stuckNow.Add(-time.Hour)}))
	if !strings.Contains(withRows, "gc-1") || !strings.Contains(withRows, "lower bound") {
		t.Errorf("partial read with rows = %q, want the row and a lower-bound note", withRows)
	}
	empty := renderStuck(stuckView(false))
	if empty == "" || !strings.Contains(empty, "partial") {
		t.Errorf("partial read with no rows = %q, want a line saying the read was partial, not silence", empty)
	}
	assertNoStarvation(t, "partial rows", withRows)
	assertNoStarvation(t, "partial empty", empty)
}

// TestCityStatusTextStuckCreatingBothProducers drives the three states through
// the shared text renderer from each producer: the API-decoded snapshot and the
// local-fallback one.
func TestCityStatusTextStuckCreatingBothProducers(t *testing.T) {
	prev := cityStatusNow
	cityStatusNow = func() time.Time { return stuckNow }
	t.Cleanup(func() { cityStatusNow = prev })

	text := func(s cityStatusSnapshot) string {
		var buf bytes.Buffer
		renderCityStatusText(s, newDrainOps(nil), &buf)
		return buf.String()
	}
	row := api.StatusStuckCreatingSessionView{ID: "gc-77", Template: "myrig/worker", State: "creating", Since: stuckNow.Add(-2 * time.Hour)}

	t.Run("api producer", func(t *testing.T) {
		absent := text(snapshotFromStatusView(t.TempDir(), api.StatusView{CityName: "c"}))
		if !strings.Contains(absent, "Stuck creating: unknown (not measured)") {
			t.Errorf("absent block text = %q, want the unknown line", absent)
		}
		zero := text(snapshotFromStatusView(t.TempDir(), api.StatusView{CityName: "c", StuckCreating: stuckView(true)}))
		if strings.Contains(zero, "Stuck creating") {
			t.Errorf("measured zero text = %q, want it silent", zero)
		}
		listed := text(snapshotFromStatusView(t.TempDir(), api.StatusView{CityName: "c", StuckCreating: stuckView(true, row)}))
		if !strings.Contains(listed, "gc-77") || !strings.Contains(listed, "2h") {
			t.Errorf("listed text = %q, want gc-77 aged 2h", listed)
		}
		assertNoStarvation(t, "api text", absent+zero+listed)
	})

	t.Run("local producer", func(t *testing.T) {
		infos := []session.Info{stuckInfo("gc-77", "creating", stuckNow.Add(-2*time.Hour))}
		listed := text(cityStatusSnapshot{CityName: "c", StuckCreating: stuckCreatingFromSessionSnapshot(true, &config.City{}, newSessionBeadSnapshotFromInfos(infos), stuckNow)})
		if !strings.Contains(listed, "gc-77") || !strings.Contains(listed, "2h") {
			t.Errorf("local listed text = %q, want gc-77 aged 2h", listed)
		}
		zero := text(cityStatusSnapshot{CityName: "c", StuckCreating: stuckCreatingFromSessionSnapshot(true, &config.City{}, newSessionBeadSnapshotFromInfos(nil), stuckNow)})
		if strings.Contains(zero, "Stuck creating") {
			t.Errorf("local measured zero text = %q, want it silent", zero)
		}
		unknown := text(cityStatusSnapshot{CityName: "c", StuckCreating: stuckCreatingFromSessionSnapshot(true, &config.City{}, newSessionBeadSnapshotWithError(errors.New("boom")), stuckNow)})
		if !strings.Contains(unknown, "Stuck creating: unknown (not measured)") {
			t.Errorf("local load-error text = %q, want the unknown line", unknown)
		}
	})
}

func stuckInfo(id, state string, startedAt time.Time) session.Info {
	return session.Info{
		ID:                     id,
		Type:                   session.BeadType,
		Template:               "myrig/worker",
		MetadataState:          state,
		PendingCreateStartedAt: startedAt.UTC().Format(time.RFC3339),
	}
}

// TestStuckCreatingFromSessionSnapshot pins the local-fallback producer's
// mapping of the session read onto the three measured states.
func TestStuckCreatingFromSessionSnapshot(t *testing.T) {
	cfg := &config.City{}

	t.Run("a load error is unknown, never an empty measured block", func(t *testing.T) {
		if got := stuckCreatingFromSessionSnapshot(true, cfg, newSessionBeadSnapshotWithError(errors.New("timed out")), stuckNow); got != nil {
			t.Fatalf("got %+v, want nil (not measured)", got)
		}
	})
	t.Run("no bead store is a measured zero", func(t *testing.T) {
		got := stuckCreatingFromSessionSnapshot(false, cfg, newSessionBeadSnapshotFromInfos(nil), stuckNow)
		if got == nil || !got.Complete || len(got.Sessions) != 0 {
			t.Fatalf("got %+v, want a complete, empty block", got)
		}
	})
	t.Run("a store with no snapshot is unknown", func(t *testing.T) {
		if got := stuckCreatingFromSessionSnapshot(true, cfg, nil, stuckNow); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("abandoned creates are listed oldest first and healthy rows are not", func(t *testing.T) {
		notASession := stuckInfo("gc-x", "creating", stuckNow.Add(-24*time.Hour))
		notASession.Type = "task"
		infos := []session.Info{
			stuckInfo("gc-new", "creating", stuckNow.Add(-5*time.Minute)),
			stuckInfo("gc-old", "start-pending", stuckNow.Add(-3*time.Hour)),
			stuckInfo("gc-young", "creating", stuckNow.Add(-10*time.Second)),
			stuckInfo("gc-active", "active", stuckNow.Add(-3*time.Hour)),
			notASession,
		}
		got := stuckCreatingFromSessionSnapshot(true, cfg, newSessionBeadSnapshotFromInfos(infos), stuckNow)
		if got == nil || !got.Complete {
			t.Fatalf("got %+v, want a complete block", got)
		}
		var ids []string
		for _, s := range got.Sessions {
			ids = append(ids, s.ID)
		}
		if strings.Join(ids, ",") != "gc-old,gc-new" {
			t.Fatalf("ids = %v, want [gc-old gc-new]", ids)
		}
		if got.Sessions[0].State != "start-pending" || got.Sessions[0].Template != "myrig/worker" || !got.Sessions[0].Since.Equal(stuckNow.Add(-3*time.Hour).Truncate(time.Second)) {
			t.Errorf("row 0 = %+v", got.Sessions[0])
		}
	})
}

// TestStatusJSONStuckCreatingThreeStates: the --json block mirrors the wire
// block with the same three states, from either producer.
func TestStatusJSONStuckCreatingThreeStates(t *testing.T) {
	marshal := func(s cityStatusSnapshot) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(cityStatusJSONFromSnapshot(s, s.Summary))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}

	if _, ok := marshal(cityStatusSnapshot{CityName: "c"})["stuck_creating"]; ok {
		t.Error("an unmeasured block must be absent from --json")
	}
	if got := string(marshal(cityStatusSnapshot{CityName: "c", StuckCreating: stuckView(true)})["stuck_creating"]); got != `{"sessions":[],"complete":true}` {
		t.Errorf("measured zero = %s, want {\"sessions\":[],\"complete\":true}", got)
	}
	populated := stuckView(false,
		api.StatusStuckCreatingSessionView{ID: "gc-1", Template: "myrig/worker", State: "creating", Since: stuckNow.Add(-time.Hour)},
		api.StatusStuckCreatingSessionView{ID: "gc-2", State: "start-pending"},
	)
	want := `{"sessions":[{"id":"gc-1","template":"myrig/worker","state":"creating","since":"2026-11-15T11:00:00Z"},{"id":"gc-2","state":"start-pending"}],"complete":false}`
	if got := string(marshal(cityStatusSnapshot{CityName: "c", StuckCreating: populated})["stuck_creating"]); got != want {
		t.Errorf("populated = %s\nwant      %s", got, want)
	}
	// The API-decoded producer reaches the same builder.
	viaAPI := snapshotFromStatusView(t.TempDir(), api.StatusView{CityName: "c", StuckCreating: populated})
	if got := string(marshal(viaAPI)["stuck_creating"]); got != want {
		t.Errorf("api-decoded = %s\nwant        %s", got, want)
	}
}
