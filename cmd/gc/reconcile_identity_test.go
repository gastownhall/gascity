package main

import (
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// TestCompareIdentityTable pins v5 O2's table row by row (I11), including
// subprocess's seed window, acp's leftover sidecar, a same-ID empty token
// and an empty row token.
//
// Kills: an empty token matching an empty row token (Current), a token match
// that also requires the row's session ID (a legacy-adopted runtime read
// Foreign), Foreign without a non-matching token, a same-ID empty token read
// StaleSelf, the epoch compare flipped or made strict, an unparsed epoch or
// generation read as 0, an unread identity decided from its fields, and
// "no ID, no token" read as anything but Ownerless.
func TestCompareIdentityTable(t *testing.T) {
	row := session.Info{ID: "gc-1", InstanceToken: "tok-1", Generation: "3"}
	noToken := session.Info{ID: "gc-1", Generation: "3"}
	padded := session.Info{ID: " gc-1 ", InstanceToken: " tok-1 ", Generation: " 3 "}
	read := func(id, epoch, token string) runtimeIdentity {
		return runtimeIdentity{Known: true, SessionID: id, Epoch: epoch, Token: token}
	}
	cases := []struct {
		name string
		row  session.Info
		rt   runtimeIdentity
		want identityVerdict
	}{
		// Read error, unsupported leaf or never read.
		{"unread", row, runtimeIdentity{}, identityUnknown},
		{"read error keeps no verdict even with the row's token", row, runtimeIdentity{SessionID: "gc-1", Epoch: "3", Token: "tok-1"}, identityUnknown},

		// A non-empty token equal to the row's decides, whatever the ID.
		{"the row's ID and token", row, read("gc-1", "3", "tok-1"), identityCurrent},
		{"the row's token under another row's ID", row, read("gc-2", "9", "tok-1"), identityCurrent},
		{"legacy-adopted: the row's token, no session ID", row, read("", "", "tok-1"), identityCurrent},
		{"the row's token, unreadable epoch", row, read("gc-1", "x", "tok-1"), identityCurrent},
		{"whitespace on the row is not a mismatch", padded, read("gc-1", "3", "tok-1"), identityCurrent},

		// Another row's ID and not the row's token.
		{"another row's ID, empty token", row, read("gc-2", "1", ""), identityForeign},
		{"another row's ID and token", row, read("gc-2", "1", "tok-2"), identityForeign},
		{"another row's ID and token, empty row token", noToken, read("gc-2", "1", "tok-2"), identityForeign},

		// The row's ID: an empty token never matches; else the epoch decides.
		{"the row's ID, empty token", row, read("gc-1", "3", ""), identityUnknown},
		{"the row's ID, both tokens empty", noToken, read("gc-1", "3", ""), identityUnknown},
		{"older token, older epoch", row, read("gc-1", "2", "tok-0"), identityStaleSelf},
		{"older token, epoch = generation", row, read("gc-1", "3", "tok-0"), identityStaleSelf},
		{"a token, empty row token, epoch = generation", noToken, read("gc-1", "3", "tok-0"), identityStaleSelf},
		{"acp leftover sidecar of an earlier incarnation", row, read("gc-1", "1", "tok-old"), identityStaleSelf},
		{"newer epoch: census lag", row, read("gc-1", "4", "tok-9"), identityNewerSelf},
		{"empty epoch", row, read("gc-1", "", "tok-0"), identityUnknown},
		{"garbled epoch", row, read("gc-1", "3x", "tok-0"), identityUnknown},
		{"unparsable row generation", session.Info{ID: "gc-1", InstanceToken: "tok-1"}, read("gc-1", "0", "tok-0"), identityUnknown},

		// No session ID.
		{"no ID, no token", row, read("", "", ""), identityOwnerless},
		{"no ID, no token, empty row token", noToken, read("", "", ""), identityOwnerless},
		{"no ID, another token", row, read("", "3", "tok-2"), identityUnknown},
		{"no ID, a token, empty row token", noToken, read("", "3", "tok-2"), identityUnknown},

		// A sidecar read straddling a re-seed shows a prefix of the new seed.
		{"seed window: token written, ID not yet", row, read("", "", "tok-1"), identityCurrent},
		{"seed window: ID written, token not yet", row, read("gc-1", "", ""), identityUnknown},
		{"seed window: ID and another token, epoch not yet", row, read("gc-1", "", "tok-2"), identityUnknown},
	}
	for _, tc := range cases {
		if got := compareIdentity(tc.row, tc.rt); got != tc.want {
			t.Errorf("%s: compareIdentity(%+v, %+v) = %s, want %s", tc.name, tc.row, tc.rt, got, tc.want)
		}
	}
}

// TestCompareIdentityTotal sweeps every combination of session ID, token,
// epoch and row token, and checks each verdict against O2's row conditions:
// exactly one verdict per input, and no verdict outside its row.
//
// Kills: any branch reordered so that one row's verdict leaks into another's
// inputs (for example, Foreign checked before the token match, or Ownerless
// on a runtime that carries a token).
func TestCompareIdentityTotal(t *testing.T) {
	for _, known := range []bool{true, false} {
		for _, rowToken := range []string{"tok-1", ""} {
			for _, id := range []string{"gc-1", "gc-2", ""} {
				for _, token := range []string{"tok-1", "tok-2", ""} {
					for _, epoch := range []string{"", "x", "2", "3", "4"} {
						row := session.Info{ID: "gc-1", InstanceToken: rowToken, Generation: "3"}
						rt := runtimeIdentity{Known: known, SessionID: id, Epoch: epoch, Token: token}
						got := compareIdentity(row, rt)
						if want := o2Row(row, rt); got != want {
							t.Errorf("compareIdentity(%+v, %+v) = %s, want %s", row, rt, got, want)
						}
					}
				}
			}
		}
	}
}

// o2Row reads CONTRACT v5 O2's table literally, one row per clause, in the
// table's order of precedence.
func o2Row(row session.Info, rt runtimeIdentity) identityVerdict {
	ownID := rt.SessionID == row.ID
	tokenMatch := rt.Token != "" && row.InstanceToken != "" && rt.Token == row.InstanceToken
	epoch, err := strconv.Atoi(rt.Epoch)
	generation, _ := strconv.Atoi(row.Generation)
	switch {
	case !rt.Known:
		return identityUnknown
	case tokenMatch:
		return identityCurrent
	case rt.SessionID != "" && !ownID:
		return identityForeign
	case ownID && rt.Token == "":
		return identityUnknown
	case ownID && err != nil:
		return identityUnknown
	case ownID && epoch <= generation:
		return identityStaleSelf
	case ownID:
		return identityNewerSelf
	case rt.Token == "":
		return identityOwnerless
	}
	return identityUnknown
}

// TestOwnsNameIdentityOnly pins C8.2(a)'s bead-scoped name as an input to
// leg L2 alone: ownsName answers for row's own bead-scoped names, and
// compareIdentity never reads the name, so no other consumer of the verdict
// (observeRow, adoption, the classified key) treats such a name as own.
//
// Kills: ownsName accepting another row's or a named session's name, and
// ownsName folded into compareIdentity (a Foreign runtime read Current under
// a bead-scoped name).
func TestOwnsNameIdentityOnly(t *testing.T) {
	row := session.Info{ID: "gc-7", Template: "rig/worker", InstanceToken: "tok-7", Generation: "2"}
	for name, want := range map[string]bool{
		PoolSessionName("rig/worker", "gc-7"): true,
		"legacy-gc-7":                         true,
		PoolSessionName("rig/worker", "gc-8"): false,
		"worker-gc-77":                        false,
		"mayor":                               false,
		"":                                    false,
	} {
		if got := ownsName(row, name); got != want {
			t.Errorf("ownsName(%q) = %v, want %v", name, got, want)
		}
	}

	scoped := row
	scoped.SessionNameMetadata = PoolSessionName("rig/worker", "gc-7")
	foreign := runtimeIdentity{Known: true, SessionID: "gc-8", Epoch: "1", Token: "tok-8"}
	if got := compareIdentity(scoped, foreign); got != identityForeign {
		t.Errorf("compareIdentity on a bead-scoped name = %s, want foreign: the name is L2's input, not the verdict's", got)
	}
	if !ownsName(scoped, scoped.SessionNameMetadata) || ownsName(scoped, "mayor") {
		t.Error("ownsName must read the name it is given, not the row's stored session name")
	}
}
