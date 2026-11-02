package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func putExecutableOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", name, err)
	}
	t.Setenv("PATH", dir)
}

// fakeAdoptionProvider implements runtime.Provider for adoption barrier tests.
type fakeAdoptionProvider struct {
	runtime.Provider
	running          []string
	alive            map[string]bool
	processNameCalls map[string][]string
	listErr          error
	// tokens simulates each running session's real, already-established
	// GC_INSTANCE_TOKEN (e.g. a runtime that survived a supervisor restart).
	// A name with no entry has no live token, i.e. GetMeta returns "".
	tokens map[string]string
	// stamps records LL5's SetMeta writes by name, then key.
	stamps map[string]map[string]string
}

type adoptionLockProbeStore struct {
	beads.Store

	targetSessionName string
	listed            chan struct{}
	createAttempted   chan struct{}
	allowCreate       <-chan struct{}
}

type adoptionListFailureStore struct {
	beads.Store
}

type adoptionClockAdvanceStore struct {
	beads.Store

	advance  func()
	advanced bool
}

type adoptionSessionNameLookupFailStore struct {
	beads.Store
}

func (s *adoptionLockProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.Store.List(query)
	if query.Label == sessionBeadLabel {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (s *adoptionListFailureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.TrimSpace(query.Metadata["session_name"]) != "" {
		return nil, errors.New("live list failed")
	}
	return s.Store.List(query)
}

func (s *adoptionClockAdvanceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.Store.List(query)
	if strings.TrimSpace(query.Metadata["session_name"]) != "" && !s.advanced {
		s.advanced = true
		s.advance()
	}
	return result, err
}

func (s *adoptionSessionNameLookupFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.TrimSpace(query.Label) == "agent:worker" || strings.TrimSpace(query.Metadata["agent_name"]) == "worker" || strings.TrimSpace(query.Metadata["template"]) == "worker" || strings.TrimSpace(query.Metadata["common_name"]) == "worker" {
		return nil, errors.New("unexpected per-agent session name lookup")
	}
	return s.Store.List(query)
}

func (s *adoptionLockProbeStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == sessionBeadType && b.Metadata["session_name"] == s.targetSessionName {
		select {
		case s.createAttempted <- struct{}{}:
		default:
		}
		<-s.allowCreate
	}
	return s.Store.Create(b)
}

type adoptionBarrierOutcome struct {
	result adoptionResult
	passed bool
}

func (f *fakeAdoptionProvider) ListRunning(_ string) ([]string, error) {
	return f.running, f.listErr
}

func (f *fakeAdoptionProvider) IsRunning(name string) bool {
	for _, running := range f.running {
		if running == name {
			return true
		}
	}
	return false
}

func (f *fakeAdoptionProvider) ProcessAlive(name string, processNames []string) bool {
	if f.processNameCalls == nil {
		f.processNameCalls = make(map[string][]string)
	}
	f.processNameCalls[name] = append([]string(nil), processNames...)
	if f.alive == nil {
		return true
	}
	return f.alive[name]
}

func (f *fakeAdoptionProvider) IsAttached(string) bool { return false }

func (f *fakeAdoptionProvider) GetMeta(name, key string) (string, error) {
	if key != "GC_INSTANCE_TOKEN" {
		return "", nil
	}
	return f.tokens[name], nil
}

func (f *fakeAdoptionProvider) SetMeta(name, key, value string) error {
	if f.stamps == nil {
		f.stamps = make(map[string]map[string]string)
	}
	if f.stamps[name] == nil {
		f.stamps[name] = make(map[string]string)
	}
	f.stamps[name][key] = value
	return nil
}

func (f *fakeAdoptionProvider) GetLastActivity(string) (time.Time, error) { return time.Time{}, nil }

func TestAdoptionBarrier_NoRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: nil}
	cfg := &config.City{}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass with no running sessions")
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0", result.Total)
	}
}

func TestAdoptionBarrier_PartialListUsesVisibleSessionsButFailsBarrier(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-worker"},
		listErr: &runtime.PartialListError{Err: runtime.ErrSessionNotFound},
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Fatal("barrier should fail closed on partial session listing")
	}
	if result.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1 visible session adopted", result.Adopted)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("partially failed")) {
		t.Fatalf("stderr = %q, want partial failure warning", stderr.String())
	}
}

func TestAdoptionBarrier_AdoptsRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clk, &stderr, false)
	if !passed {
		t.Errorf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Adopted != 2 {
		t.Errorf("Adopted = %d, want 2", result.Adopted)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}

	// Verify beads were created with correct labels.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 2 {
		t.Errorf("beads count = %d, want 2", len(beadList))
	}
	// Verify agent: label is present on adopted beads.
	for _, b := range beadList {
		hasAgentLabel := false
		for _, l := range b.Labels {
			if len(l) > len("agent:") && l[:len("agent:")] == "agent:" {
				hasAgentLabel = true
				break
			}
		}
		if !hasAgentLabel {
			t.Errorf("bead %q missing agent: label, labels = %v", b.Title, b.Labels)
		}
		if b.Metadata["continuation_epoch"] != "1" {
			t.Errorf("bead %q continuation_epoch = %q, want 1", b.Title, b.Metadata["continuation_epoch"])
		}
	}
}

