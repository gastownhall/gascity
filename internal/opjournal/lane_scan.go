package opjournal

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// laneScanStatuses are the open statuses a conversation-lane op can hold:
// gc.op.status is pushed down as metadata, and "not closed" cannot be (DES-O1
// §7.3), so the scan runs one keyed list per status.
var laneScanStatuses = []Status{StatusSubmitted, StatusAccepted, StatusExecuting}

// OpenLaneConversations lists the distinct conversations that hold an open
// conversation-lane op, in the order the scan first meets them, for a lane
// host's anti-entropy scan (DES-O1 §7.3). It runs three keyed lists, Label
// gc:op with gc.op.executor=lane and gc.op.status in submitted, accepted and
// executing, each of at most limit rows; a row whose conversation key is
// empty is skipped, since no pass can serve it. truncated reports that a list
// returned limit rows, so conversations behind them may remain unread. A
// limit below 1 reads every match.
func (s *Store) OpenLaneConversations(limit int) (convs []string, truncated bool, err error) {
	if limit < 0 {
		limit = 0
	}
	seen := map[string]bool{}
	for _, status := range laneScanStatuses {
		rows, err := s.store.List(beads.ListQuery{
			Label:    LabelOp,
			Metadata: map[string]string{keyExecutor: string(ExecutorLane), keyStatus: string(status)},
			Sort:     beads.SortCreatedAsc,
			Limit:    limit,
			TierMode: beads.TierBoth,
		})
		if err != nil {
			return nil, false, fmt.Errorf("listing open lane ops (%s): %w", status, err)
		}
		if limit > 0 && len(rows) >= limit {
			truncated = true
		}
		for _, b := range rows {
			conv := b.Metadata[keyConversation]
			if conv == "" || seen[conv] {
				continue
			}
			seen[conv] = true
			convs = append(convs, conv)
		}
	}
	return convs, truncated, nil
}
