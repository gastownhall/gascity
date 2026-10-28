package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The file nudge queue's characterization goldens. Each scenario drives the
// flock'd state.json queue through the facades its production callers use and
// records, per step, the result, whether state.json was rewritten, the store
// opens and closes, the session census calls, the wake pings, the shadow-store
// operations and the state.json bytes. Moving the queue behind a seam must
// leave every golden byte-identical. Regenerate with -chartest-update only for
// a deliberate behavior change.
//
// The tests replace package seams, so they stay serial.

// nqCharVerbs is the one part of this harness a later change may edit: each
// entry is the facade a production caller uses for one queue verb, so a
// facade whose signature changes is re-pointed here and nowhere else.
var nqCharVerbs = struct {
	enqueue       func(city string, store beads.NudgesStore, item queuedNudge) error
	claimDue      func(city string, target nudgeTarget, now time.Time) ([]queuedNudge, error)
	claimBatch    func(city string, target nudgeTarget, now time.Time) ([]queuedNudge, error)
	release       func(city string, ids []string, notBefore time.Time) error
	recordFailure func(city string, store beads.NudgesStore, ids []string, cause error, now time.Time) ([]queuedNudge, error)
	ack           func(city string, ids []string, outcome, reason, boundary string) error
	rollback      func(city string, store beads.NudgesStore, item queuedNudge, reason string) error
	withdrawWait  func(city string, ids []string) error
	deferred      func(city string, item queuedNudge) error
	sweep         func(city string, now time.Time) error
	classify      func(city string, ids []string, now time.Time) (map[string]nudgeDropStatus, error)
	empty         func(city string) bool
	pollHasDue    func(target nudgeTarget, now time.Time) bool
	snapshotFor   func(city string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error)
	listFor       func(city string, target nudgeTarget, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error)
	listForAgent  func(city, agent string, now time.Time) ([]queuedNudge, []queuedNudge, []queuedNudge, error)
	recordSkips   func(city string, counts map[string]int64) error
	dispatchSkips func(city string) map[string]int64
	rekey         func(city string, items []queuedNudge, bookkeepErr error) ([]queuedNudge, error)
}{
	enqueue:       enqueueQueuedNudgeWithStore,
	claimDue:      claimDueQueuedNudgesForTarget,
	claimBatch:    claimDueQueuedNudgeBatchForTarget,
	release:       releaseQueuedNudgeClaimsUntil,
	recordFailure: recordQueuedNudgeFailureDetailed,
	ack:           ackQueuedNudgesWithOutcome,
	rollback:      rollbackQueuedNudge,
	withdrawWait:  withdrawQueuedWaitNudges,
	deferred: func(city string, item queuedNudge) error {
		return openNudgeQueue(city, beads.NudgesStore{}).Queue.EnqueueDeferred(item)
	},
	sweep:        runNudgeQueueMaintenanceSweep,
	classify:     classifyQueuedNudgeIDs,
	empty:        nudgeQueueIsEmpty,
	pollHasDue:   nudgePollTargetHasDueWork,
	snapshotFor:  listQueuedNudgesForTargetSnapshot,
	listFor:      listQueuedNudgesForTarget,
	listForAgent: listQueuedNudges,
	recordSkips:  recordNudgeDispatchSkips,
	dispatchSkips: func(city string) map[string]int64 {
		skips, err := openNudgeQueue(city, beads.NudgesStore{}).Skips.DispatchSkips()
		if err != nil {
			return nil
		}
		return skips
	},
	rekey: rekeyQueuedNudgesAfterFailedBookkeeping,
}

// nqLockProbeStore is the shadow store's delegate. Every bead-store call it
// serves first probes the queue's flock on a fresh descriptor, and reports a
// call made while the lock is held (ga-fsp5w9: the critical section is pure).
type nqLockProbeStore struct {
	beads.Store
	h *nqCharHarness
}

func (s nqLockProbeStore) Create(b beads.Bead) (beads.Bead, error) {
	s.h.probe("store Create")
	return s.Store.Create(b)
}

func (s nqLockProbeStore) Get(id string) (beads.Bead, error) {
	s.h.probe("store Get " + id)
	return s.Store.Get(id)
}

func (s nqLockProbeStore) Update(id string, opts beads.UpdateOpts) error {
	s.h.probe("store Update " + id)
	return s.Store.Update(id, opts)
}

func (s nqLockProbeStore) Close(id string) error {
	s.h.probe("store Close " + id)
	return s.Store.Close(id)
}

func (s nqLockProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.h.probe("store List " + beadstest.DescribeListQuery(query))
	return s.Store.List(query)
}

func (s nqLockProbeStore) SetMetadata(id, key, value string) error {
	s.h.probe("store SetMetadata " + id)
	return s.Store.SetMetadata(id, key, value)
}

