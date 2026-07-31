// Package citytransport is the Gas City event ingest client: the closed wire
// schema for POST /api/v1/city/{city}/events, its byte-stable codec, and a
// bounded-retry HTTP transport.
//
// This package is deliberately DECISION-FREE. It serializes what it is handed
// and reports what the server said. It does not decide whether an event may
// carry content, what order events go in, which events are acknowledged, or
// when a durable checkpoint may advance. Every one of those is a signed-policy
// decision owned by pkg/citysource, which imports this package. The dependency
// never points the other way, and forbidden_semantics_test.go enforces both the
// import direction and the absence of decision logic in this package's source.
//
// The wire schema is CLOSED: decoding rejects unknown fields, so a server that
// grows a field cannot smuggle it into a producer that has not been taught the
// field's policy. Encoding is canonical (compact, fixed field order, no HTML
// escaping) so a mapped offer round-trips byte-stably.
//
// The city path segment carries the SALTED city hash, never the cleartext city
// name — matching the redaction precedent already set by pkg/eventexport
// (schema v2 replaced cleartext city_id with city_hash).
package citytransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// SchemaVersion is the version of THIS wire — the api/v1 City event ingest
// upload. It is independent of pkg/eventexport.SchemaVersion, which versions
// the older operator-configured [events.export] wire. Bump when the default
// bytes or the default redaction posture of an Upload changes.
const SchemaVersion = 1

// Outcome is the per-offer admission result vocabulary. It mirrors the
// reconciliation outcome set exactly; the producer never invents one and never
// interprets an unrecognized value as success (see Acknowledged).
type Outcome string

// The admission outcome vocabulary, mirroring the reconciliation design's
// outcome set. Acknowledged() decides which of these mean "durably held".
const (
	OutcomeAdmit                  Outcome = "ADMIT"
	OutcomeAckReplay              Outcome = "ACK_REPLAY"
	OutcomeAckDuplicate           Outcome = "ACK_DUPLICATE"
	OutcomeAckStale               Outcome = "ACK_STALE"
	OutcomeParkGap                Outcome = "PARK_GAP"
	OutcomeQuarantineConflict     Outcome = "QUARANTINE_CONFLICT"
	OutcomeQuarantineIncomparable Outcome = "QUARANTINE_INCOMPARABLE"
	OutcomeQuarantineTimeBound    Outcome = "QUARANTINE_TIME_BOUND"
	OutcomeQuarantineGapExpired   Outcome = "QUARANTINE_GAP_EXPIRED"
	OutcomeEpochResetApplied      Outcome = "EPOCH_RESET_APPLIED"
	OutcomeRejectInvalidReset     Outcome = "REJECT_INVALID_RESET"
	OutcomeContractDigestMismatch Outcome = "CONTRACT_DIGEST_MISMATCH"
)

// Acknowledged reports whether an outcome means the server holds the record
// durably, which is the only class that may ever contribute to a checkpoint.
//
// It is a pure predicate on the vocabulary, stated here so the vocabulary and
// its durability meaning stay in one place. Deciding what to DO with a run of
// acknowledged outcomes — contiguity, watermark advancement — is citysource's
// job, not this package's. Note the default: an outcome this build does not
// recognize is NOT acknowledged, so a server that invents an outcome cannot
// advance a producer checkpoint by accident.
func Acknowledged(o Outcome) bool {
	switch o {
	case OutcomeAdmit, OutcomeAckReplay, OutcomeAckDuplicate:
		return true
	default:
		return false
	}
}

// Offer is one mapped City event on the wire. It is the ENTIRE set of
// source-derived fields that ever leaves the box for this wire.
//
// Every identity-ish field is an opaque reference, already gated upstream.
// Title and Formula are free-form content and ship ONLY when the mapper's
// signed content policy permitted them; this package neither sets nor clears
// them.
type Offer struct {
	Seq          uint64 `json:"seq"`
	Type         string `json:"type"`
	RecordTS     string `json:"record_ts"`
	SemanticHash string `json:"semantic_hash"`
	ActorHash    string `json:"actor_hash,omitempty"`
	Ref          string `json:"ref,omitempty"`
	RunRef       string `json:"run_ref,omitempty"`
	SessionRef   string `json:"session_ref,omitempty"`
	StepRef      string `json:"step_ref,omitempty"`
	Title        string `json:"title,omitempty"`
	Formula      string `json:"formula,omitempty"`
	// Force keyed literals so a positional Offer{...} cannot transpose the two
	// adjacent free-form content fields, or land content in an opaque-ref slot,
	// at the wire boundary. Mirrors eventexport.Envelope.
	_ struct{}
}

