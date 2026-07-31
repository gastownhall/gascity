package cityinference

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// AC3 core: a foreign link and an absent link are the same answer. If they were
// not, this adapter would be an existence oracle over the other tenant's
// records, and the two-tenant spy is how that is proven rather than asserted.
func TestForeignAndAbsentLinksAreIndistinguishable(t *testing.T) {
	foreign := happyInvocation(t)
	foreign.TranscriptRecordID = "trc_beta" // real, but owned by the other tenant
	foreign.UpstreamReqID = "req-foreign"

	absent := happyInvocation(t)
	absent.TranscriptRecordID = "trc_never_created"
	absent.UpstreamReqID = "req-absent"

	mismatched := happyInvocation(t)
	mismatched.SessionTeamID = "ses_2" // same tenant, wrong container for trc_1
	mismatched.RunTeamID = "run_2"
	mismatched.UpstreamReqID = "req-mismatch"

	incompatible := happyInvocation(t)
	incompatible.TranscriptRecordID = "run_1" // a real ID of the wrong kind
	incompatible.UpstreamReqID = "req-wrongkind"

	var messages []string
	for _, inv := range []CityInvocation{foreign, absent, mismatched, incompatible} {
		api, p := newHarness(t)
		_, err := p.Push(context.Background(), []CityInvocation{inv})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: want ErrNotFound, got %v", inv.UpstreamReqID, err)
		}
		messages = append(messages, err.Error())
		page, listErr := api.ListInferences(context.Background(), ListFilter{})
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(page.Items) != 0 {
			t.Fatalf("%s: an invalid link still wrote a record", inv.UpstreamReqID)
		}
	}
	for _, msg := range messages[1:] {
		if msg != messages[0] {
			t.Fatalf("refusals are distinguishable: %q vs %q", messages[0], msg)
		}
	}
}

// The valid same-workspace link is the control: the identical shape succeeds
// when the records really are this tenant's.
func TestSameWorkspaceLinkSucceeds(t *testing.T) {
	_, p := newHarness(t)
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("valid link refused: %v", err)
	}
}

// Content and step linkage cannot enter through the City side at all: there is
// no field on the invocation that could carry either. This is the structural
// half of "forbidden content is never sent" — the scanner is the other half,
// and a drift guard here fails the day someone adds the field.
func TestCityInvocationCannotCarryContentOrStep(t *testing.T) {
	forbidden := map[string]bool{
		"Prompt": true, "Completion": true, "Text": true, "Content": true,
		"Messages": true, "Transcript": true, "RawResponse": true, "Payload": true,
		"Step": true, "StepID": true, "SourceStepRef": true, "Ordinal": true,
	}
	typ := reflect.TypeOf(CityInvocation{})
	for i := 0; i < typ.NumField(); i++ {
		if forbidden[typ.Field(i).Name] {
			t.Fatalf("CityInvocation grew a forbidden field %q", typ.Field(i).Name)
		}
	}
}

// A refused invocation performs no inference write at all: the refusal happens
// at this adapter's edge, so the spy sees zero calls rather than a 4xx. What
// this adapter does send is then scanned byte-for-byte.
func TestRefusedInvocationIssuesNoWriteAndSentBytesAreClean(t *testing.T) {
	api, p := newHarness(t)
	bad := happyInvocation(t)
	bad.UpstreamReqID = "err_upstream_timeout" // an attempt row, refused before send
	if _, err := p.Push(context.Background(), []CityInvocation{bad}); !errors.Is(err, ErrAttemptRow) {
		t.Fatalf("want ErrAttemptRow, got %v", err)
	}
	if api.Writes() != 0 {
		t.Fatalf("refused invocation issued %d writes", api.Writes())
	}

	// Every byte this adapter did send is scanner-clean: no content, no
	// credential, no unadmitted field.
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if api.Writes() == 0 {
		t.Fatal("nothing was recorded to scan")
	}
	for i, req := range api.Requests {
		if req.OperationID != OpCreateInference {
			continue
		}
		if err := ScanForbidden(req.Body); err != nil {
			t.Fatalf("request %d carried forbidden bytes: %v", i, err)
		}
	}
}

// Only the three frozen operations appear on the wire.
func TestOnlyFrozenOperationsAreCalled(t *testing.T) {
	api, p := newHarness(t)
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	frozen := map[string]bool{OpCreateInference: true, OpListInferences: true, OpGetInference: true}
	for _, req := range api.Requests {
		if !frozen[req.OperationID] {
			t.Fatalf("unfrozen operation %q", req.OperationID)
		}
	}
}

// Rollback: disabling City inference stops this producer and nothing else.
// Acknowledged records persist, reads keep working, and a custom (non-City)
// inference producer on the same API is unaffected.
func TestDisablingCityInferenceLeavesOtherProducersOperational(t *testing.T) {
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	city, err := NewProducer(api, testMapper(), NewMemoryStore())
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	res, err := city.Push(context.Background(), []CityInvocation{happyInvocation(t)})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	accepted := res.Uploaded[0].InferenceTeamID

	city.Disabled = true
	writesBefore := api.Writes()
	next := happyInvocation(t)
	next.UpstreamReqID = "req-after-rollback"
	if _, err := city.Push(context.Background(), []CityInvocation{next}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
	if api.Writes() != writesBefore {
		t.Fatal("a disabled producer issued a write")
	}
	if _, err := api.GetInference(context.Background(), accepted); err != nil {
		t.Fatalf("acknowledged record did not persist through rollback: %v", err)
	}

	// A custom orchestrator with no City in the picture uploads the same shape
	// on its own source identity and is untouched by City's rollback.
	custom := Mapper{Source: Source{SourceID: "src_custom", Kind: "custom_orchestrator", Tenant: tenantAlpha, Epoch: 3}}
	other, err := NewProducer(api, custom, NewMemoryStore())
	if err != nil {
		t.Fatalf("new custom producer: %v", err)
	}
	inv := happyInvocation(t)
	inv.UpstreamReqID = "req-custom-1"
	if _, err := other.Push(context.Background(), []CityInvocation{inv}); err != nil {
		t.Fatalf("custom producer refused after City rollback: %v", err)
	}
}

// A second City source for the same deployment is a distinct source identity,
// so its idempotency keys never collide with the first one's. Silent key reuse
// across sources is how a structural double count becomes invisible.
func TestIdempotencyKeyIsScopedToTheSource(t *testing.T) {
	a := testSource()
	b := testSource()
	b.SourceID = "src_city_2"
	if IdempotencyKey(a, "mfi1_x/primary") == IdempotencyKey(b, "mfi1_x/primary") {
		t.Fatal("two sources share an idempotency key")
	}
	c := testSource()
	c.Epoch = 8
	if IdempotencyKey(a, "mfi1_x/primary") == IdempotencyKey(c, "mfi1_x/primary") {
		t.Fatal("an epoch reset reused the pre-reset key")
	}
	if IdempotencyKey(a, "mfi1_x/primary") == IdempotencyKey(a, "mfi1_x/secondary") {
		t.Fatal("two observations share an idempotency key")
	}
	// Stability across a fresh Source value: the preimage holds only enrollment
	// data, so a redeploy that rebuilds the struct resumes the same key.
	if IdempotencyKey(testSource(), "mfi1_x/primary") != IdempotencyKey(a, "mfi1_x/primary") {
		t.Fatal("key derivation is not stable across an equivalent source")
	}
}
