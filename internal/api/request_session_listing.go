package api

import (
	"strings"
	"sync"
)

// requestSessionListing answers the ListRunning(prefix) of every unlimited
// pool one request expands from a single ListRunning("") of the provider,
// filtered by prefix in memory. A provider's ListRunning(prefix) is its
// running sessions with that prefix, so one successful full listing answers
// every prefix in the provider's order, as each pool's own listing would at
// that instant. On tmux every listing forks `tmux list-sessions`, so a city
// with hundreds of unlimited pools paid hundreds of forks per request
// (#8453).
//
// A full listing that fails or comes back partial does not answer a prefix:
// a composite's partial error is scoped to the sessions it could not classify
// under the prefix it was asked, so each pool then asks the provider itself,
// as it did before. The full listing is read on the first pool that needs
// one, so a request that expands none lists nothing.
type requestSessionListing struct {
	sp   sessionLister
	once sync.Once
	all  []string
	err  error
}

// newRequestSessionListing returns the listing for one request over sp, or
// nil for a nil sp so expansion keeps its provider-less behavior.
func newRequestSessionListing(sp sessionLister) sessionLister {
	if sp == nil {
		return nil
	}
	return &requestSessionListing{sp: sp}
}

// ListRunning returns the running sessions with prefix.
func (l *requestSessionListing) ListRunning(prefix string) ([]string, error) {
	l.once.Do(func() { l.all, l.err = l.sp.ListRunning("") })
	if l.err != nil {
		return l.sp.ListRunning(prefix)
	}
	var matched []string
	for _, name := range l.all {
		if strings.HasPrefix(name, prefix) {
			matched = append(matched, name)
		}
	}
	return matched, nil
}
