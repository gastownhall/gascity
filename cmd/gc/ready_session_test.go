package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// requireReadySessions skips where the platform has no abstract unix sockets:
// there `gc hook` runs every work query without a session, which the
// unsupported-platform test pins instead.
func requireReadySessions(t *testing.T) {
	t.Helper()
	if !readySessionsSupported() {
		t.Skipf("ready sessions need abstract unix sockets, which %s does not have", runtime.GOOS)
	}
}

// startTestReadySession starts a session for the ambient city and points this
// process's `gc ready` at it, the way the wrapper points a work query's.
func startTestReadySession(t *testing.T) *readySession {
	t.Helper()
	requireReadySessions(t)
	s, err := startReadySession(io.Discard)
	if err != nil {
		t.Fatalf("startReadySession: %v", err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Errorf("closing the ready session: %v", err)
		}
	})
	for _, kv := range s.env() {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}
	return s
}

// countReadyRigOpens swaps the rig-leg opener for one that counts opens per
// rig, optionally wrapping each opened store.
func countReadyRigOpens(t *testing.T, wrap func(rig string, store beads.Store) beads.Store) func(rig string) int {
	t.Helper()
	var mu sync.Mutex
	opens := map[string]int{}
	prev := readyRigStoreOpener
	readyRigStoreOpener = func(cfg *config.City) rigStoreOpener {
		inner := prev(cfg)
		return func(rigPath, cityPath string) (beads.Store, error) {
			rig := rigPathBase(rigPath)
			mu.Lock()
			opens[rig]++
			mu.Unlock()
			store, err := inner(rigPath, cityPath)
			if err != nil || wrap == nil {
				return store, err
			}
			return wrap(rig, store), nil
		}
	}
	t.Cleanup(func() { readyRigStoreOpener = prev })
	return func(rig string) int {
		mu.Lock()
		defer mu.Unlock()
		return opens[rig]
	}
}

func rigPathBase(rigPath string) string {
	rigPath = strings.TrimRight(rigPath, "/")
	if i := strings.LastIndex(rigPath, "/"); i >= 0 {
		return rigPath[i+1:]
	}
	return rigPath
}

// readyAnswer is what a work query reads off `gc ready`: the exit code, the
// array on stdout, and the "gc ready: ..." line a failure prints. Other
// stderr — config-load warnings — is not part of the answer: in-process each
// call prints its own, and under a session the session prints them once.
type readyAnswer struct {
	code   int
	stdout string
	errors string
	stderr string
}

func askReady(opts readyOpts) readyAnswer {
	var stdout, stderr bytes.Buffer
	code := cmdReady(opts, &stdout, &stderr)
	var errLines []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "gc ready: ") {
			errLines = append(errLines, line)
		}
	}
	return readyAnswer{code: code, stdout: stdout.String(), errors: strings.Join(errLines, "\n"), stderr: stderr.String()}
}

// claimQuestions are the questions a fresh seat's generated work query asks
// `gc ready`, in order, for a seat with two identities and routed work waiting:
// in-progress work for each identity, assigned work for each identity, then the
// routed pool tier.
func claimQuestions() []readyOpts {
	poolTier := readyOpts{
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker"},
		unassigned:     true,
		excludeTypes:   []string{"epic"},
		excludeLabels:  []string{"hold:mayor", "hold:external", "human"},
		limit:          20,
	}
	return []readyOpts{
		{status: readyStatusInProgress, assignee: "gc-session-1", limit: 1},
		{status: readyStatusInProgress, assignee: "worker-1", limit: 1},
		{assignee: "gc-session-1", limit: 1},
		{assignee: "worker-1", limit: 1},
		poolTier,
	}
}

// seedReadySessionCity puts routed, assigned and blocked work in the city
// store and in every rig of the fixture city, so every arm has rows to read.
func seedReadySessionCity(t *testing.T, cityDir string) {
	t.Helper()
	scopes := []string{cityDir}
	for _, name := range readyCountedRigNames {
		scopes = append(scopes, cityDir+"/rigs/"+name)
	}
	for _, scope := range scopes {
		store, err := openStoreAtForCity(scope, cityDir)
		if err != nil {
			t.Fatalf("open store at %s: %v", scope, err)
		}
		mustCreateReadyBead(t, store, beads.Bead{Title: "routed", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		blocker := mustCreateReadyBead(t, store, beads.Bead{Title: "blocker", Type: "task"})
		claimed := mustCreateReadyBead(t, store, beads.Bead{Title: "claimed", Type: "task"})
		inProgress, owner := readyStatusInProgress, "worker-1"
		if err := store.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &owner}); err != nil {
			t.Fatalf("claim %s: %v", claimed.ID, err)
		}
		if err := store.DepAdd(claimed.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("block %s on %s: %v", claimed.ID, blocker.ID, err)
		}
	}
}

