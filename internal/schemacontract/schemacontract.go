// Package schemacontract extracts the wire contract of the built-in CLI JSON
// schemas and gates changes to it against a committed baseline.
package schemacontract

import (
	_ "embed" // EmbeddedBaseline is the committed baseline file.
	"encoding/json"
)

// BaselinePath is the repository-relative path of the committed baseline.
const BaselinePath = "internal/schemacontract/baseline.json"

// baselineFormat is the only baseline layout this package reads and writes.
const baselineFormat = 1

// EmbeddedBaseline holds the committed baseline bytes.
//
//go:embed baseline.json
var EmbeddedBaseline []byte

// Field is the contract record of one field path in a schema.
type Field struct {
	Path     string          `json:"path"`
	Types    []string        `json:"types"`
	Const    json.RawMessage `json:"const,omitempty"`
	Required bool            `json:"required"`
}

// Entry is the contract of one schema at one schema_version.
type Entry struct {
	SchemaVersion string             `json:"schema_version"`
	Note          string             `json:"note,omitempty"`
	Issue         string             `json:"issue,omitempty"`
	PR            string             `json:"pr,omitempty"`
	JSONL         bool               `json:"jsonl"`
	RawJSON       bool               `json:"raw_json"`
	Variants      map[string][]Field `json:"variants"`
}

// Baseline is the committed record of every schema's contract history.
type Baseline struct {
	Format  int                `json:"format"`
	Schemas map[string][]Entry `json:"schemas"`
}

// MergeSummary counts what Merge changed.
type MergeSummary struct {
	Seeded          int
	EntriesAdded    int
	RecordsAppended int
}
