package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// keyedLock is a set of refcounted, capacity-1 channel tokens keyed by string —
// the in-process serialization primitive the rig-create admission axes share
// (per-(city, rig name) and per-(city, request_id)). It mirrors
// sourceworkflow.WithLock's in-process tier: a value in a key's channel means
// "free"; taking it acquires, returning it releases. The map entry is deleted
// when the last waiter departs, so idle keys leak no memory (unlike a
// map[string]*sync.Mutex).
//
// It is deliberately the in-process tier ONLY: admission is process-local by
// construction (the live index is process-local, single-replica accepted,
// G13 §12). A concurrent CLI `gc rig add` in another process is out of scope
// and is caught by CreateRig's under-lock duplicate guard.
type keyedLock struct {
	mu    sync.Mutex
	locks map[string]*refToken
}

// refToken is a single refcounted admission token. token has capacity 1: a
// value in the channel means "free"; taking it acquires, returning it releases.
type refToken struct {
	token chan struct{}
	refs  int
}

// newKeyedLock returns an empty keyed-lock set.
func newKeyedLock() *keyedLock {
	return &keyedLock{locks: map[string]*refToken{}}
}

// do runs fn while holding key's token, blocking until it is free or ctx is
// done. It returns ctx.Err() if the wait is canceled, otherwise fn's error.
func (k *keyedLock) do(ctx context.Context, key string, fn func() error) error {
	lk := k.acquire(key)
	defer k.release(key, lk)

	select {
	case <-lk.token:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { lk.token <- struct{}{} }()

	return fn()
}

// acquire returns the shared token for key, creating it (pre-loaded as "free")
// on first use and bumping the refcount.
func (k *keyedLock) acquire(key string) *refToken {
	k.mu.Lock()
	defer k.mu.Unlock()
	lk := k.locks[key]
	if lk == nil {
		lk = &refToken{token: make(chan struct{}, 1)}
		lk.token <- struct{}{}
		k.locks[key] = lk
	}
	lk.refs++
	return lk
}

// release drops one reference and deletes the map entry when the last waiter
// departs, so idle keys hold no memory.
func (k *keyedLock) release(key string, lk *refToken) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cur := k.locks[key]
	if cur == nil || cur != lk {
		return
	}
	if cur.refs > 0 {
		cur.refs--
	}
	if cur.refs == 0 {
		delete(k.locks, key)
	}
}

// rigNameLockSet serializes rig-create admission per (city, rig name) (G13 §7 /
// G16) so the live-index read-modify-write for a single name is a critical
// section. rigRequestIDLockSet serializes it per (city, request_id) so two
// concurrent same-request_id POSTs that take DIFFERENT name locks cannot each
// createIdemRecord and leave two durable records for one (city, request_id) —
// the double-record 500-poison. Admission takes the name lock FIRST, then the
// request_id lock: a fixed global acquisition order, so the two axes never
// deadlock.
var (
	rigNameLockSet      = newKeyedLock()
	rigRequestIDLockSet = newKeyedLock()
)

// withRigNameLock serializes rig-create admission for one (city, rig name).
//
// The lock is held for admission only (validate → index → durable fallback →
// collision → record + entry + cursor). The clone/provision runs outside it;
// the byName live entry — not a held lock — excludes same-name work for the
// provision's lifetime.
//
// An empty rig name is an error, not a bypass: unlike sourceworkflow.WithLock,
// which early-returns fn() unlocked on an empty id, this refuses. Rig name is
// already minLength:"1" on the wire plus the G13 validator, so the refusal is a
// programming-error backstop, not a normal path.
func withRigNameLock(ctx context.Context, cityPath, rigName string, fn func() error) error {
	if strings.TrimSpace(rigName) == "" {
		return errors.New("rig name lock: empty rig name")
	}
	key := filepath.Clean(strings.TrimSpace(cityPath)) + "\x00" + rigName
	return rigNameLockSet.do(ctx, key, fn)
}

// withRigRequestIDLock serializes admission for one (city, request_id). An empty
// request_id needs no serialization — an absent client id mints a unique
// synthetic id per request and reserves no durable record — so it runs fn
// directly. It is taken INSIDE withRigNameLock (name lock first) so the two axes
// keep a single global acquisition order and cannot deadlock.
func withRigRequestIDLock(ctx context.Context, cityPath, requestID string, fn func() error) error {
	if strings.TrimSpace(requestID) == "" {
		return fn()
	}
	key := filepath.Clean(strings.TrimSpace(cityPath)) + "\x00" + requestID
	return rigRequestIDLockSet.do(ctx, key, fn)
}
