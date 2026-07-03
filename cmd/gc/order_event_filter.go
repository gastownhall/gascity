package main

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/events"
)

type orderTriggerEventPayload struct {
	Bead         *orderTriggerEventBead `json:"bead"`
	Labels       []string               `json:"labels"`
	NoHistory    bool                   `json:"no_history"`
	NoHistoryAlt bool                   `json:"noHistory"`
}

type orderTriggerEventBead struct {
	Labels       []string `json:"labels"`
	NoHistory    bool     `json:"no_history"`
	NoHistoryAlt bool     `json:"noHistory"`
}

func orderTriggerEventPredicate(event events.Event) bool {
	return !isNoHistoryBeadLifecycleEvent(event)
}

func isNoHistoryBeadLifecycleEvent(event events.Event) bool {
	switch event.Type {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
	default:
		return false
	}
	bead, ok := orderTriggerEventBeadFromPayload(event.Payload)
	if !ok {
		return false
	}
	return bead.noHistory()
}

func orderTriggerEventBeadFromPayload(raw json.RawMessage) (orderTriggerEventBead, bool) {
	if len(raw) == 0 {
		return orderTriggerEventBead{}, false
	}
	var payload orderTriggerEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return orderTriggerEventBead{}, false
	}
	if payload.Bead != nil {
		return *payload.Bead, true
	}
	if len(payload.Labels) > 0 || payload.NoHistory || payload.NoHistoryAlt {
		return orderTriggerEventBead{
			Labels:       payload.Labels,
			NoHistory:    payload.NoHistory,
			NoHistoryAlt: payload.NoHistoryAlt,
		}, true
	}
	return orderTriggerEventBead{}, false
}

func (bead orderTriggerEventBead) noHistory() bool {
	return bead.NoHistory || bead.NoHistoryAlt
}
