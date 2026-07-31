package cityartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Links are the same-workspace resources an artifact is attached to. The
// artifact owns none of them: they are references to resources that exist
// independently, and the server's link verifier is what keeps a caller from
// attaching to something it cannot see. A foreign link and an absent link are
// one answer, here and there.
type Links struct {
	ProjectID string `json:"project_id,omitempty"`
	IssueID   string `json:"issue_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// Empty reports whether the artifact declares no links at all. An unlinked
// artifact is legal; it is simply not attached to anything.
func (l Links) Empty() bool {
	return l.ProjectID == "" && l.IssueID == "" && l.RunID == "" && l.SessionID == ""
}

// CreateRequest is the closed create body of createArtifact.
//
// There is no producer, source, id, status, digest or display member, exactly
// as the server's body has none: provenance is credential-derived and identity
// is server-minted. A City producer that wanted to name its own artifact has
// nowhere to write it.
type CreateRequest struct {
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	Links     Links  `json:"links"`
}

// FinalizeRequest is the closed finalize body of finalizeArtifact. Finalization
// asserts the content the producer believes it uploaded; the server checks that
// assertion against what it actually stored.
type FinalizeRequest struct {
	Digest string `json:"digest,omitempty"`
}

// Part is one content part of uploadArtifactContent. Sequence is the producer's
// own ordering; the server stores parts in the order it accepts them and this
// adapter never sends a part out of order.
type Part struct {
	Bytes     []byte `json:"-"`
	MediaType string `json:"media_type"`
	Sequence  int    `json:"sequence"`
}

// Digest is the part's content digest, in the server's `sha256:<hex>` spelling.
// It is what a part upload's identity is taken over, so the same bytes retried
// are the same request and different bytes under the same sequence are a
// conflict rather than an overwrite.
func (p Part) Digest() string {
	sum := sha256.Sum256(p.Bytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Artifact is the public read DTO, normalized. There is no storage location, no
// signed URL, no upstream token and no raw upstream body anywhere in this
// shape, because there is none in the server's.
type Artifact struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	MediaType   string     `json:"media_type"`
	ByteSize    int64      `json:"byte_size"`
	Digest      string     `json:"digest,omitempty"`
	Status      string     `json:"status"`
	SourceID    string     `json:"source_id"`
	Producer    string     `json:"producer"`
	Links       Links      `json:"links"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
	Display     string     `json:"display,omitempty"`
}

// Finalized reports whether the server has sealed this artifact. An unfinalized
// artifact is writable and unreadable; a finalized one is readable and frozen.
func (a Artifact) Finalized() bool { return a.FinalizedAt != nil }

// EvidenceEntry is one normalized evidence statement, read behind its own
// scope. It is never merged into the metadata read.
type EvidenceEntry struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Statement  string    `json:"statement"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Reference is one normalized outbound reference, read behind its own scope.
type Reference struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	TargetID string `json:"target_id"`
}

// Chunk is a bounded read of an artifact's bytes. It carries bytes and a media
// type, never a location: this API hands a caller content, not a way to fetch
// content elsewhere.
type Chunk struct {
	Bytes      []byte
	MediaType  string
	TotalSize  int64
	Start, End int64
}

// Range is a bounded content read. A zero End means "to the end".
type Range struct{ Start, End int64 }

// ListQuery is the bounded filter of listArtifacts. The source filter is a
// request, not an authority: the server validates it against the caller's own
// enrolment and this adapter never assumes it was honored.
type ListQuery struct {
	SourceID string
	Limit    int
	Cursor   string
}

// API is the seam onto the public v1 client.
//
// It carries exactly the eight frozen artifact operations and no transport
// vocabulary at all — no base URL, no header, no status code, no retry policy.
// That is what keeps this package from hand-rolling HTTP: the concrete
// implementation is the generated public client for the canonical contract, and
// everything in this package is written against these signatures.
//
// NOTE: the contract program generates a public TypeScript client and a
// server-side Go interface; there is no generated Go *client* module a Go
// producer can import today. This interface is the shape that client binds to,
// operation for operation, so binding it is a wiring change and never a logic
// change.
//
// The idempotency key is a caller-supplied argument rather than something an
// implementation invents, because the key is part of THIS package's dedup
// contract: [Producer] derives it from stable source identity so a retry
// replays and a changed payload conflicts. Read operations take no key: the
// frozen matrix gives them idempotency policy "none".
type API interface {
	CreateArtifact(ctx context.Context, body CreateRequest, idempotencyKey string) (Artifact, error)
	ListArtifacts(ctx context.Context, q ListQuery) ([]Artifact, string, error)
	GetArtifact(ctx context.Context, artifactID string) (Artifact, error)
	GetArtifactEvidence(ctx context.Context, artifactID string) (Artifact, []EvidenceEntry, error)
	GetArtifactReferences(ctx context.Context, artifactID string) (Artifact, []Reference, error)
	UploadArtifactContent(ctx context.Context, artifactID string, part Part, idempotencyKey string) (Artifact, error)
	FinalizeArtifact(ctx context.Context, artifactID string, body FinalizeRequest, idempotencyKey string) (Artifact, error)
	GetArtifactContent(ctx context.Context, artifactID string, rng Range) (Artifact, Chunk, error)
}
