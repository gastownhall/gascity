package cityartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrIdempotencyConflict is the fake's spelling of the server refusing a
// replayed key whose payload changed. It exists so a test can assert that this
// adapter never lands there by accident: a correct producer derives a key from
// stable identity, so a conflict means the derivation drifted.
var ErrIdempotencyConflict = errors.New("cityartifact: idempotency key replayed with a different payload")

// Fake is an in-memory stand-in for the Beads Team Server's artifact routes,
// implementing the semantics this adapter is written against:
//
//   - identity is server-minted; a producer's own ID never becomes an artifact ID
//   - writes require the enrolled source that created the artifact
//   - an unfinalized artifact is NOT readable, by anyone
//   - finalize is one-way and idempotent, and checks an asserted digest
//   - a mutation key replays an identical payload and conflicts on a changed one
//   - the four categories are four separate reads
//
// One Fake is one tenant. [Fake.Client] binds a credential to it, which is what
// lets a test run a City producer and a custom producer against the same server
// and compare what came out.
type Fake struct {
	mu      sync.Mutex
	seq     int
	clock   time.Time
	records map[string]*fakeRecord
	keys    map[string]fakeKey
	links   map[string]bool
	faults  map[string][]error
	calls   []string
}

type fakeRecord struct {
	id          string
	sourceID    string
	producer    string
	kind        string
	mediaType   string
	links       Links
	parts       [][]byte
	createdAt   time.Time
	updatedAt   time.Time
	finalizedAt *time.Time
	evidence    []EvidenceEntry
	references  []Reference
}

type fakeKey struct {
	digest   string
	artifact Artifact
}

// NewFake returns an empty tenant with a fixed clock.
func NewFake() *Fake {
	return &Fake{
		clock:   time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		records: map[string]*fakeRecord{},
		keys:    map[string]fakeKey{},
		links:   map[string]bool{},
		faults:  map[string][]error{},
	}
}

// Authorize marks link targets this tenant can see. Anything not authorized is
// foreign, and foreign and absent are one answer.
func (f *Fake) Authorize(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.links[id] = true
	}
}

// FailNext queues one failure for an operation. Queued failures are consumed in
// order, so a test can fail an upload once and let the retry through.
func (f *Fake) FailNext(operationID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[operationID] = append(f.faults[operationID], err)
}

// Calls returns the operation IDs dispatched so far, in order. It is how a test
// asserts that a refusal stopped the adapter instead of sending it looking for
// another way in.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// PartCount reports how many parts the server actually stored, which is what
// "restart resumes without duplicate parts" is measured against.
func (f *Fake) PartCount(artifactID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[artifactID]
	if !ok {
		return 0
	}
	return len(rec.parts)
}

// ArtifactCount reports how many artifacts exist in the tenant.
func (f *Fake) ArtifactCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// Seed installs evidence and references on an artifact the way the server's
// upstream would; the public API has no route that writes them.
func (f *Fake) Seed(artifactID string, evidence []EvidenceEntry, refs []Reference) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec, ok := f.records[artifactID]; ok {
		rec.evidence, rec.references = evidence, refs
	}
}

// Client binds one enrolled credential to this tenant.
func (f *Fake) Client(sourceID, producer string) API {
	return &fakeClient{f: f, sourceID: sourceID, producer: producer}
}

type fakeClient struct {
	f        *Fake
	sourceID string
	producer string
}

func (f *Fake) enter(operationID string) error {
	f.calls = append(f.calls, operationID)
	queued := f.faults[operationID]
	if len(queued) == 0 {
		return nil
	}
	err := queued[0]
	f.faults[operationID] = queued[1:]
	return err
}

func (f *Fake) mint(prefix string) string {
	f.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", prefix, f.seq)))
	return prefix + "_" + hex.EncodeToString(sum[:])[:20]
}

