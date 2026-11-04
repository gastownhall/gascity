package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The two-city matrix over the fixture in rig_federation_two_city_fixture_test.go.
// Every guarded path is a column, run in both cities, with federation = "rig"
// (each city acts only on its own rows) and federation = "city" (today's
// leak, pinned):
//
//   - reads: discovery List and Ready, the routed, assigned and legacy
//     gc.run_target tiers, the by-path open, bounded pages, Count, and the
//     demand leg's cached view (TestTwoCityMatrixReads,
//     TestTwoCityCountAgreesWithList);
//   - claims: the hook's candidates, claim refusal and continuation pin, the
//     gc bd write check, agent-script, and the orphan sweep's is-foreign gate
//     (TestTwoCityMatrixClaimPaths);
//   - writes through the controller's store: assign, release, route re-stamp,
//     close, judged on the stored row even when the cache is stale
//     (TestTwoCityMatrixWrites, TestTwoCityWritesAreJudgedOnTheStoredRowNotTheCache);
//   - release: the Go pool sweep, the shell sweep's release-if-current behind
//     each of its two fences, on_death and on_boot
//     (TestTwoCityMatrixPoolSweep, TestTwoCityOrphanSweepReleaseIsFencedTwice,
//     TestTwoCityOnDeathAndOnBootSelectOnlyOwnRows);
//   - wake: cache notifications and the ledger feed (TestTwoCityMatrixEvents,
//     TestTwoCityMatrixLedgerFeed);
//   - autoclose, sling routes, reload, aliasing, and whole controller ticks
//     (TestTwoCityAutocloseSweepClosesOnlyOwnConvoys,
//     TestTwoCityMatrixSlingRoutes, TestTwoCityReloadGuardsTheFirstRigAndBack,
//     TestTwoCityAliasedRigIsRefusedInItsOwnCityOnly,
//     TestTwoCityControllerTicksTouchNoOtherCitysRows).

// ---------------------------------------------------------------------------
// The read columns.

func tcReadColumns() []tcColumn {
	list := func(f *tcFixture, c *tcCity) []string {
		return f.selectIDs(func(r tcRow) bool { return tcNotClosed(r) && f.mayActOn(c, r) })
	}
	byRoute := func(c *tcCity, r tcRow) bool {
		return slices.Contains(c.routes(), r.bead.Metadata[beadmeta.RoutedToMetadataKey])
	}
	byActor := func(c *tcCity, r tcRow) bool {
		return slices.Contains(c.actors(), r.bead.Assignee)
	}
	byTarget := func(c *tcCity, r tcRow) bool {
		v := r.bead.Metadata[beadmeta.RunTargetMetadataKey]
		return v != "" && slices.Contains(c.routes(), v)
	}
	return []tcColumn{
		{
			name: "read: API list, gc ready, dashboard (discovery List)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				rows, err := c.rigStore(t).List(beads.ListQuery{AllowScan: true})
				if err != nil {
					t.Fatalf("city %s: List: %v", c.label, err)
				}
				return tcIDs(rows)
			},
			want:   list,
			active: tcAll,
			leaks:  true,
		},
		{
			name: "ready: the routed tier (gc.routed_to = each route the city serves)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				var got []string
				for _, v := range c.routes() {
					rows, err := c.rigStore(t).List(beads.ListQuery{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: v}})
					if err != nil {
						t.Fatalf("city %s: routed %s: %v", c.label, v, err)
					}
					got = append(got, tcIDs(rows)...)
				}
				return got
			},
			want: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool {
					return byRoute(c, r) && (f.mode == config.RigFederationCity || r.owner == c.owner)
				})
			},
			active: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool { return byRoute(c, r) })
			},
			leaks: true,
		},
		{
			name: "ready: the assigned tiers (each identity the city's agents claim under)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				var got []string
				for _, v := range c.actors() {
					rows, err := c.rigStore(t).List(beads.ListQuery{Assignee: v})
					if err != nil {
						t.Fatalf("city %s: assignee %s: %v", c.label, v, err)
					}
					got = append(got, tcIDs(rows)...)
					rows, err = c.rigStore(t).ListByAssignee(v, "in_progress", 0)
					if err != nil {
						t.Fatalf("city %s: ListByAssignee %s: %v", c.label, v, err)
					}
					got = append(got, tcIDs(rows)...)
				}
				return got
			},
			want: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool {
					return byActor(c, r) && (f.mode == config.RigFederationCity || r.owner == c.owner)
				})
			},
			active: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool { return byActor(c, r) })
			},
			leaks: true,
		},
		{
			name: "ready: the legacy gc.run_target tier",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				var got []string
				for _, v := range c.routes() {
					rows, err := c.rigStore(t).ListByMetadata(map[string]string{beadmeta.RunTargetMetadataKey: v}, 0)
					if err != nil {
						t.Fatalf("city %s: run target %s: %v", c.label, v, err)
					}
					got = append(got, tcIDs(rows)...)
				}
				return got
			},
			want: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool {
					if !byTarget(c, r) {
						return false
					}
					if f.mode == config.RigFederationCity {
						return true
					}
					// An identity predicate the rig does not admit reads
					// nothing, Unattributed rows included.
					return r.bead.Metadata[beadmeta.RunTargetMetadataKey] == c.worker && f.mayActOn(c, r)
				})
			},
			active: func(f *tcFixture, c *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool { return byTarget(c, r) })
			},
			leaks: true,
		},
		{
			name: "read: discovery Ready",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				rows, err := c.rigStore(t).Ready()
				if err != nil {
					t.Fatalf("city %s: Ready: %v", c.label, err)
				}
				return tcIDs(rows)
			},
			want: func(f *tcFixture, c *tcCity) []string {
				raw, _ := f.engine.Ready()
				ready := tcIDs(raw)
				return f.selectIDs(func(r tcRow) bool { return slices.Contains(ready, r.id) && f.mayActOn(c, r) })
			},
			active: func(f *tcFixture, _ *tcCity) []string {
				raw, _ := f.engine.Ready()
				return tcIDs(raw)
			},
			leaks: true,
		},
		{
			name: "read: by-path open (gc ready's in-process fallback, one-shot verbs)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				res, err := openStoreResultAtForCityWithConfig(c.rigDir, c.path, c.cfg, gate.ModeUnset, false, false, false, nil)
				if err != nil {
					t.Fatalf("city %s: open by path: %v", c.label, err)
				}
				if ns := rigNamespaceOfStore(res.Store); ns != nil {
					// The guard of a store opened by path in this process
					// answers through this city's controller resolver.
					if r, ok := ns.sessions.(rigNamespaceCityResolver); !ok || r.current() != ownSessionResolver(c.cs.rigSessions) {
						t.Fatalf("city %s: the by-path guard does not resolve through the city's own controller resolver (%T)", c.label, ns.sessions)
					}
				}
				rows, err := res.Store.List(beads.ListQuery{AllowScan: true})
				if err != nil {
					t.Fatalf("city %s: List by path: %v", c.label, err)
				}
				return tcIDs(rows)
			},
			want:   list,
			active: tcAll,
			leaks:  true,
		},
		{
			name: "read: bounded pages refill (List with a limit of 1..4)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				var got []string
				for limit := 1; limit <= 4; limit++ {
					rows, err := c.rigStore(t).List(beads.ListQuery{AllowScan: true, Limit: limit, Sort: beads.SortCreatedAsc})
					if err != nil {
						t.Fatalf("city %s: List limit %d: %v", c.label, limit, err)
					}
					if len(rows) != limit {
						t.Errorf("city %s: a page of %d returned %d rows; a short page reads as exhausted", c.label, limit, len(rows))
					}
					got = append(got, tcIDs(rows)...)
				}
				return got
			},
			want: func(f *tcFixture, c *tcCity) []string {
				// The engine's own order, filtered: each page must be the
				// first rows this city may act on, however many rows of
				// other cities come first.
				raw, _ := f.engine.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
				var out []string
				for _, b := range raw {
					if f.mayActOn(c, f.row(b.ID)) && len(out) < 4 {
						out = append(out, b.ID)
					}
				}
				sort.Strings(out)
				return out
			},
			active: tcAll,
			leaks:  true,
		},
		{
			name: "demand / scale_check (the v2 demand leg's cached view)",
			run: func(t *testing.T, _ *tcFixture, c *tcCity) []string {
				view, _ := demandLegCache(splitStrictFrontOf(c.rigStore(t)))
				if view == nil {
					t.Fatalf("city %s: the rig store gave the demand reads no cache-fed view", c.label)
				}
				rows, ok := view.CachedList(beads.ListQuery{AllowScan: true})
				if !ok {
					t.Fatalf("city %s: the demand view could not answer from the cache", c.label)
				}
				return tcIDs(rows)
			},
			want:   list,
			active: tcAll,
			leaks:  true,
		},
		{
			name: "hook claim: candidates read from the rig's leg",
			run: func(t *testing.T, f *tcFixture, c *tcCity) []string {
				all, err := f.engine.List(beads.ListQuery{AllowScan: true})
				if err != nil {
					t.Fatal(err)
				}
				g := c.hookGuard().forLeg(c.rig, c.rigDir)
				return tcIDs(g.filterCandidates(all, io.Discard))
			},
			want:   list,
			active: tcAll,
			leaks:  true,
		},
	}
}

