package beadmeta

import (
	"slices"
	"testing"
)

func TestExcludeClaimantsMetadataKeyPinned(t *testing.T) {
	if ExcludeClaimantsMetadataKey != "gc.exclude_claimants" {
		t.Fatalf("ExcludeClaimantsMetadataKey = %q, want %q", ExcludeClaimantsMetadataKey, "gc.exclude_claimants")
	}
	if !slices.Contains(KnownMetadataKeys, ExcludeClaimantsMetadataKey) {
		t.Fatalf("ExcludeClaimantsMetadataKey is not registered in KnownMetadataKeys")
	}
}

func TestExcludedClaimants(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"empty", "", nil},
		{"only separators and space", " , ,, ", nil},
		{"one", "myrig/review.one", []string{"myrig/review.one"}},
		{"trimmed list", " myrig/review.one , myrig/review.two,", []string{"myrig/review.one", "myrig/review.two"}},
		{"tabs and newlines trimmed", "\tmyrig/review.one\n,myrig/review.two ", []string{"myrig/review.one", "myrig/review.two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExcludedClaimants(tc.value); !slices.Equal(got, tc.want) {
				t.Fatalf("ExcludedClaimants(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

func TestExcludesClaimant(t *testing.T) {
	md := map[string]string{ExcludeClaimantsMetadataKey: "myrig/review.one, myrig/review.two"}
	cases := []struct {
		name       string
		metadata   map[string]string
		identities []string
		want       bool
	}{
		{"nil metadata", nil, []string{"myrig/review.one"}, false},
		{"no key", map[string]string{"gc.routed_to": "myrig/review"}, []string{"myrig/review.one"}, false},
		{"no identities", md, nil, false},
		{"empty identity never matches", map[string]string{ExcludeClaimantsMetadataKey: ","}, []string{"", " "}, false},
		{"listed identity", md, []string{"sess-1", "myrig/review.two"}, true},
		{"identity is trimmed", md, []string{" myrig/review.one "}, true},
		{"unlisted identity", md, []string{"myrig/review.three"}, false},
		{"pool name is not a member", md, []string{"myrig/review"}, false},
		{"case-sensitive like route identities", md, []string{"MyRig/Review.One"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExcludesClaimant(tc.metadata, tc.identities...); got != tc.want {
				t.Fatalf("ExcludesClaimant(%v, %v) = %v, want %v", tc.metadata, tc.identities, got, tc.want)
			}
		})
	}
}