// TestReadySessionAnswersExactlyAsTheInProcessReader is the equivalence the
// whole mechanism rests on: for every arm a work query asks — the claim's
// tiers, the --status arms the workflows pack inventories with, bounds and
// orders, and a malformed question — the session's answer is the in-process
// reader's, byte for byte, with the same exit code and the same error line.
func TestReadySessionAnswersExactlyAsTheInProcessReader(t *testing.T) {
	requireReadySessions(t)
	cityDir := newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	seedReadySessionCity(t, cityDir)

	questions := append(claimQuestions(),
		readyOpts{},
		readyOpts{status: readyStatusInProgress},
		readyOpts{status: readyStatusOpen},
		readyOpts{status: "blocked"},
		readyOpts{sortOrder: readySortNewest, limit: 2},
		readyOpts{status: "not-a-status"},
	)
	want := make([]readyAnswer, len(questions))
	for i, q := range questions {
		want[i] = askReady(q)
	}
	if want[0].code != 0 || want[len(want)-1].code == 0 {
		t.Fatalf("fixture: in-process answers %+v; want the first to succeed and the malformed one to fail", want)
	}

	startTestReadySession(t)
	for i, q := range questions {
		got := askReady(q)
		if got.code != want[i].code || got.stdout != want[i].stdout || got.errors != want[i].errors {
			t.Fatalf("question %d (%+v) through the session = %+v, want the in-process answer %+v", i, q, got, want[i])
		}
	}
}

// TestReadySessionOpensTheStoresOnceForAClaim is the claim path's call count.
// A fresh seat's work query asks five questions; each used to be a separate
// process that opened every serving rig store again. Through one session each
// rig store is opened once for the whole claim.
func TestReadySessionOpensTheStoresOnceForAClaim(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	opens := countReadyRigOpens(t, nil)
	questions := claimQuestions()

	for _, q := range questions {
		if a := askReady(q); a.code != 0 {
			t.Fatalf("in-process %+v = %+v", q, a)
		}
	}
	for _, rig := range readyCountedRigNames {
		if got := opens(rig); got != len(questions) {
			t.Fatalf("without a session, rig %q opened %d times over %d questions, want once per question", rig, got, len(questions))
		}
	}

	startTestReadySession(t)
	for _, q := range questions {
		if a := askReady(q); a.code != 0 {
			t.Fatalf("through the session %+v = %+v", q, a)
		}
	}
	for _, rig := range readyCountedRigNames {
		if got := opens(rig) - len(questions); got != 1 {
			t.Fatalf("through one session, rig %q opened %d times over a claim's %d questions, want exactly once", rig, got, len(questions))
		}
	}
}

// readyFailOnceStore fails its next Ready read while fail is set, the way a
// leg hits a transient lock or a dropped connection, and counts its closes.
type readyFailOnceStore struct {
	beads.Store
	fail   *atomic.Bool
	closes *atomic.Int32
}

func (s readyFailOnceStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("database is locked")
	}
	return s.Store.Ready(query...)
}

// CloseStore counts the close and closes the store it wraps.
func (s readyFailOnceStore) CloseStore() error {
	s.closes.Add(1)
	return closeBeadStoreHandle(s.Store)
}