func (f *Fake) render(rec *fakeRecord) Artifact {
	size := 0
	for _, p := range rec.parts {
		size += len(p)
	}
	art := Artifact{
		ID:          rec.id,
		Kind:        rec.kind,
		MediaType:   rec.mediaType,
		ByteSize:    int64(size),
		Status:      "open",
		SourceID:    rec.sourceID,
		Producer:    rec.producer,
		Links:       rec.links,
		CreatedAt:   rec.createdAt,
		UpdatedAt:   rec.updatedAt,
		FinalizedAt: rec.finalizedAt,
	}
	if rec.finalizedAt != nil {
		art.Status = "final"
		art.Digest = rec.digest()
	}
	return art
}

func (r *fakeRecord) digest() string {
	h := sha256.New()
	for _, p := range r.parts {
		h.Write(p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// replay implements the mutation idempotency contract: an identical payload
// under a known key returns the first response without dispatching a second
// mutation, and a different payload under that key is a conflict.
// The key is namespaced by the enrolled source, as the server's idempotency
// namespace is: one source cannot replay into another's accepted response.
func (f *Fake) replay(sourceID, key, digest string) (Artifact, bool, error) {
	if strings.TrimSpace(key) == "" {
		return Artifact{}, false, fmt.Errorf("%w: mutation requires an idempotency key", ErrInvalidArtifact)
	}
	key = sourceID + "\x1f" + key
	prior, ok := f.keys[key]
	if !ok {
		return Artifact{}, false, nil
	}
	if prior.digest != digest {
		return Artifact{}, false, ErrIdempotencyConflict
	}
	return prior.artifact, true, nil
}

func (c *fakeClient) CreateArtifact(_ context.Context, body CreateRequest, idempotencyKey string) (Artifact, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpCreateArtifact); err != nil {
		return Artifact{}, err
	}
	digest := payloadDigest(body)
	if art, replayed, err := f.replay(c.sourceID, idempotencyKey, digest); err != nil || replayed {
		return art, err
	}
	// Links are verified before anything is stored. A foreign link means the
	// artifact is never created, so no part upload can follow it.
	for _, target := range []string{body.Links.ProjectID, body.Links.IssueID, body.Links.RunID, body.Links.SessionID} {
		if target == "" {
			continue
		}
		if !f.links[target] {
			return Artifact{}, ErrNotReadable
		}
	}
	rec := &fakeRecord{
		id:        f.mint("art"),
		sourceID:  c.sourceID,
		producer:  c.producer,
		kind:      body.Kind,
		mediaType: body.MediaType,
		links:     body.Links,
		createdAt: f.clock,
		updatedAt: f.clock,
	}
	f.records[rec.id] = rec
	art := f.render(rec)
	f.keys[c.sourceID+"\x1f"+idempotencyKey] = fakeKey{digest: digest, artifact: art}
	return art, nil
}

func (c *fakeClient) UploadArtifactContent(_ context.Context, artifactID string, part Part, idempotencyKey string) (Artifact, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpUploadArtifactContent); err != nil {
		return Artifact{}, err
	}
	digest := part.Digest() + "/" + part.MediaType
	if art, replayed, err := f.replay(c.sourceID, idempotencyKey, digest); err != nil || replayed {
		return art, err
	}
	rec, err := f.owned(artifactID, c.sourceID)
	if err != nil {
		return Artifact{}, err
	}
	if rec.finalizedAt != nil {
		return Artifact{}, ErrFinalized
	}
	if len(part.Bytes) == 0 {
		return Artifact{}, fmt.Errorf("%w: content part is empty", ErrInvalidArtifact)
	}
	rec.parts = append(rec.parts, append([]byte(nil), part.Bytes...))
	rec.updatedAt = f.clock
	art := f.render(rec)
	f.keys[c.sourceID+"\x1f"+idempotencyKey] = fakeKey{digest: digest, artifact: art}
	return art, nil
}

func (c *fakeClient) FinalizeArtifact(_ context.Context, artifactID string, body FinalizeRequest, idempotencyKey string) (Artifact, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpFinalizeArtifact); err != nil {
		return Artifact{}, err
	}
	digest := payloadDigest(body)
	if art, replayed, err := f.replay(c.sourceID, idempotencyKey, digest); err != nil || replayed {
		return art, err
	}
	rec, err := f.owned(artifactID, c.sourceID)
	if err != nil {
		return Artifact{}, err
	}
	if want := strings.TrimSpace(body.Digest); want != "" && want != rec.digest() {
		return Artifact{}, fmt.Errorf("%w: asserted digest does not match stored content", ErrChangedManifest)
	}
	if rec.finalizedAt == nil {
		at := f.clock
		rec.finalizedAt = &at
		rec.updatedAt = at
	}
	art := f.render(rec)
	f.keys[c.sourceID+"\x1f"+idempotencyKey] = fakeKey{digest: digest, artifact: art}
	return art, nil
}

