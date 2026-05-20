package session

import (
	"reflect"
	"testing"
)

func TestReleaseClaimedIdentityOnCloseState(t *testing.T) {
	cases := map[string]bool{
		"gc_swept":         true,
		"orphaned":         true,
		"stale-session":    true,
		"duplicate":        true,
		"duplicate-repair": true,
		"reconfigured":     true,
		"failed-create":    true,
		"drained":          false,
		"suspended":        false,
		"active":           false,
		"":                 false,
	}
	for state, want := range cases {
		if got := ReleaseClaimedIdentityOnCloseState(state); got != want {
			t.Errorf("ReleaseClaimedIdentityOnCloseState(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestReleaseClaimedIdentityPatch_MovesAliasIntoHistory(t *testing.T) {
	meta := map[string]string{
		"alias":        "zack/gastown.furiosa",
		"agent_name":   "zack/gastown.furiosa",
		"session_name": "gastown__polecat-pe-r7c1",
	}
	got := ReleaseClaimedIdentityPatch(meta)
	want := map[string]string{
		"alias":         "",
		"agent_name":    "",
		"alias_history": "zack/gastown.furiosa",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReleaseClaimedIdentityPatch = %v, want %v", got, want)
	}
}

func TestReleaseClaimedIdentityPatch_PrependsToExistingHistory(t *testing.T) {
	meta := map[string]string{
		"alias":         "rig-2/worker",
		"agent_name":    "rig-2/worker",
		"alias_history": "rig-1/worker,old-alias",
	}
	got := ReleaseClaimedIdentityPatch(meta)
	want := map[string]string{
		"alias":         "",
		"agent_name":    "",
		"alias_history": "rig-2/worker,rig-1/worker,old-alias",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReleaseClaimedIdentityPatch = %v, want %v", got, want)
	}
}

func TestReleaseClaimedIdentityPatch_AliasAlreadyInHistoryIsDeduped(t *testing.T) {
	meta := map[string]string{
		"alias":         "shared",
		"alias_history": "shared,older",
	}
	got := ReleaseClaimedIdentityPatch(meta)
	if got["alias_history"] != "shared,older" {
		t.Fatalf("alias_history = %q, want %q", got["alias_history"], "shared,older")
	}
}

func TestReleaseClaimedIdentityPatch_EmptyAliasOmitsHistoryWrite(t *testing.T) {
	meta := map[string]string{
		"agent_name": "stale",
	}
	got := ReleaseClaimedIdentityPatch(meta)
	if _, ok := got["alias_history"]; ok {
		t.Fatalf("alias_history written when current alias was empty: %v", got)
	}
	if got["alias"] != "" || got["agent_name"] != "" {
		t.Fatalf("identity fields not cleared: %v", got)
	}
}
