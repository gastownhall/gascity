package exec

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The optional import op (S5 §1.4): a generic exec provider that keeps ids
// takes a whole bead, its id, status and timestamps included, on stdin:
//
//	script capabilities          -> stdout: the optional ops it offers, one per
//	                                line or as a JSON array of strings
//	script import (stdin: bead)  -> stdout: the bead as stored
//
// A script that does not know `capabilities` exits 2 (unknown op) and offers
// nothing. The reverse migration (`gc storage migrate --to-work`) needs import
// to copy a bead back under its own id; without it the reverse refuses the
// provider and names the snapshot as the way back.

// ImportOp is the optional op that creates a bead under its own id.
const ImportOp = "import"

// ErrImportUnsupported is the refusal of a CreateWithForeignID on a script
// whose capabilities do not list ImportOp.
var ErrImportUnsupported = errors.New("the exec provider offers no import op")

var _ beads.ForeignIDCreator = (*Store)(nil)

// importRequest is the bead an import op reads: the create document plus the
// fields a create mints (id, status, timestamps, close reason).
type importRequest struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	Type        string            `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Ephemeral   bool              `json:"ephemeral,omitempty"`
	NoHistory   bool              `json:"no_history,omitempty"`
	DeferUntil  *time.Time        `json:"defer_until,omitempty"`
	CloseReason string            `json:"close_reason,omitempty"`
}

// Capabilities asks the script which optional ops it offers. A script that
// does not know the op offers none.
func (s *Store) Capabilities() ([]string, error) {
	out, err := s.run(nil, "capabilities")
	if err != nil {
		return nil, fmt.Errorf("exec beads capabilities: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var ops []string
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &ops); err != nil {
			return nil, fmt.Errorf("exec beads capabilities: parsing JSON: %w", err)
		}
		return ops, nil
	}
	return strings.Fields(out), nil
}

// ImportSupported reports whether the script lists ImportOp among its
// capabilities. The answer is asked once per store.
func (s *Store) ImportSupported() (bool, error) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importKnown {
		return s.importOK, nil
	}
	ops, err := s.Capabilities()
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op == ImportOp {
			s.importOK = true
		}
	}
	s.importKnown = true
	return s.importOK, nil
}

// CreateWithForeignID creates b under its own id through the import op,
// keeping its status, timestamps and every field the protocol carries. A
// script without the op is refused with ErrImportUnsupported before it is
// asked to write. It satisfies beads.ForeignIDCreator.
func (s *Store) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	if strings.TrimSpace(b.ID) == "" {
		return beads.Bead{}, errors.New("exec beads import: empty id")
	}
	ok, err := s.ImportSupported()
	if err != nil {
		return beads.Bead{}, err
	}
	if !ok {
		return beads.Bead{}, fmt.Errorf("exec beads import %q: %w", b.ID, ErrImportUnsupported)
	}
	if b.Type == "" {
		b.Type = "task"
	}
	data, err := json.Marshal(importRequest{
		ID: b.ID, Title: b.Title, Status: b.Status, Type: b.Type, Priority: b.Priority,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, Assignee: b.Assignee, From: b.From,
		ParentID: b.ParentID, Ref: b.Ref, Description: b.Description, Labels: b.Labels,
		Metadata: b.Metadata, Ephemeral: b.Ephemeral, NoHistory: b.NoHistory,
		DeferUntil: b.DeferUntil, CloseReason: b.CloseReason,
	})
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads import: marshaling: %w", err)
	}
	out, err := s.run(data, ImportOp)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads import: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return s.Get(b.ID)
	}
	return parseBead(out)
}
