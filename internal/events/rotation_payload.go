package events

// RotatedPayload is the structured payload of an events.rotated
// anchor event. The anchor is the first record written to a freshly
// rotated active log; its payload tells readers which archive holds
// the events immediately before this anchor and the seq range that
// archive covers.
//
// Field semantics:
//
//   - PriorArchive — basename of the gzipped archive (e.g.
//     "events.jsonl.archive-20260507T180000Z-seq-1234-5678.gz"). The
//     active log and its archives live in the same directory, so a
//     basename is enough to locate it.
//   - PriorFirstSeq, PriorLastSeq — first and last Seq values found in
//     the archive, inclusive on both ends. PriorLastSeq is strictly
//     less than the anchor's own Seq (FR-03 monotonicity).
type RotatedPayload struct {
	PriorArchive  string `json:"prior_archive"`
	PriorFirstSeq uint64 `json:"prior_first_seq"`
	PriorLastSeq  uint64 `json:"prior_last_seq"`
}

// IsEventPayload marks RotatedPayload as an events.Payload variant.
func (RotatedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(EventsRotated, RotatedPayload{})
}
