package extmsg

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// hasLabel checks if a bead has a specific label.
func hasLabel(b beads.Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func normalizeConversationRef(ref ConversationRef) ConversationRef {
	ref.ScopeID = strings.TrimSpace(ref.ScopeID)
	ref.Provider = strings.ToLower(strings.TrimSpace(ref.Provider))
	ref.AccountID = strings.TrimSpace(ref.AccountID)
	ref.ConversationID = strings.TrimSpace(ref.ConversationID)
	ref.ParentConversationID = strings.TrimSpace(ref.ParentConversationID)
	ref.Kind = ConversationKind(strings.ToLower(strings.TrimSpace(string(ref.Kind))))
	return ref
}

func validateConversationRef(ref ConversationRef) (ConversationRef, error) {
	ref = normalizeConversationRef(ref)
	switch {
	case ref.ScopeID == "":
		return ConversationRef{}, fmt.Errorf("%w: scope_id required", ErrInvalidConversation)
	case ref.Provider == "":
		return ConversationRef{}, fmt.Errorf("%w: provider required", ErrInvalidConversation)
	case ref.AccountID == "":
		return ConversationRef{}, fmt.Errorf("%w: account_id required", ErrInvalidConversation)
	case ref.ConversationID == "":
		return ConversationRef{}, fmt.Errorf("%w: conversation_id required", ErrInvalidConversation)
	}
	switch ref.Kind {
	case ConversationDM, ConversationRoom, ConversationThread:
		return ref, nil
	default:
		return ConversationRef{}, fmt.Errorf("%w: invalid kind %q", ErrInvalidConversation, ref.Kind)
	}
}

func normalizeCaller(caller Caller) Caller {
	caller.Kind = CallerKind(strings.ToLower(strings.TrimSpace(string(caller.Kind))))
	caller.ID = strings.TrimSpace(caller.ID)
	caller.Provider = strings.ToLower(strings.TrimSpace(caller.Provider))
	caller.AccountID = strings.TrimSpace(caller.AccountID)
	return caller
}

func authorizeMutation(caller Caller, ref ConversationRef) error {
	caller = normalizeCaller(caller)
	ref = normalizeConversationRef(ref)
	switch caller.Kind {
	case CallerController:
		return nil
	case CallerAdapter:
		if caller.Provider == "" || caller.AccountID == "" {
			return ErrInvalidCaller
		}
		if caller.Provider != ref.Provider || caller.AccountID != ref.AccountID {
			return ErrUnauthorized
		}
		return nil
	default:
		return ErrUnauthorized
	}
}

func normalizeHandle(handle string) string {
	return strings.ToLower(strings.TrimSpace(handle))
}

func validateHandle(handle string) (string, error) {
	handle = normalizeHandle(handle)
	if handle == "" {
		return "", ErrInvalidHandle
	}
	return handle, nil
}

func copyMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func encodeMetadataFields(meta map[string]string, fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields)+len(meta))
	for k, v := range fields {
		if strings.TrimSpace(k) == "" {
			continue
		}
		out[k] = v
	}
	for k, v := range copyMetadata(meta) {
		out[metadataPrefix+k] = v
	}
	return out
}

func decodePrefixedMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range meta {
		if strings.HasPrefix(k, metadataPrefix) {
			out[strings.TrimPrefix(k, metadataPrefix)] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseTime(meta map[string]string, key string) (time.Time, error) {
	raw := strings.TrimSpace(meta[key])
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", key, err)
	}
	return t, nil
}

func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseBool(meta map[string]string, key string) bool {
	v, _ := strconv.ParseBool(strings.TrimSpace(meta[key]))
	return v
}

func parseInt(meta map[string]string, key string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(meta[key]))
	return v
}

func parseInt64(meta map[string]string, key string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(meta[key]), 10, 64)
	return v
}

func conversationTitle(ref ConversationRef) string {
	ref = normalizeConversationRef(ref)
	return ref.Provider + "/" + ref.AccountID + "/" + ref.ConversationID
}

func recordStatus(b beads.Bead) BindingStatus {
	if b.Status == "closed" {
		return BindingEnded
	}
	return BindingActive
}

func zeroNow(now time.Time) time.Time {
	if now.IsZero() {
		return timeNow()
	}
	return now.UTC()
}

func sortConversationRefs(bindings []SessionBindingRecord) {
	sort.Slice(bindings, func(i, j int) bool {
		return conversationLockKey(bindings[i].Conversation) < conversationLockKey(bindings[j].Conversation)
	})
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func sameConversationRef(a, b ConversationRef) bool {
	a = normalizeConversationRef(a)
	b = normalizeConversationRef(b)
	return a.ScopeID == b.ScopeID &&
		a.Provider == b.Provider &&
		a.AccountID == b.AccountID &&
		a.ConversationID == b.ConversationID &&
		a.ParentConversationID == b.ParentConversationID &&
		a.Kind == b.Kind
}
