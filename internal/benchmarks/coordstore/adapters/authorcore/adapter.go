// Package authorcore provides the round-2 author-store-core proof-of-concept
// adapter. It is intentionally small: a zero-fork, in-process hot store with
// two-tier coordination-store semantics.
package authorcore

import (
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/memstore"
)

// New returns a new author-store-core proof-of-concept adapter.
func New() *memstore.Adapter {
	return memstore.New("ac", nil)
}
