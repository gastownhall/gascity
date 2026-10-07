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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

var drainAckRowNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// drainAckRowStore returns a conditional-write store holding one awake
// session row at generation 3 with instance token tok-a.
func drainAckRowStore(t *testing.T) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	stampedMemStore(t, store)
	return store, createDrainAckRow(t, store)
}

func createDrainAckRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	row, err := store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":   "worker",
			"state":          string(sessionpkg.StateAwake),
			"generation":     "3",
			"instance_token": "tok-a",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return row
}

func preWakeRow(t *testing.T, store beads.Store, id string) {
	t.Helper()
	patch := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
		Now:           drainAckRowNow,
		InstanceToken: "tok-b",
		Generation:    4,
	})
	if err := store.SetMetadataBatch(id, patch); err != nil {
		t.Fatalf("PreWake: %v", err)
	}
}

// checkAndCommit runs checkDrainAckRow and its commit back to back.
func checkAndCommit(store beads.Store, id string, operator bool, envToken string) error {
	commit, err := checkDrainAckRow(store, id, operator, envToken, drainAckRowNow)
	if err != nil {
		return err
	}
	return commit()
}

func rowAck(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b := mustGetBead(t, store, id)
	return b.Metadata[sessionpkg.DrainAckIncarnationKey] + "@" + b.Metadata[sessionpkg.DrainAckAtKey]
}

const noRowAck = "@"

// TestDrainAckRowRefusesAfterPreWakeBetweenReadAndCAS pins I-ACK-1: a PreWake
// landing between the CLI's read and its CAS fails the revision check, and the
// re-read refuses instead of acking the next incarnation. Kills: a blind write,
// a retry that skips the token check (self), and a retry that binds to the
// fresh generation (operator).
func TestDrainAckRowRefusesAfterPreWakeBetweenReadAndCAS(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator bool
		envToken string
		want     string
	}{
		{name: "self", envToken: "tok-a", want: "does not match"},
		{name: "operator", operator: true, want: "restarted while the ack was being written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, row := drainAckRowStore(t)
			store := &interleavedStore{Store: mem, id: row.ID}
			store.between = func() { preWakeRow(t, mem, row.ID) }

			err := checkAndCommit(store, row.ID, tc.operator, tc.envToken)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ack = %v, want a refusal containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "tok-") {
				t.Fatalf("refusal %q leaks the instance token", err)
			}
			if got := rowAck(t, mem, row.ID); got != noRowAck {
				t.Fatalf("row ack = %q, want none", got)
			}
		})
	}
}

// TestDrainAckRowRefusesUnprovableSelfAck pins the self form's proof: a pane
// with no token, or a superseded one, is refused by the check, before any
// commit exists. Kills: dropping either token case.
func TestDrainAckRowRefusesUnprovableSelfAck(t *testing.T) {
	for _, tc := range []struct {
		name, envToken, want string
	}{
		{name: "no token", envToken: "  ", want: "GC_INSTANCE_TOKEN is not set"},
		{name: "stale token", envToken: "tok-old", want: "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, row := drainAckRowStore(t)
			commit, err := checkDrainAckRow(store, row.ID, false, tc.envToken, drainAckRowNow)
			if err == nil || !strings.Contains(err.Error(), tc.want) || commit != nil {
				t.Fatalf("check = (commit %v, %v), want a refusal containing %q", commit != nil, err, tc.want)
			}
		})
	}
}

