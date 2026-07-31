package cityparity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/pkg/cityartifact"
	"github.com/gastownhall/gascity/pkg/cityinference"
	"github.com/gastownhall/gascity/pkg/cityneutral"
)

// trio is the three adapters wired to each other the way a deployment wires
// them: through server-minted Team IDs and nothing else. Each holds its own
// API, its own checkpoint store and its own source epoch.
type trio struct {
	nf *cityneutral.Fake
	np *cityneutral.Producer

	af *cityartifact.Fake
	ap *cityartifact.Producer

	iapi *cityinference.FakeAPI
	ip   *cityinference.Producer

	runID, sessID, recID string
	baseArtifactID       string
}

func newTrio(t *testing.T) *trio {
	t.Helper()
	ctx := context.Background()
	s := cityStyle
	tr := &trio{}

	tr.nf = cityneutral.NewFake(s.uploader, s.sourceID, s.sourceKind)
	np, err := cityneutral.NewProducer(tr.nf, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1},
		AllowRawContent: true,
	}, cityneutral.NewMemoryStore())
	if err != nil {
		t.Fatalf("neutral NewProducer: %v", err)
	}
	tr.np = np
	res, err := np.Push(ctx, s.chain())
	if err != nil {
		t.Fatalf("neutral base Push: %v", err)
	}
	tr.runID, tr.sessID = res.RunTeamID, res.Sessions[0].TeamID
	records, err := tr.nf.ListTranscriptRecords(ctx, tr.sessID)
	if err != nil || len(records) == 0 {
		t.Fatalf("base transcript: %v (%d records)", err, len(records))
	}
	tr.recID = records[0].ID

	tr.af = cityartifact.NewFake()
	tr.af.Authorize(linkedProject, linkedIssue, tr.runID, tr.sessID)
	ap, err := cityartifact.NewProducer(tr.af.Client(s.sourceID, s.sourceKind),
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}},
		cityartifact.NewMemoryStore())
	if err != nil {
		t.Fatalf("artifact NewProducer: %v", err)
	}
	tr.ap = ap
	ares, err := ap.Push(ctx, s.artifact(tr.runID, tr.sessID))
	if err != nil {
		t.Fatalf("artifact base Push: %v", err)
	}
	tr.baseArtifactID = ares.ArtifactID

	tr.iapi = newInferenceAPI(tr.runID, tr.sessID, tr.recID)
	ip, err := cityinference.NewProducer(tr.iapi,
		cityinference.Mapper{Source: inferenceSource(s)}, cityinference.NewMemoryStore())
	if err != nil {
		t.Fatalf("inference NewProducer: %v", err)
	}
	tr.ip = ip
	if _, err := ip.Push(ctx, []cityinference.CityInvocation{s.invocation(tr.runID, tr.sessID, tr.recID)}); err != nil {
		t.Fatalf("inference base Push: %v", err)
	}
	return tr
}

// nextRound is one fresh unit of work per adapter. Fresh work matters: a replay
// of already-acknowledged work is skipped without a call, so it would sail past
// an injected transport fault and prove nothing.
func (tr *trio) nextRound(tag string) (cityneutral.CityChain, cityartifact.CityArtifact, cityinference.CityInvocation) {
	s := cityStyle
	chain := s.chain()
	chain.Run.RunID = "city-run-" + tag
	chain.Sessions[0].SessionID = "city-sess-" + tag

	art := s.artifact(tr.runID, tr.sessID)
	art.ArtifactID = "city-artifact-" + tag

	inv := s.invocation(tr.runID, tr.sessID, tr.recID)
	inv.UpstreamReqID = "req-" + tag
	return chain, art, inv
}

// pushRound offers each adapter its own fresh work and reports each outcome
// separately. No adapter's failure is allowed to short-circuit another's push,
// because that is exactly the coupling under test.
func (tr *trio) pushRound(tag string) (nErr, aErr, iErr error) {
	ctx := context.Background()
	chain, art, inv := tr.nextRound(tag)
	_, nErr = tr.np.Push(ctx, chain)
	_, aErr = tr.ap.Push(ctx, art)
	_, iErr = tr.ip.Push(ctx, []cityinference.CityInvocation{inv})
	return nErr, aErr, iErr
}

