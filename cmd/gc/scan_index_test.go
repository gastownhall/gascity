package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A3's indexes and memos over legacy's linear scans. Each is checked
// against the scan it replaces: the legacy path where one remains (a nil
// index or memo), a frozen copy where the scan was rewritten in place.

// Kills: an ID index that keeps the last of a duplicated ID, or points at
// the wrong row.
func TestFindInfoByIDMatchesScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for seed := 0; seed < 50; seed++ {
		var infos []session.Info
		for i := 0; i < rng.Intn(40); i++ {
			infos = append(infos, session.Info{ID: fmt.Sprintf("gc-%d", rng.Intn(25)), Alias: fmt.Sprintf("a-%d", i)})
		}
		snap := newSessionBeadSnapshotFromInfos(infos)
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("gc-%d", i)
			var want session.Info
			found := false
			for _, info := range infos {
				if info.ID == id {
					want, found = info, true
					break
				}
			}
			got, ok := snap.FindInfoByID(id)
			if ok != found || !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: FindInfoByID(%s) = %v %+v, want the scan's %v %+v", seed, id, ok, got.Alias, found, want.Alias)
			}
		}
	}
}

// Kills (S-11): a mutator of the snapshot's rows that leaves the ID index
// built: a lookup after addInfo misses the new row, and one after a
// write-back or a patch reads a row the index no longer describes.
func TestFindInfoByIDIndexInvalidatedByMutators(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sessionBeadSnapshot)
		id     string
		want   string
	}{
		{"addInfo", func(s *sessionBeadSnapshot) { s.addInfo(session.Info{ID: "gc-new", Alias: "added"}) }, "gc-new", "added"},
		{"WriteBackReconcileInfos", func(s *sessionBeadSnapshot) {
			s.WriteBackReconcileInfos(map[string]session.Info{"gc-1": {ID: "gc-moved", Alias: "written"}})
		}, "gc-moved", "written"},
		{"ApplyOpenInfoPatch", func(s *sessionBeadSnapshot) {
			s.ApplyOpenInfoPatch("gc-2", session.MetadataPatch{"alias": "patched"})
		}, "gc-2", "patched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := newSessionBeadSnapshotFromInfos([]session.Info{{ID: "gc-1", Alias: "one"}, {ID: "gc-2", Alias: "two"}})
			if _, ok := snap.FindInfoByID("gc-1"); !ok {
				t.Fatal("gc-1 missing before the mutation")
			}
			tc.mutate(snap)
			if snap.byID.Load() != nil {
				t.Fatalf("%s left the ID index built", tc.name)
			}
			if got, ok := snap.FindInfoByID(tc.id); !ok || got.Alias != tc.want {
				t.Fatalf("after %s, FindInfoByID(%s) = %v %q, want %q", tc.name, tc.id, ok, got.Alias, tc.want)
			}
		})
	}
}

// Kills: an awake work index that misses an identity sessionAssigneeMatches
// accepts (bead ID, session name, alias, named identity, the configured
// named fallback), keys untrimmed assignees, or loses work order (the
// anchor is the first match).
func TestAwakeSetIndexOracle(t *testing.T) {
	byName := 0
	for seed := int64(0); seed < 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
		in := AwakeInput{Now: allocNow, ScaleCheckCounts: map[string]int{}, AttachedSessions: map[string]bool{}}
		for i := 0; i < 3; i++ {
			template := fmt.Sprintf("t%d", i)
			in.Agents = append(in.Agents, AwakeAgent{QualifiedName: template, Suspended: rng.Intn(6) == 0, SleepAfterIdle: time.Minute})
			in.ScaleCheckCounts[template] = rng.Intn(4)
		}
		in.NamedSessions = []AwakeNamedSession{{Identity: "boss", Template: "t0", Mode: pick("always", "on_demand"), RuntimeName: "s-boss"}}
		var identities []string
		for i := 0; i < rng.Intn(25); i++ {
			b := AwakeSessionBead{
				ID: fmt.Sprintf("gc-%d", i), SessionName: fmt.Sprintf("s-%d", i),
				Template: fmt.Sprintf("t%d", rng.Intn(4)), State: pick("active", "active", "asleep", "creating", "closed"),
				Drained: rng.Intn(8) == 0, IdleSince: allocNow.Add(-time.Hour),
			}
			switch rng.Intn(40) {
			case 0:
				b.SessionName = "s-boss"
			case 1:
				b.SessionName = ""
			}
			switch rng.Intn(5) {
			case 0:
				b.Alias = pick("al-"+b.ID, "shared")
			case 1:
				b.NamedIdentity = pick("boss", "other")
			case 2:
				b.ConfiguredNamedSession = true
			}
			if rng.Intn(4) == 0 {
				b.CurrentlyProcessingBeadID = fmt.Sprintf("w-%d", rng.Intn(30))
			}
			in.SessionBeads = append(in.SessionBeads, b)
			identities = append(identities, b.ID, b.SessionName, b.Alias, "boss", "shared")
		}
		for i := 0; i < rng.Intn(30); i++ {
			assignee := "nobody"
			if len(identities) > 0 {
				assignee = identities[rng.Intn(len(identities))]
			}
			in.WorkBeads = append(in.WorkBeads, AwakeWorkBead{
				ID: fmt.Sprintf("w-%d", i), Assignee: pick(assignee, assignee, " "+assignee+" ", ""),
				Status: pick("open", "in_progress"), Ready: rng.Intn(2) == 0, Blocked: rng.Intn(4) == 0,
			})
		}
		names := map[string]bool{}
		for _, b := range in.SessionBeads {
			names[b.SessionName] = true
		}
		for _, key := range []awakeKey{awakeKeyBeadID, awakeKeySessionName} {
			if key == awakeKeySessionName && (names[""] || len(names) < len(in.SessionBeads)) {
				// Rows sharing a name share its key, and legacy's last
				// write to it follows map order: not comparable.
				continue
			}
			if key == awakeKeySessionName {
				byName++
			}
			want := computeAwakeSetKeyed(in, key)
			indexed := in
			indexed.workIndex = newAwakeWorkIndex(in.WorkBeads)
			if got := computeAwakeSetKeyed(indexed, key); !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d key %v: indexed awake set\n%+v\nwant the scan's\n%+v", seed, key, got, want)
			}
		}
	}
	if byName < 100 {
		t.Fatalf("only %d cities compared keyed by session name", byName)
	}
}

