package beads

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStringMapRejectsNonStringClaimantExclusions(t *testing.T) {
	for _, value := range []string{`["review.author"]`, `{}`, `true`, `42`, `null`} {
		t.Run(value, func(t *testing.T) {
			var md StringMap
			err := json.Unmarshal([]byte(`{"gc.exclude_claimants":`+value+`}`), &md)
			if err == nil || !strings.Contains(err.Error(), "gc.exclude_claimants") {
				t.Fatalf("decode %s: got %v, want exclusion type error", value, err)
			}
			_, err = metadataMapFromNative(json.RawMessage(`{"gc.exclude_claimants":` + value + `}`))
			if err == nil || !strings.Contains(err.Error(), "gc.exclude_claimants") {
				t.Fatalf("native decode %s: got %v, want exclusion type error", value, err)
			}
		})
	}
}

func TestClaimantExclusionsAcceptOnlyStringValues(t *testing.T) {
	for _, value := range []string{`"review.author,review.peer"`, `""`, `"true"`} {
		var md StringMap
		raw := []byte(`{"gc.exclude_claimants":` + value + `,"flag":true}`)
		if err := json.Unmarshal(raw, &md); err != nil {
			t.Fatal(err)
		}
		if md["flag"] != "true" {
			t.Fatal("ordinary metadata coercion changed")
		}
		if _, err := metadataMapFromNative(raw); err != nil {
			t.Fatal(err)
		}
	}
}
