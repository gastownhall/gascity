package cityinference

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Source is the enrolled producer identity this adapter uploads under.
type Source struct {
	SourceID string
	// Kind is the source class, e.g. "city". It is in the idempotency preimage
	// so two producers holding the same native IDs cannot collide.
	Kind string
	// Tenant is the enrolled tenant handle in the provider's namespace. It is
	// the first component of the identity triplet, and it is enrollment data:
	// an invocation may not carry a different one.
	Tenant string
	// Epoch is the frozen ingest epoch. It advances only on a declared reset,
	// never on a restart and never on a credential rotation, and the adapter
	// refuses to guess one.
	Epoch uint64
}

// CityInvocation is one City model invocation as this adapter reads it.
//
// There is no step field, and that absence is the design. The frozen contract
// admits no step reference for this domain, so a field here would exist only to
// be dropped silently or fabricated — and the honest expression of a step City
// cannot prove is coverage class unavailable, which [ExpectedCoverage] reports
// unconditionally. There is likewise no prompt, completion or message-text
// field: content lives behind transcript and artifact content authorization.
type CityInvocation struct {
	// SessionNativeID is the provider-namespace session identifier. The empty
	// string is a legal legacy value, not a null, and it is never replaced with
	// a synthesized one.
	SessionNativeID string
	// UpstreamReqID is the provider-assigned request identifier, or a locally
	// generated gen_ value. An err_ value is an attempt row and is refused.
	UpstreamReqID string

	Provider string
	Model    string
	// Status is City-native and is normalized into the closed outcome
	// vocabulary; an unmappable status becomes OutcomeUnknown rather than
	// traveling as itself.
	Status  string
	Started time.Time
	Ended   *time.Time

	// RunTeamID, SessionTeamID and TranscriptRecordID are server-minted Team
	// IDs this City read back from its own neutral upserts. They are links to
	// already-authorized canonical records; this adapter produces none of them.
	RunTeamID          string
	SessionTeamID      string
	TranscriptRecordID string

	// ObservationID names WHICH observation of the invocation this is. Two
	// honest observations of one call (a metered one and an estimated one)
	// carry different values and stand as two contributions on one inference.
	ObservationID string

	Usage *UsageObservation
}

// Mapper turns a City invocation into a neutral request body and refuses any
// mapping that would let a City fact reach a neutral authority field.
type Mapper struct {
	Source Source
}

// inferenceIDDomain is the preimage domain of the derived external inference ID.
const inferenceIDDomain = "gascity/inference-id/manifold.triplet.v1"

// inferenceIDAlphabet is lowercase base32 with no padding, matching the server.
var inferenceIDAlphabet = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// DeriveExternalInferenceID mints the external inference ID from a native
// triplet. It is byte-for-byte the server's derivation, which is the point:
// two independent derivers converge with no coordination, and that is what
// makes an exact-tuple duplicate detectable at all.
//
// Two properties are load-bearing and neither is stylistic. The framing is
// length-prefixed, so the preimage stays injective when a component is empty (a
// legacy row carries session_id = "") or contains a delimiter byte — a naive
// join does not. The digest is one-way, because the provider request ID must
// never become a public identifier and a record identity is by definition
// visible on a read path.
func DeriveExternalInferenceID(id NativeIdentity) (string, error) {
	if id.Kind != NativeIdentityKind {
		return "", fmt.Errorf("%w: unknown native identity kind %q", ErrInvalidInvocation, id.Kind)
	}
	h := sha256.New()
	h.Write([]byte(inferenceIDDomain))
	for _, s := range []string{id.Tenant, id.SessionID, id.UpstreamReqID} {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{0x3a})
		h.Write([]byte(s))
	}
	return "mfi1_" + inferenceIDAlphabet.EncodeToString(h.Sum(nil)[:20]), nil
}

// Prefix classes of an upstream request ID.
const (
	reqIDPrefixError     = "err_"
	reqIDPrefixGenerated = "gen_"
)

// ClassifyReqID decides identity scope from the request-id prefix, and refuses
// the one prefix that is not an inference at all.
//
// err_ rows are attempt rows: a request that never produced a metered
// invocation. Admitting one would let a failed attempt stand as a call in every
// sum built over this domain, so it is a categorical reject rather than a
// zero-usage record.
func ClassifyReqID(reqID string) (IdentityScope, error) {
	switch {
	case reqID == "":
		return "", fmt.Errorf("%w: upstream_req_id is required", ErrInvalidInvocation)
	case strings.HasPrefix(reqID, reqIDPrefixError):
		return "", fmt.Errorf("%w: %s rows never produced a metered invocation", ErrAttemptRow, reqIDPrefixError)
	case strings.HasPrefix(reqID, reqIDPrefixGenerated):
		return ScopeObservation, nil
	}
	return ScopeInvocation, nil
}

