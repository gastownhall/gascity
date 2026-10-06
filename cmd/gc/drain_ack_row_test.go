package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
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

// ackDrainRow runs the check and its commit, as the CLI does around the claim
// release.
func ackDrainRow(store beads.Store, id string, operator bool, envToken string) error {
	commit, err := checkDrainAckRow(store, id, operator, envToken, drainAckRowNow)
	if err != nil || commit == nil {
		return err
	}
	return commit()
}

func assertNoRowAck(t *testing.T, store beads.Store, id string) {
	t.Helper()
	b := mustGetBead(t, store, id)
	if got := b.Metadata[drainAckIncarnationKey] + b.Metadata[drainAckAtKey]; got != "" {
		t.Fatalf("row ack written: %s=%q %s=%q", drainAckIncarnationKey, b.Metadata[drainAckIncarnationKey],
			drainAckAtKey, b.Metadata[drainAckAtKey])
	}
}

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

			err := ackDrainRow(store, row.ID, tc.operator, tc.envToken)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ack = %v, want a refusal containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "tok-") {
				t.Fatalf("refusal %q leaks the instance token", err)
			}
			assertNoRowAck(t, mem, row.ID)
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
			assertNoRowAck(t, store, row.ID)
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
			if err := ackDrainRow(store, row.ID, tc.operator, tc.envToken); err != nil {
				t.Fatalf("ack: %v", err)
			}
			b := mustGetBead(t, mem, row.ID)
			if got := b.Metadata[drainAckIncarnationKey]; got != "3" {
				t.Errorf("%s = %q, want the read generation 3", drainAckIncarnationKey, got)
			}
			if got := b.Metadata[drainAckAtKey]; got != "2026-10-06T12:00:00Z" {
				t.Errorf("%s = %q, want 2026-10-06T12:00:00Z", drainAckAtKey, got)
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
	assertNoRowAck(t, store, row.ID)
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

// swapDrainAckSeams replaces the release and poke seams for one test, logs
// each release into log, and returns the poke count.
func swapDrainAckSeams(t *testing.T, log *[]string) *int {
	t.Helper()
	originalRelease, originalPoke := drainAckReleaseHeldClaims, drainAckPokeController
	t.Cleanup(func() { drainAckReleaseHeldClaims, drainAckPokeController = originalRelease, originalPoke })
	pokes := 0
	drainAckReleaseHeldClaims = func(string, string, io.Writer) { *log = append(*log, "release") }
	drainAckPokeController = func(string, reconcilekey.Key) error { pokes++; return nil }
	return &pokes
}

// TestAckRuntimeDrainBranches pins E3's v2 (strict) branch table through the
// CLI flow:
//
//	mismatch (either store)       -> nothing written, drainAckRefused
//	match, no conditional writer  -> legacy release + env ack, warning, exit 0
//	match, CAS contention         -> nothing written, drainAckRefused
//	match, CAS lands              -> row, then release, then env ack, exit 0
//
// The env writes, when made, are exactly a bare legacy setDrainAck's (the
// oracle). Kills: any side effect before the check, an env ack or claim
// release without a committed row ack, refusing on an unfenced store, the
// release moving after the env ack, and a wrong exit code on any branch.
func TestAckRuntimeDrainBranches(t *testing.T) {
	oracle := runtime.NewFake()
	if err := newDrainOps(oracle).setDrainAck("worker"); err != nil {
		t.Fatalf("oracle setDrainAck: %v", err)
	}
	fenced := func(t *testing.T) (beads.Store, beads.Store, string) {
		mem, row := drainAckRowStore(t)
		return mem, mem, row.ID
	}
	unfenced := func(t *testing.T) (beads.Store, beads.Store, string) {
		mem := beads.NewMemStore()
		return mem, mem, createDrainAckRow(t, mem).ID
	}
	contended := func(t *testing.T) (beads.Store, beads.Store, string) {
		mem, row := drainAckRowStore(t)
		return &contendedStore{Store: mem}, mem, row.ID
	}
	for _, tc := range []struct {
		name       string
		store      func(*testing.T) (beads.Store, beads.Store, string)
		envToken   string
		wantCode   int
		wantAcked  bool
		wantRowAck bool
		wantStderr string
	}{
		{name: "mismatch", store: fenced, envToken: "tok-old", wantCode: drainAckRefused, wantStderr: "refused, nothing acknowledged: GC_INSTANCE_TOKEN does not match"},
		{name: "mismatch without a conditional writer", store: unfenced, envToken: "tok-old", wantCode: drainAckRefused, wantStderr: "refused, nothing acknowledged"},
		{name: "missing token", store: fenced, wantCode: drainAckRefused, wantStderr: "refused, nothing acknowledged: GC_INSTANCE_TOKEN is not set"},
		{name: "match without a conditional writer", store: unfenced, envToken: "tok-a", wantAcked: true, wantStderr: "warning: the session store has no conditional writer"},
		{name: "match under persistent contention", store: contended, envToken: "tok-a", wantCode: drainAckRefused, wantStderr: "row ack not written, nothing acknowledged: conditional write kept losing"},
		{name: "match and CAS lands", store: fenced, envToken: "tok-a", wantAcked: true, wantRowAck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, mem, id := tc.store(t)
			sp := runtime.NewFake()
			var log []string
			pokes := swapDrainAckSeams(t, &log)
			drainAckReleaseHeldClaims = func(string, string, io.Writer) {
				rowAcked := mustGetBead(t, mem, id).Metadata[drainAckIncarnationKey] != ""
				envAcked, _ := newDrainOps(sp).isDrainAcked("worker")
				log = append(log, fmt.Sprintf("release(row=%v env=%v)", rowAcked, envAcked))
			}
			var stdout, stderr bytes.Buffer
			checkRow := func() (func() error, error) {
				commit, err := checkDrainAckRow(store, id, false, tc.envToken, drainAckRowNow)
				return drainAckForMode(true, commit, err, &stderr)
			}
			code := ackRuntimeDrain(newDrainOps(sp), checkRow, "/city", "worker", "worker", "", false, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			metaWrites := slices.DeleteFunc(slices.Clone(sp.Calls), func(c runtime.Call) bool { return c.Method == "GetMeta" })
			if !tc.wantAcked {
				if len(log) != 0 || len(metaWrites) != 0 || *pokes != 0 {
					t.Errorf("unacknowledged branch wrote: release %v, runtime %+v, pokes %d", log, metaWrites, *pokes)
				}
				assertNoRowAck(t, mem, id)
				return
			}
			if want := []string{fmt.Sprintf("release(row=%v env=false)", tc.wantRowAck)}; !slices.Equal(log, want) {
				t.Errorf("release = %v, want %v", log, want)
			}
			if !reflect.DeepEqual(metaWrites, oracle.Calls) {
				t.Errorf("runtime meta writes = %+v, want legacy's %+v", metaWrites, oracle.Calls)
			}
			if *pokes != 1 {
				t.Errorf("pokes = %d, want 1", *pokes)
			}
			if got := mustGetBead(t, mem, id).Metadata[drainAckIncarnationKey]; (got == "3") != tc.wantRowAck {
				t.Errorf("%s = %q, want row ack %v", drainAckIncarnationKey, got, tc.wantRowAck)
			}
		})
	}
}

// TestAckRuntimeDrainLegacyMatchesOracle pins legacy mode: every case v2
// refuses behaves in a legacy city exactly as the pre-E3 drain-ack does (the
// oracle is doRuntimeDrainAck, which has no row step): same exit status,
// stdout, stderr, runtime meta calls, claim release and poke. A passing check
// still writes the row ack, best-effort. Kills: a strict refusal or warning
// leaking into legacy, a legacy row write without the token check, and a
// failed legacy CAS changing the exit status.
func TestAckRuntimeDrainLegacyMatchesOracle(t *testing.T) {
	type run struct {
		code           int
		stdout, stderr string
		calls          []runtime.Call
		log            []string
		pokes          int
	}
	ack := func(t *testing.T, checkRow func(io.Writer) (func() error, error)) run {
		var log []string
		pokes := swapDrainAckSeams(t, &log)
		sp := runtime.NewFake()
		var stdout, stderr bytes.Buffer
		var check func() (func() error, error)
		if checkRow != nil {
			check = func() (func() error, error) { return checkRow(&stderr) }
		}
		code := ackRuntimeDrain(newDrainOps(sp), check, "/city", "worker", "worker", "", false, &stdout, &stderr)
		return run{code, stdout.String(), stderr.String(), sp.Calls, log, *pokes}
	}
	oracle := ack(t, nil)
	if oracle.code != 0 || len(oracle.log) != 1 || oracle.pokes != 1 {
		t.Fatalf("oracle premise: %+v", oracle)
	}

	resolveErr := errors.New(`resolving session "worker": session not found`)
	for _, tc := range []struct {
		name       string
		store      func(*testing.T) (beads.Store, string)
		envToken   string
		resolveErr error
		wantRowAck bool
		wantStderr string
	}{
		{name: "self-ack without a token", envToken: ""},
		{name: "stale self-ack", envToken: "tok-old"},
		{name: "operator ack without --operator (shell, no token)", envToken: ""},
		{name: "target without a session row", resolveErr: resolveErr},
		{name: "store unreachable", resolveErr: errors.New("opening the session store: dial tcp: connection refused")},
		{name: "persistent CAS contention", envToken: "tok-a", store: func(t *testing.T) (beads.Store, string) {
			mem, row := drainAckRowStore(t)
			return &contendedStore{Store: mem}, row.ID
		}, wantStderr: "gc runtime drain-ack: session row ack skipped: conditional write kept losing"},
		{name: "no conditional writer", envToken: "tok-a", store: func(t *testing.T) (beads.Store, string) {
			mem := beads.NewMemStore()
			return mem, createDrainAckRow(t, mem).ID
		}},
		{name: "passing check", envToken: "tok-a", wantRowAck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.store == nil {
				tc.store = func(t *testing.T) (beads.Store, string) {
					mem, row := drainAckRowStore(t)
					return mem, row.ID
				}
			}
			store, id := tc.store(t)
			got := ack(t, func(stderr io.Writer) (func() error, error) {
				if tc.resolveErr != nil {
					return drainAckForMode(false, nil, tc.resolveErr, stderr)
				}
				commit, err := checkDrainAckRow(store, id, false, tc.envToken, drainAckRowNow)
				return drainAckForMode(false, commit, err, stderr)
			})
			want := oracle
			if tc.wantStderr != "" {
				if !strings.HasPrefix(got.stderr, tc.wantStderr) {
					t.Errorf("stderr = %q, want the legacy note %q", got.stderr, tc.wantStderr)
				}
				got.stderr = oracle.stderr
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("legacy drain-ack diverged from the pre-E3 oracle:\n got %+v\nwant %+v", got, want)
			}
			if acked := mustGetBead(t, store, id).Metadata[drainAckIncarnationKey] == "3"; acked != tc.wantRowAck {
				t.Errorf("row ack written = %v, want %v", acked, tc.wantRowAck)
			}
		})
	}
}