// TestAdoptionBarrier_PreservesLiveInstanceToken verifies that adopting a
// running session whose runtime already carries a live GC_INSTANCE_TOKEN
// (the normal shape of an untracked survivor from before a supervisor
// restart) records THAT real token on the adopted bead, instead of
// fabricating an unrelated random one. A fabricated token can never match
// the token the live runtime process actually carries — it was set at
// process-launch time and cannot be changed from outside — so recording
// anything other than the real value permanently poisons the drain-ack
// token fence in session_reconciler.go (ga-lfr06j).
func TestAdoptionBarrier_PreservesLiveInstanceToken(t *testing.T) {
	store := beads.NewMemStore()
	const liveToken = "440f67722bf9e7382ad684e057191659"
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-worker"},
		tokens:  map[string]string{"test-city-worker": liveToken},
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clk, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1", result.Adopted)
	}

	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 1 {
		t.Fatalf("beads count = %d, want 1", len(beadList))
	}
	if got := beadList[0].Metadata["instance_token"]; got != liveToken {
		t.Errorf("instance_token = %q, want live runtime token %q preserved (not fabricated)", got, liveToken)
	}
}

// stampFake is a runtime.Fake whose metadata reads (getErr) and writes
// (setErr) fail per key. lateToken, when set, is the GC_INSTANCE_TOKEN every
// read after the first returns: a token that appears mid-adoption.
type stampFake struct {
	*runtime.Fake
	getErr, setErr map[string]error
	lateToken      string
	tokenReads     int
}

func newStampFake(t *testing.T, name string) *stampFake {
	t.Helper()
	f := &stampFake{Fake: runtime.NewFake(), getErr: map[string]error{}, setErr: map[string]error{}}
	if err := f.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *stampFake) GetMeta(name, key string) (string, error) {
	if err := f.getErr[key]; err != nil {
		return "", err
	}
	if key == "GC_INSTANCE_TOKEN" && f.lateToken != "" {
		if f.tokenReads++; f.tokenReads > 1 {
			return f.lateToken, nil
		}
	}
	return f.Fake.GetMeta(name, key)
}

func (f *stampFake) SetMeta(name, key, value string) error {
	if err := f.setErr[key]; err != nil {
		return err
	}
	return f.Fake.SetMeta(name, key, value)
}

func (*stampFake) SupportsTransport(string) bool { return true }

// tmuxStampFake is a stampFake whose metadata is a session environment, as
// tmux's is: it is a runtime.EnvironmentBatchProvider. A plain stampFake is
// a sidecar provider. Its batched read returns the identity keys through
// GetMeta, so an injected read failure fails it.
type tmuxStampFake struct{ *stampFake }

func (f tmuxStampFake) GetAllEnvironment(name string) (map[string]string, error) {
	env := make(map[string]string, len(identityEnvKeys))
	for _, key := range identityEnvKeys {
		v, err := f.GetMeta(name, key)
		if err != nil {
			return nil, err
		}
		env[key] = v
	}
	return env, nil
}

