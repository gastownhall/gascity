package beads

import (
	"errors"
	"testing"
)

// AC-7: the disarm guard refuses while any publisher advertises auto or
// require, and refuses an empty heartbeat set unless forced; off heartbeats
// alone allow it.
func TestCheckClaimAuthorityDisarm(t *testing.T) {
	off := ClaimAuthorityHeartbeat{City: "a", Mode: ClaimAuthorityModeOff}
	auto := ClaimAuthorityHeartbeat{City: "b", Mode: ClaimAuthorityModeAuto}
	require := ClaimAuthorityHeartbeat{City: "c", Mode: ClaimAuthorityModeRequire}
	for _, tc := range []struct {
		name       string
		heartbeats []ClaimAuthorityHeartbeat
		force      bool
		refused    bool
	}{
		{"off only", []ClaimAuthorityHeartbeat{off}, false, false},
		{"auto present", []ClaimAuthorityHeartbeat{off, auto}, false, true},
		{"auto present forced", []ClaimAuthorityHeartbeat{auto}, true, true},
		{"require present forced", []ClaimAuthorityHeartbeat{require}, true, true},
		{"none", nil, false, true},
		{"none forced", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckClaimAuthorityDisarm(tc.heartbeats, tc.force)
			if got := errors.Is(err, ErrClaimAuthorityDisarmRefused); got != tc.refused {
				t.Fatalf("CheckClaimAuthorityDisarm = %v, refused %v, want %v", err, got, tc.refused)
			}
		})
	}
}
