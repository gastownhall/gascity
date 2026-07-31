package cityinference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// FakeWorkspace is one tenant's canonical records inside a [FakeAPI].
type FakeWorkspace struct {
	Runs     map[string]bool
	Sessions map[string]string // session Team ID -> run Team ID
	Records  map[string]string // transcript record Team ID -> session Team ID
}

// NewFakeWorkspace returns an empty workspace.
func NewFakeWorkspace() *FakeWorkspace {
	return &FakeWorkspace{Runs: map[string]bool{}, Sessions: map[string]string{}, Records: map[string]string{}}
}

// AddChain registers a run, a session under it and an optional transcript
// record under the session, all as server-minted Team IDs.
func (w *FakeWorkspace) AddChain(runID, sessionID, recordID string) {
	w.Runs[runID] = true
	w.Sessions[sessionID] = runID
	if recordID != "" {
		w.Records[recordID] = sessionID
	}
}

// RecordedRequest is one outbound call as a transport spy saw it. Body holds
// the exact bytes, which is what lets a scanner assert on what left rather than
// on what a struct says should have left.
type RecordedRequest struct {
	OperationID    string
	IdempotencyKey string
	Body           []byte
}

// FakeAPI is an in-process stand-in for the public v1 client with two tenants
// and a request spy.
//
// Its refusals are the ones that matter for a producer: a link the caller's
// tenant does not own is a not-found with the same text a never-created link
// gets, so a caller cannot use this API as an existence oracle over the other
// tenant.
type FakeAPI struct {
	mu sync.Mutex

	// Tenant is whose credential the calls arrive on.
	Tenant string
	// Credential is the bearer identity. It is deliberately never read: a
	// rotation must change nothing about identity, idempotency or admission.
	Credential string

	Workspaces map[string]*FakeWorkspace

	// Requests is the spy. Every attempt is recorded, including ones that fail,
	// so a test can assert that a refusal issued no write.
	Requests []RecordedRequest

	// FailNext, when set, fails the next create before any admission and is
	// then cleared. It models a transport fault mid-batch.
	FailNext error

	replay     map[string]Inference
	inferences map[string]*Inference
	seq        int
}

// NewFakeAPI returns a fake bound to one tenant.
func NewFakeAPI(tenant string, workspaces map[string]*FakeWorkspace) *FakeAPI {
	return &FakeAPI{
		Tenant:     tenant,
		Workspaces: workspaces,
		replay:     map[string]Inference{},
		inferences: map[string]*Inference{},
	}
}

// ErrConflict is the fake's payload-divergence refusal: an exact-tuple match
// whose invariant core disagrees with the admitted record.
var ErrConflict = errors.New("cityinference: offered record diverges from the admitted record")