func TestTwoCityMatrixReads(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			for _, col := range tcReadColumns() {
				tcCheckColumn(t, f, col)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The claim-cycle columns: the non-store paths decide from the row in hand
// and the actor, before any write or bd process.

func tcClaimColumns() []tcColumn {
	acts := func(f *tcFixture, c *tcCity) []string {
		return f.selectIDs(func(r tcRow) bool { return f.mayActOn(c, r) })
	}
	return []tcColumn{
		{
			name: "hook claim: claim, promotion, stale reclaim, adoption re-stamp (claim refusal)",
			run: func(_ *testing.T, f *tcFixture, c *tcCity) []string {
				g := c.hookGuard().forLeg(c.rig, c.rigDir)
				var got []string
				for _, r := range f.rows() {
					if g.claimRefusal(r.bead, c.pool) == nil {
						got = append(got, r.id)
					}
				}
				return got
			},
			want: acts, active: tcAll, leaks: true,
		},
		{
			name: "hook claim: continuation pin",
			run: func(_ *testing.T, f *tcFixture, c *tcCity) []string {
				g := c.hookGuard().forLeg(c.rig, c.rigDir)
				var got []string
				for _, r := range f.rows() {
					if g.continuationPin(r.bead, hookClaimOptions{SessionID: c.pool, Assignee: tcCityAlias}) != "" {
						got = append(got, r.id)
					}
				}
				return got
			},
			want: acts, active: tcAll, leaks: true,
		},
		{
			name: "gc bd: update --assignee in the rig's scope (write pre-read)",
			run: func(_ *testing.T, f *tcFixture, c *tcCity) []string {
				check := newBdRigNamespaceCheck(c.cfg, c.path, c.rigDir, f.engine.Get)
				var got []string
				for _, r := range f.rows() {
					if _, _, refused := check.check([]string{"update", r.id, "--assignee", c.pool}); !refused {
						got = append(got, r.id)
					}
				}
				return got
			},
			want: acts, active: tcAll, leaks: true,
		},
		{
			name: "agent-script: bd_claim redirected into the rig's store",
			run: func(t *testing.T, f *tcFixture, c *tcCity) []string {
				t.Setenv("BEADS_ACTOR", c.pool)
				ns := rigNamespaceForScopeOrDeny(c.cfg, c.path, c.rigDir, c.cliResolver())
				var got []string
				for _, r := range f.rows() {
					if ns == nil || agentScriptRigNamespaceCheck(ns, f.engine.Get, []string{"update", r.id, "--claim"}) == nil {
						got = append(got, r.id)
					}
				}
				return got
			},
			want: acts, active: tcAll, leaks: true,
		},
		{
			name: "orphan sweep gate: gc agent is-foreign --rig-namespace answers local",
			run: func(t *testing.T, f *tcFixture, c *tcCity) []string {
				local := map[string]bool{}
				var got []string
				for _, r := range f.rows() {
					id := r.bead.Assignee
					if id == "" {
						continue
					}
					v, asked := local[id]
					if !asked {
						var out, errb bytes.Buffer
						code := doAgentIsForeignInRig(fsys.OSFS{}, c.path, c.rig, id, true, &out, &errb)
						var res AgentIsForeignJSON
						if err := json.Unmarshal(out.Bytes(), &res); err != nil {
							t.Fatalf("city %s: is-foreign %q: %v\n%s%s", c.label, id, err, out.String(), errb.String())
						}
						if res.RigNamespace != c.rig {
							t.Fatalf("city %s: is-foreign %q did not echo the rig: %+v", c.label, id, res)
						}
						if (code == agentIsForeignExitLocal) != (res.Verdict == "local") {
							t.Fatalf("city %s: is-foreign %q exit %d disagrees with verdict %q", c.label, id, code, res.Verdict)
						}
						if f.mode == config.RigFederationRig && code != agentIsForeignExitLocal && (res.Reason != "namespace" || !res.NamespaceChecked) {
							t.Errorf("city %s: is-foreign %q on a guarded rig answered %+v; want the namespace reason, so the sweep counts it rather than names it", c.label, id, res)
						}
						v = code == agentIsForeignExitLocal
						local[id] = v
					}
					if v {
						got = append(got, r.id)
					}
				}
				return got
			},
			want: func(f *tcFixture, c *tcCity) []string {
				own := []string{c.pool, c.dead, c.worker, c.workerN}
				if f.mode == config.RigFederationCity {
					// Today, colliding spellings resolve against this city's
					// own roster and session records.
					own = append(own, tcCityAlias, tcNamed, tcShortID)
				}
				return f.selectIDs(func(r tcRow) bool { return slices.Contains(own, r.bead.Assignee) })
			},
			active: func(f *tcFixture, _ *tcCity) []string {
				return f.selectIDs(func(r tcRow) bool { return r.bead.Assignee != "" })
			},
			leaks: true,
		},
	}
}

func TestTwoCityMatrixClaimPaths(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			for _, col := range tcClaimColumns() {
				tcCheckColumn(t, f, col)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The write columns: each runs on a fresh fixture, through the city's rig
// store as the controller holds it (policy over guard over cache). A refused
// write must leave the shared row untouched.

type tcWriteCol struct {
	name    string
	applies func(r tcRow) bool
	write   func(s beads.Store, c *tcCity, r tcRow) error
}

func tcWrites() []tcWriteCol {
	everyRow := func(tcRow) bool { return true }
	return []tcWriteCol{
		{
			name:    "claim: API assign, store claim (set the assignee)",
			applies: everyRow,
			write: func(s beads.Store, c *tcCity, r tcRow) error {
				return s.Update(r.id, beads.UpdateOpts{Assignee: &c.pool})
			},
		},
		{
			name:    "release: release lanes, drain-ack, session close (clear the assignee)",
			applies: func(r tcRow) bool { return r.bead.Assignee != "" },
			write: func(s beads.Store, _ *tcCity, r tcRow) error {
				empty, open := "", "open"
				return s.Update(r.id, beads.UpdateOpts{Assignee: &empty, Status: &open})
			},
		},
		{
			name:    "route recovery, canonicalisation, control repair, detached orphans (re-stamp a route)",
			applies: everyRow,
			write: func(s beads.Store, c *tcCity, r tcRow) error {
				return s.SetMetadata(r.id, beadmeta.RoutedToMetadataKey, c.worker)
			},
		},
		{
			name:    "autoclose, convoy check, order tracking, convergence (close)",
			applies: everyRow,
			write: func(s beads.Store, _ *tcCity, r tcRow) error {
				return s.Close(r.id)
			},
		},
	}
}

func TestTwoCityMatrixWrites(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		for _, w := range tcWrites() {
			t.Run("federation="+mode+"/"+w.name, func(t *testing.T) {
				for _, label := range []string{"A", "B"} {
					f := newTCFixture(t, mode)
					c := f.a
					if label == "B" {
						c = f.b
					}
					store := c.rigStore(t)
					var got, active []string
					for _, r := range f.rows() {
						if !w.applies(r) {
							continue
						}
						active = append(active, r.id)
						before, err := f.engine.Get(r.id)
						if err != nil {
							t.Fatal(err)
						}
						err = w.write(store, c, r)
						if err == nil {
							got = append(got, r.id)
							continue
						}
						if mode == config.RigFederationRig && !errors.Is(err, errRigNamespace) {
							t.Errorf("city %s: %s on %s failed with %v; want a rig namespace refusal", c.label, w.name, r.id, err)
						}
						after, err := f.engine.Get(r.id)
						if err != nil {
							t.Fatal(err)
						}
						if after.Assignee != before.Assignee || after.Status != before.Status || !mapsEqual(after.Metadata, before.Metadata) {
							t.Errorf("city %s: a refused %s changed shared row %s: %+v -> %+v", c.label, w.name, r.id, before, after)
						}
					}
					sort.Strings(got)
					want := f.selectIDs(func(r tcRow) bool { return w.applies(r) && f.mayActOn(c, r) })
					if !slices.Equal(got, want) {
						t.Errorf("%s, city %s (federation = %q):\n got  %v\n want %v\n extra %v\n missing %v", w.name, c.label, mode, got, want, tcMinus(got, want), tcMinus(want, got))
					}
					if mode == config.RigFederationRig && len(tcMinus(active, want)) == 0 {
						t.Errorf("%s, city %s: no write was refused; the column would pass vacuously", w.name, c.label)
					}
					if mode == config.RigFederationCity && len(tcMinus(got, f.selectIDs(func(r tcRow) bool { return r.owner == c.owner }))) == 0 {
						t.Errorf("%s, city %s: today's writes reached only the city's own rows; no leak pinned", w.name, c.label)
					}
				}
			})
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The Go pool sweep (releaseOrphanedPoolAssignments), handed every claimed
// row of the shared store as if each had reached it, as the second layer
// behind the guarded reads.

func TestTwoCityMatrixPoolSweep(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			for _, label := range []string{"A", "B"} {
				f := newTCFixture(t, mode)
				c, other := f.a, f.b
				if label == "B" {
					c, other = f.b, f.a
				}
				store := c.rigStore(t)
				all, err := f.engine.List(beads.ListQuery{AllowScan: true, Status: "in_progress"})
				if err != nil {
					t.Fatal(err)
				}
				stores := make([]beads.Store, len(all))
				refs := make([]string, len(all))
				for i := range all {
					stores[i] = store
					refs[i] = "rig:" + c.rig
				}
				open, err := c.hq.List(beads.ListQuery{AllowScan: true, Label: "gc:session"})
				if err != nil {
					t.Fatal(err)
				}
				released := releaseOrphanedPoolAssignments(store, beads.SessionStore{Store: c.hq}, c.cfg, c.path,
					sessionInfosFromBeads(open), all, stores, refs, map[string]beads.Store{c.rig: store}, nil, nil, nil)
				var got []string
				for _, r := range released {
					got = append(got, all[r.Index].ID)
					b, err := f.engine.Get(all[r.Index].ID)
					if err != nil || b.Assignee != "" {
						t.Errorf("city %s: reported %s released but the shared row is %+v (%v)", c.label, all[r.Index].ID, b, err)
					}
				}
				sort.Strings(got)
				lc := strings.ToLower(c.label)
				// Each city releases its own dead pool claim and its own
				// routed, unassigned in-progress row: the positive control.
				want := []string{"sh-boot" + lc, "sh-sweep-dead" + lc}
				if mode == config.RigFederationCity {
					// Today each city releases a person's claim on a row it
					// serves, and the other city's live pool claim: a bare
					// identity counts as this city's own naming, and the
					// other city's pool id shares this city's id prefix.
					want = append(want, "sh-person"+lc, "sh-other"+strings.ToLower(other.label))
					if c == f.b {
						// And city B releases city A's live claims held under
						// spellings B also uses: the pool alias (B's record of
						// it is closed) and the colliding short session id.
						want = append(want, "sh-sweep-alias", "sh-sweep-short")
					}
				}
				sort.Strings(want)
				if !slices.Equal(got, want) {
					t.Errorf("city %s (federation = %q) released %v, want %v", c.label, mode, got, want)
				}
				for _, id := range []string{"sh-sweep-dead" + strings.ToLower(other.label), "sh-pool" + strings.ToLower(other.label)} {
					if b, _ := f.engine.Get(id); b.Assignee == "" {
						t.Errorf("city %s released city %s's row %s", c.label, other.label, id)
					}
				}
				if mode == config.RigFederationRig && len(got) != 2 {
					t.Errorf("city %s: the sweep released %v; want exactly its own two rows", c.label, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Wake: another city's changes reach this city's cache (the cache stays
// unfiltered) but wake nothing here.

func TestTwoCityMatrixEvents(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			marks := map[*tcCity]int{}
			for _, c := range f.cities() {
				evs, _ := c.ep.List(events.Filter{})
				marks[c] = len(evs)
			}
			// Writes by both cities and by people, straight into the shared
			// store as another process would make them.
			created := []beads.Bead{
				{ID: "sh-new-a", Title: "A's new row", Type: "task", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, f.a.worker)},
				{ID: "sh-new-b", Title: "B's new row", Type: "task", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, f.b.worker)},
				{ID: "sh-new-bare", Title: "bare route", Type: "task", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, tcCityPool)},
				{ID: "sh-new-alias", Title: "shared alias claim", Type: "task", Status: "in_progress", Assignee: tcCityAlias},
				{ID: "sh-new-plain", Title: "unattributed", Type: "task", Status: "open"},
			}
			for _, b := range created {
				if _, err := f.engine.Create(b); err != nil {
					t.Fatal(err)
				}
			}
			title := "retitled"
			for _, id := range []string{"sh-r23a", "sh-r23b", "sh-r1"} {
				if err := f.engine.Update(id, beads.UpdateOpts{Title: &title}); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"sh-r6a", "sh-r6b", "sh-r5"} {
				if err := f.engine.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range f.cities() {
				f.syncCache(t, c, false)
				evs, _ := c.ep.List(events.Filter{})
				got := map[string]bool{}
				for _, e := range evs[marks[c]:] {
					if strings.HasPrefix(e.Type, "bead.") {
						got[e.Subject] = true
					}
				}
				var gotIDs []string
				for id := range got {
					gotIDs = append(gotIDs, id)
				}
				sort.Strings(gotIDs)
				lc := strings.ToLower(c.label)
				want := []string{"sh-new-" + lc, "sh-new-plain", "sh-r23" + lc, "sh-r6" + lc}
				if mode == config.RigFederationCity {
					want = []string{"sh-new-a", "sh-new-alias", "sh-new-b", "sh-new-bare", "sh-new-plain", "sh-r1", "sh-r23a", "sh-r23b", "sh-r5", "sh-r6a", "sh-r6b"}
				}
				sort.Strings(want)
				if !slices.Equal(gotIDs, want) {
					t.Errorf("city %s (federation = %q) recorded bead events for %v, want %v", c.label, mode, gotIDs, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The ledger feed: two cities' feeds tail one shared journal; each delivers
// (to the autoclose sweep and the wake) only the closes it may act on.

func tcFeedHarness(t *testing.T, store *journalingStore, ns *rigNamespace) *feedHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &feedHarness{t: t, ctx: ctx, store: store, ep: events.NewFake(), pokeCh: make(chan struct{}, 1), ctrlCh: make(chan struct{}, 1)}
	h.set = newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{
		work: func() func() { return func() {} },
		deferClose: func(id string) {
			h.deferredMu.Lock()
			defer h.deferredMu.Unlock()
			h.deferred = append(h.deferred, id)
		},
	})
	h.set.setWake(newLegacyWake(h.pokeCh, h.ctrlCh))
	// As wrapWithCachingStoreFeed builds a guarded rig's feed: the request
	// carries the namespace, and the cache's notifications pass the filter.
	req := &ledgerFeedRequest{set: h.set, scope: ledgerFeedRigScope("shared"), mode: config.BeadsLedgerFeedAuto, ns: ns}
	att := attachLedgerFeed(ctx, req, store, true)
	if att == nil || att.entry.feed == nil {
		t.Fatalf("no feed attached: %+v", att)
	}
	h.cache = beads.NewCachingStore(store, att.observe(rigNamespaceEventFilter(ns, cacheChangeRecorder(h.ep))))
	att.bind(ctx, h.set, h.cache)
	h.feed = att.entry.feed
	if err := h.cache.Prime(ctx); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return h
}

func TestTwoCityMatrixLedgerFeed(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			store := newJournalingStore()
			harness := map[*tcCity]*feedHarness{}
			for _, c := range f.cities() {
				var ns *rigNamespace
				if mode == config.RigFederationRig {
					ns = guardOf(t, c.rigStore(t)).ns
				}
				h := tcFeedHarness(t, store, ns)
				h.step()
				h.pokes()
				harness[c] = h
			}
			for _, r := range f.rows() {
				if _, err := store.Create(r.bead); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(r.id); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range f.cities() {
				h := harness[c]
				h.step()
				h.deferredMu.Lock()
				got := tcIDsOf(h.deferred)
				h.deferredMu.Unlock()
				want := f.selectIDs(func(r tcRow) bool { return f.mayActOn(c, r) })
				if !slices.Equal(got, want) {
					t.Errorf("city %s (federation = %q): the feed deferred %v\n want %v\n extra %v", c.label, mode, got, want, tcMinus(got, want))
				}
				for _, r := range f.rows() {
					if !f.mayActOn(c, r) && len(h.eventsFor(r.id)) > 0 {
						t.Errorf("city %s: another city's row %s produced events %v", c.label, r.id, h.eventsFor(r.id))
					}
				}
			}
		})
	}
}

func tcIDsOf(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return slices.Compact(out)
}

// ---------------------------------------------------------------------------
// Sling and formula routes into the shared store.

func TestTwoCityMatrixSlingRoutes(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			for _, c := range f.cities() {
				var allowed, all []string
				for i := range c.cfg.Agents {
					a := c.cfg.Agents[i]
					target := agentutil.RoutedToIdentity(&a)
					all = append(all, target)
					// A bead of the shared store (its prefix) and the rig's
					// workflow store ref must both admit the route.
					byPrefix := agentutil.RigRouteRefusal(agentutil.GuardedRigForPrefix(c.cfg, tcPrefix), "", target)
					byRef := agentutil.RigRouteRefusal(agentutil.GuardedRigForStoreRef(c.cfg, "rig:"+c.rig), "", target)
					if (byPrefix == nil) != (byRef == nil) {
						t.Fatalf("city %s: route %q: the prefix and store-ref checks disagree (%v / %v)", c.label, target, byPrefix, byRef)
					}
					if byPrefix == nil {
						allowed = append(allowed, target)
					} else if !errors.Is(byPrefix, errRigNamespace) {
						t.Fatalf("city %s: route %q refused with %v, not the guard's sentinel", c.label, target, byPrefix)
					}
				}
				sort.Strings(allowed)
				sort.Strings(all)
				want := all
				if mode == config.RigFederationRig {
					want = nil
					for _, v := range all {
						if strings.HasPrefix(v, c.rig+"/") {
							want = append(want, v)
						}
					}
				}
				if !slices.Equal(allowed, want) || len(want) == 0 {
					t.Errorf("city %s (federation = %q): routes into the shared store allowed %v, want %v", c.label, mode, allowed, want)
				}
				if mode == config.RigFederationRig && slices.Contains(allowed, tcCityPool) {
					t.Errorf("city %s: a bare city pool route into the shared store was allowed", c.label)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// on_death and on_boot: bd hooks run in the agent's own directory and key on
// its qualified name, which the rig name makes unique across the cities
// attaching the store (precondition P-A), so they need no guard. Pinned in
// both modes: each city's hooks select only its own rows of the shared store.

func TestTwoCityOnDeathAndOnBootSelectOnlyOwnRows(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			all, err := f.engine.List(beads.ListQuery{AllowScan: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range f.cities() {
				for i := range c.cfg.Agents {
					a := c.cfg.Agents[i]
					dir := agentCommandDir(c.path, &a, c.cfg.Rigs)
					if a.QualifiedName() != c.worker {
						if a.Dir == "" && normalizePathForCompare(dir) == normalizePathForCompare(c.rigDir) {
							t.Errorf("city %s: city-scoped agent %s runs its hooks in the shared rig", c.label, a.QualifiedName())
						}
						continue
					}
					if normalizePathForCompare(dir) != normalizePathForCompare(c.rigDir) {
						t.Fatalf("city %s: %s runs its hooks in %s, want the rig %s", c.label, c.worker, dir, c.rigDir)
					}
					death := a.EffectiveOnDeath()
					if !strings.Contains(death, "bd list --assignee="+c.worker+" ") {
						t.Fatalf("city %s: on_death does not key on %q:\n%s", c.label, c.worker, death)
					}
					boot := a.EffectiveOnBoot()
					if !strings.Contains(boot, "template="+c.worker+";") && !strings.Contains(boot, "template='"+c.worker+"';") {
						t.Fatalf("city %s: on_boot does not key on %q:\n%s", c.label, c.worker, boot)
					}
					var deaths, boots []string
					for _, b := range all {
						if b.Status == "in_progress" && b.Assignee == c.worker {
							deaths = append(deaths, b.ID)
						}
						if b.Status == "in_progress" && b.Assignee == "" && b.Metadata[beadmeta.RoutedToMetadataKey] == c.worker {
							boots = append(boots, b.ID)
						}
					}
					lc := strings.ToLower(c.label)
					if !slices.Equal(deaths, []string{"sh-death" + lc}) || !slices.Equal(boots, []string{"sh-boot" + lc}) {
						t.Errorf("city %s: on_death selects %v and on_boot %v; want only the city's own rows", c.label, deaths, boots)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reload: guarding the first rig, and dropping the guard again.

func (f *tcFixture) reload(t *testing.T, c *tcCity, federation string) {
	t.Helper()
	tcWriteCity(t, f.root, c.name, c.rig, federation)
	cfg, err := loadCityConfig(c.path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg = cfg
	c.cs.update(cfg, runtime.NewFake())
}

func TestTwoCityReloadGuardsTheFirstRigAndBack(t *testing.T) {
	f := newTCFixture(t, config.RigFederationCity)
	for _, col := range tcReadColumns()[:1] {
		tcCheckColumn(t, f, col)
	}
	// Each city reloads into federation = "rig" on its one rig: the first
	// guarded rig of the city. The store is rebuilt behind a guard whose
	// resolver is the city's own, pointed at the city's sessions store.
	f.mode = config.RigFederationRig
	for _, c := range f.cities() {
		f.reload(t, c, config.RigFederationRig)
	}
	for _, c := range f.cities() {
		f.requireShared(t, c)
		c.cs.rigSessions.mu.Lock()
		sessions := c.cs.rigSessions.store
		c.cs.rigSessions.mu.Unlock()
		if noSessionStore(sessions) {
			t.Fatalf("city %s: the reload that guarded the first rig left the resolver without the sessions store", c.label)
		}
	}
	for _, col := range append(tcReadColumns(), tcClaimColumns()...) {
		tcCheckColumn(t, f, col)
	}
	// And back: dropping the knob removes the guard on reload.
	f.mode = config.RigFederationCity
	for _, c := range f.cities() {
		f.reload(t, c, "")
	}
	for _, c := range f.cities() {
		f.requireShared(t, c)
	}
	for _, col := range tcReadColumns()[:1] {
		tcCheckColumn(t, f, col)
	}
}

// ---------------------------------------------------------------------------
// A city whose guarded rig's store is also another rig's, in the same city:
// an unguarded rig sharing it is refused, with the reason, at both seams and
// on the non-store paths, while the other city, which shares the very same
// database by design, keeps working.

func TestTwoCityAliasedRigIsRefusedInItsOwnCityOnly(t *testing.T) {
	f := newTCFixture(t, config.RigFederationRig)
	b := f.b
	gamma := filepath.Join(b.path, "gamma")
	tcWrite(t, filepath.Join(gamma, ".beads", "metadata.json"), fmt.Sprintf(`{"backend":"dolt","dolt_database":%q,"project_id":%q}`, tcDatabase, tcProjectID))
	toml, err := os.ReadFile(filepath.Join(b.path, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tcWrite(t, filepath.Join(b.path, "city.toml"), string(toml)+"\n[[rigs]]\nname = \"gamma\"\nprefix = \"gm\"\n")
	site, err := os.ReadFile(filepath.Join(b.path, ".gc", "site.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tcWrite(t, filepath.Join(b.path, ".gc", "site.toml"), string(site)+fmt.Sprintf("\n[[rig]]\nname = \"gamma\"\npath = %q\n", gamma))
	cfg, err := loadCityConfig(b.path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 2 {
		t.Fatalf("rigs = %+v", cfg.Rigs)
	}
	b.cfg = cfg
	b.cs.update(cfg, runtime.NewFake())

	// The controller seam: neither rig of city B opens.
	for _, rig := range []string{"beta", "gamma"} {
		_, err := b.cs.BeadStore(rig).List(beads.ListQuery{AllowScan: true})
		if err == nil || !errors.Is(err, errRigNamespace) {
			t.Fatalf("city B opened rig %s, whose store is also another rig's: %v", rig, err)
		}
	}
	// The by-path seam, for either scope.
	for _, scope := range []string{b.rigDir, gamma} {
		if _, err := openStoreResultAtForCityWithConfig(scope, b.path, cfg, gate.ModeUnset, false, false, false, nil); err == nil || !errors.Is(err, errRigNamespace) {
			t.Fatalf("the by-path open of %s succeeded: %v", scope, err)
		}
	}
	// The hook: every candidate of the beta leg is skipped, and the skip
	// says why (the deny-all reason names the rig sharing the store).
	var stderr bytes.Buffer
	all, err := f.engine.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	g := b.hookGuard().forLeg(b.rig, b.rigDir)
	if kept := g.filterCandidates(all, &stderr); len(kept) != 0 {
		t.Fatalf("the hook kept %v from a refused rig's leg", tcIDs(kept))
	}
	if !strings.Contains(stderr.String(), `rig \"gamma\"`) && !strings.Contains(stderr.String(), `rig "gamma"`) {
		t.Fatalf("the hook's skip does not say why:\n%s", stderr.String())
	}
	if err := g.claimRefusal(f.row("sh-r23b").bead, b.pool); err == nil || !strings.Contains(err.Error(), "gamma") {
		t.Fatalf("the hook claim on the refused rig = %v; want a refusal naming the rig that shares its store", err)
	}
	// gc bd in either scope refuses, with the reason.
	for _, scope := range []string{b.rigDir, gamma} {
		check := newBdRigNamespaceCheck(cfg, b.path, scope, f.engine.Get)
		_, msg, refused := check.check([]string{"update", "sh-r23b", "--assignee", b.pool})
		if !refused || !strings.Contains(msg, "gamma") {
			t.Fatalf("gc bd in %s: refused=%v msg=%q; want a refusal naming the rig that shares the store", scope, refused, msg)
		}
	}
	// City A attaches the same database and project id by design, and is
	// not affected: its rig still opens, guarded, and filters.
	f.requireShared(t, f.a)
	rows, err := f.a.rigStore(t).List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city A's rig stopped working: %v", err)
	}
	want := f.selectIDs(func(r tcRow) bool { return f.mayActOn(f.a, r) })
	if got := tcIDs(rows); !slices.Equal(got, want) {
		t.Fatalf("city A reads %v, want %v", got, want)
	}
}

// TestTwoCityCountAgreesWithList: Count never reports another city's rows: an
// identity this city does not admit counts 0 without a read, and anything
// else is declined so the caller falls back to the filtered List.
func TestTwoCityCountAgreesWithList(t *testing.T) {
	f := newTCFixture(t, config.RigFederationRig)
	for _, c := range f.cities() {
		counter, ok := splitStrictFrontOf(c.rigStore(t)).(beads.Counter)
		if !ok {
			base, _, _ := unwrapBeadPolicyStore(splitStrictFrontOf(c.rigStore(t)))
			counter, ok = base.(beads.Counter)
		}
		if !ok {
			t.Fatalf("city %s: the rig store counts nothing", c.label)
		}
		for _, v := range []string{tcCityAlias, tcShortID, tcPerson, tcNamed} {
			n, err := counter.Count(context.Background(), beads.ListQuery{Assignee: v})
			if err != nil || n != 0 {
				t.Errorf("city %s: Count(assignee %q) = %d, %v; want 0 without a read", c.label, v, n, err)
			}
		}
		n, err := counter.Count(context.Background(), beads.ListQuery{AllowScan: true})
		if err == nil {
			rows, _ := c.rigStore(t).List(beads.ListQuery{AllowScan: true})
			if n != len(rows) {
				t.Errorf("city %s: Count = %d, List = %d", c.label, n, len(rows))
			}
		} else if !errors.Is(err, beads.ErrCountUnsupported) {
			t.Errorf("city %s: Count over everything = %v; want a decline the caller falls back from", c.label, err)
		}
	}
}

// TestTwoCityWritesAreJudgedOnTheStoredRowNotTheCache: city A takes over one
// of city B's rows (and hands one of its own to B) in the shared store while
// B's cache still shows the old owners. B's writes are judged on the stored
// row: the row now A's is refused, the row now B's is accepted. A stale cache
// must never let a city write another city's row.
func TestTwoCityWritesAreJudgedOnTheStoredRowNotTheCache(t *testing.T) {
	f := newTCFixture(t, config.RigFederationRig)
	b := f.b
	if err := f.engine.SetMetadata("sh-r23b", beadmeta.RoutedToMetadataKey, f.a.worker); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.SetMetadata("sh-r23a", beadmeta.RoutedToMetadataKey, b.worker); err != nil {
		t.Fatal(err)
	}
	cached, err := b.cache(t).Get("sh-r23b")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Metadata[beadmeta.RoutedToMetadataKey] != b.worker {
		t.Skipf("precondition: city B's cache already absorbed the take-over (%v); the stale case is not reproduced", cached.Metadata)
	}
	store := b.rigStore(t)
	for _, w := range tcWrites() {
		err := w.write(store, b, f.row("sh-r23b"))
		if !errors.Is(err, errRigNamespace) {
			t.Errorf("city B, stale cache: %s on a row city A now owns = %v; want a refusal", w.name, err)
		}
	}
	if got, _ := f.engine.Get("sh-r23b"); got.Assignee != "" || got.Status != "open" || got.Metadata[beadmeta.RoutedToMetadataKey] != f.a.worker {
		t.Fatalf("city B wrote city A's row through a stale cache: %+v", got)
	}
	if err := store.Update("sh-r23a", beads.UpdateOpts{Assignee: &b.pool}); err != nil {
		t.Fatalf("city B, stale cache: claiming a row now B's = %v; want it accepted", err)
	}
}

// TestTwoCityAutocloseSweepClosesOnlyOwnConvoys: each city has a convoy in
// the shared store whose one member closes. Run alone, city B's controller
// sweep (its census is the guarded view) autocloses only its own convoy; with
// federation = "city" it autocloses city A's too.
func TestTwoCityAutocloseSweepClosesOnlyOwnConvoys(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			f := newTCFixture(t, mode)
			for _, c := range f.cities() {
				lc := strings.ToLower(c.label)
				if _, err := f.engine.Create(beads.Bead{ID: "sh-convoy" + lc, Title: "batch", Type: "convoy", Status: "open", Metadata: rnsMD(rigNamespaceOwnerStampKey, c.rig+"/")}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.engine.Create(beads.Bead{ID: "sh-member" + lc, Title: "task", Type: "task", Status: "open", ParentID: "sh-convoy" + lc, Metadata: rnsMD(beadmeta.RoutedToMetadataKey, c.worker)}); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range f.cities() {
				f.syncCache(t, c, true)
				c.cs.runAutocloseSweepPass(sweepTestClock(0)) // seeds the census
			}
			for _, id := range []string{"sh-membera", "sh-memberb"} {
				if err := f.engine.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			b := f.b
			b.cache(t).ReconcileNowForTest()
			b.cache(t).ReconcileNowForTest()
			for i := 1; i <= 4; i++ {
				b.cs.runAutocloseSweepPass(sweepTestClock(i))
			}
			own, _ := f.engine.Get("sh-convoyb")
			theirs, _ := f.engine.Get("sh-convoya")
			if own.Status != "closed" {
				t.Fatalf("city B did not autoclose its own convoy (status %q): the column would pass vacuously", own.Status)
			}
			wantTheirs := "open"
			if mode == config.RigFederationCity {
				wantTheirs = "closed" // today's leak
			}
			if theirs.Status != wantTheirs {
				t.Fatalf("city B left city A's convoy %q, want %q (federation = %q)", theirs.Status, wantTheirs, mode)
			}
		})
	}
}

// TestTwoCityControllerTicksTouchNoOtherCitysRows runs whole controller
// ticks of one city at a time over the shared store and diffs it: with
// federation = "rig" a city's ticks change none of the other city's rows and
// none that are no city's. With federation = "city" the diff is logged.
func TestTwoCityControllerTicksTouchNoOtherCitysRows(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		t.Run("federation="+mode, func(t *testing.T) {
			// No real bd or dolt is reachable from a tick's subprocesses.
			bin := t.TempDir()
			for _, name := range []string{"bd", "dolt"} {
				tcWrite(t, filepath.Join(bin, name), "#!/bin/sh\necho \"tc fixture: no $0\" >&2\nexit 1\n")
				if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("BEADS_DOLT_AUTO_START", "0")
			f := newTCFixture(t, mode)
			leaked := 0
			for _, c := range []*tcCity{f.b, f.a} {
				cr, sp := f.runtime(t, c)
				before := f.snapshot(t)
				sessionsBefore := map[string]bool{}
				if rows, err := c.hq.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Label: "gc:session"}); err == nil {
					for _, b := range rows {
						sessionsBefore[b.ID] = true
					}
				}
				var dirty atomic.Bool
				lastProviderName := ""
				prevPoolRunning := map[string]bool{}
				const ticks = 5
				for i := 0; i < ticks; i++ {
					f.syncCache(t, c, false)
					cr.tick(context.Background(), &dirty, &lastProviderName, c.path, &prevPoolRunning, "test")
				}
				after := f.snapshot(t)
				var changed, foreign []string
				for id, was := range before {
					if after[id] == was {
						continue
					}
					changed = append(changed, id)
					if r := f.row(id); r.owner != c.owner && r.owner != tcUnattributed {
						foreign = append(foreign, fmt.Sprintf("%s (%s): %s -> %s", id, r.what, was, after[id]))
					}
				}
				sort.Strings(changed)
				var starts []string
				for _, call := range sp.SnapshotCalls() {
					if call.Method == "Start" {
						starts = append(starts, call.Name)
					}
				}
				t.Logf("city %s, %d ticks (federation = %q): changed %v; started %v; new session templates %v", c.label, ticks, mode, changed, starts, tcSessionTemplates(t, c, sessionsBefore))
				if mode == config.RigFederationRig && len(foreign) > 0 {
					t.Errorf("city %s's ticks changed rows it does not own:\n%s", c.label, strings.Join(foreign, "\n"))
				}
				if mode == config.RigFederationRig && len(changed) == 0 {
					t.Errorf("city %s's ticks changed nothing, not even its own pool claim; the diff would pass vacuously", c.label)
				}
				if len(foreign) > 0 {
					leaked++
					t.Logf("city %s's ticks changed rows it does not own:\n%s", c.label, strings.Join(foreign, "\n"))
				}
			}
			// Today a city stamps its session name on other rows held under
			// the identities it shares with the other city.
			if mode == config.RigFederationCity && leaked == 0 {
				t.Errorf("with federation = \"city\" no city's ticks touched another's rows; there is no leak to pin")
			}
		})
	}
}

// TestTwoCityOrphanSweepReleaseIsFencedTwice: the shell sweep releases a
// claim with `gc bd release-if-current <id> <holder>`. In a guarded rig both
// layers refuse another city's row on their own: gc bd's write check, and the
// guarded store the verb opens by path. Each city still releases its own
// claims (the positive control). With federation = "city" today's verb
// releases every claim it is pointed at.
func TestTwoCityOrphanSweepReleaseIsFencedTwice(t *testing.T) {
	for _, mode := range []string{config.RigFederationRig, config.RigFederationCity} {
		for _, layer := range []string{"gc bd check", "guarded store"} {
			if mode == config.RigFederationCity && layer == "gc bd check" {
				continue // no check without a guarded rig
			}
			t.Run("federation="+mode+"/"+layer, func(t *testing.T) {
				for _, label := range []string{"A", "B"} {
					f := newTCFixture(t, mode)
					c := f.a
					if label == "B" {
						c = f.b
					}
					check := newBdRigNamespaceCheck(c.cfg, c.path, c.rigDir, f.engine.Get)
					target := execStoreTarget{ScopeRoot: c.rigDir, ScopeKind: "rig", Prefix: tcPrefix, RigName: c.rig}
					var released, claimed []string
					for _, r := range f.rows() {
						if r.bead.Assignee == "" || r.bead.Status != "in_progress" {
							continue
						}
						claimed = append(claimed, r.id)
						if layer == "gc bd check" {
							if _, _, refused := check.check([]string{"release-if-current", r.id, r.bead.Assignee}); refused {
								continue
							}
						}
						var out, errb bytes.Buffer
						code := doBdReleaseIfCurrent(c.path, c.cfg, target, r.id, r.bead.Assignee, &out, &errb)
						if code == 0 && strings.TrimSpace(out.String()) == "released" {
							released = append(released, r.id)
						} else if mode == config.RigFederationRig && layer == "guarded store" && !strings.Contains(errb.String(), "federation") {
							t.Errorf("city %s: release-if-current %s: exit %d %q %q; want the guard's refusal", c.label, r.id, code, out.String(), errb.String())
						}
					}
					sort.Strings(released)
					want := f.selectIDs(func(r tcRow) bool {
						return r.bead.Assignee != "" && r.bead.Status == "in_progress" && f.mayActOn(c, r)
					})
					if !slices.Equal(released, want) {
						t.Errorf("city %s (federation = %q, %s): released %v\n want %v\n extra %v", c.label, mode, layer, released, want, tcMinus(released, want))
					}
					if mode == config.RigFederationRig && len(tcMinus(claimed, want)) == 0 {
						t.Errorf("city %s: no claim of another city reached the release; vacuous", c.label)
					}
				}
			})
		}
	}
}
