package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// laneScanStatuses are the open statuses a conversation-lane op can hold:
// gc.op.status is pushed down as metadata, and "not closed" cannot be (DES-O1
// §7.3), so the scan runs one keyed list per status.
var laneScanStatuses = []Status{StatusSubmitted, StatusAccepted, StatusExecuting, StatusSettling}

// laneScanPages bounds each of OpenLaneConversations' keyed lists: at most
// this many pages of limit rows.
const laneScanPages = 8

// LaneScanCursor is where OpenLaneConversations' keyed lists stopped at the
// page cap: per status, the last row read. The zero cursor starts every list
// at its oldest row.
type LaneScanCursor struct {
	after map[Status]beads.SeekBoundary
}

// Done reports whether every keyed list was read to its end, so a scan from
// c starts every list over.
func (c LaneScanCursor) Done() bool { return len(c.after) == 0 }

// OpenLaneConversations lists the distinct conversations that hold an open
// conversation-lane op, in the order the scan first meets them, for a lane
// host's anti-entropy scan (DES-O1 §7.3). It runs one keyed list per open
// status, Label gc:op with gc.op.executor=lane and gc.op.status in
// submitted, accepted, executing and settling, each oldest first in pages of
// limit rows (SeekAfter) up to laneScanPages pages, so one conversation's
// backlog does not hide the conversations behind it. A list that stops at
// the page cap after a full page records where in next, and a scan from next
// reads that list on from there; a list read to its end starts over at its
// oldest row, so successive scans reach every row. A row whose conversation
// key is empty is skipped, since no pass can serve it, and so is a row whose
// kind another lane executes (ExecutorLane is shared with lifecycle kinds). A
// row whose kind this build does not know is kept, so its pass quarantines it
// where an operator sees it. A limit below 1 reads every match in one read.
func (s *Store) OpenLaneConversations(limit int, from LaneScanCursor) (convs []string, next LaneScanCursor, err error) {
	if limit < 0 {
		limit = 0
	}
	seen := map[string]bool{}
	for _, status := range laneScanStatuses {
		q := beads.ListQuery{
			Label:    LabelOp,
			Metadata: map[string]string{keyExecutor: string(ExecutorLane), keyStatus: string(status)},
			Sort:     beads.SortCreatedAsc,
			Limit:    limit,
			TierMode: beads.TierBoth,
		}
		if b, ok := from.after[status]; ok && limit > 0 {
			q.SeekAfter = &b
		}
		for page := 1; ; page++ {
			rows, err := s.store.List(q)
			if err != nil {
				return nil, LaneScanCursor{}, fmt.Errorf("listing open lane ops (%s): %w", status, err)
			}
			for _, b := range rows {
				conv := b.Metadata[keyConversation]
				if conv == "" || seen[conv] || !conversationLaneKind(Kind(b.Metadata[keyKind])) {
					continue
				}
				seen[conv] = true
				convs = append(convs, conv)
			}
			if limit == 0 || len(rows) < limit {
				break
			}
			last := rows[len(rows)-1]
			boundary := beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
			if page == laneScanPages {
				if next.after == nil {
					next.after = map[Status]beads.SeekBoundary{}
				}
				next.after[status] = boundary
				break
			}
			q.SeekAfter = &boundary
		}
	}
	return convs, next, nil
}

// conversationLaneKind reports whether a conversation lane serves an op of
// kind k: a conversation-lane kind, or one this build does not know.
func conversationLaneKind(k Kind) bool {
	spec, err := Lookup(k)
	return err != nil || spec.Lane == LaneConversation
}
