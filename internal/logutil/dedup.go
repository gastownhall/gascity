// Package logutil provides small logging helpers shared by CLI and supervisor code.
package logutil

import (
	"container/list"
	"sync"
)

// DefaultDedupCapacity is the bounded in-process warning history size.
const DefaultDedupCapacity = 1000

// Dedup tracks recently seen string keys with LRU eviction.
type Dedup struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	seen     map[string]*list.Element
}

// NewDedup returns a bounded deduper. Non-positive capacities use the default.
func NewDedup(capacity int) *Dedup {
	if capacity <= 0 {
		capacity = DefaultDedupCapacity
	}
	return &Dedup{
		capacity: capacity,
		order:    list.New(),
		seen:     make(map[string]*list.Element, capacity),
	}
}

// First reports whether key has not been seen recently.
func (d *Dedup) First(key string) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if elem, ok := d.seen[key]; ok {
		d.order.MoveToFront(elem)
		return false
	}

	elem := d.order.PushFront(key)
	d.seen[key] = elem
	if d.order.Len() > d.capacity {
		last := d.order.Back()
		if last != nil {
			d.order.Remove(last)
			delete(d.seen, last.Value.(string))
		}
	}
	return true
}
