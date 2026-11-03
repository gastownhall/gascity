package bdflags

import "testing"

// TestTopLevelVerbInventoryIsWellFormed keeps the inventory a set: no verb
// twice, no alias spelled as a verb, and every alias naming a real verb.
func TestTopLevelVerbInventoryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range TopLevelVerbs() {
		if seen[v] {
			t.Errorf("verb %q is listed twice", v)
		}
		seen[v] = true
	}
	for alias, verb := range TopLevelAliases() {
		if seen[alias] {
			t.Errorf("alias %q is also listed as a verb", alias)
		}
		if !seen[verb] {
			t.Errorf("alias %q names %q, which is not in the inventory", alias, verb)
		}
	}
}
