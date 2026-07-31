package cityartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// State is one City artifact's checkpoint.
//
// Frontier is the highest part sequence the SERVER acknowledged, and it only
// ever advances contiguously. Digests holds the digest of each acknowledged part
// so a later replay can be told apart from a rewrite; CreateDigest does the same
// job for the artifact's own definition. Together they are what makes "changed
// manifest" detectable at all.
type State struct {
	Epoch         uint64         `json:"epoch"`
	SourceID      string         `json:"source_id"`
	ArtifactID    string         `json:"artifact_id"`
	CreateDigest  string         `json:"create_digest"`
	Frontier      int            `json:"frontier"`
	Digests       map[int]string `json:"digests"`
	Finalized     bool           `json:"finalized"`
	ContentDigest string         `json:"content_digest"`
}

func (s *State) digests() map[int]string {
	if s.Digests == nil {
		s.Digests = map[int]string{}
	}
	return s.Digests
}

// Store persists checkpoints across restarts. It is an interface because the
// durable implementation is City's business; the adapter only needs Load and
// Save to be atomic with respect to each other.
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
		return State{}, false, fmt.Errorf("cityartifact: decode checkpoint %q: %w", key, err)
	}
	return st, true, nil
}

// Save implements Store. It round-trips through JSON so a caller cannot hold a
// reference into stored state and mutate it behind the producer's back.
func (s *MemoryStore) Save(_ context.Context, key string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("cityartifact: encode checkpoint %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[key] = raw
	return nil
}

// Result reports what one push did. It is returned even on error, so a caller
// that stops on a refusal still learns how far the artifact got.
type Result struct {
	// ArtifactID is the server-minted ID. City never mints one.
	ArtifactID string
	Uploaded   int
	Skipped    int
	Finalized  bool
	// Metadata is the normalized read-back, present only once the artifact is
	// finalized — an unfinalized artifact is not readable.
	Metadata Artifact
}

// Producer maps a City artifact onto the public artifact contract and advances a
// checkpoint only over acknowledged, contiguous parts.
type Producer struct {
	API    API
	Mapper Mapper
	Store  Store
}

// NewProducer validates its wiring up front: a producer missing a store would
// look like it worked and silently re-upload the world on restart.
func NewProducer(api API, mapper Mapper, store Store) (*Producer, error) {
	if api == nil {
		return nil, errors.New("cityartifact: API is required")
	}
	if store == nil {
		return nil, errors.New("cityartifact: Store is required")
	}
	if err := mapper.validateSource(); err != nil {
		return nil, err
	}
	return &Producer{API: api, Mapper: mapper, Store: store}, nil
}

// CheckpointKey is the stable checkpoint identity of one City artifact under one
// source. It excludes the epoch: a declared reset must LAND on the same key so
// the old frontier is visibly superseded rather than orphaned under a new one.
func CheckpointKey(source Source, cityArtifactID string) string {
	return source.Kind + "/" + source.SourceID + "/" + cityArtifactID
}

// Push creates, uploads, finalizes and reads back one City artifact.
//
// It advances only on acknowledgement. Every refusal returns the partial result
// with the checkpoint already saved at the last acknowledged part, so a restart
// resumes at the next part and never re-sends an accepted one. There is no
// fallback path: if the server refuses, this artifact stops. The adapter does
// not reach for a City-side forge, a second route or a different credential, and
// no other producer's checkpoint is touched.
func (p *Producer) Push(ctx context.Context, a CityArtifact) (Result, error) {
	res := Result{}

	create, err := p.Mapper.MapCreate(a)
	if err != nil {
		return res, err
	}
	if err := ScanOutbound(create); err != nil {
		return res, err
	}
	parts, err := p.mapParts(a)
	if err != nil {
		return res, err
	}

	key := CheckpointKey(p.Mapper.Source, a.ArtifactID)
	st, _, err := p.Store.Load(ctx, key)
	if err != nil {
		return res, err
	}
	if err := p.reconcileEpoch(&st); err != nil {
		return res, err
	}

	createDigest := payloadDigest(create)
	if st.ArtifactID == "" {
		art, err := p.API.CreateArtifact(ctx, create,
			idempotencyKey(p.Mapper.Source, kindArtifact, a.ArtifactID, createDigest))
		if err != nil {
			return res, normalizeUpstream(OpCreateArtifact, err)
		}
		if strings.TrimSpace(art.ID) == "" {
			return res, fmt.Errorf("%w: server returned no artifact id", ErrIdentityDrift)
		}
		st.ArtifactID = art.ID
		st.CreateDigest = createDigest
		st.Finalized = art.Finalized()
		if err := p.Store.Save(ctx, key, st); err != nil {
			return res, err
		}
	} else if st.CreateDigest != createDigest {
		// The artifact's own definition changed under an ID the server already
		// minted. Re-creating would fork the artifact and mutating is not on
		// offer, so this adapter stops with its checkpoint intact.
		return res, fmt.Errorf("%w: artifact %q definition changed after creation", ErrChangedManifest, a.ArtifactID)
	}
	res.ArtifactID = st.ArtifactID
	res.Finalized = st.Finalized

	uploaded, skipped, err := p.pushParts(ctx, key, &st, parts)
	res.Uploaded, res.Skipped = uploaded, skipped
	if err != nil {
		return res, err
	}

	if a.Complete && !st.Finalized {
		contentDigest := contentDigest(parts)
		body := p.Mapper.MapFinalize(contentDigest)
		if err := ScanOutbound(body, "digest"); err != nil {
			return res, err
		}
		final, err := p.API.FinalizeArtifact(ctx, st.ArtifactID, body,
			idempotencyKey(p.Mapper.Source, kindFinalize, a.ArtifactID, contentDigest))
		if err != nil {
			return res, normalizeUpstream(OpFinalizeArtifact, err)
		}
		if final.ID != st.ArtifactID {
			return res, fmt.Errorf("%w: finalize acknowledged artifact %q, not %q",
				ErrIdentityDrift, final.ID, st.ArtifactID)
		}
		if !final.Finalized() {
			return res, fmt.Errorf("%w: finalize returned an unsealed artifact", ErrUpstream)
		}
		st.Finalized = true
		st.ContentDigest = contentDigest
		if err := p.Store.Save(ctx, key, st); err != nil {
			return res, err
		}
		res.Finalized = true
	}

	if !st.Finalized {
		// Not readable yet, and this adapter does not pretend otherwise.
		return res, nil
	}
	meta, err := p.Metadata(ctx, st.ArtifactID)
	if err != nil {
		return res, err
	}
	if err := verifyReadBack(meta, st, create, p.Mapper.Source); err != nil {
		return res, err
	}
	res.Metadata = meta
	return res, nil
}

// reconcileEpoch applies a declared reset and refuses an undeclared one.
func (p *Producer) reconcileEpoch(st *State) error {
	src := p.Mapper.Source
	switch {
	case st.Epoch == 0:
		st.Epoch = src.Epoch
		st.SourceID = src.SourceID
	case src.Epoch > st.Epoch:
		// A declared reset. The previous frontier belongs to the previous epoch
		// and must not gate the new one, so the checkpoint restarts. The server
		// keeps the artifacts it already accepted; this producer simply stops
		// claiming to have sent them.
		*st = State{Epoch: src.Epoch, SourceID: src.SourceID}
	case src.Epoch < st.Epoch:
		return fmt.Errorf("%w: source epoch %d is behind checkpoint epoch %d",
			ErrIdentityDrift, src.Epoch, st.Epoch)
	}
	if st.SourceID != "" && st.SourceID != src.SourceID {
		return fmt.Errorf("%w: checkpoint belongs to source %q", ErrIdentityDrift, st.SourceID)
	}
	return nil
}

// mapParts normalizes the manifest and refuses a duplicate sequence before a
// single byte is sent. Two parts claiming the same sequence is a source bug that
// would otherwise show up as a silent overwrite.
func (p *Producer) mapParts(a CityArtifact) ([]Part, error) {
	out := make([]Part, 0, len(a.Parts))
	seen := map[int]bool{}
	for _, cp := range a.Parts {
		if seen[cp.Sequence] {
			return nil, fmt.Errorf("%w: part sequence %d appears twice", ErrInvalidArtifact, cp.Sequence)
		}
		seen[cp.Sequence] = true
		part, err := p.Mapper.MapPart(a, cp)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	// Out-of-order arrival is a source property, not a protocol violation: order
	// the manifest here and let contiguity be judged on sequences.
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func (p *Producer) pushParts(ctx context.Context, key string, st *State, parts []Part) (uploaded, skipped int, err error) {
	for _, part := range parts {
		digest := part.Digest()

		if part.Sequence <= st.Frontier {
			// Already acknowledged. Same bytes means a retry we can drop;
			// different bytes means the manifest was rewritten underneath us.
			if have, ok := st.digests()[part.Sequence]; ok && have != digest {
				return uploaded, skipped, fmt.Errorf("%w: part %d was %s, manifest now says %s",
					ErrChangedManifest, part.Sequence, have, digest)
			}
			skipped++
			continue
		}
		if st.Finalized {
			return uploaded, skipped, fmt.Errorf("%w: part %d arrived after finalize", ErrFinalized, part.Sequence)
		}
		if part.Sequence != st.Frontier+1 {
			return uploaded, skipped, fmt.Errorf("%w: expected part %d, got %d",
				ErrGap, st.Frontier+1, part.Sequence)
		}
		if err := ScanOutbound(part); err != nil {
			return uploaded, skipped, err
		}
		art, err := p.API.UploadArtifactContent(ctx, st.ArtifactID, part,
			idempotencyKey(p.Mapper.Source, kindPart,
				st.ArtifactID+"/"+strconv.Itoa(part.Sequence), digest))
		if err != nil {
			return uploaded, skipped, normalizeUpstream(OpUploadArtifactContent, err)
		}
		if art.ID != st.ArtifactID {
			return uploaded, skipped, fmt.Errorf("%w: part %d acknowledged against artifact %q, not %q",
				ErrIdentityDrift, part.Sequence, art.ID, st.ArtifactID)
		}
		st.Frontier = part.Sequence
		st.digests()[part.Sequence] = digest
		uploaded++
		if err := p.Store.Save(ctx, key, *st); err != nil {
			return uploaded, skipped, err
		}
	}
	return uploaded, skipped, nil
}

// Metadata reads an artifact's normalized metadata. It is the metadata category
// and it can reach nothing else: there is no byte, no evidence statement and no
// reference in what it returns.
func (p *Producer) Metadata(ctx context.Context, artifactID string) (Artifact, error) {
	art, err := p.API.GetArtifact(ctx, artifactID)
	if err != nil {
		return Artifact{}, normalizeUpstream(OpGetArtifact, err)
	}
	return art, nil
}

// Evidence reads an artifact's evidence entries. Separate category, separate
// scope, separate call — a metadata read never carries these.
func (p *Producer) Evidence(ctx context.Context, artifactID string) ([]EvidenceEntry, error) {
	_, entries, err := p.API.GetArtifactEvidence(ctx, artifactID)
	if err != nil {
		return nil, normalizeUpstream(OpGetArtifactEvidence, err)
	}
	return entries, nil
}

// References reads an artifact's outbound references. Separate category,
// separate scope, separate call.
func (p *Producer) References(ctx context.Context, artifactID string) ([]Reference, error) {
	_, refs, err := p.API.GetArtifactReferences(ctx, artifactID)
	if err != nil {
		return nil, normalizeUpstream(OpGetArtifactReferences, err)
	}
	return refs, nil
}

// Content reads a bounded range of an artifact's bytes. It is the only method in
// this package that returns content, and it only ever returns content the server
// handed over — never a location to fetch it from.
func (p *Producer) Content(ctx context.Context, artifactID string, rng Range) (Chunk, error) {
	_, chunk, err := p.API.GetArtifactContent(ctx, artifactID, rng)
	if err != nil {
		return Chunk{}, normalizeUpstream(OpGetArtifactContent, err)
	}
	return chunk, nil
}

// List enumerates artifacts this credential can see. The source filter is a
// request the server validates; nothing here assumes it was honored.
func (p *Producer) List(ctx context.Context, q ListQuery) ([]Artifact, string, error) {
	arts, cursor, err := p.API.ListArtifacts(ctx, q)
	if err != nil {
		return nil, "", normalizeUpstream(OpListArtifacts, err)
	}
	return arts, cursor, nil
}

// verifyReadBack checks the normalized read-back against what this adapter
// actually sent. It is the retrieval half of the round trip: an artifact whose
// links, kind, media type, provenance or digest came back different is a drift
// this producer reports rather than records as success.
func verifyReadBack(got Artifact, st State, sent CreateRequest, src Source) error {
	switch {
	case got.ID != st.ArtifactID:
		return fmt.Errorf("%w: read back artifact %q, expected %q", ErrIdentityDrift, got.ID, st.ArtifactID)
	case got.SourceID != src.SourceID:
		return fmt.Errorf("%w: artifact %q reports source %q, expected %q",
			ErrIdentityDrift, got.ID, got.SourceID, src.SourceID)
	case got.Kind != sent.Kind:
		return fmt.Errorf("%w: artifact %q reports kind %q, sent %q", ErrIdentityDrift, got.ID, got.Kind, sent.Kind)
	case got.MediaType != sent.MediaType:
		return fmt.Errorf("%w: artifact %q reports media type %q, sent %q",
			ErrIdentityDrift, got.ID, got.MediaType, sent.MediaType)
	case got.Links != sent.Links:
		return fmt.Errorf("%w: artifact %q reports links %+v, sent %+v", ErrIdentityDrift, got.ID, got.Links, sent.Links)
	case !got.Finalized():
		return fmt.Errorf("%w: artifact %q read back unfinalized", ErrIdentityDrift, got.ID)
	case st.ContentDigest != "" && got.Digest != "" && got.Digest != st.ContentDigest:
		return fmt.Errorf("%w: artifact %q stored digest %q, asserted %q",
			ErrChangedManifest, got.ID, got.Digest, st.ContentDigest)
	}
	return nil
}

// contentDigest is the digest of the whole manifest, taken over the parts'
// bytes in sequence order. It is the assertion finalize carries, so a producer
// can only ever assert content that actually left this process.
func contentDigest(parts []Part) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p.Bytes)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// payloadDigest is the canonical digest of a request body. It is what makes a
// replay comparable: the same City artifact maps to the same bytes, so a digest
// difference is a real source change and never a serialization artifact.
func payloadDigest(body any) string {
	raw, err := json.Marshal(body)
	if err != nil {
		// json.Marshal on these closed DTOs cannot fail; a digest that cannot be
		// computed must not compare equal to anything.
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
// key is a conflict the server refuses. Nothing volatile — no clock, no attempt
// counter, no credential material — is in the preimage, so a credential rotation
// or a restart changes nothing about which key a request travels under. The
// output is a v5-shaped UUID because the contract requires a UUID.
func idempotencyKey(source Source, kind, nativeID, discriminator string) string {
	preimage := strings.Join([]string{
		"cityartifact/idempotency/v1",
		source.Kind, source.SourceID,
		strconv.FormatUint(source.Epoch, 10),
		kind, nativeID, discriminator,
	}, "\x1f")
	sum := sha256.Sum256([]byte(preimage))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// normalizeUpstream collapses a client failure into this package's own typed
// error.
//
// The upstream text is DROPPED, not wrapped: an error string is the easiest
// place for a raw response body, an upstream token, a signed URL or a storage
// path to escape, and a producer has no use for any of them. What survives is
// the operation name, this package's sentinel, and — for a cancellation — the
// context error, whose text is a fixed constant.
func normalizeUpstream(operationID string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrContentDenied):
		return fmt.Errorf("cityartifact: %s: %w", operationID, ErrContentDenied)
	case errors.Is(err, ErrNotReadable):
		return fmt.Errorf("cityartifact: %s: %w", operationID, ErrNotReadable)
	case errors.Is(err, ErrFinalized):
		return fmt.Errorf("cityartifact: %s: %w", operationID, ErrFinalized)
	case errors.Is(err, ErrChangedManifest):
		return fmt.Errorf("cityartifact: %s: %w", operationID, ErrChangedManifest)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("cityartifact: %s: %w: %w", operationID, ErrUpstream, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("cityartifact: %s: %w: %w", operationID, ErrUpstream, context.Canceled)
	}
	return fmt.Errorf("cityartifact: %s: %w", operationID, ErrUpstream)
}
