package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/k8s"
	"github.com/gastownhall/gascity/internal/runtime/ssh"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	"github.com/gastownhall/gascity/internal/runtime/t3bridge"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCompareIdentityTable pins v5 O2's table row by row (I11), including
// subprocess's seed window, acp's leftover sidecar, a same-ID empty token,
// an empty row token and an empty row.
//
// Kills: an empty token matching an empty row token (Current), a token match
// that also requires the row's session ID (a legacy-adopted runtime read
// Foreign), Foreign without a non-matching token, a same-ID empty token read
// StaleSelf, the epoch compare flipped or made strict, an unparsed epoch or
// generation read as 0, an unread identity decided from its fields, "no ID,
// no token" read as anything but Ownerless, and a row or runtime field
// compared untrimmed.
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
		{"padded row, the row's token", padded, read("gc-1", "3", "tok-1"), identityCurrent},
		{"padded runtime token", row, read("", "", " tok-1 "), identityCurrent},

		// Another row's ID and not the row's token.
		{"another row's ID, empty token", row, read("gc-2", "1", ""), identityForeign},
		{"another row's ID and token", row, read("gc-2", "1", "tok-2"), identityForeign},
		{"another row's ID and token, empty row token", noToken, read("gc-2", "1", "tok-2"), identityForeign},

		// The row's ID: an empty token never matches; else the epoch decides.
		{"the row's ID, empty token", row, read("gc-1", "3", ""), identityUnknown},
		{"the row's ID, both tokens empty", noToken, read("gc-1", "3", ""), identityUnknown},
		{"the row's ID, whitespace token", row, read("gc-1", "3", "  "), identityUnknown},
		{"older token, older epoch", row, read("gc-1", "2", "tok-0"), identityStaleSelf},
		{"older token, epoch = generation", row, read("gc-1", "3", "tok-0"), identityStaleSelf},
		{"a token, empty row token, epoch = generation", noToken, read("gc-1", "3", "tok-0"), identityStaleSelf},
		{"acp leftover sidecar of an earlier incarnation", row, read("gc-1", "1", "tok-old"), identityStaleSelf},
		{"padded row, another token, epoch = generation", padded, read("gc-1", "3", "tok-0"), identityStaleSelf},
		{"padded runtime ID and epoch", row, read(" gc-1 ", " 3 ", "tok-0"), identityStaleSelf},
		{"newer epoch: census lag", row, read("gc-1", "4", "tok-9"), identityNewerSelf},
		{"padded row, another token, newer epoch", padded, read("gc-1", "4", "tok-0"), identityNewerSelf},
		{"empty epoch", row, read("gc-1", "", "tok-0"), identityUnknown},
		{"garbled epoch", row, read("gc-1", "3x", "tok-0"), identityUnknown},
		{"unparsable row generation", session.Info{ID: "gc-1", InstanceToken: "tok-1"}, read("gc-1", "0", "tok-0"), identityUnknown},

		// No session ID.
		{"no ID, no token", row, read("", "", ""), identityOwnerless},
		{"no ID, no token, empty row token", noToken, read("", "", ""), identityOwnerless},
		{"no ID, another token", row, read("", "3", "tok-2"), identityUnknown},
		{"no ID, a token, empty row token", noToken, read("", "3", "tok-2"), identityUnknown},

		// An empty row matches nothing of its own.
		{"empty row, empty runtime", session.Info{}, read("", "", ""), identityOwnerless},
		{"empty row, any ID", session.Info{}, read("gc-9", "", ""), identityForeign},
		{"empty row, any ID and token", session.Info{}, read("gc-9", "1", "tok-9"), identityForeign},
		{"empty row, a token with no ID", session.Info{}, read("", "", "tok-9"), identityUnknown},

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

