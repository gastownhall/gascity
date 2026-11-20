package opjournal

import (
	"strings"
	"time"
)

// Key is an op's position in its conversation's arrival order.
type Key struct {
	// Due is max(NotBefore, CreatedAt): an op deferred into the future sorts
	// by when it becomes due.
	Due       time.Time
	CreatedAt time.Time
	ID        string
}

// OrderKey returns op's order key: (max(not_before, created_at), created_at,
// id). Every reader orders ops by it, so tail and resync agree.
func OrderKey(op Op) Key {
	due := op.CreatedAt
	if op.NotBefore.After(due) {
		due = op.NotBefore
	}
	return Key{Due: due, CreatedAt: op.CreatedAt, ID: op.ID}
}

// CompareOrder orders a before b by OrderKey, for slices.SortFunc.
func CompareOrder(a, b Op) int {
	ka, kb := OrderKey(a), OrderKey(b)
	if c := ka.Due.Compare(kb.Due); c != 0 {
		return c
	}
	if c := ka.CreatedAt.Compare(kb.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(ka.ID, kb.ID)
}