// TestReadySessionReopensAfterAFailedRead: a failed read is answered exactly
// like an in-process failure, and the session then drops its legs, closing the
// stores it opened them with, so the next question opens fresh stores — what
// the next `gc ready` process would have done — instead of reading a leg that
// just failed. The fresh stores are closed with the session.
func TestReadySessionReopensAfterAFailedRead(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	var fail atomic.Bool
	var closes atomic.Int32
	opens := countReadyRigOpens(t, func(rig string, store beads.Store) beads.Store {
		if rig == readyCountedRigNames[1] {
			return readyFailOnceStore{Store: store, fail: &fail, closes: &closes}
		}
		return store
	})
	// Registered before the session's own cleanup, so it runs once the session
	// has closed.
	t.Cleanup(func() {
		if got := closes.Load(); got != 2 {
			t.Errorf("the failed rig's stores were closed %d times by the time the session closed, want twice: the failed generation's and the fresh one's", got)
		}
	})
	startTestReadySession(t)

	fail.Store(true)
	failed := askReady(readyOpts{})
	if failed.code == 0 || failed.stdout != "" || !strings.Contains(failed.stderr, "gc ready: rig "+readyCountedRigNames[1]+" store: database is locked") {
		t.Fatalf("a failed leg read through the session = %+v, want the in-process failure: exit 1, no stdout, the leg named on stderr", failed)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("the failed rig's store was closed %d times after the failed read, want once: dropping the legs closes the stores they were opened with", got)
	}
	if ok := askReady(readyOpts{}); ok.code != 0 {
		t.Fatalf("the question after the failure = %+v, want a fresh successful read", ok)
	}
	if got := opens(readyCountedRigNames[1]); got != 2 {
		t.Fatalf("the failed rig was opened %d times, want twice: once for the failed read and once fresh after it", got)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("the failed rig's stores were closed %d times after a successful read, want still once: the fresh stores serve the questions after it", got)
	}
}

// refusedReadyCityWithRig writes a city whose [storage] names a binding it has
// not migrated onto, which this build refuses to serve, with one openable rig,
// and returns that rig's name.
func refusedReadyCityWithRig(t *testing.T) string {
	t.Helper()
	const rig = "alpha"
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	rigDir := filepath.Join(cityPath, "rigs", rig)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir %s: %v", rigDir, err)
	}
	cityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cityToml = append(cityToml, "\n[[rigs]]\nname = "+strconv.Quote(rig)+"\npath = "+strconv.Quote(rigDir)+"\n"...)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), cityToml, 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	for _, scope := range []string{cityPath, rigDir} {
		if err := ensurePersistedScopeLocalFileStore(scope); err != nil {
			t.Fatalf("ensuring file store at %s: %v", scope, err)
		}
	}
	mustCreateInfraBead(t, stubInfraMigrationSource(t), beads.Bead{Title: "a session", Type: "session"})
	captureCLIStorageStderr(t)
	return rig
}

// TestReadySessionClosesTheStoresOfAFailedOpen: a question whose leg set cannot
// be opened — a dead rig, a city whose storage this build refuses — is answered
// with the failure, and the stores that did open are closed with it rather than
// held for the session's lifetime: no leg set holds them, and the next question
// opens its own.
func TestReadySessionClosesTheStoresOfAFailedOpen(t *testing.T) {
	requireReadySessions(t)
	for _, tc := range []struct {
		name string
		// city writes the city and returns the name of a rig that opens.
		city    func(t *testing.T) string
		failure string
	}{
		{
			name:    "dead rig",
			city:    func(t *testing.T) string { newReadyCityWithBrokenRig(t); return "good" },
			failure: "gc ready: rig " + strconv.Quote(readyBrokenRigName) + " store:",
		},
		{name: "refused city", city: refusedReadyCityWithRig, failure: storageMigrationCommand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			healthyRig := tc.city(t)
			var never atomic.Bool
			var closes atomic.Int32
			opens := countReadyRigOpens(t, func(rig string, store beads.Store) beads.Store {
				if rig != healthyRig {
					return store
				}
				return readyFailOnceStore{Store: store, fail: &never, closes: &closes}
			})
			startTestReadySession(t)

			for asked := int32(1); asked <= 2; asked++ {
				failed := askReady(readyOpts{})
				if failed.code == 0 || !strings.Contains(failed.stderr, tc.failure) {
					t.Fatalf("question %d through the session = %+v, want the in-process failure, naming %q", asked, failed, tc.failure)
				}
				if got := opens(healthyRig); got != int(asked) {
					t.Fatalf("fixture: rig %q was opened %d times over %d questions, want once per question", healthyRig, got, asked)
				}
				if got := closes.Load(); got != asked {
					t.Fatalf("after %d failed opens rig %q was closed %d times, want %d: a failed open hands back none of the stores it opened", asked, healthyRig, got, asked)
				}
			}
		})
	}
}

