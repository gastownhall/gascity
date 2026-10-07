package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The v2 replay (plan D3b; architecture §3.4): for each legacy tick in a
// scripts/v2-replay-export directory, rebuild the World, run the trace-only
// pass, and compare its intents with legacy's decision records.
//
// What the export cannot carry, the replay states rather than guesses:
//   - mdash keeps a trace record's columns and its fields, not its
//     session_name or session_bead_id, so the comparison is per (tick,
//     template, action class) counts, not per row;
//   - there is no city config, so each row's template becomes a default agent
//     (a named row also its named session);
//   - there is no runtime observation, so the inventory lists the rows whose
//     state says running, with their own identity;
//   - maintainer-city's rows are the export-time copy: every tick sees the
//     current rows, less those created after it.

// replayLookup reads GC_V2_REPLAY_DIR; injected, so no test sets env.
var replayLookup = os.LookupEnv

// chTime is ClickHouse's JSONEachRow DateTime64(6, 'UTC') layout.
const chTime = "2006-01-02 15:04:05.999999"

// replayTraceRow is one controller_trace row. Counters arrive as strings.
type replayTraceRow struct {
	Seq        uint64 `json:"seq,string"`
	TS         string `json:"ts"`
	TickID     string `json:"tick_id"`
	RecordType string `json:"record_type"`
	Site       string `json:"site_code"`
	Outcome    string `json:"outcome_code"`
	Reason     string `json:"reason_code"`
	Template   string `json:"template"`
	Fields     string `json:"fields"`
}

// replaySnapshot is one bead_snapshots row (bd cities).
type replaySnapshot struct {
	TS        string            `json:"ts"`
	BeadID    string            `json:"bead_id"`
	Title     string            `json:"title"`
	Status    string            `json:"status"`
	Type      string            `json:"issue_type"`
	Assignee  string            `json:"assignee"`
	CreatedAt *string           `json:"created_at"`
	Labels    []string          `json:"labels"`
	Metadata  map[string]string `json:"metadata"`
	Deleted   int               `json:"deleted"`
}

// replayTick is one legacy tick: its cycle_result and decision records.
type replayTick struct {
	ID       string
	Seq      uint64
	At       time.Time
	Decision []replayTraceRow
}

type replayExport struct {
	City  string
	Exact bool          // rows from maintainer-city's SQLite copy
	Rows  []beads.Bead  // Exact: the copy
	Snaps [][]replayRow // otherwise: each bead's snapshots in ts order
	Ticks []replayTick
}

type replayRow struct {
	At   time.Time
	Gone bool
	Bead beads.Bead
}

