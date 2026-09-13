package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// beadGraphEmittableMemberships is every value collectBeadGraph can return.
// It is the bridge the guard below walks: the Go constants on one side, the
// hand-written enum struct tag and the committed OpenAPI schema on the other.
var beadGraphEmittableMemberships = []beads.Membership{
	beads.MembershipRootIDAndParentClosure,
	beads.MembershipRootIDParentClosureAndConvoy,
}

// TestBeadGraphMembershipEnumMatchesTheVocabulary binds the published enum to
// the constants the server actually emits.
//
// Nothing else does. The enum lives in a struct tag, which is a string
// literal, so renaming a Membership constant's spelling leaves the whole unit
// tier green — the behavioral tests compare the response against the same
// renamed constant, and TestOpenAPISpecInSync compares the spec against the
// unchanged tag — while the server puts a value on the wire that the committed
// schema forbids and the generated clients reject.
func TestBeadGraphMembershipEnumMatchesTheVocabulary(t *testing.T) {
	field, ok := reflect.TypeOf(BeadGraphResponse{}).FieldByName("Membership")
	if !ok {
		t.Fatal("BeadGraphResponse has no Membership field")
	}
	tagEnum := strings.Split(field.Tag.Get("enum"), ",")
	if len(tagEnum) == 1 && tagEnum[0] == "" {
		t.Fatal("BeadGraphResponse.Membership carries no enum tag; the wire value would then be an unconstrained string and a consumer could not assert on it")
	}

	want := make([]string, 0, len(beadGraphEmittableMemberships))
	for _, m := range beadGraphEmittableMemberships {
		want = append(want, string(m))
	}
	assertSameSet(t, "the enum struct tag", tagEnum, want)

	vocabulary := map[string]bool{}
	for _, m := range beads.AllMemberships() {
		vocabulary[string(m)] = true
	}

	// Both tracked JSON copies of the spec. TestOpenAPISpecInSync already ties
	// them to the served spec; this ties the served spec to the Go constants.
	for _, specPath := range []string{
		"openapi.json",
		filepath.Join("..", "..", "docs", "reference", "schema", "openapi.json"),
	} {
		raw, err := os.ReadFile(specPath)
		if err != nil {
			t.Fatalf("read %s: %v", specPath, err)
		}
		var spec struct {
			Components struct {
				Schemas map[string]struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatalf("parse %s: %v", specPath, err)
		}
		schema, ok := spec.Components.Schemas["BeadGraphResponse"]
		if !ok {
			t.Fatalf("%s has no BeadGraphResponse schema", specPath)
		}
		property, ok := schema.Properties["membership"]
		if !ok {
			t.Fatalf("%s: BeadGraphResponse has no membership property", specPath)
		}
		assertSameSet(t, specPath, property.Enum, want)

		for _, value := range property.Enum {
			if !vocabulary[value] {
				t.Errorf("%s: membership enum names %q, which beads.AllMemberships does not define — a generated client would advertise a rule that exists nowhere in the code", specPath, value)
			}
		}
	}
}

// assertSameSet reports every value present on one side and missing from the
// other, so a rename shows up as a matched pair rather than one bare count.
func assertSameSet(t *testing.T, where string, got, want []string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, v := range got {
		gotSet[v] = true
	}
	wantSet := map[string]bool{}
	for _, v := range want {
		wantSet[v] = true
	}
	var missing, extra []string
	for v := range wantSet {
		if !gotSet[v] {
			missing = append(missing, v)
		}
	}
	for v := range gotSet {
		if !wantSet[v] {
			extra = append(extra, v)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, v := range missing {
		t.Errorf("%s omits %q, which the graph endpoint can emit; a consumer validating against the published enum would reject a valid response", where, v)
	}
	for _, v := range extra {
		t.Errorf("%s declares %q, which no membership constant the graph endpoint emits spells; regenerate the spec after renaming a beads.Membership value", where, v)
	}
}
