package opjournal

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

func TestRespondPayloadRoundTripsCanonically(t *testing.T) {
	cases := []RespondPayload{
		{Action: "approve"},
		{Action: "answer", Text: "use the second option"},
		{Action: "deny", Metadata: map[string]string{"b": "2", "a": "1"}},
	}
	for _, p := range cases {
		s, err := EncodeRespondPayload(p)
		if err != nil {
			t.Fatalf("EncodeRespondPayload(%+v): %v", p, err)
		}
		got, err := DecodeRespondPayload(s)
		if err != nil {
			t.Fatalf("DecodeRespondPayload(%q): %v", s, err)
		}
		if got.Action != p.Action || got.Text != p.Text || len(got.Metadata) != len(p.Metadata) {
			t.Fatalf("round trip %+v = %+v", p, got)
		}
		for k, v := range p.Metadata {
			if got.Metadata[k] != v {
				t.Fatalf("metadata %q = %q, want %q", k, got.Metadata[k], v)
			}
		}
	}
	if s, _ := EncodeRespondPayload(RespondPayload{Action: "deny", Metadata: map[string]string{"b": "2", "a": "1"}}); s != `{"action":"deny","metadata":{"a":"1","b":"2"}}` {
		t.Fatalf("encoding is not canonical: %s", s)
	}
}

func TestRespondPayloadRefusesWhatIsNotAnAnswer(t *testing.T) {
	encode := []RespondPayload{
		{},
		{Action: " approve"},
		{Action: "approve\n"},
		{Action: strings.Repeat("a", maxTokenLen+1)},
		{Action: "approve", Metadata: map[string]string{"": "x"}},
	}
	for _, p := range encode {
		if _, err := EncodeRespondPayload(p); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("EncodeRespondPayload(%+v) = %v, want ErrInvalidRequest", p, err)
		}
	}
	decode := []string{
		"",
		"approve",
		`{"action":"approve"} `,
		`{"action":"approve"}{"action":"deny"}`,
		`{"action":"approve","extra":1}`,
		`{"text":"x","action":"approve"}`,
		`{"action":"approve","metadata":{}}`,
		`{"action":""}`,
	}
	for _, s := range decode {
		if _, err := DecodeRespondPayload(s); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("DecodeRespondPayload(%q) = %v, want ErrInvalidRequest", s, err)
		}
	}
}

func TestInteractionOccurrenceNamesOneRaising(t *testing.T) {
	occ, err := InteractionOccurrence("tmux-abc#def", 3)
	if err != nil || occ != "tmux-abc#def#3" {
		t.Fatalf("InteractionOccurrence = %q, %v", occ, err)
	}
	req, n, err := ParseInteractionOccurrence(occ)
	if err != nil || req != "tmux-abc#def" || n != 3 {
		t.Fatalf("ParseInteractionOccurrence(%q) = %q, %d, %v", occ, req, n, err)
	}
	for _, bad := range []string{"", "req", "req#", "#1", "req#0", "req#01", "req#-1", "req#x", "req#1048577"} {
		if _, _, err := ParseInteractionOccurrence(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ParseInteractionOccurrence(%q) = %v, want ErrInvalidRequest", bad, err)
		}
	}
	for _, c := range []struct {
		req string
		n   int
	}{{"", 1}, {"r", 0}, {"r", MaxRaisings + 1}, {strings.Repeat("r", maxClientKeyLen), 1}} {
		if _, err := InteractionOccurrence(c.req, c.n); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("InteractionOccurrence(%d bytes, %d) = %v, want ErrInvalidRequest", len(c.req), c.n, err)
		}
	}
}

