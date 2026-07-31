package cityinference

import (
	"context"
	"time"
)

// Outcome is the closed normalized outcome vocabulary of an invocation. City
// may not invent one: the canonical contract has no raw passthrough, so an
// unmappable City state lands on OutcomeUnknown rather than being echoed
// through.
type Outcome string

// The closed outcome vocabulary.
const (
	OutcomeOK        Outcome = "ok"
	OutcomeError     Outcome = "error"
	OutcomeCancelled Outcome = "cancelled" //nolint:misspell // frozen wire value of the neutral contract
	OutcomeUnknown   Outcome = "unknown"
)

// Coverage is a source's claim about how complete one field group is. The
// vocabulary is the server's closed set, spelled in full because it is a wire
// vocabulary. This adapter never sends a coverage value — coverage is
// server-derived and immutable after admission — but it predicts one so a test
// and an operator can check what was claimed on City's behalf.
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

// The closed coverage field-group key set. Every one of these keys appears on
// every admitted inference: a missing key is a schema violation, not an
// implicit "unavailable", because silence is what coverage exists to abolish.
const (
	FieldGroupTokens     = "usage.tokens"
	FieldGroupCost       = "usage.cost"
	FieldGroupSavings    = "usage.savings"
	FieldGroupStep       = "attribution.step"
	FieldGroupTranscript = "attribution.transcript_record"
	FieldGroupOutcome    = "outcome"
)

// CoverageFieldGroups returns the closed key set in a stable order.
func CoverageFieldGroups() []string {
	return []string{
		FieldGroupTokens, FieldGroupCost, FieldGroupSavings,
		FieldGroupStep, FieldGroupTranscript, FieldGroupOutcome,
	}
}

// IdentityScope says whether a derived identity rests on a provider-assigned
// request ID or on a locally generated one.
type IdentityScope string

const (
	// ScopeInvocation means the upstream request ID is provider-assigned, so
	// the triplet names the invocation itself and an exact-tuple match is a
	// genuine duplicate.
	ScopeInvocation IdentityScope = "invocation"
	// ScopeObservation means the upstream request ID was generated locally.
	// Two such IDs for the same real call never converge, so the record
	// identifies our observation of an invocation and not the invocation.
	// Fold-ineligible by construction.
	ScopeObservation IdentityScope = "observation"
)

// NativeIdentity is the composite that is the only durable native identity of a
// model invocation. All three components are REQUIRED members; SessionID MAY be
// the empty string, which is a legal legacy value and not a null.
type NativeIdentity struct {
	// Kind pins the derivation. It names the triplet SHAPE, not the producer:
	// the server admits exactly one shape, so a City-flavored kind would be a
	// schema the contract rejects, not a courtesy.
	Kind          string `json:"kind"`
	Tenant        string `json:"tenant"`
	SessionID     string `json:"session_id"`
	UpstreamReqID string `json:"upstream_req_id"`
}

// NativeIdentityKind is the one admitted triplet shape.
const NativeIdentityKind = "manifold.triplet.v1"

// UsageObservation is one contribution's usage figures with their provenance.
//
// Token counts are what the party that did the work counted. Cost and savings
// are what a price table computed afterwards. They are separate members because
// they are separate field groups with separate classes, and "estimated is
// weaker than metered" is only a sound statement about the same quantity.
type UsageObservation struct {
	InputTokens       *uint64 `json:"input_tokens,omitempty"`
	OutputTokens      *uint64 `json:"output_tokens,omitempty"`
	CachedInputTokens *uint64 `json:"cached_input_tokens,omitempty"`

	// CostMicros is derived money: metered tokens times an approximate price
	// table. It is an estimate OF billing, never a billed figure, and it never
	// reaches a read path.
	CostMicros *uint64 `json:"cost_micros,omitempty"`
	// SavedInputTokens and SavedCostMicros are an explicit upper bound that is
	// never billed. Their own field group keeps them from being summed into
	// cost.
	SavedInputTokens *uint64 `json:"saved_input_tokens,omitempty"`
	SavedCostMicros  *uint64 `json:"saved_cost_micros,omitempty"`
}

func (u *UsageObservation) metered() bool {
	return u != nil && (u.InputTokens != nil || u.OutputTokens != nil || u.CachedInputTokens != nil)
}

