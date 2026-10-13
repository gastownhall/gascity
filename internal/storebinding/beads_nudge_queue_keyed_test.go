package storebinding

// The queue's cost model: a target's delivery verbs read only that target's
// records, so their request count is a property of the target, not of the
// queue. These tests count requests through a recording store rather than
// timing anything, and they pin the quarantine that keeps one undecodable
// record from failing deliveries to everyone else.

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// claimingOpRecorder is an OpRecordingStore that also forwards (and counts)
// the two-argument Claim the queue's single-winner claim needs; the recorder
// deliberately does not forward it on its own.
type claimingOpRecorder struct {
	*beadstest.OpRecordingStore
	claimer beadsAssignmentClaimer
	claims  atomic.Int64
}

func (s *claimingOpRecorder) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.claims.Add(1)
	return s.claimer.Claim(id, assignee)
}

// newRecordedNudgeQueue opens a SQLite-backed queue behind a recorder.
func newRecordedNudgeQueue(t *testing.T) (NudgeQueue, *claimingOpRecorder, beads.Store) {
	t.Helper()
	backing := openNudgeQueueStore(t, t.TempDir())
	claimer, ok := backing.(beadsAssignmentClaimer)
	if !ok {
		t.Fatalf("store %T carries no Claim", backing)
	}
	recorder := &claimingOpRecorder{OpRecordingStore: beadstest.NewOpRecordingStore(backing), claimer: claimer}
	return newNudgeQueue(t, recorder), recorder, backing
}

// seedUnrelatedNudges writes n live queue records addressed to other queue
// keys straight into the backing store, so the seeding is not recorded.
func seedUnrelatedNudges(t *testing.T, store beads.Store, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		item := testNudgeItem(fmt.Sprintf("n-unrelated-%d", i), "", now)
		item.Agent = fmt.Sprintf("beta/agent-%d", i%37)
		if _, err := store.Create(nudgeQueueRecord(item, nudgeQueueStateLive, nil)); err != nil {
			t.Fatalf("seeding unrelated nudge %d: %v", i, err)
		}
	}
}

// nudgeQueueRequestProfile is the shape of what one target's delivery verbs
// asked the store: op kinds in order, with each List's label.
func nudgeQueueRequestProfile(ops []beadstest.RecordedOp) []string {
	profile := make([]string, 0, len(ops))
	for _, op := range ops {
		entry := op.Kind
		if op.Kind == beadstest.OpList {
			entry += "(" + op.Query.Label + ")"
		}
		profile = append(profile, entry)
	}
	return profile
}

// runTargetDeliveryVerbs enqueues two nudges for the target, then lists and
// claims for it; it returns the requests the list and claim issued.
func runTargetDeliveryVerbs(t *testing.T, queue NudgeQueue, recorder *claimingOpRecorder, now time.Time) ([]beadstest.RecordedOp, int64) {
	t.Helper()
	for _, item := range []nudgequeue.Item{testNudgeItem("n-mine", "", now), testNudgeItem("n-pinned", "session-live", now)} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	recorder.Reset()
	recorder.claims.Store(0)
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent, "alpha/former"}, SessionID: "session-live"}
	pending, _, _, err := queue.ListFor(target, now)
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("ListFor pending = %v, want the target's two nudges", nudgeItemIDs(pending))
	}
	claimed, err := queue.ClaimDue(target, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("ClaimDue = %v, want the target's two nudges", nudgeItemIDs(claimed))
	}
	return recorder.Ops(), recorder.claims.Load()
}

