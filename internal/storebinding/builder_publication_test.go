package storebinding

import (
	"context"
	"errors"
	"testing"
)

func planPublicationAuthority(t *testing.T, candidate UnpublishedStoreSet) StoreSetPublicationAuthority {
	t.Helper()
	authority, err := newStoreSetPublicationAuthority(
		"attempt-1",
		candidate.generation,
		canonicalDigest([]byte("active-manifest")),
		candidate.descriptors,
		true,
	)
	if err != nil {
		t.Fatalf("newStoreSetPublicationAuthority: %v", err)
	}
	return authority
}

func planCandidate(t *testing.T) UnpublishedStoreSet {
	t.Helper()
	plan := planMixedPlan(t)
	fixtures := planBuildFixtures(t, plan)
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}
	candidate, err := builder.Build(context.Background(), fixtures.inputs(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return candidate
}

func TestUnpublishedStoreSetPublishesOnlyUnderDurableActiveAuthority(t *testing.T) {
	candidate := planCandidate(t)

	if _, err := candidate.Publish(StoreSetPublicationAuthority{}); !errors.Is(err, ErrInvalidPublicationAuthority) {
		t.Fatalf("Publish with a zero authority = %v, want %v", err, ErrInvalidPublicationAuthority)
	}
	if _, err := (UnpublishedStoreSet{}).Publish(planPublicationAuthority(t, candidate)); !errors.Is(err, ErrUnpublishedStoreSetInvalid) {
		t.Fatalf("Publish of an unbuilt candidate = %v, want %v", err, ErrUnpublishedStoreSetInvalid)
	}

	valid := planPublicationAuthority(t, candidate)
	if _, err := candidate.Publish(valid); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	wrongGeneration := valid
	wrongGeneration.generation = valid.generation + 1
	if _, err := candidate.Publish(wrongGeneration); !errors.Is(err, ErrInvalidPublicationAuthority) {
		t.Fatalf("Publish under another generation = %v, want %v", err, ErrInvalidPublicationAuthority)
	}

	foreignDescriptors := valid
	foreignDescriptors.descriptors = cloneBindingIdentities(valid.descriptors)
	for binding := range foreignDescriptors.descriptors {
		foreignDescriptors.descriptors[binding] = BindingIdentity(canonicalDigest([]byte("other")))
		break
	}
	if _, err := candidate.Publish(foreignDescriptors); !errors.Is(err, ErrInvalidPublicationAuthority) {
		t.Fatalf("Publish under a foreign descriptor identity = %v, want %v", err, ErrInvalidPublicationAuthority)
	}

	missingBinding := valid
	missingBinding.descriptors = cloneBindingIdentities(valid.descriptors)
	for binding := range missingBinding.descriptors {
		delete(missingBinding.descriptors, binding)
		break
	}
	if _, err := candidate.Publish(missingBinding); !errors.Is(err, ErrInvalidPublicationAuthority) {
		t.Fatalf("Publish naming fewer bindings than were opened = %v, want %v", err, ErrInvalidPublicationAuthority)
	}
}

func TestPublicationAuthorityMintRequiresCompleteDurableEvidence(t *testing.T) {
	candidate := planCandidate(t)
	digest := canonicalDigest([]byte("active-manifest"))

	cases := []struct {
		name             string
		attempt          AttemptID
		generation       Generation
		manifest         string
		descriptors      map[BindingName]BindingIdentity
		receiptsComplete bool
	}{
		{name: "no attempt", generation: candidate.generation, manifest: digest, descriptors: candidate.descriptors, receiptsComplete: true},
		{name: "generation zero", attempt: "a", manifest: digest, descriptors: candidate.descriptors, receiptsComplete: true},
		{name: "non-canonical manifest digest", attempt: "a", generation: candidate.generation, manifest: "manifest", descriptors: candidate.descriptors, receiptsComplete: true},
		{name: "no active descriptors", attempt: "a", generation: candidate.generation, manifest: digest, receiptsComplete: true},
		{name: "incomplete receipt bitmap", attempt: "a", generation: candidate.generation, manifest: digest, descriptors: candidate.descriptors},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			authority, err := newStoreSetPublicationAuthority(testCase.attempt, testCase.generation, testCase.manifest, testCase.descriptors, testCase.receiptsComplete)
			if err == nil {
				t.Fatal("mint accepted incomplete durable evidence")
			}
			if _, publishErr := candidate.Publish(authority); publishErr == nil {
				t.Fatal("a rejected authority still published the candidate")
			}
		})
	}
}

func TestPublicationAuthorityDetachesItsDescriptorSet(t *testing.T) {
	candidate := planCandidate(t)
	descriptors := cloneBindingIdentities(candidate.descriptors)
	authority, err := newStoreSetPublicationAuthority("attempt-1", candidate.generation, canonicalDigest([]byte("m")), descriptors, true)
	if err != nil {
		t.Fatalf("newStoreSetPublicationAuthority: %v", err)
	}
	for binding := range descriptors {
		descriptors[binding] = BindingIdentity(canonicalDigest([]byte("tampered")))
	}
	if _, err := candidate.Publish(authority); err != nil {
		t.Fatalf("minted authority followed a caller's later mutation: %v", err)
	}
}