// meta reads key straight from the fake, past any injected failure.
func (f *stampFake) meta(t *testing.T, name, key string) string {
	t.Helper()
	v, err := f.Fake.GetMeta(name, key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const adoptedName = "test-city-worker"

// adoptOne runs the barrier over sp's one running worker on store and
// returns the open rows and the barrier's stderr.
func adoptOne(t *testing.T, store beads.Store, sp runtime.Provider, dryRun bool) ([]beads.Bead, string) {
	t.Helper()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var stderr bytes.Buffer
	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, dryRun)
	if !passed || result.Adopted != 1 || result.Skipped != 0 {
		t.Fatalf("barrier passed=%v result=%+v, want one adoption; stderr: %s", passed, result, stderr.String())
	}
	rows, _ := store.ListByLabel(sessionBeadLabel, 0)
	return rows, stderr.String()
}

// Kills: a half-identified adopted runtime (v5 O2, LL5). Boot adoption stamps
// the new row's ID and generation-1 epoch on the runtime; it keeps a
// runtime's own token and mints one, on the row and the runtime, only when
// the runtime has none, with BEADS_HOLDER_TOKEN beside it on tmux only. A
// token it cannot read mints nothing and stamps nothing.
func TestAdoptionStampsSessionIDAndToken(t *testing.T) {
	const live = "440f67722bf9e7382ad684e057191659"
	for _, tc := range []struct {
		name, token string
		tokenErr    error
		sidecar     bool
	}{
		{name: "own token", token: live},
		{name: "no token"},
		{name: "no token on a sidecar provider", sidecar: true},
		{name: "unreadable token", tokenErr: errors.New("server busy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := newStampFake(t, adoptedName)
			var provider runtime.Provider = tmuxStampFake{sp}
			if tc.sidecar {
				provider = sp
			}
			if tc.token != "" {
				if err := sp.Fake.SetMeta(adoptedName, "GC_INSTANCE_TOKEN", tc.token); err != nil {
					t.Fatal(err)
				}
			}
			sp.getErr["GC_INSTANCE_TOKEN"] = tc.tokenErr
			rows, stderr := adoptOne(t, beads.NewMemStore(), provider, false)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			row := rows[0]
			token, rtToken := row.Metadata["instance_token"], sp.meta(t, adoptedName, "GC_INSTANCE_TOKEN")
			sid, epoch, holder := sp.meta(t, adoptedName, "GC_SESSION_ID"), sp.meta(t, adoptedName, "GC_RUNTIME_EPOCH"), sp.meta(t, adoptedName, "BEADS_HOLDER_TOKEN")
			if tc.tokenErr != nil {
				if token != "" || rtToken != "" || sid != "" || epoch != "" || !strings.Contains(stderr, "identity not stamped") {
					t.Fatalf("row token %q, runtime %q/%q/%q, stderr %q; want nothing minted or stamped, logged", token, rtToken, sid, epoch, stderr)
				}
				return
			}
			switch {
			case tc.token != "" && (token != live || rtToken != live || holder != ""):
				t.Fatalf("row token %q, runtime token %q, holder %q; want the runtime's own token kept, nothing minted", token, rtToken, holder)
			case tc.token == "" && tc.sidecar && (token == "" || rtToken != token || holder != ""):
				t.Fatalf("row token %q, runtime token %q, holder %q; want one minted token on row and runtime, no holder on a sidecar", token, rtToken, holder)
			case tc.token == "" && !tc.sidecar && (token == "" || rtToken != token || holder != token):
				t.Fatalf("row token %q, runtime token %q, holder %q; want one minted token on all three", token, rtToken, holder)
			}
			if sid != row.ID || epoch != "1" || stderr != "" {
				t.Fatalf("runtime GC_SESSION_ID %q epoch %q (stderr %q), want row %s at epoch 1", sid, epoch, stderr, row.ID)
			}
		})
	}
}

// Kills: re-stamping a runtime that already names a session (LL5, as the
// coordinator ruled on #7223). A runtime adopted while it still carries a
// closed row's GC_SESSION_ID keeps that ID (and gets no epoch), so the
// closed-bead reaper stops it exactly as before LL5; the new row holds the
// runtime's token, own or minted, which v5 O2 reads as Current whatever the
// session ID.
func TestAdoptionKeepsAnotherRowsSessionID(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{name: "own token", token: "440f67722bf9e7382ad684e057191659"},
		{name: "no token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			closed, err := store.Create(beads.Bead{
				Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{"session_name": adoptedName, "agent_name": "worker", "state": "active"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(closed.ID); err != nil {
				t.Fatal(err)
			}
			sp := newStampFake(t, adoptedName)
			for k, v := range map[string]string{"GC_SESSION_ID": closed.ID, "GC_INSTANCE_TOKEN": tc.token} {
				if err := sp.Fake.SetMeta(adoptedName, k, v); err != nil {
					t.Fatal(err)
				}
			}
			rows, stderr := adoptOne(t, store, sp, false)
			if len(rows) != 1 || stderr != "" {
				t.Fatalf("open rows = %d, stderr %q; want the adopted row, cleanly", len(rows), stderr)
			}
			if got, epoch := sp.meta(t, adoptedName, "GC_SESSION_ID"), sp.meta(t, adoptedName, "GC_RUNTIME_EPOCH"); got != closed.ID || epoch != "" {
				t.Fatalf("runtime GC_SESSION_ID %q epoch %q, want the closed row's %s kept and no epoch", got, epoch, closed.ID)
			}
			live := sp.meta(t, adoptedName, "GC_INSTANCE_TOKEN")
			if verdict := classifyRuntimeInstanceToken(live, nil, rows[0].Metadata["instance_token"]); live == "" || verdict != runtimeTokenMatch {
				t.Fatalf("runtime token %q vs row %q: verdict %d, want a non-empty match (Current by token)", live, rows[0].Metadata["instance_token"], verdict)
			}
			if tc.token != "" && live != tc.token {
				t.Fatalf("runtime token = %q, want its own %q kept", live, tc.token)
			}
			if n := reapRuntimesBoundToClosedBeads(store, newSessionBeadSnapshot(rows), nil, sp, nil, t.TempDir(), io.Discard); n != 1 || sp.IsRunning(adoptedName) {
				t.Fatalf("reaped %d, running %v; want the closed-bound runtime reaped as before LL5", n, sp.IsRunning(adoptedName))
			}
		})
	}
}

// Kills: a stamp failure failing the barrier or the row, a session ID
// stamped without its epoch, over an unreadable ID or after a failed token
// write (see TestAdoptionStampedEpochKeepsStaleIncarnationForeign), and a
// minted token written over one that appeared after the first read (LL5).
// Each keeps the row, passes the barrier and logs.
func TestAdoptionStampFailuresKeepRow(t *testing.T) {
	boom := errors.New("tmux gone")
	for _, tc := range []struct {
		name      string
		set       func(*stampFake)
		wantID    bool
		wantToken string // "" none, "row" the row's token, else that value
	}{
		{name: "token stamp fails", set: func(f *stampFake) { f.setErr["GC_INSTANCE_TOKEN"] = boom }},
		{name: "epoch stamp fails", set: func(f *stampFake) { f.setErr["GC_RUNTIME_EPOCH"] = boom }, wantToken: "row"},
		{name: "ID stamp fails", set: func(f *stampFake) { f.setErr["GC_SESSION_ID"] = boom }, wantToken: "row"},
		{name: "ID unreadable", set: func(f *stampFake) { f.getErr["GC_SESSION_ID"] = boom }, wantToken: "row"},
		{name: "token appears mid-adoption", set: func(f *stampFake) { f.lateToken = "late" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := newStampFake(t, adoptedName)
			tc.set(sp)
			rows, stderr := adoptOne(t, beads.NewMemStore(), sp, false)
			if len(rows) != 1 || !strings.Contains(stderr, "identity not stamped") {
				t.Fatalf("rows %d, stderr %q; want the row kept and the failure logged", len(rows), stderr)
			}
			if got := sp.meta(t, adoptedName, "GC_SESSION_ID"); (got == rows[0].ID) != tc.wantID {
				t.Fatalf("runtime GC_SESSION_ID = %q, want stamped=%v", got, tc.wantID)
			}
			want := tc.wantToken
			if want == "row" {
				want = rows[0].Metadata["instance_token"]
			}
			if got := sp.meta(t, adoptedName, "GC_INSTANCE_TOKEN"); got != want {
				t.Fatalf("runtime token = %q, want %q", got, want)
			}
		})
	}
}

