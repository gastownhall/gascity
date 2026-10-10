// Package reconcilekey defines the key vocabulary that controller wake-ups
// carry. Every trigger that used to be a key-less "poke" names the unit of
// work it concerns: the city-wide allocator, one session, or the control
// dispatcher.
//
// Under the legacy (tick-driven) reconciler every key still maps onto the
// existing poke or control-dispatcher signal, so the key is informational.
// Carrying it end to end lets a keyed reconciler route work to per-key
// controllers without revisiting each call site.
package reconcilekey

import (
	"encoding/json"
	"strings"
)

// Kind names which controller a key addresses.
type Kind string

const (
	// KindAllocator addresses the single city-wide allocator. It is also
	// what every key-less trigger means.
	KindAllocator Kind = "allocator"
	// KindSession addresses one session, by bead ID or runtime name.
	KindSession Kind = "session"
	// KindControlDispatch addresses the control-dispatcher reconcile.
	KindControlDispatch Kind = "control_dispatch"
)

// Key identifies the unit of reconcile work a trigger concerns. Build keys
// with the constructors; the zero Key means Allocator.
type Key struct {
	Kind Kind `json:"kind"`
	// SessionID is the canonical session bead ID (session keys only).
	SessionID string `json:"session_id,omitempty"`
	// SessionName is the runtime session name, for callers that only know
	// the name (session keys only). A keyed reconciler resolves it to the
	// canonical ID through its index.
	SessionName string `json:"session_name,omitempty"`
	// StoreRef optionally names the store leg holding the session bead.
	StoreRef string `json:"store_ref,omitempty"`
}

// Allocator returns the city-wide allocator key.
func Allocator() Key { return Key{Kind: KindAllocator} }

// ControlDispatch returns the control-dispatcher key.
func ControlDispatch() Key { return Key{Kind: KindControlDispatch} }

// Session returns the key for the session with bead ID id. An empty id
// degrades to the allocator key, which is what a key-less poke meant.
func Session(id string) Key {
	return Key{Kind: KindSession, SessionID: id}.Normalize()
}

// SessionNamed returns the key for the session with runtime name name, for
// callers that do not know the bead ID. An empty name degrades to Allocator.
func SessionNamed(name string) Key {
	return Key{Kind: KindSession, SessionName: name}.Normalize()
}

// SessionRef returns the key for a session identified by bead ID, runtime
// name, or both, for callers that hold whichever identity they have. Both
// empty degrades to Allocator.
func SessionRef(id, name string) Key {
	return Key{Kind: KindSession, SessionID: id, SessionName: name}.Normalize()
}

// Normalize trims fields and folds anything that does not identify a
// session or the control dispatcher into the allocator key, so key-less and
// malformed triggers keep their legacy meaning.
func (k Key) Normalize() Key {
	switch k.Kind {
	case KindControlDispatch:
		return ControlDispatch()
	case KindSession:
		out := Key{
			Kind:        KindSession,
			SessionID:   strings.TrimSpace(k.SessionID),
			SessionName: strings.TrimSpace(k.SessionName),
			StoreRef:    strings.TrimSpace(k.StoreRef),
		}
		if out.SessionID == "" && out.SessionName == "" {
			return Allocator()
		}
		return out
	default:
		return Allocator()
	}
}

// Encode renders the normalized key as single-line JSON, suitable for a
// line-framed controller socket command.
func (k Key) Encode() string {
	data, err := json.Marshal(k.Normalize())
	if err != nil {
		// A struct of strings cannot fail to marshal.
		return `{"kind":"allocator"}`
	}
	return string(data)
}

// Decode parses a payload produced by Encode and normalizes it.
func Decode(payload string) (Key, error) {
	var k Key
	if err := json.Unmarshal([]byte(payload), &k); err != nil {
		return Allocator(), err
	}
	return k.Normalize(), nil
}
