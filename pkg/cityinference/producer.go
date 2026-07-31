package cityinference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Acked is one acknowledged contribution as the checkpoint holds it.
//
// Digest is the payload digest of what the server accepted. It is what makes a
// changed replay detectable at all: without it, a re-offer with a different
// model or outcome is indistinguishable from a retry.
type Acked struct {
	InferenceTeamID string `json:"inference_team_id"`
	Digest          string `json:"digest"`
}

// State is one City source's inference checkpoint. It is keyed by stable source
// identity — derived inference ID plus observation ID — and never by anything
// that changes across a restart or a credential rotation.
type State struct {
	Epoch    uint64           `json:"epoch"`
	SourceID string           `json:"source_id"`
	Accepted map[string]Acked `json:"accepted"`
	// LastReset records the declaration that restarted this checkpoint, if one
	// did. It is evidence, not input: nothing reads it back to make a decision.
	LastReset *ResetRecord `json:"last_reset,omitempty"`
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
		return State{}, false, fmt.Errorf("cityinference: decode checkpoint %q: %w", key, err)
	}
	return st, true, nil
}

// Save implements Store. It round-trips through JSON so a caller cannot hold a
// reference into stored state and mutate it behind the producer's back.
func (s *MemoryStore) Save(_ context.Context, key string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("cityinference: encode checkpoint %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[key] = raw
	return nil
}

// Uploaded reports one accepted upload.
type Uploaded struct {
	ExternalInferenceID string
	ObservationID       string
	InferenceTeamID     string
}

// Result reports what one push did. It is returned even on error, so a caller
// that stops on a refusal still learns how far it got.
type Result struct {
	Uploaded []Uploaded
	Accepted int
	Skipped  int
}

// Producer maps City invocations onto neutral inference records and advances a
// checkpoint only over records the server acknowledged.
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
		return nil, errors.New("cityinference: API is required")
	}
	if store == nil {
		return nil, errors.New("cityinference: Store is required")
	}
	if mapper.Source.SourceID == "" || mapper.Source.Kind == "" {
		return nil, errors.New("cityinference: source identity is required")
	}
	if mapper.Source.Tenant == "" {
		return nil, errors.New("cityinference: enrolled tenant is required")
	}
	if mapper.Source.Epoch == 0 {
		return nil, errors.New("cityinference: source epoch is required and is never guessed")
	}
	return &Producer{API: api, Mapper: mapper, Store: store}, nil
}

// CheckpointKey is the durable key of this source's inference checkpoint. It
// carries no credential and no host, so a rotation or a redeploy resumes the
// same checkpoint rather than starting a second one.
func CheckpointKey(source Source) string {
	return source.Kind + "/" + source.SourceID + "/" + checkpointDomain
}

// checkpointDomain names this adapter's checkpoint namespace. It is the third
// segment of every key, and the other two City adapters now put theirs in the
// same position.
const checkpointDomain = "inference"

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
		// The accepted set belongs to the previous epoch and must not gate the
		// new one, so the checkpoint restarts under the same key. The server
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

// Push uploads a batch of City invocations, skipping what the checkpoint
// already holds and stopping on the first refusal.
//
// Order within the batch carries no meaning — this domain asserts none — so a
// caller may re-offer a batch in any order and get the same outcome.
func (p *Producer) Push(ctx context.Context, invocations []CityInvocation) (Result, error) {
	var res Result
	if p.Disabled {
		return res, ErrDisabled
	}
	key := CheckpointKey(p.Mapper.Source)
	st, _, err := p.Store.Load(ctx, key)
	if err != nil {
		return res, err
	}
	prior := st.Epoch
	if err := p.reconcileEpoch(&st); err != nil {
		return res, err
	}
	if st.Accepted == nil {
		st.Accepted = map[string]Acked{}
	}
	// An honoured reset is written before anything is uploaded. The old code
	// refused the advance and never wrote the new epoch, so every later push was
	// refused the same way until an operator deleted the checkpoint by hand —
	// and nothing in the checkpoint said why.
	if prior != 0 && st.Epoch != prior {
		if err := p.Store.Save(ctx, key, st); err != nil {
			return res, err
		}
	}

	for _, inv := range invocations {
		req, err := p.Mapper.MapInvocation(inv)
		if err != nil {
			return res, err
		}
		recordKey := req.ExternalInferenceID + "/" + req.ObservationID
		digest, err := payloadDigest(req)
		if err != nil {
			return res, err
		}
		if prior, seen := st.Accepted[recordKey]; seen {
			if prior.Digest != digest {
				return res, fmt.Errorf("%w: %s", ErrChangedReplay, recordKey)
			}
			res.Skipped++
			continue
		}

		got, err := p.API.CreateInference(ctx, req, IdempotencyKey(p.Mapper.Source, recordKey))
		if err != nil {
			return res, err
		}
		if err := p.verify(req, got); err != nil {
			return res, err
		}
		st.Accepted[recordKey] = Acked{InferenceTeamID: got.ID, Digest: digest}
		// The checkpoint advances only here, after acceptance. A crash between
		// the call and this save replays the same idempotency key and the
		// server answers with the original response.
		if err := p.Store.Save(ctx, key, st); err != nil {
			return res, err
		}
		res.Uploaded = append(res.Uploaded, Uploaded{
			ExternalInferenceID: req.ExternalInferenceID,
			ObservationID:       req.ObservationID,
			InferenceTeamID:     got.ID,
		})
		res.Accepted++
	}
	return res, nil
}