func (s nqLockProbeStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.h.probe("store SetMetadataBatch " + id)
	return s.Store.SetMetadataBatch(id, kvs)
}

// nudgeQueueLockHeldForTest reports whether some descriptor holds the queue's
// flock, by trying it on a fresh one.
func nudgeQueueLockHeldForTest(city string) bool {
	f, err := os.OpenFile(nudgequeue.LockPath(city), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// probe fails the test when what is a store call, store open or census made
// while the queue lock is held.
func (h *nqCharHarness) probe(what string) {
	if h.lockHeldByTest {
		return
	}
	if nudgeQueueLockHeldForTest(h.city) {
		h.underLock = append(h.underLock, what)
		h.t.Errorf("%s ran under the nudge queue lock", what)
	}
}

// nqCharStore is the one shadow store every frame sees: the store a caller
// passes and every store a frame opens for itself. It counts the closes of the
// handles the frames open, and fails its writes while failWrites is set.
type nqCharStore struct {
	*beadstest.OpRecordingStore
	h *nqCharHarness
}

func (s nqCharStore) CloseStore() error { //nolint:unparam // the closer interface returns an error; this fake never fails
	s.h.closes++
	return nil
}

var errNQCharInjectedWrite = errors.New("injected shadow write failure")

func (s nqCharStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.h.failWrites {
		return beads.Bead{}, errNQCharInjectedWrite
	}
	return s.OpRecordingStore.Create(b)
}

func (s nqCharStore) Update(id string, opts beads.UpdateOpts) error {
	if s.h.failWrites {
		return errNQCharInjectedWrite
	}
	return s.OpRecordingStore.Update(id, opts)
}

func (s nqCharStore) Close(id string) error {
	if s.h.failWrites {
		return errNQCharInjectedWrite
	}
	return s.OpRecordingStore.Close(id)
}

func (s nqCharStore) SetMetadata(id, key, value string) error {
	if s.h.failWrites {
		return errNQCharInjectedWrite
	}
	return s.OpRecordingStore.SetMetadata(id, key, value)
}

func (s nqCharStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.h.failWrites {
		return errNQCharInjectedWrite
	}
	return s.OpRecordingStore.SetMetadataBatch(id, kvs)
}

type nqCharHarness struct {
	t    *testing.T
	city string
	base time.Time
	rec  *beadstest.OpRecordingStore

	// storeOff makes every frame that opens its own store get none.
	storeOff   bool
	failWrites bool
	live       map[string]struct{}

	opens, closes, census, pings int
	// underLock lists the store calls, opens and census calls made while
	// the queue lock was held.
	underLock []string
	// lockHeldByTest turns the probe off while the test itself holds the
	// queue lock on purpose.
	lockHeldByTest bool
	warnings       bytes.Buffer
	out            bytes.Buffer
}

func newNQCharHarness(t *testing.T) *nqCharHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	h := &nqCharHarness{
		t:    t,
		city: t.TempDir(),
		base: time.Now().UTC().Truncate(time.Second),
		live: map[string]struct{}{"s-new": {}, "s-sib": {}},
	}
	h.rec = beadstest.NewOpRecordingStore(nqLockProbeStore{Store: beads.NewMemStore(), h: h})

	prevOpen := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		h.probe("store open")
		h.opens++
		if h.storeOff {
			return beads.NudgesStore{}, nil
		}
		s := h.store()
		return beads.NudgesStore{Store: s}, s
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prevOpen })
	prevWork := openNudgeWorkStore
	openNudgeWorkStore = func(string, string) (beads.Store, error) {
		h.probe("work store open")
		h.opens++
		return h.store(), nil
	}
	t.Cleanup(func() { openNudgeWorkStore = prevWork })
	prevCensus := liveNudgeFenceSessionIDs
	liveNudgeFenceSessionIDs = func(string) map[string]struct{} {
		h.probe("session census")
		h.census++
		return h.live
	}
	t.Cleanup(func() { liveNudgeFenceSessionIDs = prevCensus })
	prevWarn := nudgeWarningWriter
	nudgeWarningWriter = &h.warnings
	t.Cleanup(func() { nudgeWarningWriter = prevWarn })

	prevPing := pingNudgeWakeSocket
	pingNudgeWakeSocket = func(cityPath string) {
		if cityPath == h.city {
			h.pings++
		}
	}
	t.Cleanup(func() { pingNudgeWakeSocket = prevPing })
	return h
}

func (h *nqCharHarness) store() nqCharStore { return nqCharStore{OpRecordingStore: h.rec, h: h} }

// shadow is the store a caller that holds one passes to a verb.
func (h *nqCharHarness) shadow() beads.NudgesStore { return beads.NudgesStore{Store: h.store()} }