// assertBaseSurvives checks that everything acknowledged before the fault is
// still exactly as the server accepted it.
func (tr *trio) assertBaseSurvives(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := tr.nf.GetRun(ctx, tr.runID); err != nil {
		t.Errorf("acknowledged run no longer readable: %v", err)
	}
	sess, err := tr.nf.GetSession(ctx, tr.sessID)
	if err != nil {
		t.Errorf("acknowledged session no longer readable: %v", err)
	} else if !sess.Finalized {
		t.Error("acknowledged session lost its finalized state")
	}
	records, err := tr.nf.ListTranscriptRecords(ctx, tr.sessID)
	if err != nil || len(records) != 3 {
		t.Errorf("acknowledged transcript changed: %v (%d records)", err, len(records))
	}
	art, err := tr.af.Client(cityStyle.sourceID, cityStyle.sourceKind).GetArtifact(ctx, tr.baseArtifactID)
	if err != nil {
		t.Errorf("acknowledged artifact no longer readable: %v", err)
	} else if art.Status != "final" {
		t.Errorf("acknowledged artifact left status %q", art.Status)
	}
	page, err := tr.iapi.ListInferences(ctx, cityinference.ListFilter{})
	if err != nil || len(page.Items) == 0 {
		t.Errorf("acknowledged inference no longer listed: %v (%d items)", err, len(page.Items))
	}
}

// AC2 happy path: the fault-domain matrix. One adapter faults; the other two
// land their own fresh work in the same round, and every acknowledged record
// survives. The faulted adapter then recovers on retry without the others
// having been touched.
func TestFaultDomainMatrix(t *testing.T) {
	boom := errors.New("upstream said no")
	cases := []struct {
		name    string
		inject  func(tr *trio)
		wantErr func(nErr, aErr, iErr error) (others []error, faulted error)
	}{
		{
			name:   "neutral",
			inject: func(tr *trio) { tr.nf.Fail = boom },
			wantErr: func(n, a, i error) ([]error, error) {
				return []error{a, i}, n
			},
		},
		{
			name:   "artifact",
			inject: func(tr *trio) { tr.af.FailNext(cityartifact.OpCreateArtifact, boom) },
			wantErr: func(n, a, i error) ([]error, error) {
				return []error{n, i}, a
			},
		},
		{
			name:   "inference",
			inject: func(tr *trio) { tr.iapi.FailNext = boom },
			wantErr: func(n, a, i error) ([]error, error) {
				return []error{n, a}, i
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTrio(t)
			tc.inject(tr)

			others, faulted := tc.wantErr(tr.pushRound("r2"))
			if faulted == nil {
				t.Fatal("injected fault did not surface as a refusal")
			}
			for i, err := range others {
				if err != nil {
					t.Errorf("a neighboring adapter was dragged down by the %s fault (%d): %v", tc.name, i, err)
				}
			}
			tr.assertBaseSurvives(t)

			// The fault was one-shot, so the same offer must now land: a
			// refusal leaves no half-advanced checkpoint behind.
			retryOthers, retryFaulted := tc.wantErr(tr.pushRound("r2"))
			if retryFaulted != nil {
				t.Errorf("%s adapter did not recover after the fault cleared: %v", tc.name, retryFaulted)
			}
			for i, err := range retryOthers {
				if err != nil {
					t.Errorf("replaying a neighboring adapter's round was not idempotent (%d): %v", i, err)
				}
			}
			tr.assertBaseSurvives(t)
		})
	}
}