func (c *fakeClient) GetArtifact(_ context.Context, artifactID string) (Artifact, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpGetArtifact); err != nil {
		return Artifact{}, err
	}
	rec, err := f.readable(artifactID)
	if err != nil {
		return Artifact{}, err
	}
	return f.render(rec), nil
}

func (c *fakeClient) GetArtifactEvidence(_ context.Context, artifactID string) (Artifact, []EvidenceEntry, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpGetArtifactEvidence); err != nil {
		return Artifact{}, nil, err
	}
	rec, err := f.readable(artifactID)
	if err != nil {
		return Artifact{}, nil, err
	}
	return f.render(rec), append([]EvidenceEntry(nil), rec.evidence...), nil
}

func (c *fakeClient) GetArtifactReferences(_ context.Context, artifactID string) (Artifact, []Reference, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpGetArtifactReferences); err != nil {
		return Artifact{}, nil, err
	}
	rec, err := f.readable(artifactID)
	if err != nil {
		return Artifact{}, nil, err
	}
	return f.render(rec), append([]Reference(nil), rec.references...), nil
}

func (c *fakeClient) GetArtifactContent(_ context.Context, artifactID string, rng Range) (Artifact, Chunk, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpGetArtifactContent); err != nil {
		return Artifact{}, Chunk{}, err
	}
	rec, err := f.readable(artifactID)
	if err != nil {
		return Artifact{}, Chunk{}, err
	}
	var all []byte
	for _, p := range rec.parts {
		all = append(all, p...)
	}
	start, end := rng.Start, rng.End
	if end <= 0 || end > int64(len(all)) {
		end = int64(len(all))
	}
	if start < 0 || start > end {
		return Artifact{}, Chunk{}, fmt.Errorf("%w: range is outside the artifact", ErrInvalidArtifact)
	}
	return f.render(rec), Chunk{
		Bytes:     append([]byte(nil), all[start:end]...),
		MediaType: rec.mediaType,
		TotalSize: int64(len(all)),
		Start:     start,
		End:       end,
	}, nil
}

func (c *fakeClient) ListArtifacts(_ context.Context, q ListQuery) ([]Artifact, string, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(OpListArtifacts); err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(f.records))
	for id := range f.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Artifact, 0, len(ids))
	for _, id := range ids {
		rec := f.records[id]
		if q.SourceID != "" && rec.sourceID != q.SourceID {
			continue
		}
		out = append(out, f.render(rec))
	}
	return out, "", nil
}

// readable resolves an artifact a reader may see: present and finalized. Absent
// and unfinalized are one answer, so a probe cannot tell "not there" from "not
// ready".
func (f *Fake) readable(artifactID string) (*fakeRecord, error) {
	rec, ok := f.records[strings.TrimSpace(artifactID)]
	if !ok || rec.finalizedAt == nil {
		return nil, ErrNotReadable
	}
	return rec, nil
}

// owned resolves an artifact a producer may write: present and produced by this
// source. Another source's artifact is the same absence as a stranger's.
func (f *Fake) owned(artifactID, sourceID string) (*fakeRecord, error) {
	rec, ok := f.records[strings.TrimSpace(artifactID)]
	if !ok || rec.sourceID != sourceID {
		return nil, ErrNotReadable
	}
	return rec, nil
}