// Kills: a template bucket that drops or reorders a resume or new-demand
// candidate, or buckets by a different spelling than the per-agent loop
// compared (normalization, rig and pool-instance routes, the one-agent
// fallback, the session-template guard, protected and in-flight rows).
func TestPoolDesiredIndexOracle(t *testing.T) {
	for seed := int64(0); seed < 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
		cfg := &config.City{}
		for i := 0; i < 1+rng.Intn(4); i++ {
			a := config.Agent{Name: fmt.Sprintf("pool-%d", i), MaxActiveSessions: intPtr(1 + rng.Intn(6))}
			switch rng.Intn(5) {
			case 0:
				a.Dir = "rig"
			case 1:
				a.WakeMode = "fresh"
			case 2:
				a.Suspended = rng.Intn(2) == 0
			}
			cfg.Agents = append(cfg.Agents, a)
		}
		if rng.Intn(3) == 0 {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat"})
			cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "on_demand"}}
		}
		var rows []beads.Bead
		var identities []string
		for i := 0; i < rng.Intn(30); i++ {
			a := cfg.Agents[rng.Intn(len(cfg.Agents))]
			id := fmt.Sprintf("gc-%d", i)
			name := fmt.Sprintf("%s-%d", a.QualifiedName(), 1+rng.Intn(4))
			meta := []string{
				"template", pick(a.QualifiedName(), a.QualifiedName(), a.Name, ""), "agent_name", name,
				"session_name", "s-" + id, "state", pick("active", "asleep", "creating", "start_pending"),
			}
			if rng.Intn(4) > 0 {
				meta = append(meta, "pool_managed", "true", "pool_slot", fmt.Sprint(1+rng.Intn(4)))
			}
			switch rng.Intn(6) {
			case 0:
				meta = append(meta, "session_origin", "ephemeral")
			case 1:
				meta = append(meta, "state_reason", "creation_complete", "creation_complete_at", allocNow.Add(-time.Duration(rng.Intn(120))*time.Second).Format(time.RFC3339))
			case 2:
				meta = append(meta, "pending_create_claim", "true", "pending_create_started_at", allocNow.Format(time.RFC3339))
			case 3:
				meta = append(meta, "configured_named_session", "true", "configured_named_identity", "chat")
			}
			row := sessionRow(id, meta...)
			row.CreatedAt = allocNow.Add(-time.Duration(rng.Intn(3)) * time.Minute)
			if rng.Intn(10) == 0 {
				row.Status = "closed"
			}
			rows = append(rows, row)
			identities = append(identities, id, "s-"+id, name)
		}
		var work []beads.Bead
		counts, demand := map[string]int{}, map[string]scaleCheckDemand{}
		for i := 0; i < rng.Intn(30); i++ {
			assignee := pick(cfg.Agents[0].QualifiedName(), "chat", "", "nobody")
			if len(identities) > 0 && rng.Intn(3) > 0 {
				assignee = identities[rng.Intn(len(identities))]
			}
			wb := beads.Bead{
				ID: fmt.Sprintf("w-%d", i), Status: pick("open", "in_progress", "in_progress", "closed"),
				Assignee: pick(assignee, " "+assignee), Metadata: map[string]string{},
			}
			if a := cfg.Agents[rng.Intn(len(cfg.Agents))]; rng.Intn(2) == 0 {
				wb.Metadata["gc.routed_to"] = pick(a.QualifiedName(), a.Name, a.QualifiedName()+"-2")
			}
			work = append(work, wb)
		}
		for _, a := range cfg.Agents {
			if n := rng.Intn(4); n > 0 {
				counts[a.QualifiedName()] = n
				demand[a.QualifiedName()] = scaleCheckDemand{Count: n, WorkBeadIDs: []string{"w-1", "w-2"}}
			}
		}
		infos := sessionInfosFromBeads(rows)
		want := computePoolDesiredStatesAtPreA3(cfg, work, infos, counts, demand, allocNow, nil)
		got := computePoolDesiredStatesAt(cfg, work, infos, counts, demand, allocNow, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: bucketed pool desired\n%+v\nwant the frozen scan's\n%+v", seed, got, want)
		}
	}
}