// Kills: a dry run that writes to the runtime (LL5): `gc migration plan`
// reports, and writes nothing anywhere.
func TestAdoptionDryRunStampsNothing(t *testing.T) {
	sp := newStampFake(t, adoptedName)
	store := beads.NewMemStore()
	rows, _ := adoptOne(t, store, sp, true)
	if len(rows) != 0 || sp.CountCalls("SetMeta", adoptedName) != 0 {
		t.Fatalf("rows %d, SetMeta calls %d; want a dry run to write nothing", len(rows), sp.CountCalls("SetMeta", adoptedName))
	}
}

// Kills: a session ID stamped without GC_RUNTIME_EPOCH (LL5 review). After
// the adopted row restarts (preWakeCommit rotates its token and bumps its
// generation), legacy's pending-create attribution must read the adopted
// incarnation as foreign: an ID match with a token mismatch and no epoch
// reads as ours, which would commit the new row over a runtime that never
// carries its token (ga-lfr06j).
func TestAdoptionStampedEpochKeepsStaleIncarnationForeign(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, sp *stampFake, store beads.Store) string // returns the row ID
	}{
		{name: "pre-LL5 adoption (token only)", run: func(t *testing.T, sp *stampFake, _ beads.Store) string {
			if err := sp.Fake.SetMeta(adoptedName, "GC_INSTANCE_TOKEN", "adopted-tok"); err != nil {
				t.Fatal(err)
			}
			return "gc-adopted"
		}},
		{name: "epoch stamp fails", run: func(t *testing.T, sp *stampFake, store beads.Store) string {
			sp.setErr["GC_RUNTIME_EPOCH"] = errors.New("tmux gone")
			rows, _ := adoptOne(t, store, sp, false)
			return rows[0].ID
		}},
		{name: "minted token stamp fails", run: func(t *testing.T, sp *stampFake, store beads.Store) string {
			sp.setErr["GC_INSTANCE_TOKEN"] = errors.New("tmux gone")
			rows, _ := adoptOne(t, store, sp, false)
			return rows[0].ID
		}},
		{name: "stamped with epoch", run: func(t *testing.T, sp *stampFake, store beads.Store) string {
			rows, _ := adoptOne(t, store, sp, false)
			return rows[0].ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := newStampFake(t, adoptedName)
			id := tc.run(t, sp, beads.NewMemStore())
			restarted := session.Info{ID: id, InstanceToken: "rotated-token", Generation: "2"}
			if got := readPendingCreateIdentity(restarted, adoptedName, sp.Fake).attribution(); got != pendingCreateRuntimeForeign {
				t.Fatalf("attribution of the adopted incarnation after a restart = %d, want foreign", got)
			}
		})
	}
}

// TestAdoptionBarrier_AdoptedRuntimeCanLaterBeDrained is the end-to-end
// regression for ga-lfr06j: a runtime adopted at supervisor restart must
// actually be stoppable by a later drain-ack, not skipped forever as
// "session was replaced". It reproduces the incident shape (an untracked
// runtime that survived restart with its own pre-existing instance token)
// using the richer runtime.Fake, since the drain-ack half needs a real
// Stop/IsRunning/GetMeta implementation that fakeAdoptionProvider does not
// provide.
func TestAdoptionBarrier_AdoptedRuntimeCanLaterBeDrained(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	ctx := context.Background()
	if err := sp.Start(ctx, "test-city-worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The runtime survived a supervisor restart untracked: it already
	// carries its own real instance token from whenever it was originally
	// launched, which cannot be changed retroactively from outside it.
	const preRestartToken = "440f67722bf9e7382ad684e057191659"
	if err := sp.SetMeta("test-city-worker", "GC_INSTANCE_TOKEN", preRestartToken); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var barrierStderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	// Supervisor restart: adoption barrier discovers and adopts the
	// untracked survivor.
	result, passed := runAdoptionBarrier(city, sessionFrontDoor(store), sp, cfg, "test-city", clk, &barrierStderr, false)
	if !passed || result.Adopted != 1 {
		t.Fatalf("adoption failed: passed=%v adopted=%d stderr=%s", passed, result.Adopted, barrierStderr.String())
	}

	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 1 {
		t.Fatalf("beads count = %d, want 1", len(beadList))
	}
	adoptedToken := beadList[0].Metadata["instance_token"]

	// Reconciler later decides to drain the adopted bead. This must
	// actually stop the still-running pre-restart runtime.
	tracker := &asyncStartTracker{}
	var drainStderr synchronizedBuffer
	queueDrainAckAsyncStop(city, store, sp, &config.City{}, drainAckStopPendingForTest(t, store, beadList[0].ID, "test-city-worker", adoptedToken), "test-city-worker", nil, tracker, nil, &drainStderr)
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop did not complete")
	}

	if sp.IsRunning("test-city-worker") {
		t.Fatal("adopted runtime was never stopped — drain-ack skipped it forever (the ga-lfr06j leak)")
	}
	if got := drainStderr.String(); strings.Contains(got, "instance token mismatch") {
		t.Fatalf("drain stderr = %q, unexpected token mismatch for a correctly-captured adopted token", got)
	}
}

