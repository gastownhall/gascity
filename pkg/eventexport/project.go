// Package eventexport projects a city event stream down to a redacted,
// envelope-only shell and ships per-city batches to a configured HTTP endpoint.
//
// The supervisor records every event with free-form, untrusted content (bead
// titles/descriptions, mail bodies, external-message identities, filesystem
// paths). This package never sees that content: a caller hands it only the
// closed set of primitive fields that may ever leave the box — sequence, type,
// time, actor, subject, and two opaque correlation ids — and the projection
// reduces them to a fixed envelope: type, time, a salted actor hash, an
// id-regex-gated reference, and the opaque run/session ids. An unknown or
// non-allowlisted event type is dropped, and the envelope is a closed struct so
// a newly-added source field can never escape by default.
//
// The package imports only the standard library. The supervisor-coupled event
// source (which knows about internal/events) lives in a separate adapter so
// this package stays a dependency-light, OSS-consumable projection contract.
package eventexport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is stamped on every batch so the receiver can evolve the
// projection without a flag day. Bump it whenever a change alters the wire
// bytes or the redaction behavior, so a downstream consumer pinned to an older
// version rejects the batch loudly instead of mis-parsing it. A pure refactor
// that leaves the bytes identical does NOT bump — for example, adding the
// run_id/session_id fields while they are always empty (omitempty omits them)
// is byte-identical and stays at this version; the first change that actually
// emits those values must bump.
const SchemaVersion = 1

// Profile selects the redaction profile. There is exactly one today; it is part
// of the public API so Validate can stay profile-aware as profiles are added
// without a breaking signature change.
type Profile int

const (
	// ProfileRedactedEnvelope is the default-deny, envelope-only projection:
	// type/time/actor-hash/opaque-ref/opaque-ids, never free-form content.
	ProfileRedactedEnvelope Profile = iota
)

// AllowedTypes is the default-deny allowlist of exportable event types, keyed by
// the canonical wire type string. Anything absent is dropped. High-churn or
// free-form-bearing types (bead.updated, the extmsg.* family) are intentionally
// excluded. These strings are the wire values of the internal/events type
// constants; the supervisor-side adapter carries a drift test that fails CI if
// they diverge, so this package never imports internal/events.
//
// It is read-only: this is an egress trust boundary, and mutating the map at
// runtime would silently widen what may leave the box. Treat it as a constant.
var AllowedTypes = map[string]bool{
	"bead.created":                           true,
	"bead.closed":                            true,
	"order.fired":                            true,
	"order.completed":                        true,
	"order.failed":                           true,
	"session.woke":                           true,
	"session.stopped":                        true,
	"session.draining":                       true,
	"session.stranded":                       true,
	"convoy.closed":                          true,
	"controller.started":                     true,
	"events.rotated":                         true,
	"session.drain_acked_with_assigned_work": true,
	"session.reset_stalled":                  true,
	"project.identity.stamped":               true,
	"gc.store.maintenance.done":              true,
	"mail.sent":                              true, // reduced to {type, ts}; see ProjectFields
}

// mailReduced types export only {type, ts}: their actor/subject carry addressing
// that the metadata projection does not need.
var mailReduced = map[string]bool{"mail.sent": true}

// refTypes are the only types whose Subject may be exported as a ref. Their
// Subject is a guaranteed system-generated opaque store id (a bead or convoy
// id). Every other type drops its Subject entirely: a lexical filter cannot
// prove an arbitrary subject (an order slug, a scope-root directory name, a
// session/rig name, a hostname) is free of paths, author text, or third-party
// identifiers, so we never emit one.
var refTypes = map[string]bool{
	"bead.created":  true,
	"bead.closed":   true,
	"convoy.closed": true,
}

const maxRefLen = 64

// Envelope is the redacted shell that crosses the wire. It is the entire set of
// source-derived fields that ever leaves the box. run_id/session_id are opaque
// correlation ids carried as fields (never as transport headers); they let a
// receiver join an event to its run/session without the projection ever copying
// free-form content.
type Envelope struct {
	Seq       uint64 `json:"seq"`                  // source per-city seq (cursor/dedup reference)
	Type      string `json:"type"`                 // allowlisted event type
	TS        string `json:"ts"`                   // RFC3339 event time; display-only
	ActorHash string `json:"actor_hash,omitempty"` // salted hash; the cleartext actor never leaves the box
	Ref       string `json:"ref,omitempty"`        // id-regex-gated reference (opaque id/slug only)
	RunID     string `json:"run_id,omitempty"`     // opaque run-root correlation id (safeRef-gated)
	SessionID string `json:"session_id,omitempty"` // opaque session correlation id (safeRef-gated)
}

// Batch is one POST body: the events for a single city.
type Batch struct {
	CityID        string     `json:"city_id"`
	SchemaVersion int        `json:"schema_version"`
	Events        []Envelope `json:"events"`
}

