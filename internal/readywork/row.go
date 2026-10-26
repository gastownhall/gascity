package readywork

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ReadyBead is one row of the ready-work answer: the external wire shape
// `gc ready` prints and GET /v0/city/{cityName}/work/ready serves.
//
// It is a DEDICATED type, not beads.Bead, and that is the point: beads.Bead is
// the domain model, its fields change for internal reasons, and marshaling it
// directly would publish every one of those changes as a silent break in a
// contract other programs parse.
//
// # The field names are bd's, not gc's
//
// gc's own JSON surfaces do not agree with each other: `gc bd dep tree --json`
// emits `parent_id`, `depth` and `truncated`, while the HTTP bead schema emits
// `parent` and `issue_type`. `gc ready` is a drop-in for `bd ready --json`, whose
// output the hook work-query path decodes straight into []beads.Bead, so the wire
// tags here are the ones that decode round-trips: `issue_type` for the type and
// `parent` for the parent.
//
// The field SET is the one the HTTP Bead schema publishes, so the surfaces stay
// comparable. `parent` looks absent in a live capture only because it is
// omitempty and most beads have no parent; it is part of the contract.
//
// Field order is the JSON order, and the JSON is a published contract: append,
// never reorder.
type ReadyBead struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Status       string            `json:"status"`
	Type         string            `json:"issue_type"`
	Priority     *int              `json:"priority,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at,omitempty,omitzero"`
	Assignee     string            `json:"assignee,omitempty"`
	From         string            `json:"from,omitempty"`
	ParentID     string            `json:"parent,omitempty"`
	Ref          string            `json:"ref,omitempty"`
	Needs        []string          `json:"needs,omitempty"`
	Description  string            `json:"description,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Dependencies []ReadyBeadDep    `json:"dependencies,omitempty"`
	Ephemeral    bool              `json:"ephemeral,omitempty"`
	NoHistory    bool              `json:"no_history,omitempty"`
	DeferUntil   *time.Time        `json:"defer_until,omitempty"`
	IsBlocked    *bool             `json:"is_blocked,omitempty"`
	// CloseReason follows beads.Bead onto the wire so the field set stays the
	// HTTP Bead shape. Ready rows are open, so it is normally absent.
	CloseReason string `json:"close_reason,omitempty"`
	// BlockedBy carries the row's blocking dependencies with each blocker's
	// STATUS, in bd's `bd ready --json` shape. It is populated only on the
	// --status in_progress arm, which is the crash-recovery read: a resumed
	// holder must be told whether the bead it still owns is currently gated, and
	// `dependencies` cannot say, because it is a list of edges with no blocker
	// status on them.
	//
	// Every other arm leaves it absent, and absent means "not computed", which
	// every consumer already reads as not-blocked (fail-open). The ready arm
	// needs nothing: the store's own Ready already applied readiness, so a row it
	// returned is by construction unblocked.
	BlockedBy []ReadyBeadBlocker `json:"blocked_by,omitempty"`
}

// ReadyBeadDep is the wire shape of one dependency edge, for the same reason
// ReadyBead exists: beads.Dep is a domain type.
type ReadyBeadDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// ReadyBeadBlocker is one blocking dependency, carrying the field the readiness
// decision actually needs: the blocker's STATUS.
//
// The shape is bd's: `bd ready --json` emits
// `blocked_by: [{"id":..., "status":...}]` and the hook's
// isDepBlockedHookCandidate reads exactly `status` off each element. Emitting a
// bare id list (the shape `gc graph --json` uses) would decode without error and
// be silently ignored, which is worse than omitting the field.
type ReadyBeadBlocker struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// toReadyBeads projects domain beads onto the wire shape, attaching each row's
// blocked_by projection when one was computed.
func toReadyBeads(items []beads.Bead, blockers map[string][]ReadyBeadBlocker) []ReadyBead {
	out := make([]ReadyBead, 0, len(items))
	for _, b := range items {
		row := ToReadyBead(b)
		row.BlockedBy = blockers[b.ID]
		out = append(out, row)
	}
	return out
}

// ToReadyBead projects one domain bead onto the wire shape, with no blocked_by.
func ToReadyBead(b beads.Bead) ReadyBead {
	return ReadyBead{
		ID:           b.ID,
		Title:        b.Title,
		Status:       b.Status,
		Type:         b.Type,
		Priority:     b.Priority,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
		Assignee:     b.Assignee,
		From:         b.From,
		ParentID:     b.ParentID,
		Ref:          b.Ref,
		Needs:        b.Needs,
		Description:  b.Description,
		Labels:       b.Labels,
		Metadata:     b.Metadata,
		Dependencies: toReadyBeadDeps(b.Dependencies),
		Ephemeral:    b.Ephemeral,
		NoHistory:    b.NoHistory,
		DeferUntil:   b.DeferUntil,
		IsBlocked:    b.IsBlocked,
		CloseReason:  b.CloseReason,
	}
}

func toReadyBeadDeps(deps []beads.Dep) []ReadyBeadDep {
	if len(deps) == 0 {
		return nil
	}
	out := make([]ReadyBeadDep, 0, len(deps))
	for _, d := range deps {
		out = append(out, ReadyBeadDep{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type})
	}
	return out
}
