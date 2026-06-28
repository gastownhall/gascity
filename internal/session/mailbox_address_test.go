package session

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The mailbox-address codec is the session-class read half consumed by the mail
// CLI/API. These tests pin it to the exact behavior of the cmd/gc functions it
// replaces (sessionMailboxAddress / sessionMailboxAddresses) and to the
// handler_extmsg alias/session_name precedence, so the wire output is
// byte-identical after routing those callers through the front door. They also
// assert the read methods emit ZERO bead writes (Phase 1 is read-only).

func TestMailboxAddressCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "alias wins",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "session_name": "sn-1"}},
			want: "mayor",
		},
		{
			name: "id when no alias",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sess-1",
		},
		{
			name: "session_name when no alias or id",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sn-1",
		},
		{
			name: "alias trimmed",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "  mayor  "}},
			want: "mayor",
		},
		{
			name: "empty everything",
			bead: beads.Bead{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MailboxAddress(tt.bead); got != tt.want {
				t.Errorf("MailboxAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMailboxAddressesCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "alias + id + history deduped",
			bead: beads.Bead{
				ID: "sess-1",
				Metadata: map[string]string{
					"alias":         "mayor",
					"alias_history": "deacon,mayor,polecat",
					"session_name":  "sn-1",
				},
			},
			want: []string{"mayor", "sess-1", "deacon", "polecat"},
		},
		{
			name: "no addresses falls back to session_name",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "sn-1"}},
			want: []string{"sn-1"},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "sess-1"},
			want: []string{"sess-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MailboxAddresses(tt.bead); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MailboxAddresses = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExtmsgHandleSourceCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "alias wins (no id fallback)",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "session_name": "sn-1"}},
			want: "mayor",
		},
		{
			name: "session_name when no alias (id is ignored)",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sn-1",
		},
		{
			name: "empty when neither alias nor session_name",
			bead: beads.Bead{ID: "sess-1"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtmsgHandleSource(tt.bead); got != tt.want {
				t.Errorf("ExtmsgHandleSource = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestInfoStoreMailboxAddressReadOnly proves the front-door read methods load
// the session bead and apply the codec without emitting any bead writes.
func TestInfoStoreMailboxAddressReadOnly(t *testing.T) {
	b := sessionBeadFixture("sess-1", "open", map[string]string{
		"alias":         "mayor",
		"alias_history": "deacon",
		"session_name":  "sn-1",
	})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewInfoStore(beads.SessionStore{Store: rec})

	addr, err := is.MailboxAddress("sess-1")
	if err != nil {
		t.Fatalf("MailboxAddress: %v", err)
	}
	if addr != "mayor" {
		t.Errorf("MailboxAddress = %q, want mayor", addr)
	}

	addrs, err := is.MailboxAddresses("sess-1")
	if err != nil {
		t.Fatalf("MailboxAddresses: %v", err)
	}
	if want := []string{"mayor", "sess-1", "deacon"}; !reflect.DeepEqual(addrs, want) {
		t.Errorf("MailboxAddresses = %#v, want %#v", addrs, want)
	}

	if got := len(rec.Calls()); got != 0 {
		t.Errorf("read methods emitted %d bead writes, want 0", got)
	}
}

// TestInfoStoreExtmsgHandleSource proves the extmsg handle read method loads
// the bead, applies the alias/session_name codec, signals not-found via ok, and
// emits no bead writes.
func TestInfoStoreExtmsgHandleSource(t *testing.T) {
	b := sessionBeadFixture("sess-1", "open", map[string]string{"session_name": "sn-1"})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewInfoStore(beads.SessionStore{Store: rec})

	source, ok := is.ExtmsgHandleSource("sess-1")
	if !ok {
		t.Fatal("ExtmsgHandleSource ok = false, want true")
	}
	if source != "sn-1" {
		t.Errorf("source = %q, want sn-1", source)
	}

	if _, ok := is.ExtmsgHandleSource("absent"); ok {
		t.Error("ExtmsgHandleSource(absent) ok = true, want false")
	}

	if got := len(rec.Calls()); got != 0 {
		t.Errorf("ExtmsgHandleSource emitted %d bead writes, want 0", got)
	}
}

// TestInfoStoreMailboxAddressNotFound proves a missing session bead surfaces
// the verbatim Get error (beads.ErrNotFound-wrapped), matching the raw mail
// path which returned store.Get's error directly.
func TestInfoStoreMailboxAddressNotFound(t *testing.T) {
	mem := beads.NewMemStore()
	is := NewInfoStore(beads.SessionStore{Store: mem})
	if _, err := is.MailboxAddress("nope"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("err = %v, want beads.ErrNotFound", err)
	}
}