func (h *nqCharHarness) at(d time.Duration) time.Time { return h.base.Add(d) }

func (h *nqCharHarness) item(id, agent string, created time.Duration, opts queuedNudgeOptions) queuedNudge {
	opts.ID = id
	return newQueuedNudgeWithOptions(agent, "message "+id, "session", h.at(created), opts)
}

func (h *nqCharHarness) target(sessionID, epoch string, aliasHistory ...string) nudgeTarget {
	return nudgeTarget{cityPath: h.city, alias: "alpha", aliasHistory: aliasHistory, sessionID: sessionID, continuationEpoch: epoch}
}

func statIno(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return st.Ino, true
}

// step runs fn and records what it did. state.json is replaced by rename, so
// a rewrite gives the path a new inode; the step holds the old file open so
// its inode cannot be reused by the rewrite.
func (h *nqCharHarness) step(label string, fn func() string) {
	h.t.Helper()
	statePath := nudgequeue.StatePath(h.city)
	h.rec.Reset()
	h.warnings.Reset()
	opens, closes, census, pings := h.opens, h.closes, h.census, h.pings
	beforeIno, existed := statIno(statePath)
	var held *os.File
	if existed {
		f, err := os.Open(statePath)
		if err != nil {
			h.t.Fatalf("holding state.json open: %v", err)
		}
		held = f
	}
	result := fn()
	afterIno, exists := statIno(statePath)
	if held != nil {
		_ = held.Close()
	}
	rewrote := exists && (!existed || afterIno != beforeIno)

	fmt.Fprintf(&h.out, "### %s\n", label)
	fmt.Fprintf(&h.out, "result: %s\n", result)
	fmt.Fprintf(&h.out, "rewrote_state: %v\n", rewrote)
	fmt.Fprintf(&h.out, "store_opens: %d store_closes: %d census_calls: %d wake_pings: %d\n",
		h.opens-opens, h.closes-closes, h.census-census, h.pings-pings)
	for _, op := range h.rec.Ops() {
		line := "op " + op.Kind
		if op.Method != "" && op.Method != op.Kind {
			line += " via " + op.Method
		}
		if op.ID != "" {
			line += " " + op.ID
		}
		if op.Kind == beadstest.OpList {
			line += " {" + beadstest.DescribeListQuery(op.Query) + "}"
		}
		if op.Err != nil {
			line += " err=" + op.Err.Error()
		}
		fmt.Fprintln(&h.out, line)
	}
	if h.warnings.Len() > 0 {
		fmt.Fprintf(&h.out, "warnings:\n%s", h.warnings.String())
	}
	data, err := os.ReadFile(statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(&h.out, "state.json: <absent>")
	case err != nil:
		h.t.Fatalf("reading state.json: %v", err)
	default:
		fmt.Fprintf(&h.out, "state.json:\n%s", data)
	}
	fmt.Fprintln(&h.out)
}

// setState writes state.json outside any step, as a legacy or damaged file.
func (h *nqCharHarness) setState(data string) {
	h.t.Helper()
	path := nudgequeue.StatePath(h.city)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatalf("creating the queue dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		h.t.Fatalf("writing state.json: %v", err)
	}
}

func (h *nqCharHarness) readState() string {
	h.t.Helper()
	data, err := os.ReadFile(nudgequeue.StatePath(h.city))
	if err != nil {
		h.t.Fatalf("reading state.json: %v", err)
	}
	return string(data)
}

func (h *nqCharHarness) finish(name string) {
	h.t.Helper()
	canon := chartest.NewCanonicalizer(append(chartest.DefaultRules(),
		chartest.Rule{Category: "BATCH", Pattern: regexp.MustCompile(`nudge-batch-[0-9a-f]{16}`)},
		chartest.Rule{Category: "CITY", Pattern: regexp.MustCompile(regexp.QuoteMeta(h.city))},
	)...)
	chartest.CompareGolden(h.t, filepath.Join("testdata", "nudge_file_queue", name+".golden"), canon.Canonicalize(h.out.Bytes()))
}

func nqCharItems(items []queuedNudge) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		part := item.ID
		if item.DeliveryBatch != "" {
			part += "@" + item.DeliveryBatch
		}
		if item.SessionID != "" || item.ContinuationEpoch != "" {
			part += "[" + item.SessionID + "/" + item.ContinuationEpoch + "]"
		}
		parts = append(parts, part)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func nqCharErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func nqCharClaim(items []queuedNudge, err error) string {
	return nqCharItems(items) + " err=" + nqCharErr(err)
}

func nqCharBuckets(pending, inFlight, dead []queuedNudge, err error) string {
	return "pending=" + nqCharItems(pending) + " in_flight=" + nqCharItems(inFlight) + " dead=" + nqCharItems(dead) + " err=" + nqCharErr(err)
}