// verify checks what came back against what was offered.
func (p *Producer) verify(req CreateInferenceRequest, got Inference) error {
	if got.ID == "" {
		return fmt.Errorf("%w: response carries no neutral id", ErrIdentityDrift)
	}
	if got.ExternalInferenceID != req.ExternalInferenceID {
		return fmt.Errorf("%w: offered %s, accepted %s", ErrIdentityDrift,
			req.ExternalInferenceID, got.ExternalInferenceID)
	}
	// Every field group is present on every inference. A missing key would be
	// an implicit "unavailable", and an implicit claim is the thing coverage
	// exists to abolish.
	for _, group := range CoverageFieldGroups() {
		if _, ok := got.Coverage[group]; !ok {
			return fmt.Errorf("%w: coverage omits field group %q", ErrCoverageRaised, group)
		}
	}
	// Step attribution is unavailable by construction. Anything else means a
	// step was materialized from data this adapter never had.
	if Coverage(got.Coverage[FieldGroupStep]) != CoverageUnavailable {
		return fmt.Errorf("%w: step coverage came back %q", ErrCoverageRaised, got.Coverage[FieldGroupStep])
	}
	// Transcript attribution may be known only where a canonical record was
	// linked. The link is part of the invariant core, so a raised class here is
	// a fabricated link, not another contribution's.
	if req.TranscriptRecordID == "" && Coverage(got.Coverage[FieldGroupTranscript]) == CoverageKnown {
		return fmt.Errorf("%w: transcript coverage claims known with no linked record", ErrCoverageRaised)
	}
	if req.TranscriptRecordID != "" && got.TranscriptRecordID != req.TranscriptRecordID {
		return fmt.Errorf("%w: accepted record links a different transcript record", ErrIdentityDrift)
	}
	if got.Completeness != "" && Coverage(got.Completeness) != CoverageUnavailable {
		return fmt.Errorf("%w: completeness came back %q over a best-effort producer",
			ErrCoverageRaised, got.Completeness)
	}
	scope, err := ClassifyReqID(req.NativeIdentity.UpstreamReqID)
	if err != nil {
		return err
	}
	if scope == ScopeObservation && got.FoldEligible {
		return fmt.Errorf("%w: a locally generated request id came back fold-eligible", ErrCoverageRaised)
	}
	return nil
}

// idempotencyDomain separates this producer's keys from every other preimage.
const idempotencyDomain = "cityinference/idempotency/v1"

// IdempotencyKey derives the server's Idempotency-Key from stable source
// identity.
//
// The preimage holds the enrolled source, the ingest epoch, the resource kind
// and the record's stable identity, and nothing else. No credential, no key
// identifier, no wall clock and no attempt counter: a credential rotation would
// otherwise open a fresh idempotency namespace and turn a retry into a second
// admitted record.
func IdempotencyKey(source Source, recordKey string) string {
	preimage := strings.Join([]string{
		idempotencyDomain,
		source.Kind, source.SourceID,
		strconv.FormatUint(source.Epoch, 10),
		KindInference, recordKey,
	}, "\x1f")
	sum := sha256.Sum256([]byte(preimage))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func payloadDigest(req CreateInferenceRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("%w: encode request: %w", ErrInvalidInvocation, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
