package exec //nolint:revive // internal package, always imported with alias

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestGenericExecStoreDeclaresItCarriesNoDependencyPayload pins S5 §1.4: the
// exec protocol has no payload field, so a generic exec store is payload-blind,
// and a provider under the bd contract, whose rows bd holds, is not.
func TestGenericExecStoreDeclaresItCarriesNoDependencyPayload(t *testing.T) {
	for _, tc := range []struct {
		script     string
		wantAbsent bool
	}{
		{script: "/opt/providers/gc-beads-file", wantAbsent: true},
		{script: "beads-provider", wantAbsent: true},
		{script: "/city/.gc/scripts/gc-beads-bd.sh", wantAbsent: false},
		{script: "gc-beads-bd", wantAbsent: false},
	} {
		protocol, absent := beads.DepMetadataAbsent(NewStore(tc.script))
		if absent != tc.wantAbsent {
			t.Errorf("%s: absent = %v, want %v", tc.script, absent, tc.wantAbsent)
		}
		if absent && protocol != "exec" {
			t.Errorf("%s: protocol = %q, want exec", tc.script, protocol)
		}
	}
	// A typed handle answers what the store under it declares.
	if _, absent := beads.DepMetadataAbsent(beads.WorkStore{Store: NewStore("beads-provider")}); !absent {
		t.Error("a work handle over a generic exec store hides its declaration")
	}
	if _, absent := beads.DepMetadataAbsent(beads.NewMemStore()); absent {
		t.Error("a store that declares nothing reads as payload-blind")
	}
}
