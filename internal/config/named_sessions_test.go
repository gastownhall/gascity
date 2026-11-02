package config

import (
	"testing"
)

// bareCandidateCity is the requalification shape of gastownhall/gascity#5957:
// a pre-adopt top-level named session `golden-hon` whose config now holds
// only the bound, rig-scoped entries a `gc rig add --adopt` requalified it
// to.
func bareCandidateCity() *City {
	return &City{
		Workspace: Workspace{Name: "test-city"},
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "ops"},
		},
	}
}

func TestFindNamedSessionBareFallback(t *testing.T) {
	cfg := &City{
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	if ns := FindNamedSession(cfg, "contentbuild/lane.golden-hon"); ns == nil {
		t.Fatal("FindNamedSession(qualified) = nil, want the bound entry")
	}
	if ns := FindNamedSession(cfg, "golden-hon"); ns == nil || ns.QualifiedName() != "contentbuild/lane.golden-hon" {
		t.Fatalf("FindNamedSession(golden-hon) = %v, want the unique bound entry", ns)
	}
	// A dotted identity never takes the bare fallback.
	if ns := FindNamedSession(cfg, "lane.golden-hon"); ns != nil {
		t.Fatalf("FindNamedSession(lane.golden-hon) = %v, want nil: dotted identities resolve exactly", ns)
	}
	// An ambiguous leaf resolves to nothing: the caller must spell the
	// qualified form.
	if ns := FindNamedSession(bareCandidateCity(), "golden-hon"); ns != nil {
		t.Fatalf("FindNamedSession(golden-hon) = %v, want nil across two candidates", ns)
	}
	// An exact top-level entry beats the bare fallback.
	shadow := &City{
		NamedSessions: []NamedSession{
			{Name: "golden-hon"},
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	if ns := FindNamedSession(shadow, "golden-hon"); ns == nil || ns.QualifiedName() != "golden-hon" {
		t.Fatalf("FindNamedSession(golden-hon) = %v, want the top-level entry to win", ns)
	}
}

func TestNamedSessionBareCandidates(t *testing.T) {
	cfg := bareCandidateCity()
	got := NamedSessionBareCandidates(cfg, "golden-hon")
	if len(got) != 2 {
		t.Fatalf("NamedSessionBareCandidates(golden-hon) = %d entries, want both bindings", len(got))
	}
	want := []string{"contentbuild/lane.golden-hon", "contentbuild/ops.golden-hon"}
	for i, ns := range got {
		if ns.QualifiedName() != want[i] {
			t.Fatalf("candidate %d = %q, want %q (config order)", i, ns.QualifiedName(), want[i])
		}
	}
	unique := &City{
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	if got := NamedSessionBareCandidates(unique, "golden-hon"); len(got) != 1 {
		t.Fatalf("NamedSessionBareCandidates(golden-hon) = %d entries, want the one binding", len(got))
	}
	// A dotted identity has no bare candidates, and an entry with no
	// binding contributes none.
	if got := NamedSessionBareCandidates(unique, "lane.golden-hon"); got != nil {
		t.Fatalf("NamedSessionBareCandidates(lane.golden-hon) = %v, want none: dotted identities are not bare", got)
	}
	if got := NamedSessionBareCandidates(unique, "other"); len(got) != 0 {
		t.Fatalf("NamedSessionBareCandidates(other) = %v, want none", got)
	}
	topLevel := &City{NamedSessions: []NamedSession{{Name: "golden-hon"}}}
	if got := NamedSessionBareCandidates(topLevel, "golden-hon"); len(got) != 0 {
		t.Fatalf("NamedSessionBareCandidates(golden-hon) = %v, want none: a bindingless entry is not a candidate", got)
	}
}

func TestNamedSessionBareIdentity(t *testing.T) {
	cfg := &City{
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	if got := NamedSessionBareIdentity(cfg, &cfg.NamedSessions[0]); got != "golden-hon" {
		t.Fatalf("NamedSessionBareIdentity = %q, want golden-hon", got)
	}
	// Ambiguity or a top-level shadow of the leaf erases the bare form.
	for name, tc := range map[string]*City{
		"ambiguous": bareCandidateCity(),
		"shadowed": {
			NamedSessions: []NamedSession{
				{Name: "golden-hon"},
				{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			},
		},
	} {
		if got := NamedSessionBareIdentity(tc, &tc.NamedSessions[len(tc.NamedSessions)-1]); got != "" {
			t.Fatalf("%s: NamedSessionBareIdentity = %q, want empty", name, got)
		}
	}
	// A top-level entry has no bare form, and neither does a bindingless
	// one.
	topLevel := &City{NamedSessions: []NamedSession{{Name: "mayor"}}}
	if got := NamedSessionBareIdentity(topLevel, &topLevel.NamedSessions[0]); got != "" {
		t.Fatalf("NamedSessionBareIdentity(top-level) = %q, want empty", got)
	}
}

// NamedSessionPriorIdentity is the leaf of the entry itself, the bare form a
// row renamed in place by a requalification answers to. Unlike the bare
// identity, a second binding of the same leaf, such as a second rig importing
// the same pack after the rename, does not take it; a top-level entry
// configured under the leaf does.
func TestNamedSessionPriorIdentity(t *testing.T) {
	twoBindings := bareCandidateCity()
	secondRig := &City{
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			{Template: "golden-hon", Dir: "other", BindingName: "lane"},
		},
	}
	shadowed := &City{
		NamedSessions: []NamedSession{
			{Name: "golden-hon"},
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	unique := &City{
		NamedSessions: []NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			{Name: "mayor"},
			{Name: "a.b", Dir: "contentbuild", BindingName: "lane"},
		},
	}
	for _, tc := range []struct {
		name string
		cfg  *City
		ns   *NamedSession
		want string
	}{
		{"unique leaf", unique, &unique.NamedSessions[0], "golden-hon"},
		{"two bindings in one rig", twoBindings, &twoBindings.NamedSessions[0], "golden-hon"},
		{"a second rig binding the leaf", secondRig, &secondRig.NamedSessions[0], "golden-hon"},
		{"shadowed by a top-level entry", shadowed, &shadowed.NamedSessions[1], ""},
		{"top-level entry", unique, &unique.NamedSessions[1], ""},
		{"dotted leaf", unique, &unique.NamedSessions[2], ""},
		{"nil entry", unique, nil, ""},
		{"nil config", nil, &unique.NamedSessions[0], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NamedSessionPriorIdentity(tc.cfg, tc.ns); got != tc.want {
				t.Errorf("NamedSessionPriorIdentity = %q, want %q", got, tc.want)
			}
		})
	}
	// The bare identity, by contrast, needs the leaf to resolve back to the
	// entry alone.
	if got := NamedSessionBareIdentity(secondRig, &secondRig.NamedSessions[0]); got != "" {
		t.Errorf("NamedSessionBareIdentity across two rigs = %q, want empty", got)
	}
	if got := NamedSessionBareIdentity(unique, &unique.NamedSessions[2]); got != "" {
		t.Errorf("NamedSessionBareIdentity of a dotted leaf = %q, want empty", got)
	}
}
