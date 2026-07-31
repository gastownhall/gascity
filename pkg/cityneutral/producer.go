package cityneutral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SessionState is the acknowledged frontier of one session.
//
// Frontier is the highest ordinal the SERVER acknowledged, and it only ever
// advances contiguously. Digests holds the payload digest of each acknowledged
// record so a later replay can be told apart from a rewrite — the checkpoint is
// what makes "changed replay" detectable at all.
type SessionState struct {
	TeamID    string            `json:"team_id"`
	Version   uint64            `json:"version"`
	Finalized bool              `json:"finalized"`
	Frontier  uint64            `json:"frontier"`
	Digests   map[uint64]string `json:"digests"`
	MessageID map[uint64]string `json:"message_id"`
}

// State is one City run's checkpoint.
type State struct {
	Epoch      uint64                   `json:"epoch"`
	SourceID   string                   `json:"source_id"`
	RunTeamID  string                   `json:"run_team_id"`
	RunVersion uint64                   `json:"run_version"`
	Sessions   map[string]*SessionState `json:"sessions"`
	// LastReset records the declaration that restarted this checkpoint, if one
	// did. It is evidence, not input: nothing reads it back to make a decision.
	LastReset *ResetRecord `json:"last_reset,omitempty"`
}

func (s *State) session(sourceSessionID string) *SessionState {
	if s.Sessions == nil {
		s.Sessions = map[string]*SessionState{}
	}
	st, ok := s.Sessions[sourceSessionID]
	if !ok {
		st = &SessionState{Digests: map[uint64]string{}, MessageID: map[uint64]string{}}
		s.Sessions[sourceSessionID] = st
	}
	if st.Digests == nil {
		st.Digests = map[uint64]string{}
	}
	if st.MessageID == nil {
		st.MessageID = map[uint64]string{}
	}
	return st
}

// Store persists checkpoints across restarts. It is an interface because the
// durable implementation is City's business; the adapter only needs load and
// save to be atomic with respect to each other.
type Store interface {
	Load(ctx context.Context, key string) (State, bool, error)
	Save(ctx context.Context, key string, st State) error
}

// MemoryStore is an in-process Store. It is safe for concurrent use and is what
// tests and a single-shot export run on.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMemoryStore returns an empty in-process Store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string][]byte{}} }

// Load implements Store.
func (s *MemoryStore) Load(_ context.Context, key string) (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.m[key]
	if !ok {
		return State{}, false, nil
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, false, fmt.Errorf("cityneutral: decode checkpoint %q: %w", key, err)
	}
	return st, true, nil
}