// TestDrainAckRowBindsReadIncarnation pins the write: both forms name the
// generation read, the operator form needs no token, and a rekey between the
// read and the CAS (the token moves, the generation does not) does not void
// the ack. Kills: the token check on the operator form, binding to anything
// but the generation, and bounding retries by the token.
func TestDrainAckRowBindsReadIncarnation(t *testing.T) {
	rekey := func(t *testing.T, store beads.Store, id string) {
		if err := store.SetMetadata(id, "instance_token", "tok-rekeyed"); err != nil {
			t.Fatalf("rekey: %v", err)
		}
	}
	for _, tc := range []struct {
		name     string
		operator bool
		envToken string
		between  func(*testing.T, beads.Store, string)
	}{
		{name: "self", envToken: "tok-a"},
		{name: "operator without a token", operator: true},
		{name: "operator from another pane", operator: true, envToken: "tok-caller"},
		{name: "operator across a rekey", operator: true, between: rekey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, row := drainAckRowStore(t)
			store := &interleavedStore{Store: mem, id: row.ID}
			if tc.between != nil {
				store.between = func() { tc.between(t, mem, row.ID) }
			}
			if err := checkAndCommit(store, row.ID, tc.operator, tc.envToken); err != nil {
				t.Fatalf("ack: %v", err)
			}
			if got := rowAck(t, mem, row.ID); got != "3@2026-10-06T12:00:00Z" {
				t.Errorf("row ack = %q, want the read generation 3 at 2026-10-06T12:00:00Z", got)
			}
		})
	}
}

// TestDrainAckRowPatchRefusesUnbindableRows covers the remaining guards of
// the pure decision. Kills: removing the closed, empty-generation or bound
// generation case.
func TestDrainAckRowPatchRefusesUnbindableRows(t *testing.T) {
	open := beads.Bead{ID: "gc-1", Status: "open", Metadata: map[string]string{"generation": "3", "instance_token": "tok-a"}}
	closed := open
	closed.Status = "closed"
	noGeneration := beads.Bead{ID: "gc-1", Status: "open", Metadata: map[string]string{"instance_token": "tok-a"}}
	for _, tc := range []struct {
		name  string
		row   beads.Bead
		bound string
		want  string
	}{
		{name: "closed", row: closed, want: "is closed"},
		{name: "no generation", row: noGeneration, want: "no generation"},
		{name: "generation moved since first read", row: open, bound: "2", want: "restarted while"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := drainAckRowPatch(tc.row, true, "", tc.bound, drainAckRowNow)
			if err == nil || !strings.Contains(err.Error(), tc.want) || patch != nil {
				t.Fatalf("drainAckRowPatch = (%v, %v), want a refusal containing %q", patch, err, tc.want)
			}
		})
	}
}

// TestDrainAckRowWithoutConditionalWriter pins the capability rule: the
// incarnation is still checked, and a match reports errDrainAckRowUnfenced
// with nothing to commit. Kills: a blind fallback write, and skipping the
// check on such a store.
func TestDrainAckRowWithoutConditionalWriter(t *testing.T) {
	store := beads.NewMemStore()
	row := createDrainAckRow(t, store)
	if _, err := checkDrainAckRow(store, row.ID, false, "tok-old", drainAckRowNow); err == nil || errors.Is(err, errDrainAckRowUnfenced) {
		t.Fatalf("check = %v, want a token refusal on a store without a conditional writer", err)
	}
	commit, err := checkDrainAckRow(store, row.ID, false, "tok-a", drainAckRowNow)
	if !errors.Is(err, errDrainAckRowUnfenced) || commit != nil {
		t.Fatalf("check = (commit %v, %v), want errDrainAckRowUnfenced and no commit", commit != nil, err)
	}
	if got := rowAck(t, store, row.ID); got != noRowAck {
		t.Fatalf("row ack = %q, want none", got)
	}
}