// TestAdoptionBarrier_TokenlessAdoptedRuntimeCanLaterBeDrained is the
// token-less counterpart to TestAdoptionBarrier_AdoptedRuntimeCanLaterBeDrained
// above (round-2 exit contract on ga-lfr06j / ga-3kfb6y): a runtime adopted
// with NO live GC_INSTANCE_TOKEN must still be actually stoppable by a later
// drain-ack, not skipped forever as a mismatch. Adoption mints a token and
// stamps it on the runtime too (LL5, see TestAdoptionStampsSessionIDAndToken),
// so the drain-ack fence reads a match rather than a token it can never see.
func TestAdoptionBarrier_TokenlessAdoptedRuntimeCanLaterBeDrained(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	ctx := context.Background()
	if err := sp.Start(ctx, "test-city-worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Deliberately no SetMeta(GC_INSTANCE_TOKEN, ...): the runtime survived a
	// supervisor restart untracked and carries no instance token at all,
	// mirroring a pre-instance-token-era survivor.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var barrierStderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	// Supervisor restart: adoption barrier discovers and adopts the
	// untracked, token-less survivor.
	result, passed := runAdoptionBarrier(city, sessionFrontDoor(store), sp, cfg, "test-city", clk, &barrierStderr, false)
	if !passed || result.Adopted != 1 {
		t.Fatalf("adoption failed: passed=%v adopted=%d stderr=%s", passed, result.Adopted, barrierStderr.String())
	}

	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 1 {
		t.Fatalf("beads count = %d, want 1", len(beadList))
	}
	adoptedToken := beadList[0].Metadata["instance_token"]
	if live, _ := sp.GetMeta("test-city-worker", "GC_INSTANCE_TOKEN"); adoptedToken == "" || live != adoptedToken {
		t.Fatalf("row token %q, runtime token %q, want one minted token on both", adoptedToken, live)
	}

	// Reconciler later decides to drain the adopted bead. This must actually
	// stop the still-running token-less runtime, not skip it forever as an
	// unverifiable mismatch.
	tracker := &asyncStartTracker{}
	var drainStderr synchronizedBuffer
	queueDrainAckAsyncStop(city, store, sp, &config.City{}, drainAckStopPendingForTest(t, store, beadList[0].ID, "test-city-worker", adoptedToken), "test-city-worker", nil, tracker, nil, &drainStderr)
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop did not complete")
	}

	if sp.IsRunning("test-city-worker") {
		t.Fatal("token-less adopted runtime was never stopped — drain-ack skipped it forever (round-2 gap on ga-lfr06j)")
	}
	if got := drainStderr.String(); strings.Contains(got, "instance token mismatch") {
		t.Fatalf("drain stderr = %q, unexpected token mismatch for a token-less adoptee whose minted token was stamped", got)
	}
}

func TestAdoptionBarrier_SkipsExistingBead(t *testing.T) {
	store := beads.NewMemStore()
	// Pre-create a bead for mayor.
	_, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-city-mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	if result.AlreadyHadBead != 1 {
		t.Errorf("AlreadyHadBead = %d, want 1", result.AlreadyHadBead)
	}
}

func TestAdoptionBarrier_ClosedBeadDoesNotBlock(t *testing.T) {
	store := beads.NewMemStore()
	// Pre-create and close a bead for mayor.
	b, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-city-mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1 (closed bead should not prevent adoption)", result.Adopted)
	}
}

func TestAdoptionBarrier_Rerunnable(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	// First run: adopts.
	r1, _ := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if r1.Adopted != 1 {
		t.Fatalf("first run Adopted = %d, want 1", r1.Adopted)
	}

	// Second run: dedup prevents duplicates.
	r2, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("second run: barrier should pass")
	}
	if r2.Adopted != 0 {
		t.Errorf("second run Adopted = %d, want 0", r2.Adopted)
	}
	if r2.AlreadyHadBead != 1 {
		t.Errorf("second run AlreadyHadBead = %d, want 1", r2.AlreadyHadBead)
	}
}