// Identity returns the native triplet of one invocation under this mapper's
// enrolled tenant. The tenant comes from enrollment and never from the
// invocation, so a City-side field cannot redirect a record into another
// tenant's identity space.
func (m Mapper) Identity(inv CityInvocation) NativeIdentity {
	return NativeIdentity{
		Kind:          NativeIdentityKind,
		Tenant:        m.Source.Tenant,
		SessionID:     inv.SessionNativeID,
		UpstreamReqID: inv.UpstreamReqID,
	}
}

// normalizeOutcome maps a City-native status onto the closed vocabulary.
func normalizeOutcome(status string) Outcome {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "success", "succeeded", "complete", "completed":
		return OutcomeOK
	case "error", "failed", "failure":
		return OutcomeError
	case "cancelled", "canceled", "aborted": //nolint:misspell // both City spellings are real inputs
		return OutcomeCancelled
	default:
		return OutcomeUnknown
	}
}

// legacy reports the legacy shape: a row with no provider-namespace session.
// Its absent field groups classify legacy_unknown rather than unavailable,
// because collapsing the two would let an unexplained blank pass as a
// structural absence.
func (m Mapper) legacy(inv CityInvocation) bool { return inv.SessionNativeID == "" }

// ExpectedCoverage is the coverage map this adapter's offer supports.
//
// It is NOT authority: coverage is server-derived at admission and immutable
// after. It exists so a fixture can pin what City claims on its own behalf, and
// so [Producer] can refuse an accepted record whose coverage came back stronger
// than what was offered.
//
// Nothing here is a guess. Token counts are metered because the party that did
// the work counted them and we recorded them verbatim — which is not a claim
// that they are correct. Cost is estimated because a price table produced it,
// not a counter. Step attribution is unavailable unconditionally: there is no
// admitted field, so there is nothing to be known about. Transcript attribution
// is known only when a canonical record ID is linked, because that link is
// server-verified.
func (m Mapper) ExpectedCoverage(inv CityInvocation) map[string]Coverage {
	absent := CoverageUnavailable
	if m.legacy(inv) {
		absent = CoverageLegacyUnknown
	}
	cov := map[string]Coverage{
		FieldGroupTokens:     absent,
		FieldGroupCost:       absent,
		FieldGroupSavings:    absent,
		FieldGroupStep:       CoverageUnavailable,
		FieldGroupTranscript: CoverageUnavailable,
		FieldGroupOutcome:    CoverageKnown,
	}
	if u := inv.Usage; u != nil {
		if u.metered() {
			cov[FieldGroupTokens] = CoverageMetered
		}
		if u.CostMicros != nil {
			cov[FieldGroupCost] = CoverageEstimated
		}
		if u.SavedInputTokens != nil || u.SavedCostMicros != nil {
			cov[FieldGroupSavings] = CoverageEstimated
		}
	}
	if inv.TranscriptRecordID != "" {
		cov[FieldGroupTranscript] = CoverageKnown
	}
	return cov
}

// coverageRank orders the classes weakest to strongest. A fold takes the
// WEAKEST class present, never the strongest: an unproven contribution can
// never raise a folded claim.
var coverageRank = map[Coverage]int{
	CoverageLegacyUnknown: 0,
	CoverageUnavailable:   1,
	CoverageEstimated:     2,
	CoveragePartial:       3,
	CoverageMetered:       4,
	CoverageKnown:         5,
}