// Kills: a slot template memo answer that differs from the stored-template
// match it caches (keyed by the wrong spelling, or shared across pool
// templates), and a fresh-slot candidate index that misses a row the claim
// counts: a binding-qualified or alias slot name, a legacy "-gc-<n>" name,
// a canonical holder by title or label, a namepool agent's themed name, a
// padded name, or a failed-create row's slot counted as held.
func TestSlotTemplateMemoOracle(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
		cfg := &config.City{}
		for i := 0; i < 1+rng.Intn(5); i++ {
			a := config.Agent{Name: fmt.Sprintf("pool-%d", i), MaxActiveSessions: intPtr(2 + rng.Intn(8))}
			switch rng.Intn(6) {
			case 0:
				a.Dir = "rig"
			case 1:
				a.BindingName = "imp"
			case 2:
				a.NamepoolNames = []string{"ann", "bob"}
			case 4:
				a.Dir, a.BindingName = "rig", "imp"
			case 3:
				a.MaxActiveSessions = intPtr(1)
			}
			cfg.Agents = append(cfg.Agents, a)
		}
		var infos []session.Info
		for i := 0; i < rng.Intn(60); i++ {
			a := cfg.Agents[rng.Intn(len(cfg.Agents))]
			slot := fmt.Sprint(1 + rng.Intn(9))
			name := pick(a.QualifiedName()+"-"+slot, a.BindingQualifiedName()+"-"+slot, a.QualifiedName()+"-gc-"+slot,
				a.Name+"-"+slot, "ann", " "+a.QualifiedName()+"-"+slot, "other-"+slot)
			info := session.Info{
				ID: fmt.Sprintf("gc-%d", i), Template: pick(a.QualifiedName(), a.Name, "", "gone", "imp."+a.Name),
				AgentName: name, PoolSlot: pick(slot, "", "40"), SessionNameMetadata: pick("s-"+slot, a.QualifiedName()+"-"+slot),
				MetadataState: pick("active", "asleep", "failed-create"),
			}
			switch rng.Intn(6) {
			case 0:
				info.Alias = pick(a.QualifiedName()+"-"+slot, a.BindingQualifiedName()+"-"+slot, "al")
			case 1:
				info.Title = a.QualifiedName()
			case 2:
				info.Labels = []string{"agent:" + a.QualifiedName()}
			case 3:
				info.AgentName, info.Labels = "", []string{"agent:" + name}
			}
			infos = append(infos, info)
		}
		x := newPassIndex()
		x.occupancy = infos
		x.indexOccupancy()
		for i := range cfg.Agents {
			a := &cfg.Agents[i]
			m := &poolRealizeMemo{
				index: x, cfg: cfg, agent: a, all: infos,
				templates: slotTemplateMemo{template: a.QualifiedName(), matches: make(map[string]bool)},
			}
			var want []session.Info
			wantHeld := map[int]bool{}
			for j := range infos {
				slot := existingPoolSlotWithConfigInfo(cfg, a, infos[j])
				if got := existingPoolSlotWithTemplates(cfg, &m.templates, a, &infos[j]); got != slot {
					t.Fatalf("seed %d %s row %s: memo slot %d, want %d", seed, a.QualifiedName(), infos[j].ID, got, slot)
				}
				if slot > 0 || infoIdentifiesAsCanonical(infos[j], a.QualifiedName()) {
					want = append(want, infos[j])
					if slot > 0 && !isFailedCreateSessionInfo(infos[j]) {
						wantHeld[slot] = true
					}
				}
			}
			if got := m.freshOccupancy(); !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d %s: memo occupancy %v, want the scan's %v", seed, a.QualifiedName(), infoIDs(got), infoIDs(want))
			}
			if got := m.freshSlots(a); !reflect.DeepEqual(got, wantHeld) {
				t.Fatalf("seed %d %s: memo held slots %v, want %v", seed, a.QualifiedName(), got, wantHeld)
			}
			// The memo answers only for its own pool template.
			for j := range cfg.Agents {
				for _, info := range infos {
					other, stored := cfg.Agents[j].QualifiedName(), storedTemplateRef(&info)
					if got, want := m.templates.storedTemplateMatchesPoolTemplate(stored, other, cfg), storedTemplateMatchesPoolTemplate(stored, other, cfg); got != want {
						t.Fatalf("seed %d %s memo: match(%q, %q) = %v, want %v", seed, a.QualifiedName(), stored, other, got, want)
					}
				}
			}
		}
	}
}
