package eventfact

import (
	"encoding/json"
	"fmt"

	"github.com/steveyegge/beads/journalops"

	"github.com/gastownhall/gascity/internal/beads"
)

// journalOpCreate is the journal's op for a row's create (bd's
// issueops.EventCreate). A fact row is written once, so its create carries
// all of it.
const journalOpCreate = "create"

// journalImage is the part of a journal row's post-image (bd's issue, as
// json.Marshal writes it into IssueJSON) that a fact row is read from. It is
// gc's own struct, not bd's type: bd's is internal to the beads module.
// Metadata stays raw until the row is classified: bd accepts any JSON as
// metadata, so a row that is not a fact may carry an array or a string there.
type journalImage struct {
	ID        string          `json:"id"`
	IssueType string          `json:"issue_type"`
	Status    string          `json:"status"`
	Labels    []string        `json:"labels"`
	Metadata  json.RawMessage `json:"metadata"`
	NoHistory bool            `json:"no_history"`
}

// DecodeJournalRow reads the event fact a journal row created, from the row's
// post-image alone, with no store read. ok reports whether the row is the
// create of an event-fact row; the projector skips every other row (other ops,
// other row types), whatever its metadata holds. err is non-nil when the row
// cannot be read: an image that is not an issue's JSON (ok false, since what
// it was cannot be told), or an event-fact image that does not decode (ok
// true; ErrCorrupt or ErrUnsupportedSchema).
func DecodeJournalRow(row journalops.Row) (f Fact, ok bool, err error) {
	if row.Op != journalOpCreate {
		return Fact{}, false, nil
	}
	var image journalImage
	if err := json.Unmarshal([]byte(row.IssueJSON), &image); err != nil {
		return Fact{}, false, fmt.Errorf("eventfact: journal row %d (%s): post-image is not an issue: %w", row.Seq, row.IssueID, err)
	}
	b := beads.Bead{
		ID:        image.ID,
		Type:      image.IssueType,
		Status:    image.Status,
		Labels:    image.Labels,
		NoHistory: image.NoHistory,
	}
	if !IsEventFact(b) {
		return Fact{}, false, nil
	}
	if image.ID != row.IssueID {
		return Fact{}, true, corruptf(row.IssueID, "journal row %d's post-image is row %s", row.Seq, image.ID)
	}
	// Metadata decodes as the store's string projection does: a non-string
	// value reads as its JSON text.
	var meta beads.StringMap
	if len(image.Metadata) > 0 {
		if err := json.Unmarshal(image.Metadata, &meta); err != nil {
			return Fact{}, true, corruptf(row.IssueID, "journal row %d: metadata is not an object: %v", row.Seq, err)
		}
	}
	b.Metadata = meta
	f, err = Decode(b)
	if err != nil {
		return Fact{}, true, err
	}
	return f, true, nil
}
