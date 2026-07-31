package cityneutral

import (
	"context"
	"errors"
	"testing"
	"time"
)

func recordsUpTo(n uint64) []CityRecord {
	out := make([]CityRecord, 0, n)
	for i := uint64(1); i <= n; i++ {
		out = append(out, CityRecord{
			MessageID: "m" + string(rune('0'+i)), Ordinal: i, Role: "user",
			At: t0.Add(time.Duration(i) * time.Minute), Text: "turn",
		})
	}
	return out
}

func chainWith(records []CityRecord, complete bool) CityChain {
	return CityChain{
		Run: CityRun{RunID: "city-run-7", Status: "running", Started: t0, Version: 1},
		Sessions: []CitySession{{
			SessionID: "city-sess-1", Status: "running", Started: t0, Version: 1,
			Complete: complete, Records: records,
		}},
	}
}

func pushOK(t *testing.T, p *Producer, chain CityChain) Result {
	t.Helper()
	res, err := p.Push(context.Background(), chain)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	return res
}

// AC2 happy path: a restart resumes at the next accepted record and re-sends
// nothing that was already acknowledged.
func TestRestartResumesAtNextAcceptedRecord(t *testing.T) {
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()
	mapper := Mapper{Source: citySource(), AllowRawContent: true}

	first, err := NewProducer(f, mapper, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res := pushOK(t, first, chainWith(recordsUpTo(2), false))
	if res.Accepted != 2 {
		t.Fatalf("first push accepted %d, want 2", res.Accepted)
	}

	// A brand new Producer over the same store is the restart: same chain,
	// grown by two records.
	second, err := NewProducer(f, mapper, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res = pushOK(t, second, chainWith(recordsUpTo(4), false))
	if res.Accepted != 2 || res.Skipped != 2 {
		t.Fatalf("restart push accepted %d skipped %d, want 2/2", res.Accepted, res.Skipped)
	}
	recs, err := f.ListTranscriptRecords(context.Background(), res.Sessions[0].TeamID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("server holds %d records, want 4 (no duplicates)", len(recs))
	}
}

// AC2 fault/checkpoint matrix. Every row starts from the same two-record
// checkpoint so the difference between rows is only the fault under test.
func TestCheckpointFaultMatrix(t *testing.T) {
	base := recordsUpTo(2)

	cases := []struct {
		name     string
		finalize bool
		next     CityChain
		wantErr  error
		accepted int
		skipped  int
	}{
		{
			name:     "duplicate batch deduplicates on stable source identity",
			next:     chainWith(recordsUpTo(2), false),
			accepted: 0, skipped: 2,
		},
		{
			name:     "out-of-order batch is ordered before contiguity is judged",
			next:     chainWith([]CityRecord{base[1], base[0], recordsUpTo(4)[3], recordsUpTo(4)[2]}, false),
			accepted: 2, skipped: 2,
		},
		{
			name:     "partial batch advances only as far as it is contiguous",
			next:     chainWith(recordsUpTo(3), false),
			accepted: 1, skipped: 2,
		},
		{
			name: "gap stops the adapter",
			next: chainWith(append(append([]CityRecord{}, base...), CityRecord{
				MessageID: "m5", Ordinal: 5, Role: "user", At: t0.Add(5 * time.Minute), Text: "late",
			}), false),
			wantErr: ErrGap,
		},
		{
			name: "changed replay stops the adapter",
			next: chainWith([]CityRecord{base[0], {
				MessageID: "m2", Ordinal: 2, Role: "user", At: t0.Add(2 * time.Minute), Text: "rewritten",
			}}, false),
			wantErr: ErrChangedReplay,
		},
		{
			name: "reassigned message id at an accepted ordinal stops the adapter",
			next: chainWith([]CityRecord{base[0], {
				MessageID: "m9", Ordinal: 2, Role: "user", At: t0.Add(2 * time.Minute), Text: "turn",
			}}, false),
			wantErr: ErrChangedReplay,
		},
		{
			name:     "post-finalize append stops the adapter",
			finalize: true,
			next:     chainWith(recordsUpTo(3), true),
			wantErr:  ErrFinalized,
		},
		{
			name:     "re-observing a finalized session is a no-op",
			finalize: true,
			next:     chainWith(recordsUpTo(2), true),
			accepted: 0, skipped: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake("svc-city@tenant", "gc-city-01", "city")
			p := newCityProducer(t, f, nil)
			pushOK(t, p, chainWith(base, tc.finalize))

			res, err := p.Push(context.Background(), tc.next)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				// A refusal must not have advanced anything: the server still
				// holds exactly the acknowledged prefix.
				recs, lerr := f.ListTranscriptRecords(context.Background(), res.Sessions[0].TeamID)
				if lerr != nil {
					t.Fatalf("ListTranscriptRecords: %v", lerr)
				}
				if len(recs) != len(base) {
					t.Fatalf("refusal left %d records, want %d", len(recs), len(base))
				}
				return
			}
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Accepted != tc.accepted || res.Skipped != tc.skipped {
				t.Fatalf("accepted %d skipped %d, want %d/%d",
					res.Accepted, res.Skipped, tc.accepted, tc.skipped)
			}
		})
	}
}