func nqCharStatuses(statuses map[string]nudgeDropStatus, err error) string {
	names := map[nudgeDropStatus]string{
		nudgeDropStatusNotFound:    "not-found",
		nudgeDropStatusDroppable:   "droppable",
		nudgeDropStatusAlreadyDead: "already-dead",
	}
	keys := make([]string, 0, len(statuses))
	for id, status := range statuses {
		keys = append(keys, id+"="+names[status])
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, " ") + "} err=" + nqCharErr(err)
}

func TestNudgeFileQueueCharacterizationEnqueue(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	mailRef := &nudgeReference{Kind: "mail", ID: "m1"}
	alpha := h.target("s-new", "2")

	h.step("enqueue n-a with a store", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-a", "alpha", 0, queuedNudgeOptions{Reference: mailRef})))
	})
	h.step("enqueue n-a again is idempotent", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-a", "alpha", 0, queuedNudgeOptions{Reference: mailRef})))
	})
	h.step("enqueue n-b supersedes pending n-a", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-b", "alpha", 0, queuedNudgeOptions{Reference: mailRef})))
	})
	h.step("claim n-b", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(30*time.Minute)))
	})
	h.step("enqueue n-c supersedes in-flight n-b", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-c", "alpha", 0, queuedNudgeOptions{Reference: mailRef})))
	})
	h.step("enqueue n-z with the zero store", func() string {
		return nqCharErr(v.enqueue(h.city, beads.NudgesStore{}, h.item("n-z", "alpha", 0, queuedNudgeOptions{})))
	})
	h.step("enqueue n-old, already past its deadline", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-old", "alpha", -25*time.Hour, queuedNudgeOptions{})))
	})
	h.step("enqueue n-d expires n-old in its prologue", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-d", "beta", 0, queuedNudgeOptions{})))
	})
	h.step("drop n-z", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-z"}, errNudgeManualDrop, h.at(2*time.Second)))
	})
	h.step("enqueue dead n-z again leaves an open shadow", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-z", "alpha", 0, queuedNudgeOptions{})))
	})
	good := h.readState()
	h.setState("{not json\n")
	h.step("enqueue n-e over an unreadable queue rolls back its shadow", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-e", "alpha", 0, queuedNudgeOptions{})))
	})
	h.setState(good)
	h.failWrites = true
	h.step("enqueue n-f whose shadow write fails", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-f", "alpha", 0, queuedNudgeOptions{})))
	})
	h.failWrites = false
	h.finish("enqueue")
}

func TestNudgeFileQueueCharacterizationDrainClaim(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	alpha := h.target("s-new", "2")
	for _, item := range []queuedNudge{
		h.item("n-free", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-mine", "alpha", 0, queuedNudgeOptions{SessionID: "s-new", ContinuationEpoch: "2"}),
		h.item("n-sib", "alpha", 0, queuedNudgeOptions{SessionID: "s-sib", ContinuationEpoch: "1"}),
		h.item("n-legb", "alpha", 0, queuedNudgeOptions{SessionID: "s-old", ContinuationEpoch: "1"}),
		h.item("n-epoch", "alpha", 0, queuedNudgeOptions{ContinuationEpoch: "2"}),
		h.item("n-lapse", "alpha", -2*time.Hour, queuedNudgeOptions{}),
		h.item("n-other", "beta", 0, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	expiring := h.item("n-exp", "alpha", -24*time.Hour+time.Minute, queuedNudgeOptions{})
	expiring.DeliverAfter = h.at(0)
	h.step("enqueue n-exp, due now and expiring in a minute", func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), expiring)) })
	later := h.item("n-later", "alpha", 0, queuedNudgeOptions{})
	later.DeliverAfter = h.at(time.Hour)
	h.step("enqueue n-later, due in an hour", func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), later)) })

	h.step("claim an hour ago takes only n-lapse; no due Leg-B candidate, no census", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(-time.Hour)))
	})
	h.step("claim in ten minutes recovers the lapsed lease, expires n-exp and adopts n-legb (one census)", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(10*time.Minute)))
	})
	h.step("claim with nothing due", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(11*time.Minute)))
	})
	h.live = nil
	h.step("claim with the census unavailable leaves n-sib", func() string {
		return nqCharClaim(v.claimDue(h.city, h.target("s-new", "2"), h.at(2*time.Hour)))
	})
	h.finish("drain_claim")
}

