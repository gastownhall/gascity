package cityinference

import (
	"context"
	"errors"
	"testing"
)

const (
	tenantAlpha = "org_alpha"
	tenantBeta  = "org_beta"
)

func newWorkspaces() map[string]*FakeWorkspace {
	alpha := NewFakeWorkspace()
	alpha.AddChain("run_1", "ses_1", "trc_1")
	alpha.AddChain("run_2", "ses_2", "")
	beta := NewFakeWorkspace()
	beta.AddChain("run_beta", "ses_beta", "trc_beta")
	return map[string]*FakeWorkspace{tenantAlpha: alpha, tenantBeta: beta}
}

func newHarness(t *testing.T) (*FakeAPI, *Producer) {
	t.Helper()
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	p, err := NewProducer(api, testMapper(), NewMemoryStore())
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	return api, p
}

func happyInvocation(t *testing.T) CityInvocation {
	t.Helper()
	return CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1",
		Provider: "anthropic", Model: "m-1", Status: "succeeded",
		Started:            mustTime(t, "2026-07-30T10:00:00Z"),
		RunTeamID:          "run_1",
		SessionTeamID:      "ses_1",
		TranscriptRecordID: "trc_1",
		Usage:              &UsageObservation{InputTokens: ptr(120), OutputTokens: ptr(30), CostMicros: ptr(4500)},
	}
}

// AC1 happy path: transcript-level attribution succeeds with honest coverage,
// linking canonical records the workspace already owns.
func TestPushLinksCanonicalRecords(t *testing.T) {
	api, p := newHarness(t)
	res, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Accepted != 1 || len(res.Uploaded) != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	got, err := api.GetInference(context.Background(), res.Uploaded[0].InferenceTeamID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RunID != "run_1" || got.SessionID != "ses_1" || got.TranscriptRecordID != "trc_1" {
		t.Fatalf("links were not preserved: %+v", got)
	}
	if got.IdentityScope != ScopeInvocation || !got.FoldEligible {
		t.Fatalf("provider-assigned id must be fold-eligible: %+v", got)
	}
}

// AC2 happy path: a proven invocation uploads once. A retry of the same batch
// issues no second write, and a restart from the persisted checkpoint issues
// none either.
func TestProvenInvocationUploadsOnce(t *testing.T) {
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	store := NewMemoryStore()
	p, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	batch := []CityInvocation{happyInvocation(t)}

	if _, err := p.Push(context.Background(), batch); err != nil {
		t.Fatalf("first push: %v", err)
	}
	res, err := p.Push(context.Background(), batch)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Accepted != 0 || res.Skipped != 1 {
		t.Fatalf("retry re-uploaded: %+v", res)
	}

	// Restart: a fresh producer over the same store must not re-upload.
	restarted, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := restarted.Push(context.Background(), batch); err != nil {
		t.Fatalf("restarted push: %v", err)
	}
	if api.Writes() != 1 {
		t.Fatalf("want exactly one write, got %d", api.Writes())
	}
}

// A credential rotation opens no new idempotency namespace. The key preimage
// holds no credential, so a rotation mid-stream still replays.
func TestCredentialRotationDoesNotChangeIdentityOrKey(t *testing.T) {
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	api.Credential = "tok_before"
	store := NewMemoryStore()
	p, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	batch := []CityInvocation{happyInvocation(t)}
	if _, err := p.Push(context.Background(), batch); err != nil {
		t.Fatalf("push: %v", err)
	}
	before := api.Requests[0].IdempotencyKey

	api.Credential = "tok_after"
	rotated, err := NewProducer(api, testMapper(), NewMemoryStore())
	if err != nil {
		t.Fatalf("rotated producer: %v", err)
	}
	if _, err := rotated.Push(context.Background(), batch); err != nil {
		t.Fatalf("post-rotation push: %v", err)
	}
	if api.Requests[1].IdempotencyKey != before {
		t.Fatalf("rotation changed the idempotency key: %s -> %s", before, api.Requests[1].IdempotencyKey)
	}
	if api.Writes() != 2 {
		t.Fatalf("want two attempts, got %d", api.Writes())
	}
	page, err := api.ListInferences(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("rotation admitted a second record: %d", len(page.Items))
	}
}

// A changed payload under an already-acknowledged source identity stops the
// producer. Sending it would ask the server to rewrite accepted input.
func TestChangedReplayStopsTheProducer(t *testing.T) {
	api, p := newHarness(t)
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	changed := happyInvocation(t)
	changed.Model = "m-2"
	_, err := p.Push(context.Background(), []CityInvocation{changed})
	if !errors.Is(err, ErrChangedReplay) {
		t.Fatalf("want ErrChangedReplay, got %v", err)
	}
	if api.Writes() != 1 {
		t.Fatalf("changed replay issued a write: %d", api.Writes())
	}
}

// A fault mid-batch leaves the checkpoint exactly at the last acknowledged
// record. The restart re-offers only what was never acknowledged.
func TestFaultMidBatchResumesWithoutReupload(t *testing.T) {
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	store := NewMemoryStore()
	p, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	second := happyInvocation(t)
	second.UpstreamReqID = "req-2"
	second.TranscriptRecordID = ""
	batch := []CityInvocation{happyInvocation(t), second}

	api.FailNext = errors.New("transport reset")
	if _, err := p.Push(context.Background(), batch); err == nil {
		t.Fatal("want transport failure")
	}
	if api.Writes() != 1 {
		t.Fatalf("want one attempted write before the fault, got %d", api.Writes())
	}

	restarted, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	res, err := restarted.Push(context.Background(), batch)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Accepted != 2 {
		t.Fatalf("resume accepted %d, want 2", res.Accepted)
	}
	page, err := api.ListInferences(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("want 2 records, got %d", len(page.Items))
	}
}

// Two honest observations of the same call — one metered, one estimated — stand
// as two contributions on one inference. Neither is a payload divergence of the
// other, and the folded coverage never rises above the weaker one.
func TestTwoObservationsFoldOntoOneInference(t *testing.T) {
	api, p := newHarness(t)
	metered := happyInvocation(t)
	metered.ObservationID = "metered"
	estimated := happyInvocation(t)
	estimated.ObservationID = "estimated"
	estimated.Usage = &UsageObservation{CostMicros: ptr(4400), SavedCostMicros: ptr(10)}

	res, err := p.Push(context.Background(), []CityInvocation{metered, estimated})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Accepted != 2 {
		t.Fatalf("want 2 accepted, got %d", res.Accepted)
	}
	if res.Uploaded[0].InferenceTeamID != res.Uploaded[1].InferenceTeamID {
		t.Fatal("exact-tuple observations did not fold onto one inference")
	}
	got, err := api.GetInference(context.Background(), res.Uploaded[0].InferenceTeamID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Contributions) != 2 {
		t.Fatalf("want 2 contributions, got %d", len(got.Contributions))
	}
	if Coverage(got.Coverage[FieldGroupTokens]) == CoverageKnown {
		t.Fatal("folded token coverage was raised to known")
	}
}

// An undeclared epoch change is a reset the producer never infers.
func TestEpochDriftStopsTheProducer(t *testing.T) {
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	store := NewMemoryStore()
	p, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	source := testSource()
	source.Epoch = 8
	drifted, err := NewProducer(api, Mapper{Source: source}, store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	if _, err := drifted.Push(context.Background(), []CityInvocation{happyInvocation(t)}); !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("want ErrIdentityDrift, got %v", err)
	}
}
