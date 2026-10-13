package legacyop

import "sync"

// Inflight is the API's per-process registry of requests being admitted or
// executed, keyed by op id. An entry is removed only by its own request
// (Release by pointer identity, from Finish or the handler's panic recovery):
// never by age or by size, so the set is bounded by concurrent requests and
// no replay can see "open, not in flight" while the executor still runs.
type Inflight struct {
	mu sync.Mutex
	m  map[string]*Entry
}

// NewInflight returns an empty registry.
func NewInflight() *Inflight { return &Inflight{m: map[string]*Entry{}} }

// Entry is one request's hold on an op id. Digest is the request digest it
// holds the id for; the answer is set once the request has one (Accept).
type Entry struct {
	Digest string

	accepted bool
	replay   any
}

// Accepted reports whether the holder had answered when this entry was read.
func (e *Entry) Accepted() bool { return e.accepted }

// Replay is the holder's answer, as of when this entry was read.
func (e *Entry) Replay() any { return e.replay }

// Reserve holds id for a request with digest. It returns the new entry and
// true, or, when id is already held, a snapshot of the holder's entry and
// false. The snapshot is a copy: it cannot release or accept the holder's
// entry.
func (f *Inflight) Reserve(id, digest string) (*Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*Entry{}
	}
	if held, ok := f.m[id]; ok {
		snapshot := *held
		return &snapshot, false
	}
	e := &Entry{Digest: digest}
	f.m[id] = e
	return e, true
}

// Accept records the holder's answer on its entry, so a same-key request
// replays it. It does nothing unless e still holds id.
func (f *Inflight) Accept(id string, e *Entry, replay any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[id] == e {
		e.accepted = true
		e.replay = replay
	}
}

// Release removes e's hold on id. It does nothing unless e (by pointer
// identity) still holds id, so a request never releases another's hold.
func (f *Inflight) Release(id string, e *Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[id] == e {
		delete(f.m, id)
	}
}

// Len is the number of held ids.
func (f *Inflight) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}