func TestAdoptionBarrier_IgnoresNonRepairableSessionBeadsInConfigSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	sessionName := agent.SessionNameFor("test-city", "worker", cfg.Workspace.SessionTemplate)
	if _, err := store.Create(beads.Bead{
		Title:  "stale malformed worker",
		Type:   "task",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "worker",
			"session_name": "stale-worker",
			"template":     "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	sp := &fakeAdoptionProvider{running: []string{sessionName}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, result=%+v stderr=%q", result, stderr.String())
	}
	beadList, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	for _, b := range beadList {
		if b.Type != sessionBeadType {
			continue
		}
		if got := b.Metadata["agent_name"]; got != "worker" {
			t.Fatalf("adopted bead agent_name = %q, want configured agent name", got)
		}
		if got := b.Metadata["session_name"]; got != sessionName {
			t.Fatalf("adopted bead session_name = %q, want %q", got, sessionName)
		}
		return
	}
	t.Fatalf("adopted session bead not found; beads=%+v", beadList)
}

func TestAdoptionBarrier_SerializesCreateWithSessionIdentifierLock(t *testing.T) {
	const agentName = "worker-3"
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}}}
	sessionName := agent.SessionNameFor("test-city", "worker", cfg.Workspace.SessionTemplate) + "-3"
	cityPath := t.TempDir()
	baseStore := beads.NewMemStore()
	allowCreate := make(chan struct{})
	var releaseCreate sync.Once
	t.Cleanup(func() {
		releaseCreate.Do(func() {
			close(allowCreate)
		})
	})
	store := &adoptionLockProbeStore{
		Store:             baseStore,
		targetSessionName: sessionName,
		listed:            make(chan struct{}, 1),
		createAttempted:   make(chan struct{}, 1),
		allowCreate:       allowCreate,
	}
	sp := &fakeAdoptionProvider{running: []string{sessionName}}
	var stderr bytes.Buffer
	done := make(chan adoptionBarrierOutcome, 1)

	err := session.WithCitySessionAliasLock(cityPath, agentName, func() error {
		go func() {
			result, passed := runAdoptionBarrier(cityPath, sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
			done <- adoptionBarrierOutcome{result: result, passed: passed}
		}()

		select {
		case <-store.listed:
		case <-time.After(time.Second):
			t.Fatal("adoption barrier did not list existing session beads")
		}

		_, createErr := baseStore.Create(beads.Bead{
			Title:  agentName,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:" + agentName},
			Metadata: map[string]string{
				"agent_name":   agentName,
				"session_name": sessionName,
				"state":        "active",
			},
		})
		return createErr
	})
	if err != nil {
		t.Fatalf("holding session identifier lock: %v", err)
	}
	releaseCreate.Do(func() {
		close(allowCreate)
	})

	var outcome adoptionBarrierOutcome
	select {
	case outcome = <-done:
	case <-time.After(time.Second):
		t.Fatal("adoption barrier did not finish after session_name lock released")
	}
	if !outcome.passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	if outcome.result.Adopted != 0 {
		t.Fatalf("Adopted = %d, want 0 after locked peer created the bead", outcome.result.Adopted)
	}
	if outcome.result.AlreadyHadBead != 1 {
		t.Fatalf("AlreadyHadBead = %d, want 1", outcome.result.AlreadyHadBead)
	}
	select {
	case <-store.createAttempted:
		t.Fatalf("adoption barrier attempted a duplicate create; outcome=%+v stderr=%q", outcome, stderr.String())
	default:
	}

	beadList, err := baseStore.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	if len(beadList) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(beadList))
	}
	if got := beadList[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("session_name = %q, want %q", got, sessionName)
	}
}

func TestAdoptionBarrier_ReportsInLockListFailuresAsChecks(t *testing.T) {
	store := &adoptionListFailureStore{Store: beads.NewMemStore()}
	sp := &fakeAdoptionProvider{running: []string{"test-city-worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Fatal("barrier should fail when the in-lock bead check fails")
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	log := stderr.String()
	if !strings.Contains(log, `listing session beads for "test-city-worker"`) {
		t.Fatalf("stderr %q does not mention the failing session-bead check", log)
	}
	if strings.Contains(log, "creating bead for") {
		t.Fatalf("stderr %q should not report a list failure as a create failure", log)
	}
}

func TestAdoptionBarrier_StampsSyncedAtAtCreateTime(t *testing.T) {
	fakeClock := &clock.Fake{Time: time.Date(2026, 5, 15, 8, 0, 0, 0, time.UTC)}
	store := &adoptionClockAdvanceStore{
		Store: beads.NewMemStore(),
		advance: func() {
			fakeClock.Advance(time.Hour)
		},
	}
	sp := &fakeAdoptionProvider{running: []string{"test-city-worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", fakeClock, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, result=%+v stderr=%q", result, stderr.String())
	}
	beadList, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	if len(beadList) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(beadList))
	}
	if got, want := beadList[0].Metadata["synced_at"], "2026-05-15T09:00:00Z"; got != want {
		t.Fatalf("synced_at = %q, want %q", got, want)
	}
}

func TestAdoptionBarrier_DryRun(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, true)
	if !passed {
		t.Error("dry run barrier should pass")
	}
	if result.Adopted != 2 {
		t.Errorf("Adopted = %d, want 2", result.Adopted)
	}

	// Verify no beads were actually created.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 0 {
		t.Errorf("dry run created %d beads, want 0", len(beadList))
	}
}