func TestNudgeFileQueueCharacterizationBatchClaim(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	alpha := h.target("s-new", "2")
	// Clocked verbs run half an hour on, so the wall-clock verbs between them
	// never see a lease lapse.
	at := func(d time.Duration) time.Time { return h.at(30*time.Minute + d) }
	for _, item := range []queuedNudge{
		h.item("n-a1", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-a2", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-legb", "alpha", 0, queuedNudgeOptions{SessionID: "s-old", ContinuationEpoch: "1"}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	h.step("claim a new batch, adopting n-legb onto the claimer's fence", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(time.Second)))
	})
	h.step("release with a backoff keeps the pin", func() string {
		return nqCharErr(v.release(h.city, []string{"n-a1", "n-a2", "n-legb"}, at(30*time.Second)))
	})
	h.step("claim inside the backoff", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(10*time.Second)))
	})
	h.step("claim resumes the pinned batch whole", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(31*time.Second)))
	})
	h.step("an ordinary failure keeps the pin", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-a1", "n-a2", "n-legb"}, errors.New("delivery refused"), at(32*time.Second)))
	})
	h.step("a failure of one member delays only it", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-a1"}, errors.New("delivery refused"), at(40*time.Second)))
	})
	h.step("a due member resumes the batch, its not-due sibling with it", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(48*time.Second)))
	})
	h.step("a rejection unpins", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-a1", "n-a2", "n-legb"}, fmt.Errorf("refused: %w", runtime.ErrNudgeRejected), at(49*time.Second)))
	})
	h.step("enqueue n-a3", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-a3", "alpha", 0, queuedNudgeOptions{})))
	})
	h.step("claim a new batch after the rejection", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(70*time.Second)))
	})
	h.step("release one member", func() string {
		return nqCharErr(v.release(h.city, []string{"n-a1"}, time.Time{}))
	})
	h.step("enqueue n-a4", func() string {
		return nqCharErr(v.enqueue(h.city, h.shadow(), h.item("n-a4", "alpha", 0, queuedNudgeOptions{})))
	})
	h.step("a batch with a member in flight is not resumed; the unpinned item gets a new batch", func() string {
		return nqCharClaim(v.claimBatch(h.city, alpha, at(71*time.Second)))
	})
	h.step("repin the in-flight claims onto a new key", func() string {
		state, err := nudgequeue.LoadState(h.city)
		if err != nil {
			return nqCharErr(err)
		}
		items, err := v.rekey(h.city, state.InFlight, errors.New("withdrawing a sibling failed"))
		return nqCharClaim(items, err)
	})
	h.step("repin with a claim no longer in flight releases them all", func() string {
		state, err := nudgequeue.LoadState(h.city)
		if err != nil {
			return nqCharErr(err)
		}
		items := append(append([]queuedNudge(nil), state.InFlight...), h.item("n-gone", "alpha", 0, queuedNudgeOptions{}))
		rekeyed, err := v.rekey(h.city, items, errors.New("withdrawing a sibling failed"))
		return nqCharClaim(rekeyed, err)
	})
	h.step("ack every claim", func() string {
		state, err := nudgequeue.LoadState(h.city)
		if err != nil {
			return nqCharErr(err)
		}
		var ids []string
		for _, item := range append(state.Pending, state.InFlight...) {
			ids = append(ids, item.ID)
		}
		return nqCharErr(v.ack(h.city, ids, "injected", "", "provider-nudge-return"))
	})

	// A batch pinned across two of one target's keys is split for a target
	// that answers to only one of them.
	wide := h.target("", "", "old-alpha")
	narrow := h.target("", "")
	for _, item := range []queuedNudge{
		h.item("n-k1", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-k2", "old-alpha", 0, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	h.step("claim a batch across both keys", func() string {
		return nqCharClaim(v.claimBatch(h.city, wide, at(80*time.Second)))
	})
	h.step("release it", func() string {
		return nqCharErr(v.release(h.city, []string{"n-k1", "n-k2"}, time.Time{}))
	})
	h.step("a target with one of the keys does not take the split batch", func() string {
		return nqCharClaim(v.claimBatch(h.city, narrow, at(81*time.Second)))
	})
	h.step("the target with both keys resumes it", func() string {
		return nqCharClaim(v.claimBatch(h.city, wide, at(82*time.Second)))
	})
	h.finish("batch_claim")
}