// TestReadySessionClosesARigTheLegsLeaveOut: dropping a leg set closes what
// opening it opened, which is not the same set as the legs' stores. A rig keyed
// under the city's name is opened with the serving frame but left out of the
// legs (rigLegsExcludingCityName), so no leg holds it; it is closed with them
// all the same.
func TestReadySessionClosesARigTheLegsLeaveOut(t *testing.T) {
	requireReadySessions(t)
	const cityNamedRig = "readycounted"
	newReadyCityWithRigs(t, readyCountedRigNames[0], cityNamedRig)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	var never atomic.Bool
	var closes atomic.Int32
	opens := countReadyRigOpens(t, func(rig string, store beads.Store) beads.Store {
		if rig == cityNamedRig {
			return readyFailOnceStore{Store: store, fail: &never, closes: &closes}
		}
		return store
	})
	s := startTestReadySession(t)

	if a := askReady(readyOpts{}); a.code != 0 {
		t.Fatalf("gc ready through the session = %+v, want a successful read", a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, leg := range s.legs {
		if leg.label == "rig "+cityNamedRig {
			t.Fatalf("fixture: rig %q is a leg, want it left out as the rig keyed under the city's name", cityNamedRig)
		}
	}
	if got := opens(cityNamedRig); got != 1 {
		t.Fatalf("fixture: rig %q was opened %d times, want once, with the serving frame", cityNamedRig, got)
	}
	if err := s.dropLegs(); err != nil {
		t.Fatalf("dropping the legs: %v", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("dropping the legs closed rig %q %d times, want once: the session opened it, so it is the session's to close though no leg reads it", cityNamedRig, got)
	}
}

// TestReadySessionLeavesTheProcessBindingOpen: the relocated binding's leg is
// one the session borrowed rather than opened. The process-lived routes memo
// owns that store and closes it at exit (closeCLIStorageRoutes); borrowers close
// what they opened, not what they hold. So the binding is not among the stores a
// leg set owns, and after a dropped leg set, and the session itself, it still
// serves the process's graph-class reads.
func TestReadySessionLeavesTheProcessBindingOpen(t *testing.T) {
	requireReadySessions(t)
	cityPath, _ := migratedOneShotCLICity(t)
	step := seedRelocatedBindingBead(t, cityPath, "graph step")
	binding, _, err := cliSoleClassBinding(cityPath)
	if err != nil {
		t.Fatalf("resolving the process's binding: %v", err)
	}
	// Registered before the session's own cleanup, so it runs once the session
	// has closed.
	t.Cleanup(func() {
		if _, err := binding.Store.Get(step.ID); err != nil {
			t.Errorf("after the session closed, the process's binding answers Get(%s) with %v, want the bead: the session closed a store it does not own", step.ID, err)
		}
	})
	s := startTestReadySession(t)
	ask := func(when string) {
		t.Helper()
		resp := s.answer(readySessionRequest{Token: s.token, CityPath: s.cityPath})
		if !resp.Served || resp.Error != "" || !strings.Contains(string(resp.Output), step.ID) {
			t.Fatalf("%s, the session answered served=%v error=%q output=%s, want the binding's %s", when, resp.Served, resp.Error, resp.Output, step.ID)
		}
	}

	ask("on the first leg set")
	s.mu.Lock()
	for _, store := range s.owned {
		if store == binding.Store {
			s.mu.Unlock()
			t.Fatal("the session owns the process's binding, a store it borrowed from the routes memo, and would close it with its legs")
		}
	}
	err = s.dropLegs()
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("dropping the first leg set: %v", err)
	}
	ask("on the leg set opened after it")
}

// TestReadySessionServesOnlyItsOwnCity: a `gc ready` that resolved another
// city (a --city flag, a cd into another city) is declined and reads
// in-process, so a session can never answer for stores it does not hold.
func TestReadySessionServesOnlyItsOwnCity(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	s := startTestReadySession(t)

	resp := s.answer(readySessionRequest{Token: s.token, CityPath: s.cityPath + "-elsewhere"})
	if resp.Served {
		t.Fatalf("the session served a question for another city: %+v", resp)
	}
	resp = s.answer(readySessionRequest{Token: "not-the-token", CityPath: s.cityPath})
	if resp.Served {
		t.Fatalf("the session served a question carrying the wrong token: %+v", resp)
	}
	if resp = s.answer(readySessionRequest{Token: s.token, CityPath: s.cityPath}); !resp.Served {
		t.Fatalf("the session declined a question for its own city with its own token: %+v", resp)
	}
}

// serveTestReadySession serves a session over a city no test question names:
// every question these tests ask carries the wrong token, which the session
// declines before it touches a store, so no city has to exist.
func serveTestReadySession(t *testing.T) *readySession {
	t.Helper()
	requireReadySessions(t)
	ln, addr, err := listenReadySession()
	if err != nil {
		t.Fatalf("listenReadySession: %v", err)
	}
	return serveReadySession(ln, addr, "session-token", t.TempDir(), io.Discard)
}

// setReadySessionQuestionTimeout sets how long a session waits for a
// connection's question, for the rest of the test.
func setReadySessionQuestionTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := readySessionQuestionTimeout
	readySessionQuestionTimeout = d
	t.Cleanup(func() { readySessionQuestionTimeout = prev })
}

// dialSilentReadyPeer connects to the session at addr and never asks.
func dialSilentReadyPeer(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", addr, readySessionDialTimeout)
	if err != nil {
		t.Fatalf("dialing the session: %v", err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck // the test is over either way
	return conn
}

// requireReadySessionDeclines asks the session at addr a question carrying the
// wrong token and requires the round trip to come back declined.
func requireReadySessionDeclines(t *testing.T, addr string) {
	t.Helper()
	if resp, err := askReadySession(addr, readySessionRequest{Token: "not-the-token"}); err != nil || resp.Served {
		t.Fatalf("a question with the wrong token = %+v, %v; want it answered and declined", resp, err)
	}
}

// requireReadyPeerDropped requires that the session closed its end of conn:
// the peer reads EOF instead of waiting.
func requireReadyPeerDropped(t *testing.T, conn net.Conn, why string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if n, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the silent peer read %d bytes with err %v, want EOF: %s", n, err, why)
	}
}

// TestReadySessionCloseDropsAPeerThatNeverAsks: anyone on the host can reach an
// abstract socket, so a peer can connect and never ask. Closing the session —
// the wrapper does it the moment its query exits — must not wait on that peer:
// it drops the connection and returns. The question timeout is an hour here, so
// only close can free the peer.
func TestReadySessionCloseDropsAPeerThatNeverAsks(t *testing.T) {
	setReadySessionQuestionTimeout(t, time.Hour)
	s := serveTestReadySession(t)
	silent := dialSilentReadyPeer(t, s.addr)
	// A session accepts connections in the order they arrive, so once a
	// question asked after the silent peer's connect is answered, the session
	// holds the silent peer's connection too.
	requireReadySessionDeclines(t, s.addr)

	closed := make(chan error, 1)
	go func() { closed <- s.close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("closing the session: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close is still waiting on a peer that connected and never asked")
	}
	requireReadyPeerDropped(t, silent, "closing the session drops every connection still open")
}

// TestReadySessionDropsAPeerThatNeverAsks: a peer that connects and never asks
// is dropped once the question timeout passes, without waiting for the session
// to close, and the session keeps answering.
func TestReadySessionDropsAPeerThatNeverAsks(t *testing.T) {
	setReadySessionQuestionTimeout(t, 250*time.Millisecond)
	s := serveTestReadySession(t)
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Errorf("closing the ready session: %v", err)
		}
	})
	silent := dialSilentReadyPeer(t, s.addr)

	requireReadyPeerDropped(t, silent, "a question not asked within the question timeout never will be")
	requireReadySessionDeclines(t, s.addr)
}

