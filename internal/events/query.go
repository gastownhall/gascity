package events

// CountByType returns a map of event type → count for the given events.
func CountByType(evts []Event) map[string]int {
	counts := make(map[string]int, len(evts))
	for _, e := range evts {
		counts[e.Type]++
	}
	return counts
}

// CountByActor returns a map of actor → count for the given events.
func CountByActor(evts []Event) map[string]int {
	counts := make(map[string]int, len(evts))
	for _, e := range evts {
		counts[e.Actor]++
	}
	return counts
}

// CountBySubject returns a map of subject → count for the given events.
// Events with an empty Subject are counted under the empty-string key.
func CountBySubject(evts []Event) map[string]int {
	counts := make(map[string]int, len(evts))
	for _, e := range evts {
		counts[e.Subject]++
	}
	return counts
}