func TestNudgeFileQueueCharacterizationFailureAckRollback(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	alpha := h.target("s-new", "2")
	for _, item := range []queuedNudge{
		h.item("n-retry", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-fence", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-drop", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-ack1", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-ack2", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-ack3", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-ack4", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-ack5", "beta", 0, queuedNudgeOptions{}),
		h.item("n-roll", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-short", "gamma", -23*time.Hour, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	for i := 1; i <= defaultQueuedNudgeMaxAttempts; i++ {
		h.step(fmt.Sprintf("failure %d of n-retry", i), func() string {
			return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-retry"}, errors.New("delivery refused"), h.at(time.Duration(i)*time.Minute)))
		})
	}
	h.step("a fence mismatch dead-letters at once", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-fence"}, errNudgeSessionFenceMismatch, h.at(6*time.Minute)))
	})
	h.step("an operator drop dead-letters at once, with the zero store", func() string {
		return nqCharClaim(v.recordFailure(h.city, beads.NudgesStore{}, []string{"n-drop"}, errNudgeManualDrop, h.at(6*time.Minute)))
	})
	h.step("a failure of an unknown id", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-unknown"}, errors.New("x"), h.at(6*time.Minute)))
	})
	h.step("claim the acks", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(30*time.Minute)))
	})
	h.step("ack injected", func() string {
		return nqCharErr(v.ack(h.city, []string{"n-ack1"}, "injected", "", "provider-nudge-return"))
	})
	h.step("ack accepted for injection", func() string {
		return nqCharErr(v.ack(h.city, []string{"n-ack2"}, "accepted_for_injection", "", "hook-transport-accepted"))
	})
	h.step("ack injected unobserved", func() string {
		return nqCharErr(v.ack(h.city, []string{"n-ack3"}, "injected_unobserved", "", "provider-nudge-return"))
	})
	h.step("ack withdrawn as failed", func() string {
		return nqCharErr(v.ack(h.city, []string{"n-ack4"}, "failed", "mail-already-read", "delivery-withdrawn"))
	})
	h.step("ack a pending item", func() string {
		return nqCharErr(v.ack(h.city, []string{"n-ack5"}, "injected", "", "provider-nudge-return"))
	})
	h.step("ack with no ids", func() string { return nqCharErr(v.ack(h.city, nil, "injected", "", "provider-nudge-return")) })
	h.step("release with no ids", func() string { return nqCharErr(v.release(h.city, nil, time.Time{})) })
	h.step("roll back a queued item", func() string {
		return nqCharErr(v.rollback(h.city, h.shadow(), h.item("n-roll", "alpha", 0, queuedNudgeOptions{}), "managed wake failed: boom"))
	})
	h.step("roll back a missing item", func() string {
		return nqCharErr(v.rollback(h.city, h.shadow(), h.item("n-never", "alpha", 0, queuedNudgeOptions{}), "managed wake failed: boom"))
	})
	h.step("claim n-short", func() string {
		return nqCharClaim(v.claimDue(h.city, nudgeTarget{cityPath: h.city, alias: "gamma"}, h.at(31*time.Minute)))
	})
	h.step("a failure two hours on expires in-flight n-short and ages the dead letters out", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-unknown"}, errors.New("x"), h.at(2*time.Hour)))
	})
	h.finish("failure_ack_rollback")
}