// Save implements Store. It round-trips through JSON so a caller cannot hold a
// reference into stored state and mutate it behind the producer's back.
func (s *MemoryStore) Save(_ context.Context, key string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("cityneutral: encode checkpoint %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[key] = raw
	return nil
}

// SessionResult reports what one session's push did.
type SessionResult struct {
	SourceSessionID string
	TeamID          string
	Accepted        int
	Skipped         int
	Finalized       bool
}

// Result reports what one push did. It is returned even on error, so a caller
// that stops on a refusal still learns how far the chain got.
type Result struct {
	RunTeamID string
	Sessions  []SessionResult
	Accepted  int
	Skipped   int
}

// Producer maps a City chain onto neutral resources and advances a checkpoint
// only over acknowledged, contiguous records.
type Producer struct {
	API    API
	Mapper Mapper
	Store  Store

	// Disabled is the rollback switch. A disabled producer issues no request
	// and touches no checkpoint; acknowledged records stay exactly as the
	// server accepted them and every other producer keeps running.
	Disabled bool
}

// NewProducer validates its wiring up front: a producer missing a store would
// look like it worked and silently re-upload the world on restart.
func NewProducer(api API, mapper Mapper, store Store) (*Producer, error) {
	if api == nil {
		return nil, errors.New("cityneutral: API is required")
	}
	if store == nil {
		return nil, errors.New("cityneutral: Store is required")
	}
	if err := mapper.validateSource(); err != nil {
		return nil, err
	}
	return &Producer{API: api, Mapper: mapper, Store: store}, nil
}

// checkpointDomain names this adapter's checkpoint namespace. It is the third
// segment of every key, the same position cityinference puts "inference" in, so
// a City run and a City artifact that share a native ID can no longer mint one
// key over one store.
const checkpointDomain = "run"

// CheckpointKey is the stable checkpoint identity of one City run under one
// source. It excludes the epoch: a reset must LAND on the same key so the old
// frontier is visibly superseded rather than orphaned under a new one.
func CheckpointKey(source Source, cityRunID string) string {
	return source.Kind + "/" + source.SourceID + "/" + checkpointDomain + "/" + cityRunID
}

// LegacyCheckpointKey is the undomained key this adapter wrote before the
// domain segment existed. It is read once, on a miss at the current key, and it
// is never written: leaving it unread would silently restart every in-flight run
// from zero, and writing it again would re-open the collision.
func LegacyCheckpointKey(source Source, cityRunID string) string {
	return source.Kind + "/" + source.SourceID + "/" + cityRunID
}

// loadCheckpoint reads the current key and falls back once to the pre-domain
// key, so a producer that was mid-run when the domain segment shipped resumes
// its frontier instead of re-uploading the world.
//
// The legacy key is the ambiguous one, so its bytes may belong to another
// domain: a cityartifact checkpoint decodes as this State without an error.
// Adoption therefore requires this domain's own discriminator. A neutral
// checkpoint is only ever saved after a run was acknowledged, so a durable one
// always carries a run team ID; anything else is left where it lies.
func (p *Producer) loadCheckpoint(ctx context.Context, key, cityRunID string) (State, error) {
	st, ok, err := p.Store.Load(ctx, key)
	if err != nil {
		return State{}, err
	}
	if ok {
		return st, nil
	}
	legacy, ok, err := p.Store.Load(ctx, LegacyCheckpointKey(p.Mapper.Source, cityRunID))
	if err != nil {
		return State{}, err
	}
	if !ok || legacy.RunTeamID == "" {
		return State{}, nil
	}
	// Copy it forward now rather than relying on the rest of this push to save.
	// A chain whose sessions are all finalized writes nothing else, and the
	// migration has to complete for those too or the old key stays load-bearing
	// forever.
	if err := p.Store.Save(ctx, key, legacy); err != nil {
		return State{}, err
	}
	return legacy, nil
}

// reconcileEpoch applies a declared reset and refuses everything else. It is
// byte-for-byte the same decision in all three City adapters.
func (p *Producer) reconcileEpoch(st *State) error {
	src := p.Mapper.Source
	switch {
	case st.Epoch == 0:
		st.Epoch = src.Epoch
		st.SourceID = src.SourceID
	case src.Epoch > st.Epoch:
		rec, err := checkResetDeclaration(src.Reset, st.Epoch, src.Epoch)
		if err != nil {
			return err
		}
		// The previous frontier belongs to the previous epoch and must not gate
		// the new one, so the checkpoint restarts under the same key. The server
		// keeps the records it already accepted; this producer simply stops
		// claiming to have sent them. The declaration is carried into the new
		// checkpoint: a reset nobody can attribute later is indistinguishable
		// from corruption.
		*st = State{Epoch: src.Epoch, SourceID: src.SourceID, LastReset: &rec}
	case src.Epoch < st.Epoch:
		return fmt.Errorf("%w: source epoch %d is behind checkpoint epoch %d",
			ErrIdentityDrift, src.Epoch, st.Epoch)
	}
	if st.SourceID != "" && st.SourceID != src.SourceID {
		return fmt.Errorf("%w: checkpoint belongs to source %q", ErrIdentityDrift, st.SourceID)
	}
	return nil
}

// Push maps, uploads, checkpoints and finalizes one chain.
//
// It advances only on acknowledgement. Every refusal returns the partial result
// with the checkpoint already saved at the last acknowledged record, so a
// restart resumes at the next accepted record and never re-sends an accepted
// one.
func (p *Producer) Push(ctx context.Context, chain CityChain) (Result, error) {
	if p.Disabled {
		return Result{}, ErrDisabled
	}
	key := CheckpointKey(p.Mapper.Source, chain.Run.RunID)
	st, err := p.loadCheckpoint(ctx, key, chain.Run.RunID)
	if err != nil {
		return Result{}, err
	}
	if err := p.reconcileEpoch(&st); err != nil {
		return Result{}, err
	}

	res := Result{}
	runBody, err := p.Mapper.MapRun(chain.Run)
	if err != nil {
		return res, err
	}
	if err := ScanOutbound(runBody, false); err != nil {
		return res, err
	}
	run, err := p.API.UpsertRun(ctx, runBody, idempotencyKey(p.Mapper.Source, KindRun, chain.Run.RunID, runBody.SourceVersion))
	if err != nil {
		return res, fmt.Errorf("cityneutral: %s: %w", OpUpsertRun, err)
	}
	if run.ID == "" {
		return res, fmt.Errorf("%w: server returned no neutral run id", ErrIdentityDrift)
	}
	if st.RunTeamID != "" && st.RunTeamID != run.ID {
		return res, fmt.Errorf("%w: run %q was %q, server now says %q",
			ErrIdentityDrift, chain.Run.RunID, st.RunTeamID, run.ID)
	}
	st.RunTeamID = run.ID
	st.RunVersion = runBody.SourceVersion
	res.RunTeamID = run.ID
	if err := p.Store.Save(ctx, key, st); err != nil {
		return res, err
	}

	for _, sess := range chain.Sessions {
		sr, err := p.pushSession(ctx, key, &st, run.ID, sess)
		res.Sessions = append(res.Sessions, sr)
		res.Accepted += sr.Accepted
		res.Skipped += sr.Skipped
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func (p *Producer) pushSession(ctx context.Context, key string, st *State, runTeamID string, sess CitySession) (SessionResult, error) {
	sr := SessionResult{SourceSessionID: sess.SessionID}
	sstate := st.session(sess.SessionID)
	sr.TeamID = sstate.TeamID
	sr.Finalized = sstate.Finalized

	body, err := p.Mapper.MapSession(sess)
	if err != nil {
		return sr, err
	}
	if err := ScanOutbound(body, false); err != nil {
		return sr, err
	}

	// Post-finalize mutation is refused before anything is sent. A finalized
	// session may be re-observed (same version, no new records) — that is a
	// harmless idempotent read of City's own state — but any changed input
	// stops this adapter rather than asking the server to rewrite final input.
	if sstate.Finalized {
		if body.SourceVersion > sstate.Version {
			return sr, fmt.Errorf("%w: session %q version %d after finalize at %d",
				ErrFinalized, sess.SessionID, body.SourceVersion, sstate.Version)
		}
		if err := p.assertNoNewInput(sess, sstate); err != nil {
			return sr, err
		}
		sr.Skipped += len(sess.Records)
		return sr, nil
	}

	session, err := p.API.UpsertRunSession(ctx, runTeamID, body,
		idempotencyKey(p.Mapper.Source, KindSession, sess.SessionID, body.SourceVersion))
	if err != nil {
		return sr, fmt.Errorf("cityneutral: %s: %w", OpUpsertRunSession, err)
	}
	if session.ID == "" {
		return sr, fmt.Errorf("%w: server returned no neutral session id", ErrIdentityDrift)
	}
	if sstate.TeamID != "" && sstate.TeamID != session.ID {
		return sr, fmt.Errorf("%w: session %q was %q, server now says %q",
			ErrIdentityDrift, sess.SessionID, sstate.TeamID, session.ID)
	}
	if session.RunID != runTeamID {
		return sr, fmt.Errorf("%w: session %q links run %q, not %q",
			ErrIdentityDrift, sess.SessionID, session.RunID, runTeamID)
	}
	sstate.TeamID = session.ID
	sstate.Version = body.SourceVersion
	sstate.Finalized = session.Finalized
	sr.TeamID = session.ID
	sr.Finalized = session.Finalized
	if err := p.Store.Save(ctx, key, *st); err != nil {
		return sr, err
	}

	// Out-of-order arrival is a source property, not a protocol violation:
	// order the batch here and let contiguity be judged on ordinals.
	records := append([]CityRecord(nil), sess.Records...)
	sort.Slice(records, func(i, j int) bool { return records[i].Ordinal < records[j].Ordinal })

	for _, rec := range records {
		in, err := p.Mapper.MapRecord(rec)
		if err != nil {
			return sr, err
		}
		digest := payloadDigest(in)

		if rec.Ordinal <= sstate.Frontier {
			// Already acknowledged. Same bytes means a retry we can drop;
			// different bytes means the source rewrote accepted input.
			if have, ok := sstate.Digests[rec.Ordinal]; ok && have != digest {
				return sr, fmt.Errorf("%w: session %q ordinal %d (message %q)",
					ErrChangedReplay, sess.SessionID, rec.Ordinal, rec.MessageID)
			}
			if have, ok := sstate.MessageID[rec.Ordinal]; ok && have != rec.MessageID {
				return sr, fmt.Errorf("%w: session %q ordinal %d was message %q, now %q",
					ErrChangedReplay, sess.SessionID, rec.Ordinal, have, rec.MessageID)
			}
			sr.Skipped++
			continue
		}
		if rec.Ordinal != sstate.Frontier+1 {
			return sr, fmt.Errorf("%w: session %q expected ordinal %d, got %d",
				ErrGap, sess.SessionID, sstate.Frontier+1, rec.Ordinal)
		}
		if err := ScanOutbound(in, true); err != nil {
			return sr, err
		}
		out, err := p.API.CreateTranscriptRecord(ctx, sstate.TeamID, in,
			idempotencyKey(p.Mapper.Source, KindTranscriptRecord, sess.SessionID+"/"+rec.MessageID, in.SourceVersion))
		if err != nil {
			return sr, fmt.Errorf("cityneutral: %s: %w", OpCreateTranscriptRecord, err)
		}
		if out.ID == "" || out.SessionID != sstate.TeamID {
			return sr, fmt.Errorf("%w: record %q acknowledged against session %q",
				ErrIdentityDrift, rec.MessageID, out.SessionID)
		}
		sstate.Frontier = rec.Ordinal
		sstate.Digests[rec.Ordinal] = digest
		sstate.MessageID[rec.Ordinal] = rec.MessageID
		sr.Accepted++
		if err := p.Store.Save(ctx, key, *st); err != nil {
			return sr, err
		}
	}

	if sess.Complete && !sstate.Finalized {
		final, err := p.API.FinalizeSession(ctx, sstate.TeamID,
			idempotencyKey(p.Mapper.Source, kindSessionFinalize, sess.SessionID, sstate.Frontier))
		if err != nil {
			return sr, fmt.Errorf("cityneutral: %s: %w", OpFinalizeSession, err)
		}
		sstate.Finalized = final.Finalized
		sr.Finalized = final.Finalized
		if err := p.Store.Save(ctx, key, *st); err != nil {
			return sr, err
		}
	}
	return sr, nil
}

// assertNoNewInput refuses a finalized session that has grown or changed.
func (p *Producer) assertNoNewInput(sess CitySession, sstate *SessionState) error {
	for _, rec := range sess.Records {
		if rec.Ordinal > sstate.Frontier {
			return fmt.Errorf("%w: session %q record ordinal %d arrived after finalize",
				ErrFinalized, sess.SessionID, rec.Ordinal)
		}
		if have, ok := sstate.MessageID[rec.Ordinal]; ok && have != rec.MessageID {
			return fmt.Errorf("%w: session %q ordinal %d changed after finalize",
				ErrFinalized, sess.SessionID, rec.Ordinal)
		}
		in, err := p.Mapper.MapRecord(rec)
		if err != nil {
			return err
		}
		if have, ok := sstate.Digests[rec.Ordinal]; ok && have != payloadDigest(in) {
			return fmt.Errorf("%w: session %q ordinal %d payload changed after finalize",
				ErrFinalized, sess.SessionID, rec.Ordinal)
		}
	}
	return nil
}

// payloadDigest is the canonical digest of a request body. It is what makes a
// replay comparable: the same City record maps to the same bytes, so a digest
// difference is a real source change and never a serialization artifact.
func payloadDigest(body any) string {
	raw, err := json.Marshal(body)
	if err != nil {
		// json.Marshal on these closed DTOs cannot fail; a digest that cannot
		// be computed must not compare equal to anything.
		return "unencodable:" + err.Error()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// idempotencyKey derives the server's Idempotency-Key from stable source
// identity alone.
//
// Deriving rather than minting is the dedup contract: a retry after a lost
// response reuses the key and replays, while a changed payload under the same
// key is a conflict the server refuses. Nothing volatile — no clock, no
// attempt counter, no credential — is in the preimage, so a credential rotation
// or a restart changes nothing about which key a record travels under. The
// output is a v5-shaped UUID because the contract requires a UUID.
func idempotencyKey(source Source, kind, nativeID string, version uint64) string {
	preimage := strings.Join([]string{
		"cityneutral/idempotency/v1",
		source.Kind, source.SourceID,
		fmt.Sprintf("%d", source.Epoch),
		kind, nativeID,
		fmt.Sprintf("%d", version),
	}, "\x1f")
	sum := sha256.Sum256([]byte(preimage))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