func TestAdoptionBarrier_SkipsDeadSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-mayor", "test-city-worker"},
		alive: map[string]bool{
			"test-city-mayor":  true,
			"test-city-worker": false,
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1), ProcessNames: []string{"agent-cli"}},
			{Name: "worker", ProcessNames: []string{"agent-cli"}},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1 live session", result.Total)
	}
	if result.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1", result.Adopted)
	}
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 1 {
		t.Fatalf("beads count = %d, want 1", len(beadList))
	}
	if beadList[0].Metadata["session_name"] != "test-city-mayor" {
		t.Fatalf("adopted bead = %q, want live mayor", beadList[0].Metadata["session_name"])
	}
}

func TestAdoptionBarrier_UsesResolvedProviderProcessNames(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-worker"},
		alive: map[string]bool{
			"test-city-worker": true,
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{
			Provider:        "custom-provider",
			SessionTemplate: "{{.City}}-{{.Agent}}",
		},
		Providers: map[string]config.ProviderSpec{
			"custom-provider": {ProcessNames: []string{"custom-agent", "node"}},
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	var stderr bytes.Buffer

	_, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	got := sp.processNameCalls["test-city-worker"]
	if strings.Join(got, ",") != "custom-agent,node" {
		t.Fatalf("process names = %v, want [custom-agent node]", got)
	}
}

func TestAdoptionBarrier_UsesProviderlessDetectedProcessNames(t *testing.T) {
	putExecutableOnPath(t, "codex")
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-worker"},
		alive: map[string]bool{
			"test-city-worker": true,
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{
			Provider:        "codex",
			SessionTemplate: "{{.City}}-{{.Agent}}",
		},
		Providers: map[string]config.ProviderSpec{
			"codex": config.BuiltinProviderAlias("codex"),
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	var stderr bytes.Buffer

	_, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	got := sp.processNameCalls["test-city-worker"]
	if strings.Join(got, ",") != "codex,codex-raw" {
		t.Fatalf("process names = %v, want [codex codex-raw]", got)
	}
}

func TestAdoptionBarrier_NilStore(t *testing.T) {
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{}
	var stderr bytes.Buffer

	_, passed := runAdoptionBarrier("", nil, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Error("nil store: barrier should not pass")
	}
}

func TestAdoptionBarrier_PoolSlotDetection(t *testing.T) {
	store := beads.NewMemStore()
	// Pool instance session name: base "worker" produces session "worker",
	// so instance "worker-3" has session name "worker-3".
	sp := &fakeAdoptionProvider{running: []string{"worker-3"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, true)
	// Pool instance "worker-3" should resolve to config agent "worker"
	// via resolvePoolBase, with pool slot 3. AgentName should be the
	// expanded instance name "worker-3" (matching syncSessionBeads).
	found := false
	for _, d := range result.Details {
		if d.SessionName == "worker-3" && d.PoolSlot == 3 && d.AgentName == "worker-3" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected detail with PoolSlot=3, AgentName=worker-3 for worker-3, got %+v", result.Details)
	}
}

func TestAdoptionBarrier_PoolSlotResolutionUsesLoadedSnapshot(t *testing.T) {
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"agent_name":   "worker",
			"session_name": "custom-worker",
			"state":        "awake",
		},
	}); err != nil {
		t.Fatal(err)
	}
	store := &adoptionSessionNameLookupFailStore{Store: backing}
	sp := &fakeAdoptionProvider{running: []string{"custom-worker-3"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, true)

	found := false
	for _, d := range result.Details {
		if d.SessionName == "custom-worker-3" && d.PoolSlot == 3 && d.AgentName == "worker-3" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected snapshot-backed pool detail for custom-worker-3, got %+v; stderr=%q", result.Details, stderr.String())
	}
}

func TestAdoptionBarrier_PoolOutOfBounds(t *testing.T) {
	store := beads.NewMemStore()
	// Pool instance exceeding max (5).
	sp := &fakeAdoptionProvider{running: []string{"worker-7"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, true)
	found := false
	for _, d := range result.Details {
		if d.SessionName == "worker-7" && d.PoolSlot == 7 && d.OutOfBounds {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected out-of-bounds detail for worker-7, got %+v", result.Details)
	}
}

func TestParsePoolSlot(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"s-worker-3", 3},
		{"s-worker-10", 10},
		{"s-mayor", 0},
		{"worker", 0},
	}
	for _, tt := range tests {
		got := parsePoolSlot(tt.name)
		if got != tt.want {
			t.Errorf("parsePoolSlot(%q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestAdoptionBarrier_SingletonWithNumericSuffix(t *testing.T) {
	store := beads.NewMemStore()
	// Singleton agent named "db-node-1" — should NOT get pool_slot metadata.
	sp := &fakeAdoptionProvider{running: []string{"db-node-1"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "db-node-1", MaxActiveSessions: intPtr(1)}, // singleton agent
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Errorf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	// Verify no pool_slot on the bead.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	for _, b := range beadList {
		if b.Metadata["pool_slot"] != "" {
			t.Errorf("singleton agent should not have pool_slot, got %q", b.Metadata["pool_slot"])
		}
		// A2 canonical record (S19 Stage 2, write-only): a config-resolved
		// singleton gets a canonical name and NO canonical_pool_slot.
		if got := b.Metadata[session.CanonicalInstanceNameMetadata]; got != "db-node-1" {
			t.Errorf("singleton canonical_instance_name = %q, want db-node-1", got)
		}
		if got := b.Metadata[session.CanonicalPoolSlotMetadata]; got != "" {
			t.Errorf("singleton canonical_pool_slot = %q, want empty", got)
		}
	}
}

func TestAdoptionBarrier_StaleDashNSingletonAdoptsCanonicalIdentity(t *testing.T) {
	store := beads.NewMemStore()
	// "refinery-1" looks like a pool instance but the base "refinery" agent
	// has max_active_sessions=1, so it should be treated as stale singleton
	// state rather than a live pool slot.
	sp := &fakeAdoptionProvider{running: []string{"refinery-1"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "refinery", MaxActiveSessions: intPtr(1), ScaleCheck: "printf 1"},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("adopting stale singleton suffix session refinery-1")) {
		t.Errorf("stderr missing stale singleton adoption warning; got: %s", stderr.String())
	}
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	for _, b := range beadList {
		if b.Metadata["agent_name"] != "refinery" {
			t.Errorf("stale singleton agent_name = %q, want canonical refinery", b.Metadata["agent_name"])
		}
		if !containsString(b.Labels, "agent:refinery") {
			t.Errorf("stale singleton labels = %v, want canonical agent label", b.Labels)
		}
		if containsString(b.Labels, "agent:refinery-1") {
			t.Errorf("stale singleton labels = %v, must not include phantom pool identity", b.Labels)
		}
		if b.Metadata["pool_slot"] != "" {
			t.Errorf("stale singleton session should not have pool_slot metadata, got %q", b.Metadata["pool_slot"])
		}
		// A2 canonical record (S19 Stage 2, write-only): the stale-dash-N
		// singleton is stamped with the CANONICAL base name and NO slot — never
		// the phantom refinery-1 pool identity (S2-3 honesty).
		if got := b.Metadata[session.CanonicalInstanceNameMetadata]; got != "refinery" {
			t.Errorf("stale singleton canonical_instance_name = %q, want refinery", got)
		}
		if got := b.Metadata[session.CanonicalPoolSlotMetadata]; got != "" {
			t.Errorf("stale singleton canonical_pool_slot = %q, want empty", got)
		}
	}
}

func TestAdoptionBarrier_UnknownSession(t *testing.T) {
	store := beads.NewMemStore()
	// Running session that doesn't match any config agent.
	sp := &fakeAdoptionProvider{running: []string{"unknown-session"}}
	cfg := &config.City{} // no agents configured
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass (adopt permissively)")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
}

func TestProcessHintsUsesResolvedProviderProcessNames(t *testing.T) {
	putExecutableOnPath(t, "codex")

	cfg := &config.City{
		Workspace: config.Workspace{Provider: "codex"},
		Providers: map[string]config.ProviderSpec{
			"codex": config.BuiltinProviderAlias("codex"),
		},
	}

	if got := processHints(cfg, &config.Agent{Name: "worker"}); strings.Join(got, ",") != "codex,codex-raw" {
		t.Fatalf("processHints() = %v, want [codex codex-raw]", got)
	}
}

func TestProcessHintsUsesExplicitAgentProcessNames(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Provider: "codex"}}
	agent := &config.Agent{Name: "worker", ProcessNames: []string{"worker-cli"}}

	got := processHints(cfg, agent)
	if len(got) != 1 || got[0] != "worker-cli" {
		t.Fatalf("processHints() = %v, want [worker-cli]", got)
	}
	got[0] = "mutated"
	if agent.ProcessNames[0] != "worker-cli" {
		t.Fatalf("processHints() returned agent slice without cloning")
	}
}

// TestAdoptionBarrier_StampsCanonicalIdentity proves the A2 canonical stamp
// (S19 Stage 2, write-only): a config-resolved pool instance gets a canonical
// record (name + slot), while an orphan session (ends in -N, matches no agent)
// gets NO canonical record — a wrong authoritative identity is worse than an
// absent one (S2-3).
func TestAdoptionBarrier_StampsCanonicalIdentity(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"worker-3", "orphan-9"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)}

	_, passed := runAdoptionBarrier("", sessionFrontDoor(store), sp, cfg, "test-city", clk, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}

	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	byAgent := map[string]beads.Bead{}
	for _, b := range beadList {
		byAgent[b.Metadata["agent_name"]] = b
	}

	pool, ok := byAgent["worker-3"]
	if !ok {
		t.Fatalf("no adopted bead for pool instance worker-3; beads=%v", byAgent)
	}
	if got := pool.Metadata[session.CanonicalInstanceNameMetadata]; got != "worker-3" {
		t.Errorf("pool canonical_instance_name = %q, want worker-3", got)
	}
	if got := pool.Metadata[session.CanonicalPoolSlotMetadata]; got != "3" {
		t.Errorf("pool canonical_pool_slot = %q, want 3", got)
	}

	orphan, ok := byAgent["orphan-9"]
	if !ok {
		t.Fatalf("no adopted bead for orphan-9; beads=%v", byAgent)
	}
	if got := orphan.Metadata[session.CanonicalInstanceNameMetadata]; got != "" {
		t.Errorf("orphan canonical_instance_name = %q, want empty (no canonical record for orphan)", got)
	}
	if got := orphan.Metadata[session.CanonicalPoolSlotMetadata]; got != "" {
		t.Errorf("orphan canonical_pool_slot = %q, want empty", got)
	}
}