// Options controls the projection.
type Options struct {
	Salt      []byte  // actor-hash salt; makes the hash stable yet non-reversible
	ExportRef bool    // include the id-gated ref (opaque ids/slugs only)
	Profile   Profile // redaction profile (default ProfileRedactedEnvelope)
}

// ActorHash returns a salted, non-reversible, 16-hex fingerprint of an actor.
// The same actor hashes to the same value under one salt; the cleartext is never
// emitted.
func ActorHash(salt []byte, actor string) string {
	if actor == "" {
		return ""
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(":"))
	h.Write([]byte(actor))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ProjectFields reduces one event's primitive fields to its envelope, or returns
// ok=false if the event is not exportable. The caller passes only the closed set
// of fields that may ever leave the box — never payload or message — so this
// package cannot leak free-form content by construction.
//
// runID/sessionID are opaque correlation ids (e.g. a run-root or session bead
// id). Each is emitted only if it passes safeRef, so a non-opaque value (a path,
// an address, free text) is dropped to "" rather than exported.
func ProjectFields(seq uint64, typ string, ts time.Time, actor, subject, runID, sessionID string, opt Options) (Envelope, bool) {
	if !AllowedTypes[typ] {
		return Envelope{}, false
	}
	if seq == 0 || ts.IsZero() {
		return Envelope{}, false
	}
	env := Envelope{Seq: seq, Type: typ, TS: ts.UTC().Format(time.RFC3339Nano)}
	if mailReduced[typ] {
		return env, true // {type, ts} only
	}
	env.ActorHash = ActorHash(opt.Salt, actor)
	if opt.ExportRef && refTypes[typ] {
		if ref := safeRef(subject); ref != "" {
			env.Ref = ref
		}
	}
	if r := safeRef(runID); r != "" {
		env.RunID = r
	}
	if s := safeRef(sessionID); s != "" {
		env.SessionID = s
	}
	return env, true
}

// Validate re-asserts the redaction invariants on an already-projected envelope.
// It is the load-bearing trust-boundary check a receiver can run independently of
// the projector: a batch that fails Validate must be rejected, not ingested. It
// performs explicit value checks (no reflection) for opt.Profile.
func Validate(env Envelope, opt Options) error {
	if opt.Profile != ProfileRedactedEnvelope {
		return fmt.Errorf("eventexport: unknown profile %d", opt.Profile)
	}
	if !AllowedTypes[env.Type] {
		// Covers the extmsg.* family and every other non-allowlisted type.
		return fmt.Errorf("eventexport: type %q not allowlisted", env.Type)
	}
	if env.Seq == 0 {
		return errors.New("eventexport: seq must be > 0")
	}
	if t, err := time.Parse(time.RFC3339Nano, env.TS); err != nil || t.IsZero() {
		return fmt.Errorf("eventexport: invalid ts %q", env.TS)
	}
	if mailReduced[env.Type] {
		if env.ActorHash != "" || env.Ref != "" || env.RunID != "" || env.SessionID != "" {
			return fmt.Errorf("eventexport: %q must carry only {seq,type,ts}", env.Type)
		}
		return nil
	}
	if env.ActorHash != "" && !isHex16(env.ActorHash) {
		return fmt.Errorf("eventexport: actor_hash %q must be 16 hex chars", env.ActorHash)
	}
	if env.Ref != "" {
		if !opt.ExportRef || !refTypes[env.Type] {
			return fmt.Errorf("eventexport: type %q must not carry a ref", env.Type)
		}
		if safeRef(env.Ref) != env.Ref {
			return fmt.Errorf("eventexport: ref %q is not an opaque id", env.Ref)
		}
	}
	if env.RunID != "" && safeRef(env.RunID) != env.RunID {
		return fmt.Errorf("eventexport: run_id %q is not an opaque id", env.RunID)
	}
	if env.SessionID != "" && safeRef(env.SessionID) != env.SessionID {
		return fmt.Errorf("eventexport: session_id %q is not an opaque id", env.SessionID)
	}
	return nil
}

// safeRef returns s iff it is an opaque lowercase id/slug: no path separators,
// uppercase, '@', whitespace, or other free-text markers. This passes bead ids
// (mc-wisp-i6vz0e), convoy ids (gcg-4216) and order slugs
// (cascade-nudge-on-blocker-close); it rejects repo/path refs (gascity/codex-1).
func safeRef(s string) string {
	if s == "" || len(s) > maxRefLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return ""
		}
	}
	first := s[0]
	firstAlnum := (first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')
	if !firstAlnum {
		return ""
	}
	return s
}

// isHex16 reports whether s is exactly 16 lowercase hex characters (the
// ActorHash shape).
func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		hexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !hexDigit {
			return false
		}
	}
	return true
}
