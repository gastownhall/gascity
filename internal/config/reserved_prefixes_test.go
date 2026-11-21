package config

// The reserved-prefix table itself is tested in internal/beadmeta, where it
// lives. What is left to test here is the join: config's BeadClass* names are
// what every caller passes INTO that table, and they are declared in a different
// file from the table's keys.
//
// A drift there answers "no reserved prefix" for a class that has one, and that
// answer is silent in the worst direction — the id falls through to the work
// ledger, which answers it emptily and confidently rather than erroring.

import "testing"

func TestEveryRelocatedBeadClassNameKeysTheReservedTable(t *testing.T) {
	for _, class := range []string{BeadClassGraph, BeadClassMessaging, BeadClassSessions, BeadClassOrders, BeadClassNudges} {
		if _, ok := ReservedClassPrefix(class); !ok {
			t.Errorf("config names the relocated class %q, but the reserved-prefix table has no key for it — ids it mints would fall through to the work ledger", class)
		}
	}
	if _, ok := ReservedClassPrefix(BeadClassWork); ok {
		t.Errorf("work has a reserved class prefix; work beads stay on bd under the rig/HQ EffectivePrefix")
	}
}

// The validator's message names what it refuses. An operator told "gcnq is
// reserved" and then shown a list without gcnq in it has been told the rule and
// shown a contradiction.
func TestReservedPrefixListTextNamesEveryReservedPrefix(t *testing.T) {
	text := reservedClassPrefixListText()
	for _, p := range AllReservedClassPrefixes() {
		if !containsWord(text, p) {
			t.Errorf("the validator's reserved-prefix list %q omits %q, which it refuses", text, p)
		}
	}
}

func containsWord(text, word string) bool {
	for _, field := range splitCommaSpace(text) {
		if field == word {
			return true
		}
	}
	return false
}

func splitCommaSpace(text string) []string {
	var out []string
	cur := ""
	for _, r := range text {
		if r == ',' || r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