// preE3DoRuntimeDrainAck is doRuntimeDrainAck as it was before E3, frozen as
// the legacy oracle.
func preE3DoRuntimeDrainAck(dops drainOps, cityPath, targetName, sn, sessionID string, jsonOutput bool, stdout, stderr io.Writer) int {
	drainAckReleaseHeldClaims(cityPath, sn, stderr)
	if err := dops.setDrainAck(sn); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := drainAckPokeController(cityPath, reconcilekey.SessionRef(sessionID, sn)); err != nil {
		fmt.Fprintf(stderr, "gc runtime drain-ack: warning: poke failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, runtimeActionJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "runtime drain-ack",
			Action:        "drain-ack",
			Session:       sn,
			Target:        targetName,
			Status:        "acknowledged",
		}); err != nil {
			fmt.Fprintf(stderr, "gc runtime drain-ack: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "Drain acknowledged. Controller poked for immediate stop.") //nolint:errcheck // best-effort stdout
	return 0
}

// drainAckCity is a temp city on a file store with conditional writes
// required, holding the awake session row "worker" (generation 3, token
// tok-a). reconciler is its [daemon] session_reconciler; admitV2 sets the
// developer override drainAckCityMode latches with.
func drainAckCity(t *testing.T, reconciler string, admitV2 bool) (string, beads.Store, string) {
	t.Helper()
	city := t.TempDir()
	toml := "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\nconditional_writes = \"require\"\n"
	if reconciler != "" {
		toml += "\n[daemon]\nsession_reconciler = \"" + reconciler + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	original := drainAckModeLookupEnv
	t.Cleanup(func() { drainAckModeLookupEnv = original })
	drainAckModeLookupEnv = nil
	if admitV2 {
		drainAckModeLookupEnv = func(key string) (string, bool) { return "1", key == v2SkeletonEnv }
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city, io.Discard)
	if err != nil {
		t.Fatalf("load city: %v", err)
	}
	store, err := openCityStoreAtWithConfig(city, cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store = cliSessionStore(store, cfg, city)
	return city, store, createDrainAckRow(t, store).ID
}

// drainAckRun is everything a drain-ack does that a caller or legacy can see.
type drainAckRun struct {
	code           int
	stdout, stderr string
	calls          []runtime.Call
	releases       []string
	pokes          int
}

// runDrainAck runs ack with the release and poke seams recorded. Each
// release records the row ack visible at that moment.
func runDrainAck(t *testing.T, store beads.Store, id string, ack func(drainOps, io.Writer, io.Writer) int) drainAckRun {
	t.Helper()
	originalRelease, originalPoke := drainAckReleaseHeldClaims, drainAckPokeController
	t.Cleanup(func() { drainAckReleaseHeldClaims, drainAckPokeController = originalRelease, originalPoke })
	var run drainAckRun
	drainAckReleaseHeldClaims = func(string, string, io.Writer) {
		run.releases = append(run.releases, rowAck(t, store, id))
	}
	drainAckPokeController = func(string, reconcilekey.Key) error { run.pokes++; return nil }
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	run.code = ack(newDrainOps(sp), &stdout, &stderr)
	run.stdout, run.stderr, run.calls = stdout.String(), stderr.String(), sp.Calls
	return run
}

// TestRuntimeDrainAckModeSwitch drives the real entry point (runtimeDrainAck:
// mode, checkDrainAckRowAt's open and check, the row CAS, doRuntimeDrainAck)
// over temp cities. A legacy city, including one naming v2 that this build
// will not latch, is the frozen pre-E3 drain-ack in every case and writes no
// row. A v2 city writes the row ack first and is otherwise the oracle, or
// refuses with nothing written. Kills: strict-always and legacy-always mode
// mutants, a legacy row write, and a refused v2 ack that releases claims,
// sets env keys or pokes.
func TestRuntimeDrainAckModeSwitch(t *testing.T) {
	type ackCase struct {
		name        string
		sessionName string
		operator    bool
		envToken    string
		v2Refusal   string // "" = the v2 ack lands
	}
	cases := []ackCase{
		{name: "self, token matches", sessionName: "worker", envToken: "tok-a"},
		{name: "self, no token", sessionName: "worker", v2Refusal: "GC_INSTANCE_TOKEN is not set"},
		{name: "self, stale token", sessionName: "worker", envToken: "tok-old", v2Refusal: "does not match"},
		{name: "operator", sessionName: "worker", operator: true},
		{name: "target without a row", sessionName: "ghost", operator: true, v2Refusal: `resolving session "ghost"`},
	}
	for _, city := range []struct {
		name       string
		reconciler string
		admitV2    bool
		strict     bool
	}{
		{name: "legacy default"},
		{name: "v2 not admitted by this build", reconciler: "v2"},
		{name: "v2", reconciler: "v2", admitV2: true, strict: true},
	} {
		for _, tc := range cases {
			t.Run(city.name+"/"+tc.name, func(t *testing.T) {
				cityPath, store, id := drainAckCity(t, city.reconciler, city.admitV2)
				target := sessionRuntimeTarget{cityPath: cityPath, display: tc.sessionName, sessionName: tc.sessionName}
				oracle := runDrainAck(t, store, id, func(dops drainOps, stdout, stderr io.Writer) int {
					return preE3DoRuntimeDrainAck(dops, cityPath, tc.sessionName, tc.sessionName, "", false, stdout, stderr)
				})
				got := runDrainAck(t, store, id, func(dops drainOps, stdout, stderr io.Writer) int {
					return runtimeDrainAck(dops, target, tc.operator, tc.envToken, false, stdout, stderr)
				})
				ack := rowAck(t, store, id)

				switch {
				case !city.strict:
					if !reflect.DeepEqual(got, oracle) || ack != noRowAck {
						t.Fatalf("legacy drain-ack diverged from pre-E3 or wrote the row (%q):\n got %+v\nwant %+v", ack, got, oracle)
					}
				case tc.v2Refusal != "":
					if got.code != drainAckRefused || len(got.releases)+len(got.calls)+got.pokes != 0 || ack != noRowAck ||
						!strings.Contains(got.stderr, "refused, nothing acknowledged: ") || !strings.Contains(got.stderr, tc.v2Refusal) {
						t.Fatalf("v2 refusal wrote or misreported (row %q): %+v", ack, got)
					}
				default:
					if !strings.HasPrefix(ack, "3@") {
						t.Fatalf("v2 row ack = %q, want generation 3", ack)
					}
					oracle.releases = []string{ack} // the row ack lands before the release
					if !reflect.DeepEqual(got, oracle) {
						t.Fatalf("v2 drain-ack after the row ack diverged from pre-E3:\n got %+v\nwant %+v", got, oracle)
					}
				}
			})
		}
	}
}

// preWakeInCASStore lands a PreWake inside the first UpdateIfMatch, after the
// commit read the row, so the conditional write meets a moved revision and
// the retry re-reads a new generation.
type preWakeInCASStore struct {
	*beads.MemStore
	t    *testing.T
	once sync.Once
}

func (s *preWakeInCASStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.once.Do(func() { preWakeRow(s.t, s.MemStore, id) })
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

// contendedStore moves the row's revision after every read, so every CAS
// loses.
type contendedStore struct {
	beads.Store
	reads int
}

func (s *contendedStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.reads++
	if err == nil {
		err = s.SetMetadata(id, "synced_at", fmt.Sprint(s.reads))
	}
	return b, err
}

func (s *contendedStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestRuntimeDrainAckV2StoreOutcomes runs the v2 flow against a PreWake
// landing inside UpdateIfMatch and a CAS that loses every attempt (both refuse
// with nothing written: no row ack, claim release, env key or poke), and
// against a store with no conditional writer (a warning, then the pre-E3 ack).
// Kills: retries that reuse the first decision instead of re-deciding on the
// fresh row, ignoring a failed commit, releasing claims before the commit,
// and refusing on a writer-less store.
func TestRuntimeDrainAckV2StoreOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*testing.T) (beads.Store, *beads.MemStore, string)
		want  string
		acked bool
	}{
		{name: "PreWake inside UpdateIfMatch", want: "row ack not written, nothing acknowledged: GC_INSTANCE_TOKEN does not match", store: func(t *testing.T) (beads.Store, *beads.MemStore, string) {
			mem, row := drainAckRowStore(t)
			return &preWakeInCASStore{MemStore: mem, t: t}, mem, row.ID
		}},
		{name: "persistent contention", want: "row ack not written, nothing acknowledged: conditional write kept losing", store: func(t *testing.T) (beads.Store, *beads.MemStore, string) {
			mem, row := drainAckRowStore(t)
			return &contendedStore{Store: mem}, mem, row.ID
		}},
		{name: "no conditional writer", want: "warning: the session store has no conditional writer", acked: true, store: func(t *testing.T) (beads.Store, *beads.MemStore, string) {
			mem := beads.NewMemStore()
			return mem, mem, createDrainAckRow(t, mem).ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _, _ := drainAckCity(t, "v2", true)
			store, mem, id := tc.store(t)
			original := drainAckOpenSessionRow
			t.Cleanup(func() { drainAckOpenSessionRow = original })
			drainAckOpenSessionRow = func(string, *config.City, string, string) (beads.Store, string, error) { return store, id, nil }

			target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
			got := runDrainAck(t, mem, id, func(dops drainOps, stdout, stderr io.Writer) int {
				return runtimeDrainAck(dops, target, false, "tok-a", false, stdout, stderr)
			})
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.want)
			}
			switch {
			case tc.acked:
				if got.code != 0 || len(got.releases) != 1 || len(got.calls) == 0 || got.pokes != 1 {
					t.Fatalf("writer-less v2 ack = %+v, want the pre-E3 ack", got)
				}
			case got.code != drainAckRefused || len(got.releases)+len(got.calls)+got.pokes != 0:
				t.Fatalf("commit refusal = %+v, want drainAckRefused with nothing written", got)
			}
			if ack := rowAck(t, mem, id); ack != noRowAck {
				t.Fatalf("row ack = %q, want none", ack)
			}
		})
	}
}

// TestRuntimeUndrainModeSwitch drives runtimeUndrain over temp cities: a
// legacy city is doRuntimeUndrain and leaves the row untouched; a v2 city
// also clears the row ack. Kills: mode mutants on undrain, and a v2 undrain
// that leaves the row ack behind.
func TestRuntimeUndrainModeSwitch(t *testing.T) {
	for _, city := range []struct {
		name       string
		reconciler string
		admitV2    bool
		wantAck    bool
	}{
		{name: "legacy", wantAck: true},
		{name: "v2", reconciler: "v2", admitV2: true},
	} {
		t.Run(city.name, func(t *testing.T) {
			cityPath, store, id := drainAckCity(t, city.reconciler, city.admitV2)
			if err := store.SetMetadataBatch(id, map[string]string{sessionpkg.DrainAckIncarnationKey: "3", sessionpkg.DrainAckAtKey: "2026-10-06T12:00:00Z"}); err != nil {
				t.Fatalf("seed ack: %v", err)
			}
			undrain := func(run func(drainOps, runtime.Provider, events.Recorder, io.Writer, io.Writer) int) (int, string, string, []events.Event) {
				dops := newFakeDrainOps()
				dops.draining["worker"] = true
				sp := runtime.NewFake()
				if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
					t.Fatal(err)
				}
				rec := events.NewFake()
				var stdout, stderr bytes.Buffer
				code := run(dops, sp, rec, &stdout, &stderr)
				if dops.draining["worker"] {
					t.Error("env drain flag still set")
				}
				return code, stdout.String(), stderr.String(), rec.Events
			}
			wantCode, wantOut, wantErr, wantEvents := undrain(func(d drainOps, sp runtime.Provider, rec events.Recorder, o, e io.Writer) int {
				return doRuntimeUndrain(d, sp, rec, "worker", "worker", false, o, e)
			})
			code, out, errOut, evs := undrain(func(d drainOps, sp runtime.Provider, rec events.Recorder, o, e io.Writer) int {
				return runtimeUndrain(d, sp, rec, sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}, false, o, e)
			})
			if code != wantCode || out != wantOut || errOut != wantErr || len(evs) != len(wantEvents) {
				t.Errorf("undrain = (%d, %q, %q, %d events), want doRuntimeUndrain's (%d, %q, %q, %d)", code, out, errOut, len(evs), wantCode, wantOut, wantErr, len(wantEvents))
			}
			if got := rowAck(t, store, id) != noRowAck; got != city.wantAck {
				t.Errorf("row ack present = %v, want %v", got, city.wantAck)
			}
		})
	}
}

