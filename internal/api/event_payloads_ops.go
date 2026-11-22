package api

import (
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// OpChangedPayload is the typed payload for op.changed (DES-O1 D18): an
// operation-journal op as its row reads after a change, without the message
// text. The envelope's subject is the op id and its session_id the
// conversation.
type OpChangedPayload struct {
	Op ConversationOp `json:"op" doc:"The operation as its row reads after the change."`
}

// IsEventPayload marks OpChangedPayload as an events.Payload variant.
func (OpChangedPayload) IsEventPayload() {}

// FactRecordedPayload is the typed payload for fact.recorded: one occurrence
// fact. The envelope's subject is the fact id and its session_id the
// conversation.
type FactRecordedPayload struct {
	Fact ConversationFact `json:"fact" doc:"The recorded fact."`
}

// IsEventPayload marks FactRecordedPayload as an events.Payload variant.
func (FactRecordedPayload) IsEventPayload() {}

// OpChangedPayloadFor is op.changed's payload for op: a pure function of the
// op row's post-image, so a repeated post-image yields identical bytes.
func OpChangedPayloadFor(op opjournal.Op) OpChangedPayload {
	return OpChangedPayload{Op: conversationOpRecord(op)}
}

// FactRecordedPayloadFor is fact.recorded's payload for f.
func FactRecordedPayloadFor(f opjournal.Fact) FactRecordedPayload {
	return FactRecordedPayload{Fact: conversationFactWire(f)}
}

func init() {
	events.RegisterPayload(events.OpChanged, OpChangedPayload{})
	events.RegisterPayload(events.FactRecorded, FactRecordedPayload{})
}