// Upload is one POST body: the offers for a single city in a single epoch,
// stamped with the stable source identity and BOTH signed policy digests.
//
// Both digests are required on the wire and are recorded by the server against
// every admitted record. The source-contract digest binds the order/time/schema
// semantics the offers were mapped under; the content-policy digest binds
// whether Title/Formula were permitted. They are separate because they are
// separately signed and separately revocable.
type Upload struct {
	SourceID             string  `json:"source_id"`
	Epoch                uint64  `json:"epoch"`
	SchemaVersion        int     `json:"schema_version"`
	SourceContractDigest string  `json:"source_contract_digest"`
	ContentPolicyDigest  string  `json:"content_policy_digest"`
	Events               []Offer `json:"events"`
	_                    struct{}
}

// Result is the server's verdict on one offered seq.
type Result struct {
	Seq              uint64  `json:"seq"`
	Outcome          Outcome `json:"outcome"`
	AcceptedRecordID string  `json:"accepted_record_id,omitempty"`
	ReasonCode       string  `json:"reason_code,omitempty"`
	Watermark        uint64  `json:"watermark,omitempty"`
}

// Ack is the decoded 2xx response body.
type Ack struct {
	RequestID    string   `json:"request_id"`
	PolicyDigest string   `json:"policy_digest"`
	Epoch        uint64   `json:"epoch"`
	Results      []Result `json:"results"`
}

// Problem is the RFC 9457 problem document the server returns on failure. The
// producer surfaces it verbatim rather than collapsing it to a bare status, so
// a digest mismatch names both digests as evidence.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// RequestID is the RFC 9457 extension member every response of this API
	// carries for correlation.
	RequestID string `json:"request_id,omitempty"`
	// ExpectedDigest/OfferedDigest are the CONTRACT_DIGEST_MISMATCH evidence
	// pair. Both are omitempty because they are meaningless on other problems.
	ExpectedDigest string `json:"expected_digest,omitempty"`
	OfferedDigest  string `json:"offered_digest,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("citytransport: %s (%d): %s", p.Title, p.Status, p.Detail)
	}
	return fmt.Sprintf("citytransport: %s (%d)", p.Title, p.Status)
}

// Typed failure classes. Callers branch on these rather than on status codes,
// and every one of them means "no checkpoint may advance from this attempt".
var (
	// ErrMalformedResponse means the server's bytes did not decode against the
	// closed schema — including an unknown field, which is treated as a
	// protocol break rather than being ignored.
	ErrMalformedResponse = errors.New("citytransport: malformed response")
	// ErrRetriesExhausted means every bounded attempt failed retryably.
	ErrRetriesExhausted = errors.New("citytransport: retries exhausted")
	// ErrCanceled means the caller's context ended mid-flight. The upload may
	// or may not have been applied server-side; the producer must re-offer and
	// let idempotency dedupe, which is exactly why ACK_DUPLICATE exists.
	ErrCanceled = errors.New("citytransport: canceled")
	// ErrCredential means the token provider could not supply a credential.
	// Rotation-safe by construction: the caller holds its cursor and retries;
	// a credential problem is never a source-identity or epoch event.
	ErrCredential = errors.New("citytransport: credential unavailable")
)

// EncodeUpload serializes an Upload canonically: compact, fixed field order,
// no HTML escaping. Encoding the same Upload twice yields identical bytes, and
// EncodeUpload(DecodeUpload(b)) == b for any b this function produced.
func EncodeUpload(u Upload) ([]byte, error) {
	return canonicalJSON(u)
}

// DecodeUpload parses an Upload against the CLOSED schema: an unknown field is
// an error, not a silently dropped value. This is what keeps a producer from
// round-tripping a field whose policy it does not know.
func DecodeUpload(b []byte) (Upload, error) {
	var u Upload
	if err := strictUnmarshal(b, &u); err != nil {
		return Upload{}, err
	}
	return u, nil
}

// DecodeAck parses a server acknowledgement against the closed schema.
func DecodeAck(b []byte) (Ack, error) {
	var a Ack
	if err := strictUnmarshal(b, &a); err != nil {
		return Ack{}, err
	}
	return a, nil
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline; canonical bytes carry none.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}
	// Reject trailing content so two concatenated documents cannot decode as one.
	if dec.More() {
		return fmt.Errorf("%w: trailing content after JSON document", ErrMalformedResponse)
	}
	return nil
}
