package main

import "testing"

// TestBdByIDSubcommandResolvesCompletedAliases is the unit-level half of the
// gc-0nf9kp round-4 fix. bdByIDSubcommand used to compose its two-token
// compound key from the raw argv token before normalizing it through
// bdSubcommandAliases, so "protomolecule pour" (bd's own alias of "mol pour")
// never matched bdflags' "mol pour" key and fell through unresolved — the
// same fate as the single-token aliases done/close and view/show never having
// been in the table at all. This pins every completed alias resolving to
// exactly the same (sub, resolved) pair as its canonical spelling, for both
// the single-token and compound-verb paths.
func TestBdByIDSubcommandResolvesCompletedAliases(t *testing.T) {
	cases := []struct {
		name          string
		aliasArgs     []string
		canonicalArgs []string
	}{
		{"new_vs_create", []string{"new", "-t", "task"}, []string{"create", "-t", "task"}},
		{"done_vs_close", []string{"done", "repo-1"}, []string{"close", "repo-1"}},
		{"view_vs_show", []string{"view", "repo-1"}, []string{"show", "repo-1"}},
		{"protomolecule_pour_vs_mol_pour", []string{"protomolecule", "pour", "proto-1", "--assignee", "phantom-owner"}, []string{"mol", "pour", "proto-1", "--assignee", "phantom-owner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aliasSub, aliasRest, aliasResolved := bdByIDSubcommand(tc.aliasArgs)
			canonSub, canonRest, canonResolved := bdByIDSubcommand(tc.canonicalArgs)

			if !aliasResolved {
				t.Fatalf("bdByIDSubcommand(%v) did not resolve; alias table or compound-key normalization regressed", tc.aliasArgs)
			}
			if aliasResolved != canonResolved {
				t.Fatalf("resolved mismatch: alias=%v canonical=%v", aliasResolved, canonResolved)
			}
			if aliasSub != canonSub {
				t.Errorf("sub = %q, want %q (the alias's canonical form)", aliasSub, canonSub)
			}
			if len(aliasRest) != len(canonRest) {
				t.Errorf("rest = %v, want same length as canonical %v", aliasRest, canonRest)
			}
		})
	}
}