// AC2: a credential rotation mid-flight is recoverable by retry alone. It
// changes the credential and nothing else — not the source identity, not the
// epoch, not the checkpoint — so the retry replays under the same derived key
// and the server stores no duplicate.
func TestCredentialRotationDoesNotAdvanceOrDuplicate(t *testing.T) {
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	p := newCityProducer(t, f, nil)
	pushOK(t, p, chainWith(recordsUpTo(2), false))

	rotating := errors.New("401 credential rotated")
	f.Fail = rotating
	if _, err := p.Push(context.Background(), chainWith(recordsUpTo(3), false)); !errors.Is(err, rotating) {
		t.Fatalf("err = %v, want the injected rotation error", err)
	}

	// The rotation landed on the run upsert, so nothing advanced. Retrying the
	// identical chain must accept exactly the one new record.
	res, err := p.Push(context.Background(), chainWith(recordsUpTo(3), false))
	if err != nil {
		t.Fatalf("retry after rotation: %v", err)
	}
	if res.Accepted != 1 || res.Skipped != 2 {
		t.Fatalf("retry accepted %d skipped %d, want 1/2", res.Accepted, res.Skipped)
	}
	recs, err := f.ListTranscriptRecords(context.Background(), res.Sessions[0].TeamID)
	if err != nil {
		t.Fatalf("ListTranscriptRecords: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("server holds %d records, want 3", len(recs))
	}
}

// AC2: a declared reset advances the epoch, which restarts the frontier under
// the same checkpoint key. A backwards epoch is not a reset — it is drift.
func TestResetAdvancesEpochAndBackwardsEpochIsDrift(t *testing.T) {
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()
	p, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pushOK(t, p, chainWith(recordsUpTo(2), false))

	// An epoch that merely went up is not a reset: a config typo and a real
	// reset are the same number, so the advance is refused without a
	// declaration naming what it resets from and who declared it.
	undeclared := citySource()
	undeclared.Epoch = 2
	bumped, err := NewProducer(f, Mapper{Source: undeclared, AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if _, err := bumped.Push(context.Background(), chainWith(recordsUpTo(2), false)); !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("undeclared epoch advance: err = %v, want ErrIdentityDrift", err)
	}

	reset := citySource()
	reset.Epoch = 2
	reset.Reset = &ResetDeclaration{FromEpoch: 1, ToEpoch: 2, Reason: "source rebuilt", DeclaredBy: "ops@city"}
	after, err := NewProducer(f, Mapper{Source: reset, AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res := pushOK(t, after, chainWith(recordsUpTo(2), false))
	if res.Accepted != 2 {
		t.Fatalf("after reset accepted %d, want 2 (frontier restarted)", res.Accepted)
	}
	st, _, err := store.Load(context.Background(), CheckpointKey(reset, "city-run-7"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Epoch != 2 {
		t.Errorf("checkpoint epoch = %d, want the honoured reset to have been written", st.Epoch)
	}
	if st.LastReset == nil || st.LastReset.DeclaredBy != "ops@city" || st.LastReset.FromEpoch != 1 {
		t.Errorf("the honoured reset was not recorded: %+v", st.LastReset)
	}

	behind, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if _, err := behind.Push(context.Background(), chainWith(recordsUpTo(2), false)); !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("backwards epoch: err = %v, want ErrIdentityDrift", err)
	}
}

// AC2: the derived idempotency key depends on stable source identity and
// nothing else, and it is namespaced per resource kind so a run key cannot be
// replayed into a session.
func TestIdempotencyKeyIsStableAndNamespaced(t *testing.T) {
	src := citySource()
	run1 := idempotencyKey(src, KindRun, "city-run-7", 3)
	run2 := idempotencyKey(src, KindRun, "city-run-7", 3)
	if run1 != run2 {
		t.Fatalf("key is not stable: %q != %q", run1, run2)
	}
	if len(run1) != 36 || run1[14] != '5' {
		t.Fatalf("key %q is not a v5-shaped UUID", run1)
	}
	if sess := idempotencyKey(src, KindSession, "city-run-7", 3); sess == run1 {
		t.Fatalf("run and session keys collide: %q", sess)
	}
	if bumped := idempotencyKey(src, KindRun, "city-run-7", 4); bumped == run1 {
		t.Fatalf("source_version does not change the key")
	}
	other := src
	other.Epoch = 2
	if e := idempotencyKey(other, KindRun, "city-run-7", 3); e == run1 {
		t.Fatalf("epoch does not change the key")
	}
}

// A checkpoint minted for one source may not be picked up by another.
func TestCheckpointIsBoundToItsSource(t *testing.T) {
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()
	p, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res := pushOK(t, p, chainWith(recordsUpTo(2), false))

	// Same checkpoint bytes, different source identity: refuse rather than
	// inherit another producer's frontier.
	stolen := State{Epoch: 1, SourceID: "someone-else", RunTeamID: res.RunTeamID}
	if err := store.Save(context.Background(), CheckpointKey(citySource(), "city-run-7"), stolen); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := p.Push(context.Background(), chainWith(recordsUpTo(2), false)); !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("err = %v, want ErrIdentityDrift", err)
	}
}