// TestUndrainRuntimeRowClearFailure pins a failed v2 row clear: the env clear
// and its event stand, and undrain exits 1. Kills: ignoring the failure, and
// a row clear that gates the legacy clear.
func TestUndrainRuntimeRowClearFailure(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := undrainRuntime(dops, func() error { return errors.New("store down") }, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 1 || dops.draining["worker"] || len(rec.Events) != 1 || !strings.Contains(stderr.String(), "clearing the row drain-ack: store down") {
		t.Fatalf("undrain = %d (draining %v, events %v, stderr %q), want 1 after the env clear", code, dops.draining["worker"], rec.Events, stderr.String())
	}
}

// TestDrainAckStrictConfig pins mode detection: only a config the controller
// would latch as v2 is strict. Kills: treating the default, an alias, an
// unknown value, or a v2 this build refuses as v2.
func TestDrainAckStrictConfig(t *testing.T) {
	original := drainAckModeLookupEnv
	t.Cleanup(func() { drainAckModeLookupEnv = original })
	for _, tc := range []struct {
		value   string
		admitV2 bool
		want    bool
	}{
		{value: ""},
		{value: "legacy", admitV2: true},
		{value: "auto", admitV2: true},
		{value: "bogus", admitV2: true},
		{value: "v2"},
		{value: "v2", admitV2: true, want: true},
	} {
		drainAckModeLookupEnv = nil
		if tc.admitV2 {
			drainAckModeLookupEnv = func(key string) (string, bool) { return "1", key == v2SkeletonEnv }
		}
		cfg := &config.City{}
		cfg.Daemon.SessionReconciler = tc.value
		if got := drainAckStrictConfig(cfg); got != tc.want {
			t.Errorf("drainAckStrictConfig(%q, admit %v) = %v, want %v", tc.value, tc.admitV2, got, tc.want)
		}
	}
	if _, strict := drainAckCityMode(t.TempDir()); strict {
		t.Error("a directory without city.toml is strict")
	}
}

// TestHookDrainAckToleranceCallSites pins which hook drains tolerate a v2
// refusal: stale-session, missing-registration and drain-pending complete the
// drain without drain_acknowledged and exit 0; the suspension and no-work
// drains still fail, and no drain tolerates another failure. The drain-pending
// hint names --operator only under v2. Kills: an untolerant call site, a
// tolerant suspension or no-work site, a tolerated refusal reported as
// acknowledged, the tolerant wrapper swallowing every failure, and the
// --operator hint leaking into legacy.
func TestHookDrainAckToleranceCallSites(t *testing.T) {
	if drainAckRefused == 0 || drainAckRefused == 1 {
		t.Fatalf("drainAckRefused = %d must differ from success (0) and failure (1), or the hook tolerates failures", drainAckRefused)
	}
	refused := func(io.Writer) error { return errDrainAckRefused }
	failed := func(io.Writer) error { return errors.New("tmux borked") }
	type drainFn func(ack hookDrainAckFunc, stdout, stderr io.Writer) int
	stale := func(ack hookDrainAckFunc, stdout, stderr io.Writer) int {
		return writeHookClaimStaleSessionDrain(hookCommandOptions{DrainAck: true, JSON: true, DrainAckFn: ack}, stdout, stderr)
	}
	missing := func(ack hookDrainAckFunc, stdout, stderr io.Writer) int {
		return writeHookClaimMissingSessionRegistrationDrain(hookCommandOptions{DrainAck: true, JSON: true, DrainAckFn: ack}, stdout, stderr)
	}
	pending := func(strict bool) drainFn {
		return func(ack hookDrainAckFunc, stdout, stderr io.Writer) int {
			return writeHookClaimDrainPending(hookClaimLabel, "gc-7", hookClaimOptions{DrainAck: true, JSON: true, StrictDrainAck: strict}, hookClaimOps{DrainAck: ack}, stdout, stderr)
		}
	}
	// claimsErrored reaches the same writeHookClaimDrain call as an idle store
	// without the post-drain divergence read.
	noWork := func(ack hookDrainAckFunc, stdout, stderr io.Writer) int {
		return writeHookClaimNoWork(hookClaimOptions{DrainAck: true, JSON: true}, hookClaimOps{DrainAck: ack}, true, "", stdout, stderr)
	}
	suspended := func(ack hookDrainAckFunc, stdout, stderr io.Writer) int {
		return writeHookClaimSuspensionDrain(hookClaimReasonRigSuspended, hookCommandOptions{DrainAck: true, JSON: true, DrainAckFn: ack}, stdout, stderr)
	}
	for _, tc := range []struct {
		name     string
		drain    drainFn
		ack      hookDrainAckFunc
		wantCode int
	}{
		{name: "stale session, refused", drain: stale, ack: refused},
		{name: "missing registration, refused", drain: missing, ack: refused},
		{name: "drain pending (v2), refused", drain: pending(true), ack: refused},
		{name: "suspension, refused", drain: suspended, ack: refused, wantCode: 1},
		{name: "no work, refused", drain: noWork, ack: refused, wantCode: 1},
		{name: "stale session, failed", drain: stale, ack: failed, wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := tc.drain(tc.ack, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON drain record: %v\n%s", err, stdout.String())
			}
			if result.Action != "drain" || result.DrainAcknowledged {
				t.Errorf("record = %+v, want action=drain without drain_acknowledged", result)
			}
			if tolerated := strings.Contains(stderr.String(), "drain-ack refused for this seat"); tolerated != (tc.wantCode == 0) {
				t.Errorf("stderr = %q, want the tolerated refusal logged %v", stderr.String(), tc.wantCode == 0)
			}
		})
	}

	for strict, want := range map[bool]string{
		false: "gc hook --claim: drain pending for this session; run: gc runtime drain-ack gc-7 — then exit\n",
		true:  "gc hook --claim: drain pending for this session; run: gc runtime drain-ack gc-7 — then exit (an operator acking it from outside the session adds --operator)\n",
	} {
		var stdout, stderr bytes.Buffer
		pending(strict)(func(io.Writer) error { return nil }, &stdout, &stderr)
		if got := stderr.String(); got != want {
			t.Errorf("hint (strict %v) = %q, want %q", strict, got, want)
		}
	}
}

