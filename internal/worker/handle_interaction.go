package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Pending surfaces any current blocking interaction.
func (h *SessionHandle) Pending(ctx context.Context) (*PendingInteraction, error) {
	pending, _, err := h.PendingStatus(ctx)
	return pending, err
}

// PendingStatus reports both the current interaction, if any, and whether the
// underlying runtime supports interactive blocking requests at all.
func (h *SessionHandle) PendingStatus(context.Context) (*PendingInteraction, bool, error) {
	id := h.currentSessionID()
	if id == "" {
		return nil, false, nil
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return nil, false, err
	}
	if info.Closed {
		return nil, false, nil
	}
	switch info.State {
	case sessionpkg.StateAsleep, sessionpkg.StateSuspended, sessionpkg.StateDrained, sessionpkg.StateArchived:
		return nil, false, nil
	}
	pending, supported, err := h.manager.Pending(id)
	if err != nil {
		return nil, false, err
	}
	if !supported || pending == nil {
		return nil, supported, nil
	}
	return &PendingInteraction{
		RequestID: pending.RequestID,
		Kind:      pending.Kind,
		Prompt:    pending.Prompt,
		Options:   append([]string(nil), pending.Options...),
		Metadata:  cloneStringMap(pending.Metadata),
	}, true, nil
}

// Respond resolves the current blocking interaction.
func (h *SessionHandle) Respond(_ context.Context, req InteractionResponse) error {
	id := h.currentSessionID()
	if id == "" {
		return sessionpkg.ErrNoPendingInteraction
	}
	return h.manager.Respond(id, runtime.InteractionResponse{
		RequestID: req.RequestID,
		Action:    req.Action,
		Text:      req.Text,
		Metadata:  cloneStringMap(req.Metadata),
	})
}