// TestBeadsNudgeQueueTargetReadsAreKeyed pins the cost model: ListFor and
// ClaimDue for one target issue one keyed List per queue key and never the
// family-wide list, and the request count is the same with a thousand
// unrelated records in the queue as with none.
func TestBeadsNudgeQueueTargetReadsAreKeyed(t *testing.T) {
	now := time.Now().UTC()

	quietQueue, quietRecorder, _ := newRecordedNudgeQueue(t)
	quietOps, quietClaims := runTargetDeliveryVerbs(t, quietQueue, quietRecorder, now)

	busyQueue, busyRecorder, busyBacking := newRecordedNudgeQueue(t)
	seedUnrelatedNudges(t, busyBacking, 1000, now)
	busyOps, busyClaims := runTargetDeliveryVerbs(t, busyQueue, busyRecorder, now)

	for _, op := range busyOps {
		if op.Kind != beadstest.OpList {
			continue
		}
		if !strings.HasPrefix(op.Query.Label, nudgeQueueAgentLabelPrefix) {
			t.Errorf("a target read listed %+v; want only keyed %q lists (caller %v)", op.Query, nudgeQueueAgentLabelPrefix+"<key>", op.Caller)
		}
		if op.Query.IncludeClosed || op.Query.Status == "closed" {
			t.Errorf("a target read listed retired records: %+v", op.Query)
		}
	}
	quiet, busy := nudgeQueueRequestProfile(quietOps), nudgeQueueRequestProfile(busyOps)
	if strings.Join(quiet, ",") != strings.Join(busy, ",") {
		t.Fatalf("requests with 1000 unrelated records = %v, want the same as with none = %v", busy, quiet)
	}
	if quietClaims != busyClaims {
		t.Fatalf("claims with 1000 unrelated records = %d, want %d", busyClaims, quietClaims)
	}
	lists := 0
	for _, entry := range busy {
		if strings.HasPrefix(entry, beadstest.OpList) {
			lists++
		}
	}
	// ListFor and ClaimDue each read the two queue keys once.
	if lists != 4 {
		t.Fatalf("lists = %d (%v), want one per queue key per verb", lists, busy)
	}
}