// AC2: lag is not a fault. One adapter simply not pushing must leave the others
// free to advance, which is the weakest form of independence and the one a
// shared checkpoint would break first.
func TestOneAdapterLaggingDoesNotBlockTheOthers(t *testing.T) {
	ctx := context.Background()
	tr := newTrio(t)
	chain, art, inv := tr.nextRound("lag")
	_ = chain // the neutral adapter is the lagging one: it never offers this chain.

	if _, err := tr.ap.Push(ctx, art); err != nil {
		t.Errorf("artifact adapter blocked behind a lagging neutral adapter: %v", err)
	}
	if _, err := tr.ip.Push(ctx, []cityinference.CityInvocation{inv}); err != nil {
		t.Errorf("inference adapter blocked behind a lagging neutral adapter: %v", err)
	}
	if _, err := tr.nf.GetRun(ctx, tr.runID); err != nil {
		t.Errorf("lagging adapter's acknowledged run was disturbed: %v", err)
	}
}

// AC2: a credential rotation is not an identity change. Every adapter must
// resume on the same checkpoint and write nothing twice, because no adapter's
// checkpoint key may contain anything that a rotation changes.
func TestCredentialRotationChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := cityStyle
	tr := newTrio(t)

	nStore := cityneutral.NewMemoryStore()
	np, err := cityneutral.NewProducer(tr.nf, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1},
		AllowRawContent: true,
	}, nStore)
	if err != nil {
		t.Fatalf("neutral NewProducer: %v", err)
	}
	// A rotation looks like a restart with an empty local checkpoint: the
	// producer re-offers, and the server's idempotency is what makes the
	// re-offer free of duplicates.
	res, err := np.Push(ctx, s.chain())
	if err != nil {
		t.Fatalf("neutral push after rotation: %v", err)
	}
	if res.RunTeamID != tr.runID || res.Sessions[0].TeamID != tr.sessID {
		t.Errorf("rotation re-minted neutral identity: run %q->%q session %q->%q",
			tr.runID, res.RunTeamID, tr.sessID, res.Sessions[0].TeamID)
	}
	records, err := tr.nf.ListTranscriptRecords(ctx, tr.sessID)
	if err != nil || len(records) != 3 {
		t.Errorf("rotation duplicated transcript records: %v (%d)", err, len(records))
	}

	rotated := tr.af.Client(s.sourceID, s.sourceKind)
	ap, err := cityartifact.NewProducer(rotated,
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}},
		cityartifact.NewMemoryStore())
	if err != nil {
		t.Fatalf("artifact NewProducer: %v", err)
	}
	ares, err := ap.Push(ctx, s.artifact(tr.runID, tr.sessID))
	if err != nil {
		t.Fatalf("artifact push after rotation: %v", err)
	}
	if ares.ArtifactID != tr.baseArtifactID {
		t.Errorf("rotation re-minted artifact identity: %q -> %q", tr.baseArtifactID, ares.ArtifactID)
	}
	if got := tr.af.ArtifactCount(); got != 1 {
		t.Errorf("rotation created a second artifact: count=%d", got)
	}
	if got := tr.af.PartCount(tr.baseArtifactID); got != 2 {
		t.Errorf("rotation duplicated parts: count=%d", got)
	}

	tr.iapi.Credential = "rotated-bearer"
	writesBefore := tr.iapi.Writes()
	ip, err := cityinference.NewProducer(tr.iapi,
		cityinference.Mapper{Source: inferenceSource(s)}, cityinference.NewMemoryStore())
	if err != nil {
		t.Fatalf("inference NewProducer: %v", err)
	}
	if _, err := ip.Push(ctx, []cityinference.CityInvocation{s.invocation(tr.runID, tr.sessID, tr.recID)}); err != nil {
		t.Fatalf("inference push after rotation: %v", err)
	}
	page, err := tr.iapi.ListInferences(ctx, cityinference.ListFilter{})
	if err != nil {
		t.Fatalf("list after rotation: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("rotation duplicated the inference record: %d items (writes %d -> %d)",
			len(page.Items), writesBefore, tr.iapi.Writes())
	}
}