func TestRespondKeyIsTheRaisedFactID(t *testing.T) {
	f := Fact{Kind: FactInteractionRaised, Conversation: "gc-1", Generation: 2, Interaction: "perm-1#1", Basis: FactBasisObserved, At: t0}
	id, err := FactID("gc", f)
	if err != nil {
		t.Fatal(err)
	}
	f.ID = id
	key, err := RespondKey(f)
	if err != nil || key != "respond:"+id {
		t.Fatalf("RespondKey = %q, %v; want respond:%s", key, err, id)
	}
	bad := map[string]Fact{
		"no id":         func() Fact { g := f; g.ID = ""; return g }(),
		"resolved fact": func() Fact { g := f; g.Kind = FactInteractionResolved; return g }(),
		// A resolved fact under its own, consistent id: only the kind
		// tells it from the raising.
		"resolved fact with its id": func() Fact {
			g := f
			g.Kind = FactInteractionResolved
			g.ID, _ = FactID("gc", g)
			return g
		}(),
		"id of another": func() Fact { g := f; g.Interaction = "perm-1#2"; return g }(),
		"bad raising":   func() Fact { g := f; g.Interaction = "perm-1"; return g }(),
		// A request id with no raising number under its own, consistent
		// id: only the occurrence check refuses it.
		"bad raising with its id": func() Fact {
			g := f
			g.Interaction = "perm-1"
			g.ID, _ = FactID("gc", g)
			return g
		}(),
	}
	for name, g := range bad {
		if _, err := RespondKey(g); err == nil {
			t.Errorf("%s: RespondKey accepted %+v", name, g)
		}
	}
}

func TestRecordRaisingNumbersEachRaising(t *testing.T) {
	h := newLaneHarness(t)
	s := h.store
	if _, found, err := s.LatestRaising("gc-1", 1, "tmux-x"); err != nil || found {
		t.Fatalf("LatestRaising before any = found %v, %v", found, err)
	}
	first, created, err := s.RecordRaising("gc-1", 1, "tmux-x", FactBasisObserved, t0)
	if err != nil || !created || first.Raised.Interaction != "tmux-x#1" || first.Resolved {
		t.Fatalf("first RecordRaising = %+v, created %v, %v", first, created, err)
	}
	// The same raising seen again is the same occurrence.
	again, created, err := s.RecordRaising("gc-1", 1, "tmux-x", FactBasisObserved, t0.Add(time.Second))
	if err != nil || created || again.Raised.ID != first.Raised.ID {
		t.Fatalf("second look = %+v, created %v, %v", again, created, err)
	}
	// Once it is resolved, the same request id raised again is a new
	// occurrence.
	if _, err := s.RecordFact(Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: "tmux-x#1", Basis: FactBasisLane, At: t0}); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.LatestRaising("gc-1", 1, "tmux-x"); err != nil || !found || !got.Resolved || got.Raised.ID != first.Raised.ID {
		t.Fatalf("LatestRaising after resolve = %+v, found %v, %v", got, found, err)
	}
	second, created, err := s.RecordRaising("gc-1", 1, "tmux-x", FactBasisObserved, t0.Add(2*time.Second))
	if err != nil || !created || second.Raised.Interaction != "tmux-x#2" || second.Raised.ID == first.Raised.ID {
		t.Fatalf("re-raise = %+v, created %v, %v", second, created, err)
	}
	// Another generation numbers its own raisings.
	other, created, err := s.RecordRaising("gc-1", 2, "tmux-x", FactBasisObserved, t0)
	if err != nil || !created || other.Raised.Interaction != "tmux-x#1" {
		t.Fatalf("generation 2 = %+v, created %v, %v", other, created, err)
	}
}