// TestReadyReadsInProcessWhenTheSessionIsUnreachable: a session that is gone
// is never an outage. The reader says so on stderr and answers in-process.
func TestReadyReadsInProcessWhenTheSessionIsUnreachable(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	want := askReady(readyOpts{})

	t.Setenv(readySessionAddrEnv, "@gc-ready-session-gone")
	t.Setenv(readySessionTokenEnv, "token")
	got := askReady(readyOpts{})
	if got.code != 0 || got.stdout != want.stdout {
		t.Fatalf("with the session gone, gc ready = %+v, want the in-process answer %q", got, want.stdout)
	}
	if !strings.Contains(got.stderr, "ready session") {
		t.Fatalf("stderr = %q, want it to say the session was unreachable and the read went in-process", got.stderr)
	}
}

// TestRunReadySessionRunsTheQueryInsideOneSession pins the wrapper: the query
// runs with the session's address in its environment, its stdout and stderr
// pass through untouched, and its exit status is the wrapper's.
func TestRunReadySessionRunsTheQueryInsideOneSession(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	var stdout, stderr bytes.Buffer
	code := runReadySession([]string{"sh", "-c", `printf '%s' "$` + readySessionAddrEnv + `"; printf 'query stderr' >&2; exit 3`}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("wrapper exit = %d, want the query's own exit status 3", code)
	}
	if !strings.HasPrefix(stdout.String(), "@gc-ready-session-") {
		t.Fatalf("the query saw %s=%q, want the session's abstract address", readySessionAddrEnv, stdout.String())
	}
	if stderr.String() != "query stderr" {
		t.Fatalf("stderr = %q, want the query's own stderr and nothing else", stderr.String())
	}
}

