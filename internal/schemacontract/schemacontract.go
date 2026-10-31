// Package schemacontract extracts the wire contract of the built-in CLI JSON
// schemas and gates changes to it against a committed baseline.
package schemacontract

import (
	"encoding/json"
	"errors"
	"io/fs"
)

// BaselinePath is the repository-relative path of the committed baseline.
const BaselinePath = "internal/schemacontract/baseline.json"

// EmbeddedBaseline holds the committed baseline bytes.
var EmbeddedBaseline []byte

var errNotImplemented = errors.New("schemacontract: not implemented")

// Field is the contract record of one field path in a schema.
type Field struct {
	Path     string          `json:"path"`
	Types    []string        `json:"types"`
	Const    json.RawMessage `json:"const,omitempty"`
	Required bool            `json:"required"`
}

// Entry is the contract of one schema at one schema_version.
type Entry struct {
	SchemaVersion string
	Note          string
	Issue         string
	PR            string
	JSONL         bool
	RawJSON       bool
	Variants      map[string][]Field
}

// Baseline is the committed record of every schema's contract history.
type Baseline struct {
	Format  int
	Schemas map[string][]Entry
}

// MergeSummary counts what Merge changed.
type MergeSummary struct {
	Seeded          int
	EntriesAdded    int
	RecordsAppended int
}

// Extract derives the contract entry of one schema document.
func Extract(_ []byte) (Entry, error) { return Entry{}, errNotImplemented }

// ExtractAll extracts every *.schema.json under fsys, keyed by relative path.
func ExtractAll(_ fs.FS) (map[string]Entry, error) { return nil, errNotImplemented }

// Check compares the live schemas against the baseline and returns one
// message per violation.
func Check(_ Baseline, _ map[string]Entry) []string { return nil }

// ParseBaseline strictly decodes baseline bytes.
func ParseBaseline(_ []byte) (Baseline, error) { return Baseline{}, errNotImplemented }

// Marshal renders a baseline as deterministic bytes.
func Marshal(_ Baseline) ([]byte, error) { return nil, errNotImplemented }

// Merge appends what the live schemas add to the baseline without rewriting
// or deleting anything.
func Merge(_ Baseline, _ map[string]Entry) (Baseline, MergeSummary, error) {
	return Baseline{}, MergeSummary{}, errNotImplemented
}