// CreateInference implements [API].
func (f *FakeAPI) CreateInference(_ context.Context, body CreateInferenceRequest, idempotencyKey string) (Inference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, err := json.Marshal(body)
	if err != nil {
		return Inference{}, err
	}
	f.Requests = append(f.Requests, RecordedRequest{
		OperationID: OpCreateInference, IdempotencyKey: idempotencyKey, Body: raw,
	})

	if f.FailNext != nil {
		err := f.FailNext
		f.FailNext = nil
		return Inference{}, err
	}
	if prior, ok := f.replay[idempotencyKey]; ok {
		return prior, nil
	}

	ws := f.Workspaces[f.Tenant]
	if ws == nil {
		return Inference{}, ErrNotFound
	}
	// Foreign, absent, wrong-kind and mismatched links are one answer. The
	// checks are ordered so no branch can leak which of the four it was.
	if body.RunTeamID != "" && !ws.Runs[body.RunTeamID] {
		return Inference{}, ErrNotFound
	}
	if body.SessionTeamID != "" {
		run, ok := ws.Sessions[body.SessionTeamID]
		if !ok || (body.RunTeamID != "" && run != body.RunTeamID) {
			return Inference{}, ErrNotFound
		}
	}
	if body.TranscriptRecordID != "" {
		session, ok := ws.Records[body.TranscriptRecordID]
		if !ok || (body.SessionTeamID != "" && session != body.SessionTeamID) {
			return Inference{}, ErrNotFound
		}
	}

	derived, err := DeriveExternalInferenceID(body.NativeIdentity)
	if err != nil {
		return Inference{}, err
	}
	if body.ExternalInferenceID != "" && body.ExternalInferenceID != derived {
		return Inference{}, fmt.Errorf("cityinference: offered external_inference_id is not the derivation")
	}
	if body.Ordinal != nil || body.SourceStepRef != "" {
		return Inference{}, ErrSyntheticLinkage
	}
	scope, err := ClassifyReqID(body.NativeIdentity.UpstreamReqID)
	if err != nil {
		return Inference{}, err
	}

	mapper := Mapper{Source: Source{Tenant: body.NativeIdentity.Tenant}}
	inv := CityInvocation{
		SessionNativeID:    body.NativeIdentity.SessionID,
		TranscriptRecordID: body.TranscriptRecordID,
		Usage:              body.Usage,
	}
	coverage := map[string]string{}
	for group, class := range mapper.ExpectedCoverage(inv) {
		coverage[group] = string(class)
	}

	observation := body.ObservationID
	if observation == "" {
		observation = "primary"
	}
	contribution := Contribution{
		ID:            derived + ":" + observation,
		ObservationID: observation,
		SourceKind:    "city",
		UploadedBy:    f.Tenant,
		UploaderType:  "workspace_token",
		ObservedAt:    body.StartedAt,
		IngestedAt:    body.StartedAt,
		Coverage:      coverage,
	}
	if body.Usage.metered() {
		contribution.TokenUsage = &TokenUsage{
			InputTokens:       deref(body.Usage.InputTokens),
			OutputTokens:      deref(body.Usage.OutputTokens),
			CachedInputTokens: deref(body.Usage.CachedInputTokens),
		}
	}

	if existing, ok := f.inferences[derived]; ok {
		if existing.Provider != body.Provider || existing.Model != body.Model ||
			existing.Outcome != body.Outcome || existing.RunID != body.RunTeamID ||
			existing.SessionID != body.SessionTeamID || existing.TranscriptRecordID != body.TranscriptRecordID {
			return Inference{}, ErrConflict
		}
		for _, c := range existing.Contributions {
			if c.ObservationID == observation {
				// Same observation of the same call: a duplicate, not an
				// addition. The response is the admitted record unchanged.
				out := *existing
				f.replay[idempotencyKey] = out
				return out, nil
			}
		}
		existing.Contributions = append(existing.Contributions, contribution)
		existing.Coverage = foldCoverage(existing.Contributions)
		existing.TokenUsage = foldTokens(existing.Contributions, existing.FoldEligible)
		out := *existing
		f.replay[idempotencyKey] = out
		return out, nil
	}

	f.seq++
	rec := &Inference{
		ID:                  "inf_" + strconv.Itoa(f.seq),
		ExternalInferenceID: derived,
		SourceID:            "src_" + f.Tenant,
		SourceKind:          "city",
		SessionID:           body.SessionTeamID,
		RunID:               body.RunTeamID,
		TranscriptRecordID:  body.TranscriptRecordID,
		Provider:            body.Provider,
		Model:               body.Model,
		Outcome:             body.Outcome,
		StartedAt:           body.StartedAt,
		EndedAt:             body.EndedAt,
		IngestedAt:          body.StartedAt,
		Epoch:               body.Epoch,
		IdentityScope:       scope,
		FoldEligible:        scope == ScopeInvocation,
		Contributions:       []Contribution{contribution},
		Completeness:        string(CoverageUnavailable),
		ETag:                "W/\"" + derived + "\"",
	}
	rec.Coverage = foldCoverage(rec.Contributions)
	rec.TokenUsage = foldTokens(rec.Contributions, rec.FoldEligible)
	f.inferences[derived] = rec
	out := *rec
	f.replay[idempotencyKey] = out
	return out, nil
}

// ListInferences implements [API].
func (f *FakeAPI) ListInferences(_ context.Context, filter ListFilter) (Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, RecordedRequest{OperationID: OpListInferences})
	var out []Inference
	for _, rec := range f.inferences {
		if filter.SourceID != "" && rec.SourceID != filter.SourceID {
			continue
		}
		if !filter.StartedAfter.IsZero() && !rec.StartedAt.After(filter.StartedAfter) {
			continue
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return Page{Items: out}, nil
}

// GetInference implements [API].
func (f *FakeAPI) GetInference(_ context.Context, inferenceTeamID string) (Inference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, RecordedRequest{OperationID: OpGetInference})
	for _, rec := range f.inferences {
		if rec.ID == inferenceTeamID {
			return *rec, nil
		}
	}
	return Inference{}, ErrNotFound
}

// Writes counts the create attempts the spy saw.
func (f *FakeAPI) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if r.OperationID == OpCreateInference {
			n++
		}
	}
	return n
}

// foldCoverage takes the WEAKEST class present per field group, never the
// strongest: an unproven contribution can never raise a folded claim.
func foldCoverage(contributions []Contribution) map[string]string {
	out := map[string]string{}
	for _, group := range CoverageFieldGroups() {
		weakest := CoverageKnown
		for _, c := range contributions {
			class := Coverage(c.Coverage[group])
			if coverageRank[class] < coverageRank[weakest] {
				weakest = class
			}
		}
		out[group] = string(weakest)
	}
	return out
}

// foldTokens sums metered counts across fold-eligible contributions only. A
// fold-ineligible record keeps its single observation and is never summed.
func foldTokens(contributions []Contribution, foldEligible bool) *TokenUsage {
	var out *TokenUsage
	for _, c := range contributions {
		if c.TokenUsage == nil {
			continue
		}
		if out == nil {
			out = &TokenUsage{}
		}
		if !foldEligible {
			cp := *c.TokenUsage
			return &cp
		}
		out.InputTokens += c.TokenUsage.InputTokens
		out.OutputTokens += c.TokenUsage.OutputTokens
		out.CachedInputTokens += c.TokenUsage.CachedInputTokens
	}
	return out
}

func deref(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}