// TestDrainAckModeErr pins undrain's row-clear failure by mode: v2 reports
// it, legacy notes it on stderr and keeps legacy's exit status. Kills: a
// legacy undrain that fails on the row, and a v2 undrain that hides it.
func TestDrainAckModeErr(t *testing.T) {
	cause := errors.New("store down")
	var stderr bytes.Buffer
	if err := drainAckModeErr(true, cause, &stderr, "gc runtime undrain: session row ack not cleared"); !errors.Is(err, cause) || stderr.Len() != 0 {
		t.Fatalf("strict = (%v, %q), want the error and no note", err, stderr.String())
	}
	if err := drainAckModeErr(false, cause, &stderr, "gc runtime undrain: session row ack not cleared"); err != nil {
		t.Fatalf("legacy = %v, want nil", err)
	}
	if got, want := stderr.String(), "gc runtime undrain: session row ack not cleared: store down\n"; got != want {
		t.Errorf("legacy note = %q, want %q", got, want)
	}
}

// TestDrainAckStrict pins mode detection: only session_reconciler = "v2" is
// strict. Kills: treating the default, an alias, an unknown value or a config
// that failed to load as v2.
func TestDrainAckStrict(t *testing.T) {
	if drainAckStrict(nil) {
		t.Error("a config that did not load is strict")
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"legacy", false},
		{"auto", false},
		{"require", false},
		{"bogus", false},
		{"v2", true},
		{" V2 ", true},
	} {
		cfg := &config.City{}
		cfg.Daemon.SessionReconciler = tc.value
		if got := drainAckStrict(cfg); got != tc.want {
			t.Errorf("drainAckStrict(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestHookDrainAckResult pins the hook's reading of drain-ack's status: the
// stale-seat drains tolerate a v2 refusal (logged, the drain completes), the
// other drains do not, and no drain tolerates a failed ack. Kills: the
// tolerant path swallowing every failure, and the strict path swallowing a
// refusal.
func TestHookDrainAckResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		tolerant bool
		wantErr  bool
		wantLog  bool
	}{
		{name: "acked", code: 0},
		{name: "refused, stale seat", code: drainAckRefused, tolerant: true, wantLog: true},
		{name: "refused, other drain", code: drainAckRefused, wantErr: true},
		{name: "failed, stale seat", code: 1, tolerant: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := hookDrainAckResult(tc.code, tc.tolerant, &stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if logged := strings.Contains(stderr.String(), "drain-ack refused for this seat"); logged != tc.wantLog {
				t.Errorf("stderr = %q, want refusal logged %v", stderr.String(), tc.wantLog)
			}
		})
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

// TestUndrainRuntimeClearsEnvAndRowAck pins the dual clear: the env keys go
// first, exactly as legacy clears them, then the row ack. A failed row clear
// keeps the env clear and the event and exits 1. Kills: an undrain that leaves
// the row ack behind, and a row clear that gates the legacy clear.
func TestUndrainRuntimeClearsEnvAndRowAck(t *testing.T) {
	store, row := drainAckRowStore(t)
	if err := ackDrainRow(store, row.ID, false, "tok-a"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	for _, tc := range []struct {
		name     string
		clearRow func() error
		wantCode int
	}{
		{name: "row clear fails", clearRow: func() error { return errors.New("store down") }, wantCode: 1},
		{name: "row clear succeeds", clearRow: func() error { return clearDrainAckRow(store, row.ID) }, wantCode: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dops := newFakeDrainOps()
			dops.draining["worker"] = true
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
				t.Fatal(err)
			}
			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			code := undrainRuntime(dops, tc.clearRow, sp, rec, "worker", "worker", false, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if dops.draining["worker"] {
				t.Error("env drain flag still set")
			}
			if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionUndrained {
				t.Errorf("events = %v, want one SessionUndrained", rec.Events)
			}
		})
	}
	assertNoRowAck(t, store, row.ID)
}

// TestDrainAckRowKeysAreInertToLegacy pins rollback safety. Legacy's lifecycle
// projection is identical with and without the row-ack keys, and a legacy
// PreWake, which never clears them, leaves an ack that no longer names the
// row's incarnation. Kills: a key legacy reads, and binding the ack to a
// field PreWake does not move.
func TestDrainAckRowKeysAreInertToLegacy(t *testing.T) {
	store, row := drainAckRowStore(t)
	before := mustGetBead(t, store, row.ID)
	if err := ackDrainRow(store, row.ID, false, "tok-a"); err != nil {
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
	if woken.Metadata[drainAckIncarnationKey] == woken.Metadata["generation"] {
		t.Fatalf("after a legacy PreWake the leftover ack still names generation %q", woken.Metadata["generation"])
	}
}