// CreateInferenceRequest is the request body of createInference.
//
// Ordinal and SourceStepRef are present as explicitly refused members rather
// than absent from the struct, mirroring the server. That is deliberate on both
// sides: a caller that hand-builds a request and sets one gets the specific
// reason code from [Preflight] with zero bytes on the wire, instead of learning
// from a correlated 422 that it maybe mistyped a field.
type CreateInferenceRequest struct {
	NativeIdentity NativeIdentity `json:"native_identity"`

	// ExternalInferenceID is optional and, when present, must equal the
	// server's own derivation. It is a checksum this adapter offers, never a
	// choice it makes.
	ExternalInferenceID string `json:"external_inference_id,omitempty"`

	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Outcome  Outcome `json:"outcome"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	Epoch uint64 `json:"epoch,omitempty"`

	// SessionTeamID and RunTeamID are the canonical containers, always
	// server-minted Team IDs read back from a prior upsert. A City-native
	// session or run ID here is a not-found, not a shortcut.
	SessionTeamID string `json:"session_id,omitempty"`
	RunTeamID     string `json:"run_id,omitempty"`

	// TranscriptRecordID is the OPTIONAL canonical transcript-record Team ID.
	// A provider message locator offered here is a not-found in exactly the
	// same shape as a foreign record.
	TranscriptRecordID string `json:"transcript_record_id,omitempty"`

	// ObservationID discriminates two honest observations of the SAME
	// invocation. It is part of the contribution's identity and never of the
	// inference's, which is what lets a metered and an estimated observation
	// stand as two contributions on one inference without either being read as
	// a payload divergence of the other.
	ObservationID string `json:"observation_id,omitempty"`

	Usage *UsageObservation `json:"usage,omitempty"`

	// Ordinal is refused: this domain asserts no order, so offering one is
	// synthesis.
	Ordinal *uint64 `json:"ordinal,omitempty"`
	// SourceStepRef is refused: no step concept exists at the source, so the
	// honest expression is coverage class unavailable, not a synthesized link.
	SourceStepRef string `json:"source_step_ref,omitempty"`
}

// TokenUsage is a folded metered count. Nil rather than zero when nothing was
// metered: a 0 means "counted zero", never "we have no number".
type TokenUsage struct {
	InputTokens       uint64 `json:"input_tokens"`
	OutputTokens      uint64 `json:"output_tokens"`
	CachedInputTokens uint64 `json:"cached_input_tokens"`
}

// Contribution is one observation's provenance as a read returns it. It carries
// no monetary figure, because neither read scope in the frozen matrix is a
// billing-sensitive scope.
type Contribution struct {
	ID            string            `json:"id"`
	ObservationID string            `json:"observation_id"`
	SourceID      string            `json:"source_id"`
	SourceKind    string            `json:"source_kind"`
	UploadedBy    string            `json:"uploaded_by"`
	UploaderType  string            `json:"uploader_type"`
	ObservedAt    time.Time         `json:"observed_at"`
	IngestedAt    time.Time         `json:"ingested_at"`
	Coverage      map[string]string `json:"coverage"`
	TokenUsage    *TokenUsage       `json:"token_usage"`
}

// Inference is the closed neutral inference as a read returns it. ID is the
// server-minted Team ID and ExternalInferenceID is the derived handle over the
// native triplet; both are present so a caller can tell them apart, and this
// adapter treats both as read-only fact.
type Inference struct {
	ID                  string `json:"id"`
	ExternalInferenceID string `json:"external_inference_id"`
	SourceID            string `json:"source_id"`
	SourceKind          string `json:"source_kind"`

	SessionID          string `json:"session_id"`
	RunID              string `json:"run_id"`
	TranscriptRecordID string `json:"transcript_record_id"`

	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Outcome  Outcome `json:"outcome"`

	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	IngestedAt time.Time  `json:"ingested_at"`
	Epoch      uint64     `json:"epoch"`

	// IdentityScope and FoldEligible are on the wire because a consumer that
	// sums this domain has to know which rows may be deduplicated and which may
	// not. A locally generated row that silently looked foldable is how a
	// double count becomes invisible.
	IdentityScope IdentityScope `json:"identity_scope"`
	FoldEligible  bool          `json:"fold_eligible"`

	TokenUsage    *TokenUsage       `json:"token_usage"`
	Coverage      map[string]string `json:"coverage"`
	Contributions []Contribution    `json:"contributions"`

	// Completeness is always unavailable over a best-effort producer. It is a
	// member rather than an omission because "we do not know whether we
	// received every record" is a claim this API must make out loud.
	Completeness string `json:"completeness"`

	ETag string `json:"etag"`
}

// ListFilter is the ratified filter surface of listInferences: exactly the
// frozen operation's two parameters. There is no model, vendor or cost
// dimension, because a filter surface is the easiest place for a forbidden
// similarity comparator to grow back.
type ListFilter struct {
	SourceID     string
	StartedAfter time.Time
	Cursor       string
	Limit        int
}

// Page is one page of listInferences.
type Page struct {
	Items      []Inference `json:"items"`
	NextCursor string      `json:"next_cursor"`
}

// API is the seam onto the public v1 client.
//
// It carries exactly the three frozen inference operations and no transport
// vocabulary at all — no base URL, no header, no status code. That is what
// keeps this package from hand-rolling HTTP: the concrete implementation is the
// generated public client for the canonical contract, and everything below is
// written against these signatures.
//
// NOTE: the contract program generates a public TypeScript client and a
// server-side Go interface; there is no generated Go *client* module a Go
// producer can import today. This interface is the shape that client binds to,
// method-for-method, so binding it is a wiring change and never a logic change.
//
// The idempotency key is a caller-supplied argument rather than something an
// implementation invents, because the key is part of THIS package's dedup
// contract: [Producer] derives it from stable source identity so that a retry
// replays and a changed payload conflicts.
type API interface {
	CreateInference(ctx context.Context, body CreateInferenceRequest, idempotencyKey string) (Inference, error)
	ListInferences(ctx context.Context, filter ListFilter) (Page, error)
	GetInference(ctx context.Context, inferenceTeamID string) (Inference, error)
}