// TestCmdRuntimeDrainAckOperatorNeedsTarget pins the explicit operator form:
// --operator without a target is refused before anything is resolved. Kills:
// dropping the guard, which would ack the caller's own session unchecked.
func TestCmdRuntimeDrainAckOperatorNeedsTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdRuntimeDrainAck(nil, false, true, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got, want := stderr.String(), "gc runtime drain-ack: --operator requires a session alias or ID\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestDrainAckRowKeysAreInertToLegacy pins rollback safety. Legacy's lifecycle
// projection is identical with and without the row-ack keys, and a legacy
// PreWake, which never clears them, leaves an ack that no longer names the
// row's incarnation. Kills: a key legacy reads, and binding the ack to a
// field PreWake does not move.
func TestDrainAckRowKeysAreInertToLegacy(t *testing.T) {
	store, row := drainAckRowStore(t)
	before := mustGetBead(t, store, row.ID)
	if err := checkAndCommit(store, row.ID, false, "tok-a"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	after := mustGetBead(t, store, row.ID)

	project := func(b beads.Bead) sessionpkg.LifecycleView {
		input := sessionpkg.LifecycleInputFromMetadata(b.Status, b.Metadata)
		input.Now = drainAckRowNow
		return sessionpkg.ProjectLifecycle(input)
	}
	if got, want := project(after), project(before); !reflect.DeepEqual(got, want) {
		t.Errorf("legacy projection changed by the row ack:\n got %+v\nwant %+v", got, want)
	}
	if got, want := sessionpkg.LifecycleDisplayReason(after.Status, after.Metadata, drainAckRowNow),
		sessionpkg.LifecycleDisplayReason(before.Status, before.Metadata, drainAckRowNow); got != want {
		t.Errorf("legacy display reason = %q, want %q", got, want)
	}

	preWakeRow(t, store, row.ID)
	woken := mustGetBead(t, store, row.ID)
	if woken.Metadata[sessionpkg.DrainAckIncarnationKey] == woken.Metadata["generation"] {
		t.Fatalf("after a legacy PreWake the leftover ack still names generation %q", woken.Metadata["generation"])
	}
}