func TestNudgeFileQueueCharacterizationWithdrawAndDeferred(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	alpha := h.target("s-new", "2")
	waitRef := func(id string) queuedNudgeOptions {
		return queuedNudgeOptions{Reference: &nudgeReference{Kind: "bead", ID: id}}
	}
	for _, item := range []queuedNudge{
		h.item("wait-w1-0-1", "alpha", 0, waitRef("w1")),
		h.item("wait-w2-0-1", "alpha", 0, waitRef("w2")),
		h.item("wait-w3-0-1", "alpha", 0, waitRef("w3")),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	h.storeOff = true
	h.step("enqueue wait-w4-0-1 with no store at all", func() string {
		return nqCharErr(v.enqueue(h.city, beads.NudgesStore{}, h.item("wait-w4-0-1", "alpha", 0, waitRef("w4"))))
	})
	h.storeOff = false
	h.step("claim every wait nudge", func() string {
		return nqCharClaim(v.claimDue(h.city, alpha, h.at(30*time.Minute)))
	})
	h.step("release all but wait-w2-0-1", func() string {
		return nqCharErr(v.release(h.city, []string{"wait-w1-0-1", "wait-w3-0-1", "wait-w4-0-1"}, time.Time{}))
	})
	h.step("withdraw a pending and an in-flight wait nudge", func() string {
		return nqCharErr(v.withdrawWait(h.city, []string{"wait-w1-0-1", "wait-w2-0-1", "wait-w1-0-1", "", "wait-missing"}))
	})
	h.step("withdraw an item with no bead id looks its shadow up by label", func() string {
		return nqCharErr(v.withdrawWait(h.city, []string{"wait-w4-0-1"}))
	})
	h.failWrites = true
	h.step("withdraw whose shadow write fails keeps the item", func() string {
		return nqCharErr(v.withdrawWait(h.city, []string{"wait-w3-0-1"}))
	})
	h.failWrites = false
	h.storeOff = true
	h.step("withdraw with no store removes the item", func() string {
		return nqCharErr(v.withdrawWait(h.city, []string{"wait-w3-0-1"}))
	})
	h.storeOff = false
	h.step("withdraw nothing", func() string { return nqCharErr(v.withdrawWait(h.city, nil)) })

	deferred := nudgequeue.Item{
		ID: "nudge-deferred01", Agent: "alpha", SessionID: "s-new", ContinuationEpoch: "2", Source: "session",
		Message: "later", CreatedAt: h.at(0), DeliverAfter: h.at(0), ExpiresAt: h.at(24 * time.Hour),
	}
	h.step("append a deferred submit", func() string { return nqCharErr(v.deferred(h.city, deferred)) })
	h.step("append the same deferred submit again", func() string { return nqCharErr(v.deferred(h.city, deferred)) })
	h.finish("withdraw_deferred")
}

func TestNudgeFileQueueCharacterizationMaintenanceAndReads(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	alpha := h.target("s-new", "2")

	// A legacy file: a dead letter with no dead_at, one with a shadow that
	// was never confirmed terminal, and one with no shadow at all.
	h.setState(fmt.Sprintf(`{
  "dead": [
    {"id": "n-zero", "agent": "alpha", "source": "session", "message": "zero dead_at", "created_at": %[1]q, "deliver_after": %[1]q, "expires_at": %[2]q, "last_error": "boom"},
    {"id": "n-shadowless", "agent": "alpha", "source": "session", "message": "no shadow", "created_at": %[1]q, "deliver_after": %[1]q, "expires_at": %[2]q, "last_error": "boom", "dead_at": %[1]q}
  ]
}
`, h.at(-3*time.Hour).Format(time.RFC3339Nano), h.at(21*time.Hour).Format(time.RFC3339Nano)))
	for _, item := range []queuedNudge{
		h.item("n-open-shadow", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-kept", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-pad", " alpha ", 0, queuedNudgeOptions{}),
		h.item("n-lapse", "beta", -2*time.Hour, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	h.failWrites = true
	h.step("a drop whose shadow write fails leaves an open shadow", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-open-shadow"}, errNudgeManualDrop, h.at(time.Second)))
	})
	h.failWrites = false
	h.step("drop n-kept", func() string {
		return nqCharClaim(v.recordFailure(h.city, h.shadow(), []string{"n-kept"}, errNudgeManualDrop, h.at(time.Second)))
	})
	h.step("claim n-lapse an hour ago", func() string {
		return nqCharClaim(v.claimDue(h.city, nudgeTarget{cityPath: h.city, alias: "beta"}, h.at(-time.Hour)))
	})

	h.step("lock-free reads", func() string {
		return fmt.Sprintf("empty=%v alpha_due=%v beta_due=%v beta_due_before_lapse=%v skips=%v",
			v.empty(h.city), v.pollHasDue(alpha, h.at(time.Second)),
			v.pollHasDue(nudgeTarget{cityPath: h.city, alias: "beta"}, h.at(time.Second)),
			v.pollHasDue(nudgeTarget{cityPath: h.city, alias: "beta"}, h.at(-59*time.Minute)),
			v.dispatchSkips(h.city))
	})
	h.step("snapshot projection for alpha", func() string {
		return nqCharBuckets(v.snapshotFor(h.city, alpha, h.at(time.Second)))
	})
	h.step("snapshot projection for beta sees the lapsed lease as pending", func() string {
		return nqCharBuckets(v.snapshotFor(h.city, nudgeTarget{cityPath: h.city, alias: "beta"}, h.at(time.Second)))
	})
	h.step("snapshot projection a day on reads everything expired", func() string {
		return nqCharBuckets(v.snapshotFor(h.city, alpha, h.at(25*time.Hour)))
	})
	h.step("classify for drop", func() string {
		return nqCharStatuses(v.classify(h.city, []string{"n-pad", "n-lapse", "n-kept", "n-nope"}, h.at(2*time.Second)))
	})
	h.step("record dispatch skips", func() string {
		return nqCharErr(v.recordSkips(h.city, map[string]int64{"not-matched": 2, "kill-pending": 1}))
	})
	h.step("record dispatch skips again", func() string {
		return nqCharErr(v.recordSkips(h.city, map[string]int64{"not-matched": 1}))
	})
	h.step("record no dispatch skips", func() string { return nqCharErr(v.recordSkips(h.city, nil)) })
	h.step("read the dispatch skips", func() string { return fmt.Sprint(v.dispatchSkips(h.city)) })
	h.storeOff = true
	h.step("a sweep with no store keeps shadowed dead letters", func() string {
		return nqCharErr(v.sweep(h.city, h.at(2*time.Hour)))
	})
	h.storeOff = false
	h.step("a sweep inside the debounce does nothing", func() string {
		return nqCharErr(v.sweep(h.city, h.at(2*time.Hour+500*time.Millisecond)))
	})
	h.step("a sweep inside the failed payment's backoff leaves the debt", func() string {
		return nqCharErr(v.sweep(h.city, h.at(2*time.Hour+2*time.Second)))
	})
	h.step("a sweep after the backoff repairs and confirms; the next pass prunes", func() string {
		return nqCharErr(v.sweep(h.city, h.at(2*time.Hour+31*time.Second)))
	})
	h.step("list for alpha (status)", func() string {
		return nqCharBuckets(v.listFor(h.city, alpha, h.at(2*time.Hour+32*time.Second)))
	})
	h.step("list for the exact agent", func() string {
		return nqCharBuckets(v.listForAgent(h.city, " alpha ", h.at(2*time.Hour+32*time.Second)))
	})
	h.step("list for the empty agent", func() string {
		return nqCharBuckets(v.listForAgent(h.city, "", h.at(2*time.Hour+32*time.Second)))
	})
	good := h.readState()
	h.setState("{not json\n")
	h.step("an unreadable queue is not empty and has no due work", func() string {
		return fmt.Sprintf("empty=%v alpha_due=%v", v.empty(h.city), v.pollHasDue(alpha, h.at(time.Second)))
	})
	h.setState(good)
	h.finish("maintenance_reads")
}

func TestNudgeFileQueueCharacterizationCLI(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	for _, item := range []queuedNudge{
		h.item("n-1", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-2", "alpha", 0, queuedNudgeOptions{}),
		h.item("n-3", "alpha", 0, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	h.step("claim n-1, n-2 and n-3", func() string {
		return nqCharClaim(v.claimDue(h.city, h.target("", ""), h.at(30*time.Minute)))
	})
	h.step("release n-1 and n-3", func() string {
		return nqCharErr(v.release(h.city, []string{"n-1", "n-3"}, time.Time{}))
	})
	drop := func(json bool, ids ...string) string {
		var stdout, stderr bytes.Buffer
		code := doNudgeDrop(h.city, ids, json, &stdout, &stderr)
		return fmt.Sprintf("exit=%d\nstdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	h.step("gc nudge drop", func() string { return drop(false, "n-1", "n-2", "n-missing") })
	h.step("gc nudge drop of a dead letter", func() string { return drop(false, "n-1") })
	h.step("gc nudge drop --json", func() string { return drop(true, "n-3", "n-1", "n-missing") })
	h.step("queued nudge line", func() string {
		var stdout, stderr bytes.Buffer
		code := writeQueuedSessionNudgeResult(h.target("s-new", "2"), nudgeDeliveryQueue, false, "", nil, &stdout, &stderr)
		return fmt.Sprintf("exit=%d\nstdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	})
	h.finish("cli")
}

// TestNudgeFileQueueCharacterizationDispatch drives the supervisor
// dispatcher's queue reads end to end: its maintenance sweep, the due-key
// pre-check that matches an open session's trimmed queue keys against the raw
// queued agents (so a padded agent matches no session), and the skip
// counters it persists, read back the way gc nudge status reads them.
func TestNudgeFileQueueCharacterizationDispatch(t *testing.T) {
	h := newNQCharHarness(t)
	v := nqCharVerbs
	later := h.item("n-later", "beta", -time.Minute, queuedNudgeOptions{})
	later.DeliverAfter = h.at(time.Hour)
	for _, item := range []queuedNudge{
		h.item("n-alpha", "alpha", -time.Minute, queuedNudgeOptions{}),
		later,
		h.item("n-padded", " gamma ", -time.Minute, queuedNudgeOptions{}),
	} {
		h.step("enqueue "+item.ID, func() string { return nqCharErr(v.enqueue(h.city, h.shadow(), item)) })
	}
	session := func(id, agent string) beads.Bead {
		return beads.Bead{ID: id, Status: "open", Metadata: map[string]string{
			"session_name": agent + "-session", "agent_name": agent, "template": agent,
		}}
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{session("s-alpha", "alpha"), session("s-beta", "beta"), session("s-gamma", "gamma")})
	h.step("dispatch one supervisor pass", func() string {
		delivered, err := dispatchAllQueuedNudges(h.city, supervisorCfg(), h.store(), nil, runtime.NewFake(), snapshot, nil)
		return fmt.Sprintf("delivered=%d err=%s", delivered, nqCharErr(err))
	})
	h.step("read the dispatch skip counters", func() string {
		skips := v.dispatchSkips(h.city)
		keys := make([]string, 0, len(skips))
		for key, count := range skips {
			keys = append(keys, fmt.Sprintf("%s=%d", key, count))
		}
		sort.Strings(keys)
		return "{" + strings.Join(keys, " ") + "}"
	})
	h.finish("dispatch")
}
