// Package eventexport ships a redacted, envelope-only projection of the city
// event stream to a configured HTTP endpoint.
//
// The supervisor already records every event to .gc/events.jsonl; those records
// carry free-form, untrusted content (bead titles/descriptions, mail bodies,
// external-message identities, filesystem paths). This package projects each
// event down to a fixed metadata shell — type, time, a salted actor hash, and an
// id-regex-gated reference — and never copies subject, message, or payload. The
// projection is the trust boundary at the source: an unknown or non-allowlisted
// event type is dropped, and the output is built from a closed set of fields so a
// newly-added event field can never escape by default.
package eventexport

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// SchemaVersion is stamped on every batch so the receiver can evolve the
// projection without a flag day.
const SchemaVersion = 1

// AllowedTypes is the default-deny allowlist of exportable event types. Anything
// absent is dropped. High-churn or free-form-bearing types (bead.updated, the
// extmsg.* family) are intentionally excluded.
var AllowedTypes = map[string]bool{
	events.BeadCreated:                       true,
	events.BeadClosed:                        true,
	events.OrderFired:                        true,
	events.OrderCompleted:                    true,
	events.OrderFailed:                       true,
	events.SessionWoke:                       true,
	events.SessionStopped:                    true,
	events.SessionDraining:                   true,
	events.SessionStranded:                   true,
	events.ConvoyClosed:                      true,
	events.ControllerStarted:                 true,
	events.EventsRotated:                     true,
	events.SessionDrainAckedWithAssignedWork: true,
	events.SessionResetStalled:               true,
	events.ProjectIdentityStamped:            true,
	events.StoreMaintenanceDone:              true,
	events.MailSent:                          true, // reduced to {type, ts}; see Project
}

// mailReduced types export only {type, ts}: their actor/subject carry addressing
// that the metadata projection does not need.
var mailReduced = map[string]bool{events.MailSent: true}

// refTypes are the only types whose Subject may be exported as a ref. Their
// Subject is a guaranteed system-generated opaque store id (a bead or convoy
// id). Every other type drops its Subject entirely: a lexical filter cannot
// prove an arbitrary subject (an order slug, a scope-root directory name, a
// session/rig name, a hostname) is free of paths, author text, or third-party
// identifiers, so we never emit one.
var refTypes = map[string]bool{
	events.BeadCreated:  true,
	events.BeadClosed:   true,
	events.ConvoyClosed: true,
}

const maxRefLen = 64

// Envelope is the redacted shell that crosses the wire. It is the entire set of
// source-derived fields that ever leaves the box.
type Envelope struct {
	Seq       uint64 `json:"seq"`                  // source per-city seq (cursor/dedup reference)
	Type      string `json:"type"`                 // allowlisted event type
	TS        string `json:"ts"`                   // RFC3339 event time; display-only
	ActorHash string `json:"actor_hash,omitempty"` // salted hash; the cleartext actor never leaves the box
	Ref       string `json:"ref,omitempty"`        // id-regex-gated reference (opaque id/slug only)
}

// Batch is one POST body: the events for a single city.
type Batch struct {
	CityID        string     `json:"city_id"`
	SchemaVersion int        `json:"schema_version"`
	Events        []Envelope `json:"events"`
}

// Options controls the projection.
type Options struct {
	Salt      []byte // actor-hash salt; makes the hash stable yet non-reversible
	ExportRef bool   // include the id-gated ref (opaque ids/slugs only)
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

// Project reduces one event to its envelope, or returns ok=false if the event is
// not exportable. It reads only Seq/Type/Ts/Actor/Subject and never Payload or
// Message.
func Project(e events.Event, opt Options) (Envelope, bool) {
	if !AllowedTypes[e.Type] {
		return Envelope{}, false
	}
	if e.Seq == 0 || e.Ts.IsZero() {
		return Envelope{}, false
	}
	env := Envelope{Seq: e.Seq, Type: e.Type, TS: e.Ts.UTC().Format(time.RFC3339Nano)}
	if mailReduced[e.Type] {
		return env, true // {type, ts} only
	}
	env.ActorHash = ActorHash(opt.Salt, e.Actor)
	if opt.ExportRef && refTypes[e.Type] {
		if ref := safeRef(e.Subject); ref != "" {
			env.Ref = ref
		}
	}
	return env, true
}

// safeRef returns subject iff it is an opaque lowercase id/slug: no path
// separators, uppercase, '@', whitespace, or other free-text markers. This
// passes bead ids (mc-wisp-i6vz0e), convoy ids (gcg-4216) and order slugs
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
