package cityneutral

import (
	"context"
	"time"
)

// Status is the closed lifecycle-independent execution status of a neutral run
// or session. City may not invent one: the canonical contract has no raw
// passthrough, so an unmappable City state lands on StatusUnknown rather than
// being echoed through.
type Status string

// The closed status vocabulary.
const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled" //nolint:misspell // frozen wire value of the neutral contract
	StatusUnknown   Status = "unknown"
)

// Lifecycle is the input-completeness half, orthogonal to Status: a run can be
// succeeded while its input is still partial because records are still
// arriving. Only final is terminal, and the transition is one-way.
type Lifecycle string

// The closed lifecycle vocabulary.
const (
	LifecyclePartial Lifecycle = "partial"
	LifecycleFinal   Lifecycle = "final"
)

// Role is the closed speaking role of a transcript record.
type Role string

// The closed role vocabulary.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	RoleUnknown   Role = "unknown"
)

// Coverage is the source's own claim about how complete this record's input is.
// The vocabulary is the server's closed set, spelled in full because it is a
// wire vocabulary; this producer emits only known, partial and unavailable.
type Coverage string

// The closed coverage vocabulary.
const (
	CoverageKnown         Coverage = "known"
	CoverageUnavailable   Coverage = "unavailable"
	CoveragePartial       Coverage = "partial"
	CoverageMetered       Coverage = "metered"
	CoverageEstimated     Coverage = "estimated"
	CoverageLegacyUnknown Coverage = "legacy_unknown"
)

// ContributorRef is the represented author of a record: attribution only, never
// an authorization input. It is distinct from the uploader (the credential the
// request arrived on, which the server derives and this package never sends)
// and from the source (the enrolled producer identity).
type ContributorRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Upsert is the request body of upsertRun and upsertRunSession. One input type
// serves both, exactly as the server's contract does: what differs is the
// container, which comes from the path and the credential and never from the
// body.
type Upsert struct {
	SourceEntityID string          `json:"source_entity_id"`
	SourceVersion  uint64          `json:"source_version"`
	Epoch          uint64          `json:"epoch"`
	Status         Status          `json:"status"`
	Lifecycle      Lifecycle       `json:"lifecycle"`
	StartedAt      time.Time       `json:"started_at"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	ProjectID      string          `json:"project_id,omitempty"`
	IssueID        string          `json:"issue_id,omitempty"`
	Title          string          `json:"title,omitempty"`
	Coverage       Coverage        `json:"coverage,omitempty"`
	Contributor    *ContributorRef `json:"contributor,omitempty"`
}

// TranscriptRecordIngest is the request body of createTranscriptRecord.
type TranscriptRecordIngest struct {
	SourceMessageID string          `json:"source_message_id"`
	SourceVersion   uint64          `json:"source_version"`
	Epoch           uint64          `json:"epoch"`
	Ordinal         *uint64         `json:"ordinal,omitempty"`
	Role            Role            `json:"role"`
	Author          *ContributorRef `json:"author,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Text            string          `json:"text,omitempty"`
	ContentRef      string          `json:"content_ref,omitempty"`
	Coverage        Coverage        `json:"coverage,omitempty"`
}

// Run is the closed neutral run as a read returns it. ID is the server-minted
// Team ID; SourceRunID is the bounded source-native ID. Both are present so a
// caller can tell them apart, and this producer treats ID as read-only fact.
type Run struct {
	ID          string    `json:"id"`
	SourceRunID string    `json:"source_run_id"`
	SourceID    string    `json:"source_id"`
	SourceKind  string    `json:"source_kind"`
	Status      Status    `json:"status"`
	Lifecycle   Lifecycle `json:"lifecycle"`
	Epoch       uint64    `json:"epoch"`
	Version     uint64    `json:"version"`
	Finalized   bool      `json:"finalized"`
	ETag        string    `json:"etag"`
}

// Session is the closed neutral session as a read returns it. RunID is the
// same-workspace link to its run, resolved by the server at attach time.
type Session struct {
	ID              string    `json:"id"`
	RunID           string    `json:"run_id"`
	SourceSessionID string    `json:"source_session_id"`
	SourceID        string    `json:"source_id"`
	SourceKind      string    `json:"source_kind"`
	Status          Status    `json:"status"`
	Lifecycle       Lifecycle `json:"lifecycle"`
	Epoch           uint64    `json:"epoch"`
	Version         uint64    `json:"version"`
	Finalized       bool      `json:"finalized"`
	ETag            string    `json:"etag"`
}

// TranscriptRecord is the closed neutral transcript record as a read returns
// it.
type TranscriptRecord struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	SourceMessageID string          `json:"source_message_id"`
	SourceID        string          `json:"source_id"`
	SourceKind      string          `json:"source_kind"`
	Role            Role            `json:"role"`
	Author          *ContributorRef `json:"author"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Ordinal         *uint64         `json:"ordinal"`
	Epoch           uint64          `json:"epoch"`
	Version         uint64          `json:"version"`
	ContentStatus   string          `json:"content_status"`
	ETag            string          `json:"etag"`
}

// TranscriptItem is one entry of the ordered session transcript aggregate.
type TranscriptItem struct {
	RecordID        string          `json:"record_id"`
	SourceMessageID string          `json:"source_message_id"`
	Role            Role            `json:"role"`
	Author          *ContributorRef `json:"author"`
	Ordinal         *uint64         `json:"ordinal"`
	ContentStatus   string          `json:"content_status"`
	Text            string          `json:"text"`
}

// API is the seam onto the public v1 client.
//
// It carries exactly the eight frozen operations this adapter needs and no
// transport vocabulary at all — no base URL, no header, no status code. That is
// what keeps this package from hand-rolling HTTP: the concrete implementation
// is the generated public client for the canonical contract, and everything
// below is written against these signatures.
//
// NOTE: the contract program generates a public TypeScript client and a
// server-side Go interface; there is no generated Go *client* module a Go
// producer can import today. This interface is the shape that client binds to,
// method-for-method, so binding it is a wiring change and never a logic change.
//
// The idempotency key is a caller-supplied argument rather than something an
// implementation invents, because the key is part of THIS package's
// dedup contract: [Producer] derives it from stable source identity so that a
// retry replays and a changed payload conflicts.
type API interface {
	UpsertRun(ctx context.Context, body Upsert, idempotencyKey string) (Run, error)
	GetRun(ctx context.Context, runTeamID string) (Run, error)
	UpsertRunSession(ctx context.Context, runTeamID string, body Upsert, idempotencyKey string) (Session, error)
	GetSession(ctx context.Context, sessionTeamID string) (Session, error)
	FinalizeSession(ctx context.Context, sessionTeamID string, idempotencyKey string) (Session, error)
	CreateTranscriptRecord(ctx context.Context, sessionTeamID string, body TranscriptRecordIngest, idempotencyKey string) (TranscriptRecord, error)
	ListTranscriptRecords(ctx context.Context, sessionTeamID string) ([]TranscriptRecord, error)
	GetSessionTranscript(ctx context.Context, sessionTeamID string) ([]TranscriptItem, error)
}