// The latest raising is found by a galloping search over the pinned fact
// ids, so a request raised many times costs a few reads, not one per
// raising.
func TestLatestRaisingFindsTheLastOfMany(t *testing.T) {
	h := newLaneHarness(t)
	const n = 37
	for i := 1; i <= n; i++ {
		occ, _ := InteractionOccurrence("tmux-x", i)
		for _, k := range []FactKind{FactInteractionRaised, FactInteractionResolved} {
			if _, err := h.store.RecordFact(Fact{Kind: k, Conversation: "gc-1", Generation: 1, Interaction: occ, Basis: FactBasisObserved, At: t0}); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := len(h.rec.Ops())
	got, found, err := h.store.LatestRaising("gc-1", 1, "tmux-x")
	if err != nil || !found || got.Raised.Interaction != "tmux-x#37" || !got.Resolved {
		t.Fatalf("LatestRaising = %+v, found %v, %v", got, found, err)
	}
	if reads := len(h.rec.Ops()) - before; reads > 16 {
		t.Fatalf("LatestRaising read %d rows for %d raisings", reads, n)
	}
}

// respondReq is the operator's lane respond on conversation gc-1's
// generation 1 answering occurrence occ, keyed by the occurrence's raised
// fact under prefix ("k" when prefix is empty, or occ is not an occurrence).
func respondReq(occ, prefix string, p RespondPayload) Request {
	payload, _ := EncodeRespondPayload(p)
	key := "k"
	if prefix != "" {
		if id, err := FactID(prefix, Fact{Kind: FactInteractionRaised, Conversation: "gc-1", Generation: 1, Interaction: occ}); err == nil {
			key = "respond:" + id
		}
	}
	return Request{
		Conversation: "gc-1", Kind: KindRespond, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "operator", Actor: ActorOperator}, Payload: payload,
		Scope: Scope{Kind: ScopeSession, Generation: 1, Interaction: occ},
	}
}

func TestProposeRespondIsKeyedByItsOccurrence(t *testing.T) {
	h := newLaneHarness(t)
	approve := RespondPayload{Action: "approve"}
	op := h.propose(t, respondReq("perm-1#1", "gc", approve))
	// The same answer again is the same op.
	if again, created, err := h.store.Propose(respondReq("perm-1#1", "gc", approve)); err != nil || created || again.ID != op.ID {
		t.Fatalf("same answer = %s, created %v, %v", again.ID, created, err)
	}
	// A different answer to the same occurrence is key reuse: the first
	// answer wins.
	if _, _, err := h.store.Propose(respondReq("perm-1#1", "gc", RespondPayload{Action: "deny"})); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("different answer = %v, want ErrKeyReuse", err)
	}
	// The next raising of the same request id is another occurrence, so its
	// answer is another op.
	next := h.propose(t, respondReq("perm-1#2", "gc", approve))
	if next.ID == op.ID {
		t.Fatal("the second raising's answer reused the first raising's op")
	}
	refused := map[string]Request{
		"request-id key": func() Request {
			r := respondReq("perm-1#3", "gc", approve)
			r.ClientKey = "respond:1:perm-1"
			return r
		}(),
		"key of another occurrence": func() Request {
			r := respondReq("perm-1#3", "gc", approve)
			r.Scope.Interaction = "perm-1#4"
			return r
		}(),
		"request id, not an occurrence": func() Request {
			r := respondReq("perm-1#3", "gc", approve)
			r.Scope.Interaction = "perm-1"
			return r
		}(),
		"payload not json": func() Request {
			r := respondReq("perm-1#3", "gc", approve)
			r.Payload = "allow"
			return r
		}(),
		"payload not canonical": func() Request {
			r := respondReq("perm-1#3", "gc", approve)
			r.Payload = `{ "action": "approve" }`
			return r
		}(),
	}
	for name, r := range refused {
		if _, _, err := h.store.Propose(r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Propose = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// A legacy respond op keeps its client key (DES-O0API §3.2).
func TestProposeLegacyRespondKeepsItsClientKey(t *testing.T) {
	h := newLaneHarness(t)
	r := respondReq("perm-1", "", RespondPayload{Action: "approve"})
	r.Executor, r.ClientKey = ExecutorLegacyAPI, "k-client"
	if op := h.propose(t, r); op.ClientKey != "k-client" {
		t.Fatalf("client key %q, want k-client", op.ClientKey)
	}
}

// A legacy /respond row the build before OJ-16 recorded (text always present,
// at_least_once) still reads, keeps the request digest that build gave it,
// so a same-key retry across the upgrade replays it, and expires as any
// legacy op: none of it is quarantined as corrupt.
func TestLegacyRespondRowsOfThePreviousBuildStillRead(t *testing.T) {
	h := newHarness(t)
	// The digests are the previous build's (b44d3097eb) for these requests.
	rows := []struct{ payload, digest string }{
		{`{"action":"approve","text":""}`, "b3eadcff1510ae498c0139e8a49d8db7008eeb9f4ba555186eb6369ad3c47678"},
		{`{"action":"approve","text":"yes"}`, "0cc6fbabba6b14b87350ac6d6d9554a1efc14652ab74219709db36d390f71673"},
	}
	var want []string
	for i, row := range rows {
		r := legacyReq(ExecutorLegacyAPI, fmt.Sprintf("k-client-%d", i))
		r.Kind, r.Payload = KindRespond, row.payload
		r.Policy = Policy{Guarantee: GuaranteeAtLeastOnce}
		r.Scope = Scope{Kind: ScopeSession, Generation: 1, Interaction: "req-1"}
		op := h.propose(t, r)
		if op.PayloadDigest != row.digest {
			t.Fatalf("%s: digest %s, want the previous build's %s", row.payload, op.PayloadDigest, row.digest)
		}
		if got, err := h.store.Get(op.ID); err != nil || got.Payload != row.payload {
			t.Fatalf("%s: Get = %q, %v", row.payload, got.Payload, err)
		}
		if again, created, err := h.store.Propose(r); err != nil || created || again.ID != op.ID {
			t.Fatalf("%s: same-key retry = %s, created %v, %v; want %s replayed", row.payload, again.ID, created, err, op.ID)
		}
		want = append(want, op.ID)
	}
	scan, err := h.store.ListExpiredLegacy(t0.Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(scan.Expired)) != fmt.Sprint(want) || len(scan.Undecodable) != 0 {
		t.Fatalf("ListExpiredLegacy = expired %v undecodable %v, want %v expired", ids(scan.Expired), scan.Undecodable, want)
	}
}

// raise records the raising of request id req that conversation gc-1's
// generation 1 shows now, as a producer or the lane would.
func (h *laneHarness) raise(t *testing.T, req string) Fact {
	t.Helper()
	r, _, err := h.store.RecordRaising("gc-1", 1, req, FactBasisObserved, h.clk.Now())
	if err != nil {
		t.Fatalf("RecordRaising(%s): %v", req, err)
	}
	return r.Raised
}

// laneRespond is principal's lane respond to raised, keyed by it.
func laneRespond(t *testing.T, raised Fact, principal string, p RespondPayload) Request {
	t.Helper()
	key, err := RespondKey(raised)
	if err != nil {
		t.Fatalf("RespondKey: %v", err)
	}
	payload, err := EncodeRespondPayload(p)
	if err != nil {
		t.Fatalf("EncodeRespondPayload: %v", err)
	}
	return Request{
		Conversation: raised.Conversation, Kind: KindRespond, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: principal, Actor: ActorOperator}, Payload: payload,
		Scope: Scope{Kind: ScopeSession, Generation: raised.Generation, Interaction: raised.Interaction},
	}
}

// A lane respond is at most once by default, and nothing else: a second
// attempt could answer a later, identical prompt (finding #1). A legacy
// respond keeps the guarantee it names.
func TestLaneRespondIsAtMostOnce(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, respondReq("perm-1#1", "gc", RespondPayload{Action: "approve"}))
	if op.Policy.Guarantee != GuaranteeAtMostOnce {
		t.Fatalf("default guarantee %s", op.Policy.Guarantee)
	}
	r := respondReq("perm-1#2", "gc", RespondPayload{Action: "approve"})
	r.Policy.Guarantee = GuaranteeAtLeastOnce
	if _, _, err := h.store.Propose(r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an at_least_once lane respond = %v, want ErrInvalidRequest", err)
	}
	legacy := respondReq("perm-1", "", RespondPayload{Action: "approve"})
	legacy.Executor, legacy.ClientKey, legacy.Policy.Guarantee = ExecutorLegacyAPI, "k-legacy", GuaranteeAtLeastOnce
	if got := h.propose(t, legacy); got.Policy.Guarantee != GuaranteeAtLeastOnce {
		t.Fatalf("legacy guarantee %s", got.Policy.Guarantee)
	}
}

// A stored lane respond this build cannot read (a payload field a newer
// build added, an at_least_once guarantee, a scope naming no occurrence) is
// quarantined, never closed (DES-O1 §4.1; finding #8).
func TestLaneRespondUnreadableRowIsQuarantined(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"newer payload": {keyPayload: `{"action":"approve","later":1}`},
		"at least once": {keyGuarantee: string(GuaranteeAtLeastOnce)},
		"no occurrence": {keyScopeInteraction: "tmux-abc"},
		"not an answer": {keyPayload: "approve"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneRespond(t, h.raise(t, "tmux-abc"), "operator", RespondPayload{Action: "approve"}))
			for k, v := range meta {
				if err := h.mem.SetMetadata(op.ID, k, v); err != nil {
					t.Fatal(err)
				}
			}
			ops, quarantined, err := h.store.ListOpenTolerant("gc-1")
			if err != nil || len(ops) != 0 || len(quarantined) != 1 || quarantined[0].ID != op.ID {
				t.Fatalf("ListOpenTolerant = %d ops, quarantined %+v, %v", len(ops), quarantined, err)
			}
			exec := respondExec(h, "tmux-abc")
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if len(res.Quarantined) != 1 || len(res.Closed) != 0 || len(exec.responds) != 0 {
				t.Fatalf("pass %+v", res)
			}
			b, err := h.mem.Get(op.ID)
			if err != nil || b.Metadata[keyStatus] == string(StatusClosed) {
				t.Fatalf("the quarantined row was written: %+v, %v", b.Metadata, err)
			}
		})
	}
}

