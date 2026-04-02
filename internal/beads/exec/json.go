// Package exec implements [beads.Store] by delegating each operation to
// a user-supplied script via fork/exec. This follows the same pattern as
// the session exec provider: a single script receives the operation name
// as its first argument and communicates via stdin/stdout JSON.
package exec

import (
	"encoding/json"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// createRequest is the JSON wire format sent on stdin for create operations.
// Intentionally separate from [beads.Bead] to own the serialization contract.
type createRequest struct {
	Title       string            `json:"title"`
	Type        string            `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Description string            `json:"description,omitempty"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// updateRequest is the JSON wire format sent on stdin for update operations.
// Null/missing fields are not applied. Labels appends (does not replace).
type updateRequest struct {
	Title        *string           `json:"title,omitempty"`
	Status       *string           `json:"status,omitempty"`
	Priority     *int              `json:"priority,omitempty"`
	Description  *string           `json:"description,omitempty"`
	ParentID     *string           `json:"parent_id,omitempty"`
	Assignee     *string           `json:"assignee,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	RemoveLabels []string          `json:"remove_labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// beadWire is the JSON wire format returned by the script for bead data.
// Matches [beads.Bead] JSON tags — the same shape that bd already produces.
type beadWire struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	Type        string            `json:"type"`
	Priority    *int              `json:"priority,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Assignee    string            `json:"assignee"`
	From        string            `json:"from"`
	ParentID    string            `json:"parent_id"`
	Ref         string            `json:"ref"`
	Needs       []string          `json:"needs"`
	Description string            `json:"description"`
	Labels      []string          `json:"labels"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// marshalCreate converts a Bead to JSON for the exec script's create operation.
func marshalCreate(b beads.Bead) ([]byte, error) {
	r := createRequest{
		Title:       b.Title,
		Type:        b.Type,
		Priority:    b.Priority,
		Labels:      b.Labels,
		ParentID:    b.ParentID,
		Ref:         b.Ref,
		Needs:       b.Needs,
		Description: b.Description,
		Assignee:    b.Assignee,
		From:        b.From,
		Metadata:    b.Metadata,
	}
	return json.Marshal(r)
}

// marshalUpdate converts update options to JSON for the exec script.
func marshalUpdate(opts beads.UpdateOpts) ([]byte, error) {
	r := updateRequest{
		Title:        opts.Title,
		Status:       opts.Status,
		Priority:     opts.Priority,
		Description:  opts.Description,
		ParentID:     opts.ParentID,
		Assignee:     opts.Assignee,
		Labels:       opts.Labels,
		RemoveLabels: opts.RemoveLabels,
		Metadata:     opts.Metadata,
	}
	return json.Marshal(r)
}