func readReplayJSONL(t *testing.T, path string, each func(line []byte) error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for n := 1; sc.Scan(); n++ {
		if err := each(sc.Bytes()); err != nil {
			t.Fatalf("%s:%d: %v", path, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

func parseCHTime(s string) (time.Time, error) { return time.ParseInLocation(chTime, s, time.UTC) }

// loadReplayExport reads dir as scripts/v2-replay-export writes it.
func loadReplayExport(t *testing.T, dir string) replayExport {
	t.Helper()
	var man struct {
		City   string `json:"city"`
		Source string `json:"session_rows_source"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err == nil {
		err = json.Unmarshal(raw, &man)
	}
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	x := replayExport{City: man.City, Exact: strings.HasPrefix(man.Source, "sqlite")}
	snaps := make(map[string][]replayRow)
	readReplayJSONL(t, filepath.Join(dir, "session_rows.jsonl"), func(line []byte) error {
		if x.Exact {
			var r struct{ Bead beads.Bead }
			err := json.Unmarshal(line, &r)
			x.Rows = append(x.Rows, r.Bead)
			return err
		}
		var s replaySnapshot
		if err := json.Unmarshal(line, &s); err != nil {
			return err
		}
		at, err := parseCHTime(s.TS)
		b := beads.Bead{ID: s.BeadID, Title: s.Title, Status: s.Status, Type: s.Type, Assignee: s.Assignee, Labels: s.Labels, Metadata: s.Metadata}
		if s.CreatedAt != nil {
			b.CreatedAt, _ = parseCHTime(*s.CreatedAt)
		}
		snaps[s.BeadID] = append(snaps[s.BeadID], replayRow{At: at, Gone: s.Deleted != 0, Bead: b})
		return err
	})
	for _, id := range slices.Sorted(maps.Keys(snaps)) {
		x.Snaps = append(x.Snaps, snaps[id])
	}
	ticks := make(map[string]*replayTick)
	tick := func(id string) *replayTick {
		if ticks[id] == nil {
			ticks[id] = &replayTick{ID: id}
		}
		return ticks[id]
	}
	readReplayJSONL(t, filepath.Join(dir, "controller_trace.jsonl"), func(line []byte) error {
		var r replayTraceRow
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		switch r.RecordType {
		case "decision":
			tick(r.TickID).Decision = append(tick(r.TickID).Decision, r)
		case "cycle_result":
			var f struct{ Phase string }
			_ = json.Unmarshal([]byte(r.Fields), &f) // mdash blanks fields over 16 KiB: phase unknown
			if f.Phase != "" && f.Phase != "tick" {
				return nil // startup and shutdown are not ticks
			}
			at, err := parseCHTime(r.TS)
			tk := tick(r.TickID)
			tk.Seq, tk.At = r.Seq, at
			return err
		}
		return nil
	})
	for _, tk := range ticks {
		if tk.Seq != 0 { // decisions of a tick with no tick cycle_result are not replayed
			x.Ticks = append(x.Ticks, *tk)
		}
	}
	slices.SortFunc(x.Ticks, func(a, b replayTick) int { return cmp.Compare(a.Seq, b.Seq) })
	return x
}

// rowsAt are the session rows as of at.
func (x replayExport) rowsAt(at time.Time) []beads.Bead {
	var out []beads.Bead
	if x.Exact {
		for _, b := range x.Rows {
			if !b.CreatedAt.After(at) {
				out = append(out, b)
			}
		}
		return out
	}
	for _, snaps := range x.Snaps {
		var last *replayRow
		for i := range snaps {
			if !snaps[i].At.After(at) {
				last = &snaps[i]
			}
		}
		if last != nil && !last.Gone {
			out = append(out, last.Bead)
		}
	}
	return out
}

// replayCity is the config the rows imply: a default agent per template,
// and a named session for each configured named identity.
func replayCity(name string, rows []beads.Bead) *config.City {
	cfg := &config.City{Workspace: config.Workspace{Name: name}}
	seen := make(map[string]bool)
	for _, b := range rows {
		tmpl, named := b.Metadata["template"], b.Metadata["configured_named_identity"]
		if tmpl != "" && !seen[tmpl] {
			seen[tmpl] = true
			dir, n := config.ParseQualifiedName(tmpl)
			cfg.Agents = append(cfg.Agents, config.Agent{Name: n, Dir: dir, StartCommand: "true"})
		}
		if named != "" && !seen["named:"+named] {
			seen["named:"+named] = true
			dir, n := config.ParseQualifiedName(tmpl)
			cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: n, Dir: dir, Mode: b.Metadata["configured_named_mode"]})
		}
	}
	return cfg
}

// replayRunning are the states legacy writes for a running runtime.
var replayRunning = map[string]bool{"active": true, "awake": true, "draining": true}

// replayPass runs a fresh planner's trace-only pass over rows at at.
func replayPass(t *testing.T, x replayExport, rows []beads.Bead, at time.Time) *passTrace {
	t.Helper()
	cache, _ := newDemandCache(t, true, rows...) // the export has no work beads: demand is empty on every leg
	var names []string
	attrs := make(map[string]InventoryAttrs)
	for _, b := range rows {
		if name := b.Metadata["session_name"]; name != "" && replayRunning[b.Metadata["state"]] {
			names = append(names, name)
			attrs[name] = InventoryAttrs{DeadKnown: true, AttachedKnown: true, Identity: runtimeIdentity{
				Known: true, SessionID: b.ID, Token: b.Metadata["instance_token"], ReadAt: at,
			}}
		}
	}
	clk := &clock.Fake{Time: at}
	obs := NewObservationCache(clk, time.Minute, "replay")
	obs.PublishInventory(obsPass(clk, 1, 1, completeBackend("", names...)), attrs)
	env := &reconcileEnv{Gen: 1, Cfg: replayCity(x.City, rows), SP: &sleepCountingProvider{}}
	p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	p.tracePass(gatherEnv{
		CityPath: t.TempDir(), CityName: x.City,
		Env:          func() *reconcileEnv { return env },
		Sessions:     func() beads.Store { return cache },
		RigStores:    func() map[string]beads.Store { return nil },
		Recording:    func() *externalReadsRecording { return nil },
		Observations: func() *ObservationCache { return obs },
		Episodes:     func() (map[string]session.StartupHealthEpisode, error) { return readStartupHealthEpisodes(cache) },
		ResolveTemplate: func(_ *reconcileEnv, info session.Info) (TemplateParams, error) {
			return TemplateParams{SessionName: info.SessionNameMetadata}, nil
		},
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}, at)
	return p.out.record.Load()
}

// legacyAction is what one legacy decision record does: its action class
// and the CONTRACT v5 §4 arm that owns it, or "§12.2/<item>" for a legacy
// action v2 deliberately never takes (§12.1 for a latch-refused one). A
// site/outcome/reason key wins over site/outcome. Records not listed take no
// action.
type legacyAction struct{ Class, Arm string }

var legacyActions = map[string]legacyAction{
	"reconciler.session.wake_decision/start_candidate":     {"start", "A18"},
	"reconciler.session.drain/drain":                       {"drain", "A20"},
	"reconciler.session.orphan_or_suspended/drain":         {"drain", "A20"},
	"reconciler.session.config_drift/drain":                {"drain", "A14"},
	"reconciler.session.config_drift/restart_in_place":     {"stop", "A14"},
	"reconciler.session.config_drift/relaunch":             {"stop", "A14"},
	"reconciler.session.config_drift/repair_in_place":      {"stop", "A14"},
	"reconciler.session.idle_timeout/stop":                 {"stop", "A15"},
	"reconciler.session.bead_reassign_cycle/restart":       {"stop", "A13"},
	"reconciler.session.recycle_named_phantom/recycled":    {"zombie", "A12"},
	"reconciler.session.drain_ack/stop_pending":            {"signal", "A11"},
	"reconciler.session.drain_ack/cancel_reconciler_ack":   {"drain-cancel", "A19"},
	"reconciler.drain.cancel/cancel":                       {"drain-cancel", "A19"},
	"reconciler.drain.cancel/cancel_pending":               {"drain-cancel", "A19"},
	"reconciler.drain.cancel/cancel_assigned_work":         {"drain-cancel", "A19"},
	"reconciler.drain.stale/cancel":                        {"drain-cancel", "A19"},
	"reconciler.drain.complete/complete":                   {"stop", "A4"},
	"reconciler.session.rollback_pending_create/rollback":  {"rollback", "A10"},
	"reconciler.session.rollback_pending_create/applied":   {"rollback", "A10"},
	"reconciler.session.rollback_pending_create/healed":    {"heal", "A6"},
	"reconciler.session.close_orphan/closed":               {"close", "A21"},
	"reconciler.session.close_failed_create/closed":        {"close", "A21"},
	"reconciler.drain.timeout/complete":                    {"stop", "§12.2/1"},
	"reconciler.drain.timeout/retry":                       {"stop", "§12.2/1"},
	"reconciler.drain.cancel/cancel_min_floor":             {"drain-cancel", "§12.2/14"},
	"reconciler.session.reset_stalled/failed":              {"stop", "§12.2/14"},
	"reconciler.session.terminal_provider_error/unhealthy": {"zombie", "A12"},
	"reconciler.session.idle_timeout/stop/max_session_age": {"stop", "§12.1/PAR-AGE"},
}

// v2Class is an intent kind's action class. A create is a bring-up, as
// legacy's start candidate covers the row it materializes.
func v2Class(kind string) string {
	switch kind {
	case intentStart, intentCreate:
		return "start"
	case intentDrainBegin, intentDrainBeginFresh:
		return "drain"
	case intentSignal, intentSignalFresh:
		return "signal"
	case intentDrainVoid:
		return "drain-cancel"
	case intentRowHeal:
		return "heal"
	}
	return kind
}

var replayDestructive = map[string]bool{"drain": true, "signal": true, "stop": true, "close": true, "rollback": true, "zombie": true}

// replayDivergence is one (tick, template, class) whose counts differ.
type replayDivergence struct {
	Family   string   `json:"family"`
	Item     string   `json:"item,omitempty"`
	Tick     string   `json:"tick"`
	At       string   `json:"at"`
	Template string   `json:"template"`
	Class    string   `json:"class"`
	Legacy   int      `json:"legacy"`
	V2       int      `json:"v2"`
	Records  []string `json:"legacy_records,omitempty"` // site/outcome/reason
	Intents  []string `json:"v2_intents,omitempty"`     // kind/reason[/cause]
}

// replaySummary is what the replay compared.
type replaySummary struct {
	City          string         `json:"city"`
	Ticks         int            `json:"ticks"`
	LegacyActions int            `json:"legacy_actions"`
	V2Intents     int            `json:"v2_intents"`
	PassErrors    []string       `json:"pass_errors,omitempty"`
	NoAction      map[string]int `json:"legacy_no_action_records"` // site/outcome, by count
	Divergences   map[string]int `json:"divergences_by_family"`
}

type replayCell struct {
	legacy, v2       int
	records, intents []string
	arms             []string
}

// replay compares every tick in x and returns the divergences, grouped by
// family, and the summary.
func replay(t *testing.T, x replayExport) ([]replayDivergence, replaySummary) {
	t.Helper()
	built := make(map[string]bool)
	for _, arm := range rowArms {
		built[arm.name] = true
	}
	sum := replaySummary{City: x.City, Ticks: len(x.Ticks), NoAction: map[string]int{}, Divergences: map[string]int{}}
	var out []replayDivergence
	for _, tk := range x.Ticks {
		rows := x.rowsAt(tk.At)
		rec := replayPass(t, x, rows, tk.At)
		if rec.Err != "" {
			sum.PassErrors = append(sum.PassErrors, tk.ID+": "+rec.Err)
		}
		cells := make(map[[2]string]*replayCell)
		cell := func(tmpl, class string) *replayCell {
			k := [2]string{tmpl, class}
			if cells[k] == nil {
				cells[k] = &replayCell{}
			}
			return cells[k]
		}
		for _, d := range tk.Decision {
			act, ok := legacyActions[d.Site+"/"+d.Outcome+"/"+d.Reason]
			if !ok {
				act, ok = legacyActions[d.Site+"/"+d.Outcome]
			}
			if !ok {
				sum.NoAction[d.Site+"/"+d.Outcome]++
				continue
			}
			sum.LegacyActions++
			c := cell(d.Template, act.Class)
			c.legacy++
			c.records = append(c.records, d.Site+"/"+d.Outcome+"/"+d.Reason)
			if !slices.Contains(c.arms, act.Arm) {
				c.arms = append(c.arms, act.Arm)
			}
		}
		info := make(map[string]session.Info)
		for _, r := range rows {
			info[r.ID] = sessionInfoFromBead(r)
		}
		for _, it := range slices.Concat(rec.Admitted, rec.Deferred) {
			sum.V2Intents++
			tmpl := cmp.Or(it.Create.Template, info[it.Key.ID].Template)
			if it.Create.Named != nil {
				tmpl = it.Create.Named.Template
			}
			c := cell(tmpl, v2Class(it.Kind))
			c.v2++
			c.intents = append(c.intents, strings.TrimSuffix(it.Kind+"/"+it.Reason+"/"+it.Cause, "/"))
		}
		for _, k := range slices.SortedFunc(maps.Keys(cells), func(a, b [2]string) int { return cmp.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }) {
			c := cells[k]
			if c.legacy == c.v2 {
				continue
			}
			d := replayDivergence{Tick: tk.ID, At: tk.At.Format(time.RFC3339Nano), Template: k[0], Class: k[1], Legacy: c.legacy, V2: c.v2, Records: c.records, Intents: c.intents}
			slices.Sort(c.arms)
			d.Family, d.Item = replayFamily(d, c.arms, built, rec, info, tk.At)
			sum.Divergences[d.Family]++
			out = append(out, d)
		}
	}
	slices.SortStableFunc(out, func(a, b replayDivergence) int { return cmp.Compare(a.Family, b.Family) })
	return out, sum
}

// replayFamily files a divergence: a v5 §12.2 (or §12.1 latch) difference
// by item, a deferred arm, INC-003, unknown-never-destroys, or unexplained.
// arms are the legacy records' owning arms. The two evidence families need
// as many rows of the template showing the cause as legacy acted beyond v2.
// C8.9's fresh attach and pending reads run in the effect, which a trace-only
// pass never runs, so the replay cannot see that family.
func replayFamily(d replayDivergence, arms []string, built map[string]bool, rec *passTrace, info map[string]session.Info, at time.Time) (family, item string) {
	if d.V2 > d.Legacy {
		return "unexplained", ""
	}
	var sec string
	var items []string
	for _, a := range arms {
		s, it, _ := strings.Cut(a, "/")
		if !strings.HasPrefix(s, "§12") || (sec != "" && s != sec) {
			sec = ""
			break
		}
		sec, items = s, append(items, it)
	}
	if sec != "" {
		return "v5 " + sec, strings.Join(items, ",")
	}
	if !slices.ContainsFunc(arms, func(a string) bool { return built[a] }) {
		return "deferred arm", strings.Join(arms, ",")
	}
	short, undesired, unknown, undesiredRecords := d.Legacy-d.V2, 0, 0, 0
	for _, r := range rec.Rows {
		if r.Template == d.Template && wakeGracePreservesUndesiredRow(info[r.Key.ID], at) {
			undesired++
		}
		if r.Template == d.Template && r.Reason == decideLivenessUnknown {
			unknown++
		}
	}
	for _, rr := range d.Records {
		if strings.HasSuffix(rr, "/orphaned") || strings.HasSuffix(rr, "/suspended") {
			undesiredRecords++
		}
	}
	switch {
	case d.Class == "drain" && min(undesired, undesiredRecords) >= short:
		return "INC-003", ""
	case replayDestructive[d.Class] && unknown >= short:
		return "unknown-never-destroys", ""
	}
	return "unexplained", ""
}

// writeReplay writes divergence.jsonl and divergence_summary.json into dir.
func writeReplay(t *testing.T, dir string, divs []replayDivergence, sum replaySummary) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, d := range divs {
		if err := enc.Encode(d); err != nil {
			t.Fatal(err)
		}
	}
	s, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"divergence.jsonl": buf.Bytes(), "divergence_summary.json": append(s, '\n')} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReplay replays the export in GC_V2_REPLAY_DIR and writes its report
// beside it. It fails on an export with no tick, which compares nothing.
func TestReplay(t *testing.T) {
	dir, ok := replayLookup("GC_V2_REPLAY_DIR")
	if !ok || dir == "" {
		t.Skip("GC_V2_REPLAY_DIR is not set")
	}
	x := loadReplayExport(t, dir)
	if len(x.Ticks) == 0 {
		t.Fatalf("%s: no legacy tick in controller_trace.jsonl: nothing to compare", dir)
	}
	divs, sum := replay(t, x)
	writeReplay(t, dir, divs, sum)
	t.Logf("%s: %d ticks, %d legacy actions, %d v2 intents; divergences by family %v", x.City, sum.Ticks, sum.LegacyActions, sum.V2Intents, sum.Divergences)
}

// Kills a driver that silently compares nothing: the sample exports (both
// session-row shapes) replay to exactly their expected reports, and the
// gated test reads its directory through the injected lookup.
func TestReplaySampleMatchesExpectedDivergences(t *testing.T) {
	for _, city := range []string{"mc", "bd"} {
		t.Run(city, func(t *testing.T) {
			src, out := filepath.Join("testdata", "v2replay", city), t.TempDir()
			for _, name := range []string{"manifest.json", "session_rows.jsonl", "controller_trace.jsonl", "events.jsonl"} {
				data, err := os.ReadFile(filepath.Join(src, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			saved := replayLookup
			t.Cleanup(func() { replayLookup = saved })
			replayLookup = func(key string) (string, bool) { return out, key == "GC_V2_REPLAY_DIR" }
			TestReplay(t)
			for _, name := range []string{"divergence.jsonl", "divergence_summary.json"} {
				got, err := os.ReadFile(filepath.Join(out, name))
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join(src, "expected", name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s/%s:\n got\n%s\n want\n%s", city, name, got, want)
				}
			}
		})
	}
}

// Kills a family filed on too little evidence: each family needs its own
// cause, a v2-only intent is never explained, an idle drain on a row inside
// INC-003's grace is not INC-003, and one row in grace explains one drain.
func TestReplayFamilies(t *testing.T) {
	at := time.Date(2026, 10, 5, 0, 2, 0, 0, time.UTC)
	k := rowKey{Leg: rowLeg, ID: "r1"}
	info := map[string]session.Info{"r1": {Template: "t", LastWokeAt: at.Add(-time.Minute).Format(time.RFC3339)}}
	built := map[string]bool{"A20": true, "A21": true}
	for _, tc := range []struct {
		name, arm, class, record, reason string
		v2                               int
		family, item                     string
	}{
		{"v2 only", "A20", "heal", "", "", 1, "unexplained", ""},
		{"§12.2", "§12.2/1", "stop", "reconciler.drain.timeout/complete/drain_timeout", "", 0, "v5 §12.2", "1"},
		{"deferred", "A18", "start", "x/start_candidate/wake", "", 0, "deferred arm", "A18"},
		{"INC-003", "A20", "drain", "x/drain/orphaned", "", 0, "INC-003", ""},
		{"unknown", "A21", "close", "x/closed/orphaned", decideLivenessUnknown, 0, "unknown-never-destroys", ""},
		{"idle drain", "A20", "drain", "x/drain/idle", "", 0, "unexplained", ""},
		{"INC-003 on one row of two", "A20", "drain", "x/drain/orphaned,x/drain/orphaned", "", 0, "unexplained", ""},
	} {
		rec := &passTrace{Rows: []rowTrace{{Key: k, Template: "t", Reason: tc.reason}}}
		records := strings.Split(tc.record, ",")
		d := replayDivergence{Template: "t", Class: tc.class, Legacy: len(records) * (1 - tc.v2), V2: tc.v2, Records: records}
		if f, i := replayFamily(d, strings.Split(tc.arm, ","), built, rec, info, at); f != tc.family || i != tc.item {
			t.Errorf("%s: family %q item %q, want %q %q", tc.name, f, i, tc.family, tc.item)
		}
	}
}