// TestRunReadySessionRunsTheQueryWhenNoSessionCanStart: the session is an
// optimization, so failing to start one never fails the query. It runs as it
// would have without the wrapper — no session in its environment, its own exit
// status — and the wrapper says on stderr why there was no session.
func TestRunReadySessionRunsTheQueryWhenNoSessionCanStart(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	prev := readySessionListen
	readySessionListen = func() (net.Listener, string, error) { return nil, "", errors.New("no socket for you") }
	t.Cleanup(func() { readySessionListen = prev })

	var stdout, stderr bytes.Buffer
	code := runReadySession([]string{"sh", "-c", `printf '[%s]' "$` + readySessionAddrEnv + `"; exit 4`}, &stdout, &stderr)
	if code != 4 || stdout.String() != "[]" {
		t.Fatalf("wrapper = %d with stdout %q, want the query run without a session (empty %s) and its exit status 4", code, stdout.String(), readySessionAddrEnv)
	}
	if !strings.Contains(stderr.String(), "no socket for you") {
		t.Fatalf("stderr = %q, want the reason there was no session", stderr.String())
	}
}

// TestHookRunsTheGeneratedFederatedQueryInsideAReadySession pins where the
// session is used: only for the generated work query of a split city, the one
// that asks `gc ready` several questions. A single-store city's query and an
// operator's own work_query run exactly as before.
func TestHookRunsTheGeneratedFederatedQueryInsideAReadySession(t *testing.T) {
	federated := config.QueryTopology{FederatedReady: true}
	if got := hookUsesReadySession(&config.Agent{Name: "worker"}, federated); got != readySessionsSupported() {
		t.Fatalf("generated query on a split city uses a ready session = %v, want %v (whether this platform can run one)", got, readySessionsSupported())
	}
	if hookUsesReadySession(&config.Agent{Name: "worker"}, config.QueryTopology{}) {
		t.Fatal("a single-store city's generated query asks bd, not gc ready, and must run without a session")
	}
	if hookUsesReadySession(&config.Agent{Name: "worker", WorkQuery: "bd ready --json"}, federated) {
		t.Fatal("an operator's own work_query must run exactly as written")
	}
}

// TestShellWorkQueryRunsInsideAReadySessionWhenAsked pins the runner half
// end to end: with the request in the query's environment the query runs
// under `gc internal ready-session` — this binary, re-executed as gc — and sees
// a live session; without it, it is the bare sh it always was.
func TestShellWorkQueryRunsInsideAReadySessionWhenAsked(t *testing.T) {
	requireReadySessions(t)
	newReadyCityWithRigs(t)
	query := `printf '[%s]' "$` + readySessionAddrEnv + `"`

	plain, err := shellWorkQueryWithEnv(query, "", os.Environ())
	if err != nil || plain != "[]" {
		t.Fatalf("without the request the query printed %q (err %v), want no session: []", plain, err)
	}
	asked, err := shellWorkQueryWithEnv(query, "", append(os.Environ(), readySessionRequestEnv+"=1"))
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv with the request: %v", err)
	}
	if !strings.HasPrefix(asked, "[@gc-ready-session-") {
		t.Fatalf("with the request the query printed %q, want the address of the ready session it runs in", asked)
	}
}