// TestCompareIdentityTotal sweeps row ID, row token, generation, session ID,
// token and epoch, padded and not, and checks that exactly one of O2's rows
// (read literally, with no precedence, by o2Rows) holds for each input, and
// that compareIdentity returns that row's verdict.
//
// Kills: any branch reordered so that one row's verdict leaks into another's
// inputs (Foreign checked before the token match, Ownerless on a runtime that
// carries a token), and a field compared or parsed untrimmed.
func TestCompareIdentityTotal(t *testing.T) {
	n := 0
	for _, known := range []bool{true, false} {
		for _, rowID := range []string{"gc-1", " gc-1 ", "gc-1\n"} {
			for _, rowToken := range []string{"tok-1", "", "   ", " tok-1 "} {
				for _, generation := range []string{"3", " 3 ", "", "x", "0"} {
					for _, id := range []string{"gc-1", " gc-1", "gc-2", ""} {
						for _, token := range []string{"tok-1", "tok-1 ", "tok-2", "", "TOK-1"} {
							for _, epoch := range []string{"", "x", "2", "3", " 3", "4", "03", "-1", "+3", "99999999999999999999"} {
								n++
								row := session.Info{ID: rowID, InstanceToken: rowToken, Generation: generation}
								rt := runtimeIdentity{Known: known, SessionID: id, Epoch: epoch, Token: token}
								rows, verdicts := o2Rows(row, rt)
								if len(verdicts) != 1 {
									t.Errorf("O2 is not total and disjoint for %+v, %+v: rows %v", row, rt, rows)
									continue
								}
								if got := compareIdentity(row, rt); got != verdicts[0] {
									t.Errorf("compareIdentity(%+v, %+v) = %s, want %s (%s)", row, rt, got, verdicts[0], rows[0])
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 10000 {
		t.Fatalf("swept %d inputs, want every combination", n)
	}
}

// o2Rows reads CONTRACT v5 O2's table literally: every row as its own
// predicate, with no precedence, so an overlap or a gap shows up as other
// than one row. Fields are trimmed. The table is silent on an unparsable
// generation; it reads as an unreadable epoch (Unknown).
func o2Rows(row session.Info, rt runtimeIdentity) (rows []string, verdicts []identityVerdict) {
	add := func(name string, v identityVerdict) { rows, verdicts = append(rows, name), append(verdicts, v) }
	if !rt.Known {
		add("read error or unsupported", identityUnknown)
		return rows, verdicts
	}
	rowID, rowToken := strings.TrimSpace(row.ID), strings.TrimSpace(row.InstanceToken)
	id, token := strings.TrimSpace(rt.SessionID), strings.TrimSpace(rt.Token)
	tokenMatch := token != "" && rowToken != "" && token == rowToken
	ownID := id != "" && id == rowID
	otherID := id != "" && id != rowID
	if tokenMatch {
		add("token equal to the row's", identityCurrent)
	}
	if otherID && !tokenMatch {
		add("another row's ID", identityForeign)
	}
	if ownID && token == "" {
		add("the row's ID, empty token", identityUnknown)
	}
	if ownID && token != "" && !tokenMatch {
		epoch, err := strconv.Atoi(strings.TrimSpace(rt.Epoch))
		generation, genErr := strconv.Atoi(strings.TrimSpace(row.Generation))
		switch {
		case err != nil || genErr != nil:
			add("the row's ID, epoch unreadable", identityUnknown)
		case epoch <= generation:
			add("the row's ID, epoch <= generation", identityStaleSelf)
		default:
			add("the row's ID, epoch > generation", identityNewerSelf)
		}
	}
	if id == "" && token == "" {
		add("empty, empty", identityOwnerless)
	}
	if id == "" && token != "" && !tokenMatch {
		add("empty ID, not the row's token", identityUnknown)
	}
	return rows, verdicts
}

// TestOwnRuntime pins O2's own-runtime test: Current, or StaleSelf with the
// epoch equal to the generation.
//
// Kills: StaleSelf owned at any epoch (a re-woken incarnation's runtime torn
// down as this one's), NewerSelf or Ownerless owned, and an untrimmed epoch
// or generation compare.
func TestOwnRuntime(t *testing.T) {
	row := session.Info{ID: "gc-1", InstanceToken: "tok-1", Generation: " 3 "}
	read := func(epoch, token string) runtimeIdentity {
		return runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: epoch, Token: token}
	}
	for _, tc := range []struct {
		name string
		rt   runtimeIdentity
		want bool
	}{
		{"current", read("1", "tok-1"), true},
		{"stale-self, epoch = generation", read(" 3", "tok-0"), true},
		{"stale-self, older epoch", read("2", "tok-0"), false},
		{"newer-self", read("4", "tok-0"), false},
		{"unknown: empty token", read("3", ""), false},
		{"ownerless", runtimeIdentity{Known: true}, false},
		{"foreign", runtimeIdentity{Known: true, SessionID: "gc-2", Epoch: "3", Token: "tok-2"}, false},
		{"unread", runtimeIdentity{}, false},
	} {
		if got := ownRuntime(compareIdentity(row, tc.rt), row, tc.rt); got != tc.want {
			t.Errorf("%s: ownRuntime = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestOwnsNameIdentityOnly pins C8.2(a)'s bead-scoped name as legacy's
// predicate (staleAsyncStartRuntimeAttribution): a pool-managed row, its
// stored session name embedding its bead ID, and that stored name asked
// about. compareIdentity never reads the name, so no other consumer of the
// verdict (observeRow, adoption, the classified key) treats such a name as
// own.
//
// Kills: a non-pool row owning its name, the s-<id> fallback name owned, a
// pool row's stored name that does not embed its bead ID owned, a name other
// than the stored one owned, and ownsName folded into
// compareIdentity (a Foreign runtime read Current under a bead-scoped name).
func TestOwnsNameIdentityOnly(t *testing.T) {
	scopedName := PoolSessionName("rig/worker", "gc-7")
	row := session.Info{ID: "gc-7", Template: "rig/worker", PoolManaged: true, SessionNameMetadata: scopedName, InstanceToken: "tok-7", Generation: "2"}
	for name, want := range map[string]bool{
		scopedName:                            true,
		" " + scopedName + " ":                true,
		PoolSessionName("rig/worker", "gc-8"): false,
		"legacy-gc-7":                         false,
		"mayor":                               false,
		"":                                    false,
	} {
		if got := ownsName(row, name); got != want {
			t.Errorf("ownsName(%q) = %v, want %v", name, got, want)
		}
	}

	manual := row
	manual.PoolManaged, manual.SessionOrigin = false, "manual"
	if ownsName(manual, scopedName) {
		t.Error("a non-pool row must not own its name")
	}
	fallback := row
	fallback.SessionNameMetadata, fallback.SessionName = "", "s-gc-7"
	if ownsName(fallback, "s-gc-7") {
		t.Error("the s-<id> fallback name must not count as own")
	}
	aliased := row
	aliased.SessionNameMetadata = "worker-alpha"
	if ownsName(aliased, "worker-alpha") {
		t.Error("a pool row's stored name that does not embed its bead ID must not count as own")
	}

	foreign := runtimeIdentity{Known: true, SessionID: "gc-8", Epoch: "1", Token: "tok-8"}
	if got := compareIdentity(row, foreign); got != identityForeign {
		t.Errorf("compareIdentity on a bead-scoped name = %s, want foreign: the name is L2's input, not the verdict's", got)
	}
}

// TestIdentityReadableLeaves pins the leaves whose identity is read, so
// Ownerless can arise only on a backend that seeds identity before it lists
// the name (v5 X3): tmux, acp and subprocess (LL3). Every other leaf reads
// not Known.
//
// Kills: a known leaf type made identity-readable without revisiting X3.
//
// auto and hybrid are absent: they are never leaves (ResolveBackend and
// inventoryLeaves walk through them), and they forward the batched env read
// to the routed leaf.
func TestIdentityReadableLeaves(t *testing.T) {
	for name, tc := range map[string]struct {
		leaf runtime.Provider
		want bool
	}{
		"tmux":       {(*tmux.Provider)(nil), true},
		"acp":        {(*acp.Provider)(nil), true},
		"subprocess": {(*subprocess.Provider)(nil), true},
		"exec":       {(*exec.Provider)(nil), false},
		"herdr":      {(*herdr.Provider)(nil), false},
		"k8s":        {(*k8s.Provider)(nil), false},
		"ssh":        {(*ssh.Provider)(nil), false},
		"t3bridge":   {(*t3bridge.Provider)(nil), false},
	} {
		if got := identityReadable(tc.leaf); got != tc.want {
			t.Errorf("identityReadable(%s) = %v, want %v", name, got, tc.want)
		}
	}
}