// MapInvocation turns one City invocation into a neutral create request.
//
// The container Team IDs travel exactly as City read them back. A City-native
// run or session ID placed in one of those fields is not translated here into
// something plausible: it goes as given and resolves to a not-found, which is
// the same answer a foreign record gets.
func (m Mapper) MapInvocation(inv CityInvocation) (CreateInferenceRequest, error) {
	if m.Source.Tenant == "" {
		return CreateInferenceRequest{}, fmt.Errorf("%w: enrolled tenant is required", ErrInvalidInvocation)
	}
	if m.Source.Epoch == 0 {
		return CreateInferenceRequest{}, fmt.Errorf("%w: source epoch is required and is never guessed", ErrInvalidInvocation)
	}
	if _, err := ClassifyReqID(inv.UpstreamReqID); err != nil {
		return CreateInferenceRequest{}, err
	}
	if inv.Provider == "" || inv.Model == "" {
		return CreateInferenceRequest{}, fmt.Errorf("%w: provider and model are required", ErrInvalidInvocation)
	}
	if inv.Started.IsZero() {
		return CreateInferenceRequest{}, fmt.Errorf("%w: started_at is required", ErrInvalidInvocation)
	}
	if inv.Ended != nil && inv.Ended.Before(inv.Started) {
		return CreateInferenceRequest{}, fmt.Errorf("%w: ended_at precedes started_at", ErrInvalidInvocation)
	}

	id := m.Identity(inv)
	external, err := DeriveExternalInferenceID(id)
	if err != nil {
		return CreateInferenceRequest{}, err
	}

	observation := inv.ObservationID
	if observation == "" {
		observation = "primary"
	}

	req := CreateInferenceRequest{
		NativeIdentity:      id,
		ExternalInferenceID: external,
		Provider:            inv.Provider,
		Model:               inv.Model,
		Outcome:             normalizeOutcome(inv.Status),
		StartedAt:           inv.Started.UTC(),
		Epoch:               m.Source.Epoch,
		SessionTeamID:       inv.SessionTeamID,
		RunTeamID:           inv.RunTeamID,
		TranscriptRecordID:  inv.TranscriptRecordID,
		ObservationID:       observation,
		Usage:               inv.Usage,
	}
	if inv.Ended != nil {
		ended := inv.Ended.UTC()
		req.EndedAt = &ended
	}
	if err := Preflight(req); err != nil {
		return CreateInferenceRequest{}, err
	}
	return req, nil
}

// FoldBasis names what a correlation claim rests on.
type FoldBasis string

// The one admissible basis, and the inadmissible ones spelled out so a test can
// enumerate them. They exist as named values precisely so that offering one is
// a refusal with a reason rather than a silent "no match".
const (
	FoldExactTuple  FoldBasis = "exact_tuple"
	FoldTimestamp   FoldBasis = "timestamp_proximity"
	FoldModel       FoldBasis = "model_equality"
	FoldVendor      FoldBasis = "vendor_equality"
	FoldTokenCount  FoldBasis = "token_count_equality"
	FoldCost        FoldBasis = "cost_equality"
	FoldTextual     FoldBasis = "textual_similarity"
	FoldProviderLoc FoldBasis = "provider_locator"
	FoldCityField   FoldBasis = "city_field"
)

// ForbiddenFoldBases returns every basis this adapter refuses, in a stable
// order, so a decision-table test can assert the closed set rather than a
// sample of it.
func ForbiddenFoldBases() []FoldBasis {
	return []FoldBasis{
		FoldTimestamp, FoldModel, FoldVendor, FoldTokenCount,
		FoldCost, FoldTextual, FoldProviderLoc, FoldCityField,
	}
}

// CorrelationClaim is an offered assertion that two invocations are the same
// call.
type CorrelationClaim struct {
	Basis FoldBasis
	A, B  CityInvocation
}

// Fold decides whether two contributions may be folded into one authoritative
// sum.
//
// Only an exact identity tuple folds, and only when the tuple names an
// invocation rather than an observation of one: two locally generated request
// IDs for the same real call never converge, so folding on them would hide a
// double count. Any other basis is refused outright — the refusal is the
// answer, because a producer that gets "false" back from a timestamp comparison
// will try a wider window next.
func (m Mapper) Fold(claim CorrelationClaim) (bool, error) {
	if claim.Basis != FoldExactTuple {
		return false, fmt.Errorf("%w: basis %q is not a comparator in this domain", ErrHeuristicFold, claim.Basis)
	}
	scopeA, err := ClassifyReqID(claim.A.UpstreamReqID)
	if err != nil {
		return false, err
	}
	scopeB, err := ClassifyReqID(claim.B.UpstreamReqID)
	if err != nil {
		return false, err
	}
	if m.Identity(claim.A) != m.Identity(claim.B) {
		return false, nil
	}
	if scopeA != ScopeInvocation || scopeB != ScopeInvocation {
		return false, nil
	}
	return true, nil
}