// hidingStore answers a Get of one id ErrNotFound once, as a read that ran
// just before another observer's write would.
type hidingStore struct {
	*beads.MemStore
	mu   sync.Mutex
	hide string
}

func (s *hidingStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s.MemStore, true }

func (s *hidingStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	hidden := id == s.hide
	if hidden {
		s.hide = ""
	}
	s.mu.Unlock()
	if hidden {
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.MemStore.Get(id)
}

// RecordRaising that loses its create race to another observer answers from
// the store: the raising as recorded, and whether it is resolved by now
// (finding #12).
func TestRecordRaisingAfterALostRaceReadsTheStore(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	hs := &hidingStore{MemStore: mem}
	st, err := NewLaneStore(beads.SessionStore{Store: hs}, &clock.Fake{Time: t0}, WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := st.RecordRaising("gc-1", 1, "tmux-x", FactBasisObserved, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordFact(Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: "tmux-x#1", Basis: FactBasisLane, At: t0}); err != nil {
		t.Fatal(err)
	}
	// The latest-raising read misses #1, so this call numbers the raising
	// #1 and finds it already recorded.
	hs.mu.Lock()
	hs.hide = first.Raised.ID
	hs.mu.Unlock()
	got, created, err := st.RecordRaising("gc-1", 1, "tmux-x", FactBasisObserved, t0.Add(time.Minute))
	if err != nil || created || got.Raised.ID != first.Raised.ID || !got.Resolved || !got.Raised.At.Equal(t0) {
		t.Fatalf("RecordRaising after a lost race = %+v, created %v, %v; want the stored, resolved raising", got, created, err)
	}
}
