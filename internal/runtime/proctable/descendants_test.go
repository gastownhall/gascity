package proctable

import "testing"

func TestDescendantAliveMatchesDeepDescendant(t *testing.T) {
	// caffeinate(100) -> sh(101) -> sleep(102); pane foreground is caffeinate,
	// so ProcessAlive-style callers only know root pid 100, but the wanted
	// process ("sleep") lives two hops down.
	records := []ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "sh"},
		{PID: 102, PPID: 101, Name: "sleep"},
	}
	if !DescendantAlive(records, []int{100}, []string{"sleep"}) {
		t.Error("DescendantAlive = false; want true for a matching deep descendant")
	}
}

func TestDescendantAliveRootItselfMatches(t *testing.T) {
	records := []ProcessRecord{{PID: 100, PPID: 1, Name: "sleep"}}
	if !DescendantAlive(records, []int{100}, []string{"sleep"}) {
		t.Error("DescendantAlive = false; want true when the root pid itself matches")
	}
}

func TestDescendantAliveNoMatch(t *testing.T) {
	records := []ProcessRecord{
		{PID: 100, PPID: 1, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "sh"},
	}
	if DescendantAlive(records, []int{100}, []string{"definitely-not-a-real-process"}) {
		t.Error("DescendantAlive = true; want false when no descendant matches")
	}
}

func TestDescendantAliveEmptyInputs(t *testing.T) {
	records := []ProcessRecord{{PID: 100, PPID: 1, Name: "sleep"}}
	if DescendantAlive(records, []int{100}, nil) {
		t.Error("DescendantAlive = true with no names; want false")
	}
	if DescendantAlive(records, nil, []string{"sleep"}) {
		t.Error("DescendantAlive = true with no roots; want false")
	}
}

func TestDescendantAliveIgnoresCycles(t *testing.T) {
	// Malformed/racy snapshot with a self-referential ppid must not hang.
	records := []ProcessRecord{
		{PID: 100, PPID: 100, Name: "caffeinate"},
		{PID: 101, PPID: 100, Name: "sh"},
	}
	if DescendantAlive(records, []int{100}, []string{"definitely-not-a-real-process"}) {
		t.Error("DescendantAlive = true; want false")
	}
}