// The closed admitted key set of the create request, per nesting level. A
// closed allowlist rather than a blocklist of content-shaped names: a blocklist
// has to guess what content will be called next, and this side of the wire is
// where prompt text would first appear if the struct ever drifted.
var admittedKeys = map[string]map[string]bool{
	"": {
		"native_identity": true, "external_inference_id": true, "provider": true,
		"model": true, "outcome": true, "started_at": true, "ended_at": true,
		"epoch": true, "session_id": true, "run_id": true, "transcript_record_id": true,
		"observation_id": true, "usage": true, "ordinal": true, "source_step_ref": true,
	},
	"native_identity": {
		"kind": true, "tenant": true, "session_id": true, "upstream_req_id": true,
	},
	"usage": {
		"input_tokens": true, "output_tokens": true, "cached_input_tokens": true,
		"cost_micros": true, "saved_input_tokens": true, "saved_cost_micros": true,
	},
}

// credentialEvidence matches the shapes a credential takes when it leaks into a
// payload by accident.
var credentialEvidence = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._~+/-]{8,}|sk-[a-z0-9]{8,}|authorization|api[_-]?key|secret|password|private[_-]?key)`)

// ScanForbidden reads outbound request bytes and refuses anything the contract
// does not admit.
//
// It works on bytes rather than on the struct so a test can point it at what a
// transport spy actually recorded. Two classes of refusal: a key outside the
// closed admitted set (which is how model content would arrive), and a value
// carrying credential evidence.
func ScanForbidden(raw []byte) error {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("%w: outbound payload is not a JSON object", ErrContentLeak)
	}
	for _, level := range []string{"", "native_identity", "usage"} {
		obj := body
		if level != "" {
			raw, ok := body[level]
			if !ok {
				continue
			}
			obj = nil
			if err := json.Unmarshal(raw, &obj); err != nil {
				return fmt.Errorf("%w: %s is not a JSON object", ErrContentLeak, level)
			}
		}
		for key := range obj {
			if !admittedKeys[level][key] {
				where := key
				if level != "" {
					where = level + "." + key
				}
				return fmt.Errorf("%w: field %q is not admitted by the inference contract", ErrContentLeak, where)
			}
		}
	}
	if credentialEvidence.Match(raw) {
		return fmt.Errorf("%w: payload matches a credential shape", ErrCredentialLeak)
	}
	return nil
}

// Preflight refuses a request before a byte leaves the process.
//
// It runs on every request [Producer] sends, including one a caller built by
// hand, so the never-synthesize rules are enforced at this adapter's edge and
// not only by a 422 the caller has to correlate back.
func Preflight(req CreateInferenceRequest) error {
	if req.Ordinal != nil {
		return fmt.Errorf("%w: this domain asserts no order, so `ordinal` may not be offered", ErrSyntheticLinkage)
	}
	if req.SourceStepRef != "" {
		return fmt.Errorf("%w: step attribution is unavailable by construction, so `source_step_ref` may not be offered", ErrSyntheticLinkage)
	}
	if req.NativeIdentity.Kind != NativeIdentityKind {
		return fmt.Errorf("%w: unknown native identity kind %q", ErrInvalidInvocation, req.NativeIdentity.Kind)
	}
	if err := boundedNativeID("native_identity.tenant", req.NativeIdentity.Tenant, false); err != nil {
		return err
	}
	// session_id is checked only when non-empty: "" is the legal legacy value,
	// and rejecting it would force a producer to invent one.
	if err := boundedNativeID("native_identity.session_id", req.NativeIdentity.SessionID, true); err != nil {
		return err
	}
	if err := boundedNativeID("native_identity.upstream_req_id", req.NativeIdentity.UpstreamReqID, false); err != nil {
		return err
	}
	if _, err := ClassifyReqID(req.NativeIdentity.UpstreamReqID); err != nil {
		return err
	}
	if req.ExternalInferenceID != "" {
		derived, err := DeriveExternalInferenceID(req.NativeIdentity)
		if err != nil {
			return err
		}
		if derived != req.ExternalInferenceID {
			return fmt.Errorf("%w: offered external_inference_id is not the derivation of the triplet", ErrInvalidInvocation)
		}
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: encode request: %w", ErrInvalidInvocation, err)
	}
	return ScanForbidden(raw)
}

func boundedNativeID(field, value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%w: %s is required", ErrInvalidInvocation, field)
	}
	if len(value) > maxNativeIDLen {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInvocation, field, maxNativeIDLen)
	}
	if strings.ContainsAny(value, "\x00\n\r") {
		return fmt.Errorf("%w: %s contains a control character", ErrInvalidInvocation, field)
	}
	return nil
}
