package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// laneScanStatuses are the open statuses a conversation-lane op can hold:
// gc.op.status is pushed down as metadata, and "not closed" cannot be (DES-O1
// §7.3), so the scan runs one keyed list per status.
var laneScanStatuses = []Status{StatusSubmitted, StatusAccepted, StatusExecuting}

// laneScanPages bounds each of OpenLaneConversations' keyed lists: at most
// this many pages of limit rows.
const laneScanPages = 8

// OpenLaneConversations lists the distinct conversations that hold an open
// conversation-lane op, in the order the scan first meets them, for a lane
// host's anti-entropy scan (DES-O1 §7.3). It runs three keyed lists, Label
// gc:op with gc.op.executor=lane and gc.op.status in submitted, accepted and
// executing, each oldest first in pages of limit rows (SeekAfter) up to
// laneScanPages pages, so one conversation's backlog does not hide the
// conversations behind it. A row whose conversation key is empty is skipped,
// since no pass can serve it. truncated reports that a list stopped at the
// page cap after a full page, so conversations behind it may remain unread.
// A limit below 1 reads every match in one read.
func (s *Store) OpenLaneConversations(limit int) (convs []string, truncated bool, err error) {
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
		for page := 1; ; page++ {
			rows, err := s.store.List(q)
			if err != nil {
				return nil, false, fmt.Errorf("listing open lane ops (%s): %w", status, err)
			}
			for _, b := range rows {
				conv := b.Metadata[keyConversation]
				if conv == "" || seen[conv] {
					continue
				}
				seen[conv] = true
				convs = append(convs, conv)
			}
			if limit == 0 || len(rows) < limit {
				break
			}
			if page == laneScanPages {
				truncated = true
				break
			}
			last := rows[len(rows)-1]
			q.SeekAfter = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	return convs, truncated, nil
}