// TestBeadsNudgeQueueHotVerbsNeverScanTheQueue proves maintenance is off the
// delivery path: no verb but Maintain lists the queue family or its retired
// records, and the reads write nothing.
func TestBeadsNudgeQueueHotVerbsNeverScanTheQueue(t *testing.T) {
	queue, recorder, _ := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}

	first := testNudgeItem("n-first", "", now)
	first.Reference = &reference
	second := testNudgeItem("n-second", "", now)
	second.Reference = &reference
	for _, item := range []nudgequeue.Item{first, second, testNudgeItem("n-other", "", now)} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	if err := queue.EnqueueDeferred(testNudgeItem("n-deferred", "", now)); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	claimed := claimAll(t, queue, now)
	if len(claimed) != 3 {
		t.Fatalf("ClaimDue = %v, want the three live nudges", nudgeItemIDs(claimed))
	}
	if err := queue.Ack([]string{"n-second"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := queue.ReleaseClaims([]string{"n-other"}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	if _, err := queue.RecordFailure([]string{"n-deferred"}, errors.New("refused"), now); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := queue.WithdrawQueuedWaitNudges([]string{"n-other"}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	if err := queue.Rollback(testNudgeItem("n-ghost", "", now), "never landed"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, op := range recorder.Ops() {
		if op.Kind == beadstest.OpList && (op.Query.Label == nudgeQueueLabel || op.Query.IncludeClosed || op.Query.Status == "closed") {
			t.Errorf("a delivery verb scanned the queue: %+v (caller %v)", op.Query, op.Caller)
		}
	}

	recorder.Reset()
	if _, _, _, err := queue.ListFor(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if _, _, _, err := queue.ListForAgent(testNudgeAgent, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	for _, op := range recorder.Ops() {
		if op.Kind != beadstest.OpList && op.Kind != beadstest.OpGet {
			t.Errorf("a read wrote: %s %s (caller %v)", op.Kind, op.ID, op.Caller)
		}
	}
}

// TestBeadsNudgeQueueRecordsCarryTheTargetLabel pins the record shape: a
// session-pinned record is keyed by its session as well as its queue key, an
// unpinned one by its queue key only, and a record written before the target
// label existed still reads.
func TestBeadsNudgeQueueRecordsCarryTheTargetLabel(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-pinned", "session-live", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Enqueue(testNudgeItem("n-unpinned", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	pinned, err := store.Get(nudgeQueueBeadID("n-pinned"))
	if err != nil {
		t.Fatalf("Get pinned: %v", err)
	}
	if !nudgeQueueHasLabel(pinned, nudgeQueueTargetLabelPrefix+"session-live") {
		t.Errorf("pinned record labels = %v, want %q", pinned.Labels, nudgeQueueTargetLabelPrefix+"session-live")
	}
	if !nudgeQueueHasLabel(pinned, nudgeQueueLabel) || !nudgeQueueHasLabel(pinned, nudgeQueueAgentLabelPrefix+testNudgeAgent) {
		t.Errorf("pinned record labels = %v, want the family and queue-key labels kept", pinned.Labels)
	}
	unpinned, err := store.Get(nudgeQueueBeadID("n-unpinned"))
	if err != nil {
		t.Fatalf("Get unpinned: %v", err)
	}
	for _, label := range unpinned.Labels {
		if strings.HasPrefix(label, nudgeQueueTargetLabelPrefix) {
			t.Errorf("unpinned record carries %q, want no target label", label)
		}
	}

	legacy := testNudgeItem("n-legacy", "session-live", now)
	record := nudgeQueueRecord(legacy, nudgeQueueStateLive, nil)
	record.Labels = []string{nudgeQueueLabel, nudgeQueueAgentLabelPrefix + legacy.Agent}
	if _, err := store.Create(record); err != nil {
		t.Fatalf("seeding a record without a target label: %v", err)
	}
	claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}, SessionID: "session-live"}, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if ids := nudgeItemIDs(claimed); !containsNudgeID(ids, "n-legacy") || !containsNudgeID(ids, "n-pinned") {
		t.Fatalf("ClaimDue = %v, want the legacy record served beside the labeled one", ids)
	}
}

// TestBeadsNudgeQueueKeyedReadsIgnoreOtherFamilies pins the family filter: the
// queue-key label is shared with the nudge shadow beads in the same store, and
// a keyed read must not mistake one for a corrupt queue record.
func TestBeadsNudgeQueueKeyedReadsIgnoreOtherFamilies(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if _, err := store.Create(beads.Bead{
		Title:  "nudge:n-shadow",
		Type:   "chore",
		Labels: []string{"gc:nudge", nudgeQueueAgentLabelPrefix + testNudgeAgent},
	}); err != nil {
		t.Fatalf("seeding a shadow-shaped bead: %v", err)
	}
	if err := queue.Enqueue(testNudgeItem("n-real", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	pending, _, _, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-real" {
		t.Fatalf("pending = %v, want only the queue record", nudgeItemIDs(pending))
	}
	if got := queue.(*beadsNudgeQueue).quarantined.Load(); got != 0 {
		t.Fatalf("quarantined = %d, want a bead outside the queue family not counted as corrupt", got)
	}
}

// TestBeadsNudgeQueueQuarantinesACorruptRecord is the isolation property: an
// undecodable record fails nobody's delivery, direct access to it still says
// what it is, and Maintain names it after finishing the rest of its pass.
func TestBeadsNudgeQueueQuarantinesACorruptRecord(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, owner := range []string{testNudgeAgent, "beta/broken"} {
		nudgeID := "n-corrupt-" + strings.ReplaceAll(owner, "/", "-")
		if _, err := store.Create(beads.Bead{
			ID:     nudgeQueueBeadID(nudgeID),
			Title:  "nudge-queue:" + nudgeID,
			Type:   nudgeQueueBeadType,
			Labels: []string{nudgeQueueLabel, nudgeQueueAgentLabelPrefix + owner},
			Metadata: map[string]string{
				nudgeMetaNudgeID:      nudgeID,
				nudgeMetaAgent:        owner,
				nudgeMetaQueueState:   nudgeQueueStateLive,
				nudgeMetaDeliverAfter: "not-a-time",
			},
		}); err != nil {
			t.Fatalf("seeding a corrupt record for %s: %v", owner, err)
		}
	}
	expiring := testNudgeItem("n-expiring", "", now)
	expiring.Agent = "gamma/other"
	expiring.ExpiresAt = now.Add(time.Minute)
	for _, item := range []nudgequeue.Item{testNudgeItem("n-good", "", now), expiring} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}

	pending, _, _, err := queue.ListFor(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err != nil {
		t.Fatalf("ListFor beside a corrupt record: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-good" {
		t.Fatalf("pending = %v, want the good nudge served and the corrupt one skipped", nudgeItemIDs(pending))
	}
	claimed := claimAll(t, queue, now)
	if len(claimed) != 1 || claimed[0].ID != "n-good" {
		t.Fatalf("ClaimDue = %v, want the good nudge claimed past the corrupt one", nudgeItemIDs(claimed))
	}
	if got := queue.(*beadsNudgeQueue).quarantined.Load(); got != 2 {
		t.Fatalf("quarantined = %d, want each skip counted", got)
	}

	if err := queue.Ack([]string{"n-corrupt-alpha-polly"}, "delivered", "", "commit"); !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Ack of the corrupt record = %v, want ErrBeadsNudgeQueueCorrupt", err)
	}
	if _, err := queue.Snapshot(); !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Snapshot = %v, want the whole-queue diagnostic to surface the corrupt record", err)
	}

	afterExpiry := now.Add(2 * time.Minute)
	err = queue.Maintain(afterExpiry)
	if !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Maintain = %v, want the corrupt records reported", err)
	}
	for _, nudgeID := range []string{"n-corrupt-alpha-polly", "n-corrupt-beta-broken"} {
		if !strings.Contains(err.Error(), nudgeQueueBeadID(nudgeID)) {
			t.Errorf("Maintain error %q does not name corrupt record %s", err, nudgeQueueBeadID(nudgeID))
		}
	}
	record, getErr := store.Get(nudgeQueueBeadID("n-expiring"))
	if getErr != nil {
		t.Fatalf("Get expiring: %v", getErr)
	}
	if record.Metadata[nudgeMetaQueueState] != nudgeQueueStateDead {
		t.Fatalf("expiring record queue_state = %q, want Maintain to finish its pass past the corrupt records", record.Metadata[nudgeMetaQueueState])
	}
}

// TestBeadsNudgeQueueClaimRepairsWhatItTouches pins the targeted repair that
// replaced the per-call maintenance pass: an expired candidate is durably
// dead-lettered instead of claimed, while other targets' records are left for
// Maintain.
func TestBeadsNudgeQueueClaimRepairsWhatItTouches(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	mine := testNudgeItem("n-mine", "", now)
	mine.ExpiresAt = now.Add(time.Minute)
	theirs := testNudgeItem("n-theirs", "", now)
	theirs.Agent = "beta/other"
	theirs.ExpiresAt = now.Add(time.Minute)
	for _, item := range []nudgequeue.Item{mine, theirs} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	afterExpiry := now.Add(2 * time.Minute)
	if claimed := claimAll(t, queue, afterExpiry); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want an expired nudge never claimed", nudgeItemIDs(claimed))
	}
	touched, err := store.Get(nudgeQueueBeadID("n-mine"))
	if err != nil {
		t.Fatalf("Get mine: %v", err)
	}
	if touched.Metadata[nudgeMetaQueueState] != nudgeQueueStateDead || touched.Metadata[nudgeMetaLastError] != "expired" {
		t.Fatalf("touched record metadata = %v, want it dead-lettered as expired", touched.Metadata)
	}
	untouched, err := store.Get(nudgeQueueBeadID("n-theirs"))
	if err != nil {
		t.Fatalf("Get theirs: %v", err)
	}
	if untouched.Metadata[nudgeMetaQueueState] != nudgeQueueStateLive {
		t.Fatalf("another target's record queue_state = %q, want it left for Maintain", untouched.Metadata[nudgeMetaQueueState])
	}
	// The read still answers truthfully before Maintain records it.
	_, _, dead, err := queue.ListForAgent("beta/other", afterExpiry)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 1 || dead[0].LastError != "expired" {
		t.Fatalf("dead = %+v, want the expired nudge read as dead before Maintain", dead)
	}
}

// TestBeadsNudgeQueueSupersedeIsScopedToItsQueueKey pins supersession on the
// keyed read: older live items for the same (agent, source, reference) are
// dead-lettered, and another agent's item with the same reference is not.
func TestBeadsNudgeQueueSupersedeIsScopedToItsQueueKey(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}

	old := testNudgeItem("n-old", "", now)
	old.Reference = &reference
	neighbor := testNudgeItem("n-neighbor", "", now)
	neighbor.Agent = "beta/other"
	neighbor.Reference = &reference
	replacement := testNudgeItem("n-new", "", now)
	replacement.Reference = &reference
	for _, item := range []nudgequeue.Item{old, neighbor, replacement} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-new" {
		t.Fatalf("pending = %v, want only the replacement", nudgeItemIDs(pending))
	}
	if len(dead) != 1 || dead[0].ID != "n-old" || dead[0].LastError != "superseded" {
		t.Fatalf("dead = %+v, want the older item superseded", dead)
	}
	pending, _, _, err = queue.ListForAgent("beta/other", now)
	if err != nil {
		t.Fatalf("ListForAgent(neighbor): %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-neighbor" {
		t.Fatalf("neighbor pending = %v, want another agent's item untouched", nudgeItemIDs(pending))
	}
}

func containsNudgeID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