// AC2 / FINDING: the three adapters do NOT agree on what a declared reset does.
//
// A declared reset — the source epoch advancing — is the loud signal the event
// contract lacks, and all three adapters put the epoch in their idempotency
// preimage, so all three agree that a reset re-keys identity. They disagree on
// what to do next: cityneutral and cityartifact restart the checkpoint and keep
// producing, while cityinference refuses the push. The inference refusal never
// writes a new epoch to its checkpoint either, so it is not a pause: the
// adapter stays refused until an operator deletes the checkpoint out of band.
//
// This test asserts what the three adapters do today. It passes, and that is
// the point: the divergence is real, it is a certification finding against
// API-7.23-AC2, and this name is its citation.
func TestDeclaredResetIsNotHandledUniformly(t *testing.T) {
	ctx := context.Background()
	s := cityStyle

	nStore := cityneutral.NewMemoryStore()
	aStore := cityartifact.NewMemoryStore()
	iStore := cityinference.NewMemoryStore()

	nf := cityneutral.NewFake(s.uploader, s.sourceID, s.sourceKind)
	np1, err := cityneutral.NewProducer(nf, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1},
		AllowRawContent: true,
	}, nStore)
	if err != nil {
		t.Fatalf("neutral NewProducer: %v", err)
	}
	// The chain and the artifact stay open across the reset. A reset that
	// replays terminal work is refused for a different reason (the record is
	// finalized), and that refusal would mask the epoch semantics under test.
	open := s.chain()
	open.Sessions[0].Complete = false
	openArtifact := s.artifact("", "")
	openArtifact.Complete = false

	res, err := np1.Push(ctx, open)
	if err != nil {
		t.Fatalf("neutral epoch-1 push: %v", err)
	}
	runID, sessID := res.RunTeamID, res.Sessions[0].TeamID

	af := cityartifact.NewFake()
	af.Authorize(linkedProject, linkedIssue, runID, sessID)
	ap1, err := cityartifact.NewProducer(af.Client(s.sourceID, s.sourceKind),
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}}, aStore)
	if err != nil {
		t.Fatalf("artifact NewProducer: %v", err)
	}
	openArtifact.RunID, openArtifact.SessionID = runID, sessID
	if _, err := ap1.Push(ctx, openArtifact); err != nil {
		t.Fatalf("artifact epoch-1 push: %v", err)
	}

	records, err := nf.ListTranscriptRecords(ctx, sessID)
	if err != nil || len(records) == 0 {
		t.Fatalf("transcript: %v (%d)", err, len(records))
	}
	recID := records[0].ID
	iapi := newInferenceAPI(runID, sessID, recID)
	ip1, err := cityinference.NewProducer(iapi, cityinference.Mapper{Source: inferenceSource(s)}, iStore)
	if err != nil {
		t.Fatalf("inference NewProducer: %v", err)
	}
	if _, err := ip1.Push(ctx, []cityinference.CityInvocation{s.invocation(runID, sessID, recID)}); err != nil {
		t.Fatalf("inference epoch-1 push: %v", err)
	}

	// The reset: epoch 2, same enrolled source, same durable checkpoints.
	np2, err := cityneutral.NewProducer(nf, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 2},
		AllowRawContent: true,
	}, nStore)
	if err != nil {
		t.Fatalf("neutral NewProducer epoch 2: %v", err)
	}
	if _, err := np2.Push(ctx, open); err != nil {
		t.Errorf("cityneutral no longer absorbs a declared reset: %v", err)
	}

	ap2, err := cityartifact.NewProducer(af.Client(s.sourceID, s.sourceKind),
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 2}}, aStore)
	if err != nil {
		t.Fatalf("artifact NewProducer epoch 2: %v", err)
	}
	if _, err := ap2.Push(ctx, openArtifact); err != nil {
		t.Errorf("cityartifact no longer absorbs a declared reset: %v", err)
	}

	iSource := inferenceSource(s)
	iSource.Epoch = 2
	ip2, err := cityinference.NewProducer(iapi, cityinference.Mapper{Source: iSource}, iStore)
	if err != nil {
		t.Fatalf("inference NewProducer epoch 2: %v", err)
	}
	_, err = ip2.Push(ctx, []cityinference.CityInvocation{s.invocation(runID, sessID, recID)})
	if !errors.Is(err, cityinference.ErrIdentityDrift) {
		t.Fatalf("cityinference reset behavior changed: want ErrIdentityDrift, got %v", err)
	}

	// And the refusal is durable: the checkpoint still holds epoch 1, so every
	// later push under the declared epoch is refused the same way. Two adapters
	// resume by themselves and one needs an operator.
	if _, err := ip2.Push(ctx, []cityinference.CityInvocation{s.invocation(runID, sessID, recID)}); !errors.Is(err, cityinference.ErrIdentityDrift) {
		t.Fatalf("second push after reset: want a durable ErrIdentityDrift, got %v", err)
	}
	st, ok, err := iStore.Load(ctx, cityinference.CheckpointKey(iSource))
	if err != nil || !ok {
		t.Fatalf("inference checkpoint after reset: %v (found=%t)", err, ok)
	}
	if st.Epoch != 1 {
		t.Fatalf("inference checkpoint epoch = %d, want the refused push to have left it at 1", st.Epoch)
	}
}

