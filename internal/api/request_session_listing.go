package api

import (
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
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
//
// The same listing answers absence (listedAbsent): when the provider attests
// its listing complete ([runtime.ListingAttestation]), a session the
// successful full listing does not name is not running, so a request that
// observes every configured agent row probes only the listed ones.
type requestSessionListing struct {
	sp       sessionLister
	once     sync.Once
	all      []string
	err      error
	attested bool
	listed   map[string]struct{}
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
	l.list()
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

// list reads the full listing once. Attestation is read before the listing
// and after it, and only both count, as [runtime.ListRunningWithAttestation]
// documents.
func (l *requestSessionListing) list() {
	l.once.Do(func() {
		before := listingAttested(l.sp)
		l.all, l.err = l.sp.ListRunning("")
		l.attested = before && listingAttested(l.sp)
		if l.err == nil && l.attested {
			l.listed = make(map[string]struct{}, len(l.all))
			for _, name := range l.all {
				l.listed[name] = struct{}{}
			}
		}
	})
}

// absent reports whether the listing proves sessionName is not running: the
// full listing succeeded, the provider attests it complete, and it does not
// name sessionName.
func (l *requestSessionListing) absent(sessionName string) bool {
	if !listingAttested(l.sp) {
		// An unattested listing proves nothing, so it is not read for this.
		return false
	}
	l.list()
	if l.listed == nil {
		return false
	}
	_, ok := l.listed[sessionName]
	return !ok
}

// listedAbsent reports whether pools, a request listing from
// newRequestSessionListing, proves sessionName is not running. Any other
// lister proves nothing.
func listedAbsent(pools sessionLister, sessionName string) bool {
	l, ok := pools.(*requestSessionListing)
	return ok && l.absent(sessionName)
}

func listingAttested(sp sessionLister) bool {
	p, ok := sp.(runtime.Provider)
	return ok && runtime.ListRunningAttested(p)
}