// TestReadySessionRequestRidesEveryStoresOwnEnv pins where the request lives:
// in each store's env, the env its query runs with. A rig-scoped agent's
// primary leg runs with the rig's env, rebuilt from the rig's own query env
// plus the identity keys, so a request set only on the hook's env never
// reaches the query — which is the exact miss this guards.
func TestReadySessionRequestRidesEveryStoresOwnEnv(t *testing.T) {
	rigEnv := []string{"GC_RIG=alpha", "PATH=/bin"}
	cityEnv := []string{"GC_RIG=", "PATH=/bin", readySessionRequestEnv + "=0"}
	stores := []hookStore{{dir: "/rigs/alpha", env: rigEnv}, {dir: "/city", env: cityEnv}}

	got := withReadySessionRequest(stores)
	for i, st := range got {
		if hookClaimEnvValue(st.env, readySessionRequestEnv) != "1" {
			t.Fatalf("store %d (%s) env = %v, want the ready-session request in it", i, st.dir, st.env)
		}
		if n := strings.Count(strings.Join(st.env, "\n"), readySessionRequestEnv+"="); n != 1 {
			t.Fatalf("store %d env carries the request %d times, want once: %v", i, n, st.env)
		}
	}
	if len(rigEnv) != 2 || len(cityEnv) != 3 || stores[0].env[0] != "GC_RIG=alpha" {
		t.Fatalf("withReadySessionRequest wrote through to the caller's store envs: %v / %v", rigEnv, cityEnv)
	}
}

// TestReadySessionReadsFreshForEveryQuestion: the session keeps the opened
// stores, never their answers. Work created between two questions — another
// agent routing a bead mid-claim — is in the second answer, exactly as a
// second `gc ready` process would have seen it.
func TestReadySessionReadsFreshForEveryQuestion(t *testing.T) {
	requireReadySessions(t)
	cityDir := newReadyCityWithRigs(t)
	t.Setenv("GC_RIG", readyCountedRigNames[0])
	opens := countReadyRigOpens(t, nil)
	startTestReadySession(t)

	if first := askReady(readyOpts{}); first.code != 0 || strings.Contains(first.stdout, "created mid-claim") {
		t.Fatalf("first question = %+v, want an answer without the bead not yet created", first)
	}
	rigStore, err := openStoreAtForCity(cityDir+"/rigs/"+readyCountedRigNames[1], cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	created := mustCreateReadyBead(t, rigStore, beads.Bead{Title: "created mid-claim", Type: "task"})

	second := askReady(readyOpts{})
	if second.code != 0 || !strings.Contains(second.stdout, created.ID) {
		t.Fatalf("second question = %+v, want the bead %s created after the first answer", second, created.ID)
	}
	if got := opens(readyCountedRigNames[1]); got != 1 {
		t.Fatalf("rig opened %d times across both questions, want once: fresh reads, reused stores", got)
	}
}

// TestReadySessionWireCarriesEveryReadyFlag: a flag the session's wire did
// not carry would be dropped in transit, and the session would answer a
// different question than the one asked — silently, with exit 0. Every
// readyOpts field must survive the trip through the JSON request, so a field
// added to readyOpts later fails here until the wire carries it.
func TestReadySessionWireCarriesEveryReadyFlag(t *testing.T) {
	opts := readyOpts{
		assignee:       "worker-1",
		unassigned:     true,
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=worker", "gc.kind"},
		excludeTypes:   []string{"epic"},
		excludeLabels:  []string{"human", "hold:mayor"},
		sortOrder:      readySortOldest,
		limit:          7,
		status:         readyStatusOpen,
	}
	fields := reflect.ValueOf(opts)
	for i := range fields.NumField() {
		if fields.Field(i).IsZero() {
			t.Fatalf("readyOpts.%s is zero in this fixture; set it so the round trip below proves the session wire carries it", fields.Type().Field(i).Name)
		}
	}

	encoded, err := json.Marshal(readySessionRequestFor("token", "/city", opts))
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	var decoded readySessionRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding the request: %v", err)
	}
	if got := decoded.readyOpts(); !reflect.DeepEqual(got, opts) {
		t.Fatalf("flags after the session wire = %+v, want %+v", got, opts)
	}
}