// AC2 / FINDING: only cityinference has a programmatic rollback switch. The
// other two roll back by the caller ceasing to call Push, which is a rollback
// of the scheduler and not of the adapter. Rolling back one adapter is
// supported in all three cases; doing it the same way is not.
func TestRollbackSwitchIsNotUniform(t *testing.T) {
	ctx := context.Background()
	tr := newTrio(t)
	tr.ip.Disabled = true

	_, art, inv := tr.nextRound("rb")
	if _, err := tr.ip.Push(ctx, []cityinference.CityInvocation{inv}); !errors.Is(err, cityinference.ErrDisabled) {
		t.Fatalf("disabled inference producer: want ErrDisabled, got %v", err)
	}
	if _, err := tr.ap.Push(ctx, art); err != nil {
		t.Errorf("artifact adapter stopped when the inference adapter rolled back: %v", err)
	}
	tr.assertBaseSurvives(t)

	writes := tr.iapi.Writes()
	if _, err := tr.ip.Push(ctx, []cityinference.CityInvocation{inv}); !errors.Is(err, cityinference.ErrDisabled) {
		t.Fatalf("rollback is not sticky: %v", err)
	}
	if got := tr.iapi.Writes(); got != writes {
		t.Errorf("a rolled-back adapter still issued writes: %d -> %d", writes, got)
	}
}

// sharedKV is one durable key-value store shared by two adapters, which is what
// an operator gets by handing both the same backing table.
type sharedKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newSharedKV() *sharedKV { return &sharedKV{m: map[string][]byte{}} }

func (kv *sharedKV) get(key string) ([]byte, bool) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	b, ok := kv.m[key]
	return b, ok
}

func (kv *sharedKV) put(key string, b []byte) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.m[key] = b
}

type neutralKV struct{ kv *sharedKV }

func (s neutralKV) Load(_ context.Context, key string) (cityneutral.State, bool, error) {
	var st cityneutral.State
	b, ok := s.kv.get(key)
	if !ok {
		return st, false, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, false, err
	}
	return st, true, nil
}

func (s neutralKV) Save(_ context.Context, key string, st cityneutral.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.kv.put(key, b)
	return nil
}

type artifactKV struct{ kv *sharedKV }

func (s artifactKV) Load(_ context.Context, key string) (cityartifact.State, bool, error) {
	var st cityartifact.State
	b, ok := s.kv.get(key)
	if !ok {
		return st, false, nil
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, false, err
	}
	return st, true, nil
}

func (s artifactKV) Save(_ context.Context, key string, st cityartifact.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.kv.put(key, b)
	return nil
}

