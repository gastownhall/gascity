package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestInfoLogicalConversationIDScopesBeadByContinuationEpoch(t *testing.T) {
	cases := []struct {
		name  string
		epoch map[string]string
		want  string
	}{
		{name: "missing epoch reads as the default", epoch: nil, want: "s#1"},
		{name: "first epoch", epoch: map[string]string{"continuation_epoch": "1"}, want: "s#1"},
		{name: "each reset is its own conversation", epoch: map[string]string{"continuation_epoch": "2"}, want: "s#2"},
		{name: "epoch trimmed", epoch: map[string]string{"continuation_epoch": " 3 "}, want: "s#3"},
		{name: "non-positive epoch reads as the default", epoch: map[string]string{"continuation_epoch": "0"}, want: "s#1"},
		{name: "invalid epoch reads as the default", epoch: map[string]string{"continuation_epoch": "garbage"}, want: "s#1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: tc.epoch}
			if got := infoFromPersistedBead(b).LogicalConversationID(); got != tc.want {
				t.Fatalf("LogicalConversationID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A provider with a session-id flag runs each new conversation under a new key,
// including after a fresh wake that keeps the epoch, so that key is part of the
// identity. Any other provider's key is learned late and is not: it names the
// conversation the epoch already identifies.
func TestInfoLogicalConversationIDScopesEpochByKeyWithSessionIDFlag(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{name: "minted key", meta: map[string]string{"continuation_epoch": "2", "session_id_flag": "--session-id", "session_key": "k1"}, want: "s#2/k1"},
		{name: "fresh wake in the same epoch", meta: map[string]string{"continuation_epoch": "2", "session_id_flag": "--session-id", "session_key": "k2"}, want: "s#2/k2"},
		{name: "key trimmed", meta: map[string]string{"continuation_epoch": "2", "session_id_flag": "--session-id", "session_key": " k1 "}, want: "s#2/k1"},
		{name: "key not minted yet", meta: map[string]string{"continuation_epoch": "2", "session_id_flag": "--session-id"}, want: "s#2"},
		{name: "key reported late without a session-id flag", meta: map[string]string{"continuation_epoch": "2", "session_key": "thread-1"}, want: "s#2"},
		{name: "blank session-id flag", meta: map[string]string{"continuation_epoch": "2", "session_id_flag": " ", "session_key": "thread-1"}, want: "s#2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: tc.meta}
			if got := infoFromPersistedBead(b).LogicalConversationID(); got != tc.want {
				t.Fatalf("LogicalConversationID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInfoLogicalConversationIDEmptyWithoutBeadID(t *testing.T) {
	if got := (Info{ContinuationEpoch: "2"}).LogicalConversationID(); got != "" {
		t.Fatalf("LogicalConversationID() = %q, want empty without a bead ID", got)
	}
}