// AC2 edge / FINDING: cityneutral and cityartifact mint the SAME checkpoint key
// for a City run and a City artifact that share a native ID, and their two
// State documents deserialize into each other without an error. cityinference
// is the only one of the three whose key names its domain ("/inference").
//
// Under separate stores this is invisible. Under one shared store — the same
// table, the obvious operational choice, and the only thing "shared checkpoint
// corruption" could mean — one adapter silently erases the other's frontier.
// That is a cross-adapter fault domain, and this test is its proof.
func TestNeutralAndArtifactCheckpointKeysCollide(t *testing.T) {
	ctx := context.Background()
	s := cityStyle
	const nativeID = "shared-native-id"

	nKey := cityneutral.CheckpointKey(
		cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}, nativeID)
	aKey := cityartifact.CheckpointKey(
		cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}, nativeID)
	iKey := cityinference.CheckpointKey(inferenceSource(s))

	if nKey != aKey {
		t.Fatalf("checkpoint keys no longer collide (%q vs %q): the finding is fixed, "+
			"update this certification to assert disjointness instead", nKey, aKey)
	}
	if iKey == nKey {
		t.Fatalf("inference key now collides too: %q", iKey)
	}

	kv := newSharedKV()
	af := cityartifact.NewFake()
	af.Authorize(linkedProject, linkedIssue)
	ap, err := cityartifact.NewProducer(af.Client(s.sourceID, s.sourceKind),
		cityartifact.Mapper{Source: cityartifact.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1}},
		artifactKV{kv})
	if err != nil {
		t.Fatalf("artifact NewProducer: %v", err)
	}
	art := s.artifact("", "")
	art.ArtifactID = nativeID
	art.ProjectID, art.IssueID = linkedProject, linkedIssue
	ares, err := ap.Push(ctx, art)
	if err != nil {
		t.Fatalf("artifact push onto the shared store: %v", err)
	}
	if ares.ArtifactID == "" {
		t.Fatal("artifact push produced no ID")
	}

	before, _, err := (artifactKV{kv}).Load(ctx, aKey)
	if err != nil {
		t.Fatalf("load artifact checkpoint: %v", err)
	}
	if before.ArtifactID == "" || !before.Finalized {
		t.Fatalf("artifact checkpoint is not the complete frontier this test needs: %+v", before)
	}

	// The other adapter's State decodes out of the artifact's bytes with no
	// error at all. Nothing in either document says which domain wrote it.
	crossed, ok, err := (neutralKV{kv}).Load(ctx, nKey)
	if err != nil || !ok {
		t.Fatalf("an artifact checkpoint should have decoded as a neutral one: %v (found=%t)", err, ok)
	}
	if crossed.Epoch != before.Epoch || crossed.SourceID != before.SourceID {
		t.Fatalf("cross-decode lost the fields that make it look legitimate: %+v", crossed)
	}
	if crossed.RunTeamID != "" {
		t.Fatalf("cross-decoded neutral state carried a run ID: %+v", crossed)
	}

	nf := cityneutral.NewFake(s.uploader, s.sourceID, s.sourceKind)
	np, err := cityneutral.NewProducer(nf, cityneutral.Mapper{
		Source:          cityneutral.Source{SourceID: s.sourceID, Kind: s.sourceKind, Epoch: 1},
		AllowRawContent: true,
	}, neutralKV{kv})
	if err != nil {
		t.Fatalf("neutral NewProducer: %v", err)
	}
	chain := s.chain()
	chain.Run.RunID = nativeID
	if _, err := np.Push(ctx, chain); err != nil {
		t.Fatalf("neutral push under the colliding key: %v", err)
	}

	after, _, err := (artifactKV{kv}).Load(ctx, aKey)
	if err != nil {
		t.Fatalf("reload artifact checkpoint: %v", err)
	}
	if after.ArtifactID != "" || after.Finalized || after.Frontier != 0 {
		t.Fatalf("expected the artifact frontier to be erased by the neighbor, got %+v", after)
	}
	t.Logf("FINDING API-7.23-AC2: %s erased the artifact frontier %q (parts %d) under shared key %q",
		"cityneutral", before.ArtifactID, before.Frontier, aKey)
}
