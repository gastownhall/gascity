// Package eventexport projects a city event stream down to a redacted,
// envelope-only shell and ships per-city batches to a configured HTTP endpoint.
//
// The supervisor records every event with free-form, untrusted content (bead
// titles/descriptions, mail bodies, external-message identities, filesystem
// paths). This package never sees that content: a caller hands it only a
// TaggedEvent — the closed set of primitive fields that may ever leave the box
// (sequence, type, time, actor, subject, and two opaque correlation ids) — and
// the projection reduces it to a fixed envelope: type, time, a salted actor
// hash, an id-regex-gated reference, and the opaque run/session ids. An unknown
// or non-allowlisted event type is dropped, and the envelope is a closed struct
// so a newly-added source field can never escape by default.
//
// EXCEPTION (opt-in): two envelope fields — Title and Formula — carry free-form
// content (a bead's human title; a run's formula name). They are the deliberate
// reversal of the envelope-only default, emitted ONLY when the package-internal
// content opt-in is set, length-capped, and never on a mail-reduced type. With
// the opt-in unset (the default) the projection stays envelope-only and no
// content leaves the box.
//
// This is the RECEIVER-READY half of a staged rollout: the projection accepts,
// gates, length-caps, and validates Title/Formula so a receiver can parse and
// trust-check populated batches, but the producer half is deliberately absent.
// The content opt-in (Options.emitContent) is UNEXPORTED, the Exporter Config
// carries no content knob, and the eventfeed adapter does not source the fields,
// so no out-of-package caller of ProjectEvent can enable content and nothing can
// egress end to end today BY DESIGN. Exposing a reachable operator opt-in and the
// typed source fields through the producer path — and the SchemaVersion decision
// that reachable content egress then requires — is tracked separately (ga-mt1e99).
//
// The package imports only the standard library. The supervisor-coupled event
// source (which knows about internal/events) lives in a separate adapter so
// this package stays a dependency-light, OSS-consumable projection contract.
//
// The trust boundary has two faces: the producer projects with ProjectEvent and
// may self-check with Validate; a receiver re-validates each batch it ingests
// with ValidateBatch/ValidateEnvelope, which depend on NO producer configuration
// (ExportRef is a producer knob, not on the wire).
package eventexport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SchemaVersion is stamped on every batch so the receiver can evolve the
// projection without a flag day. Bump it whenever a change alters the DEFAULT
// wire bytes OR the DEFAULT redaction policy (e.g. widening the type allowlist —
// see the allowlist golden test), so a downstream consumer pinned to an older
// version rejects the batch loudly (ValidateBatch -> ErrSchemaMismatch) instead
// of mis-handling it. A pure refactor that leaves bytes and policy identical does
// NOT bump.
//
// Off-by-default opt-in exemption: an OPTIONAL field that is gated behind a
// default-false, package-private producer flag and is omitempty does NOT bump on
// its own. With the flag off — the default every existing receiver sees — the wire
// bytes and the redaction policy are byte-identical (the golden wire test proves
// the empty field is omitted), so an old receiver's batches are unchanged. The
// version describes the DEFAULT contract; a field no reachable producer can turn
// on yet has not changed it. This is why Title/Formula (gated by the UNEXPORTED
// Options.emitContent, default false, with no reachable Config knob on the
// Exporter) were added without a bump: because the opt-in is package-private, no
// out-of-package importer of ProjectEvent can enable content, so the exemption is
// compiler-enforced rather than a convention. The forcing function moves to the
// producer: when a future change exposes a reachable content opt-in (the
// eventfeed/cmd producer path — see ga-mt1e99), that change owns the decision of
// whether reachable content egress warrants a bump and the receiver coordination
// it implies.
//
// v2 replaced the cleartext city_id with a salted, non-reversible city_hash so
// an operator-chosen city name (which can itself embed a customer/org
// identifier) no longer leaves the box.
const SchemaVersion = 2

// Profile selects the redaction profile. There is exactly one today; it is part
// of the public API so Validate can stay profile-aware as profiles are added
// without a breaking signature change. ProfileRedactedEnvelope must remain the
// zero value forever (append new profiles, never insert).
type Profile int

const (
	// ProfileRedactedEnvelope is the default-deny, envelope-only projection:
	// type/time/actor-hash/opaque-ref/opaque-ids, never free-form content.
	ProfileRedactedEnvelope Profile = iota
)

const (
	maxRefLen     = 64  // run_id/session_id/ref over this are DROPPED, not truncated.
	minSaltLen    = 16  // below this the salted actor hash is brute-forceable; fail closed.
	maxContentLen = 256 // free-form title/formula over this are DROPPED, not truncated.
)

// allowedTypes is the default-deny allowlist of exportable event types, keyed by
// the canonical wire type string. Anything absent is dropped. High-churn or
// free-form-bearing types (bead.updated, the extmsg.* family) are intentionally
// excluded. It is UNEXPORTED so no importer can widen the egress surface at
// runtime; query it via IsAllowed and enumerate it via AllowedTypeList. The
// strings are the wire values of the internal/events type constants; the
// supervisor-side adapter carries a drift test that fails CI if they diverge, so
// this package never imports internal/events.
var allowedTypes = map[string]bool{
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
	"mail.sent":                              true, // reduced to {type, ts}; see ProjectEvent
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

// IsAllowed reports whether an event type is on the export allowlist.
func IsAllowed(typ string) bool { return allowedTypes[typ] }

// AllowedTypeList returns the allowlisted event types as a sorted, fresh copy —
// for docs, ingest, conformance, and drift enumeration. Mutating the result does
// not affect the allowlist (it is an un-widenable constant of this build).
func AllowedTypeList() []string {
	out := make([]string, 0, len(allowedTypes))
	for t := range allowedTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

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
	StepID    string `json:"step_id,omitempty"`    // opaque acting-work-bead (run step) id; safeRef-gated, EmitCorrelation
	// Title/Formula are the DELIBERATE exception to envelope-only: free-form content
	// (a bead's human title; a run's formula name), gated by the package-internal
	// content opt-in (Options.emitContent), length-capped (dropped, not truncated),
	// and NOT routed through the opaque-id machinery. They ship empty unless it is set.
	Title   string `json:"title,omitempty"`
	Formula string `json:"formula,omitempty"`
	// Force keyed literals so a positional Envelope{...} can never transpose the two
	// adjacent free-form content fields (or land content in an opaque-id slot) at the
	// redaction boundary; mirrors TaggedEvent. Unexported + blank, so json ignores it.
	_ struct{}
}

// Batch is one POST body: the events for a single city. CityHash is a salted,
// non-reversible partition key (see CityHash); the cleartext city name never
// crosses the wire.
type Batch struct {
	CityHash      string     `json:"city_hash"`
	SchemaVersion int        `json:"schema_version"`
	Events        []Envelope `json:"events"`
}

// Options controls the projection.
//
// emitContent is UNEXPORTED on purpose: it is the content opt-in that reverses
// the envelope-only default, and keeping it package-private is what makes the
// SchemaVersion no-bump exemption sound. An out-of-package importer constructs
// Options with keyed literals and so CANNOT enable content, which means no caller
// of the exported ProjectEvent can emit Title/Formula on a SchemaVersion==2
// batch. The field exists only for in-package projection tests and the future
// producer path (ga-mt1e99), which owns exposing a reachable opt-in and the
// SchemaVersion decision that reachable content egress then requires.
type Options struct {
	Salt            []byte  // actor-hash salt; must be >= 16 bytes (ProjectEvent fails closed otherwise)
	ExportRef       bool    // include the id-gated ref (opaque ids/slugs only)
	Profile         Profile // redaction profile (default ProfileRedactedEnvelope)
	EmitCorrelation bool    // emit opaque run_id/session_id/step_id; default false (the production export sets it true)
	emitContent     bool    // emit free-form Title/Formula; default false. REVERSES the envelope-only default. UNEXPORTED so no out-of-package caller can enable content egress; the reachable producer opt-in is staged (ga-mt1e99).
}

// ActorHash returns a salted, non-reversible, 16-hex fingerprint of an actor.
// The same actor hashes to the same value under one salt; the cleartext is never
// emitted. It is a CORRELATION TOKEN, not an anonymity guarantee: with a weak or
// known salt, a city's small actor namespace is brute-forceable — which is why
// ProjectEvent requires len(Salt) >= 16.
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

// CityHash returns a salted, non-reversible, 16-hex partition key for a city
// name. Like ActorHash, it lets the receiver group batches per city without the
// cleartext city name — operator-chosen, and able to embed a customer/org
// identifier — ever leaving the box. It is domain-separated from ActorHash so a
// city and an actor that share a name do not hash alike.
func CityHash(salt []byte, city string) string {
	if city == "" {
		return ""
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(":city:"))
	h.Write([]byte(city))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ProjectEvent reduces one tagged event to its envelope, or returns ok=false if
// the event is not exportable. The caller passes a TaggedEvent — the closed set
// of fields that may ever leave the box — never payload or message, so this
// package cannot leak free-form content by construction.
//
// It fails closed (ok=false) for a non-allowlisted type, seq==0, a zero
// timestamp, or a salt shorter than 16 bytes. run_id/session_id are emitted only
// when opt.EmitCorrelation is set and only if opaque; like ref, an opaque id over
// 64 bytes is DROPPED, not truncated.
func ProjectEvent(te TaggedEvent, opt Options) (Envelope, bool) {
	if !allowedTypes[te.Type] {
		return Envelope{}, false
	}
	if te.Seq == 0 || te.Ts.IsZero() {
		return Envelope{}, false
	}
	// Fail-closed: a weak salt makes ActorHash a reversible SHA-256 prefix over a
	// city's small actor namespace. Refuse to project rather than emit it.
	if len(opt.Salt) < minSaltLen {
		return Envelope{}, false
	}
	env := Envelope{Seq: te.Seq, Type: te.Type, TS: te.Ts.UTC().Format(time.RFC3339Nano)}
	if mailReduced[te.Type] {
		return env, true // {type, ts} only
	}
	env.ActorHash = ActorHash(opt.Salt, te.Actor)
	if opt.ExportRef && refTypes[te.Type] {
		if ref := safeRef(te.Subject); ref != "" {
			env.Ref = ref
		}
	}
	if opt.EmitCorrelation {
		if r := safeRef(te.RunID); r != "" {
			env.RunID = r
		}
		if s := safeRef(te.SessionID); s != "" {
			env.SessionID = s
		}
		if st := safeRef(te.StepID); st != "" {
			env.StepID = st
		}
	}
	// Content fields are the deliberate exception to the envelope-only default:
	// gated by the package-internal emitContent (NOT the opaque-id machinery),
	// length-capped (dropped, not truncated), and — via the mailReduced
	// early-return above — never on a mail-reduced type.
	if opt.emitContent {
		if t := capContent(te.Title); t != "" {
			env.Title = t
		}
		if f := capContent(te.Formula); f != "" {
			env.Formula = f
		}
	}
	return env, true
}

// ValidateEnvelope re-asserts the wire-authoritative redaction invariants on a
// projected envelope, with NO producer configuration. It is the trust-boundary
// check a receiver runs on each row it ingests: ExportRef is a producer-side knob
// that is NOT on the wire, so a ref is valid here iff its type may carry one and
// the value is opaque — never gated on a flag the receiver cannot see.
func ValidateEnvelope(env Envelope) error {
	if !allowedTypes[env.Type] {
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
		if env.ActorHash != "" || env.Ref != "" || env.RunID != "" || env.SessionID != "" || env.StepID != "" || env.Title != "" || env.Formula != "" {
			return fmt.Errorf("eventexport: %q must carry only {seq,type,ts}", env.Type)
		}
		return nil
	}
	if env.ActorHash != "" && !isHex16(env.ActorHash) {
		return fmt.Errorf("eventexport: actor_hash %q must be 16 hex chars", env.ActorHash)
	}
	if env.Ref != "" {
		if !refTypes[env.Type] {
			return fmt.Errorf("eventexport: type %q must not carry a ref", env.Type)
		}
		if !IsOpaqueRef(env.Ref) {
			return fmt.Errorf("eventexport: ref %q is not an opaque id", env.Ref)
		}
	}
	if env.RunID != "" && !IsOpaqueRef(env.RunID) {
		return fmt.Errorf("eventexport: run_id %q is not an opaque id", env.RunID)
	}
	if env.SessionID != "" && !IsOpaqueRef(env.SessionID) {
		return fmt.Errorf("eventexport: session_id %q is not an opaque id", env.SessionID)
	}
	if env.StepID != "" && !IsOpaqueRef(env.StepID) {
		return fmt.Errorf("eventexport: step_id %q is not an opaque id", env.StepID)
	}
	// Title/Formula are free-form content (the content opt-in exception): the wire
	// invariant is a length bound, NOT opaqueness — charset is unrestricted.
	if len(env.Title) > maxContentLen {
		return fmt.Errorf("eventexport: title exceeds %d bytes", maxContentLen)
	}
	if len(env.Formula) > maxContentLen {
		return fmt.Errorf("eventexport: formula exceeds %d bytes", maxContentLen)
	}
	return nil
}

// Validate is the producer's defense-in-depth self-check: ValidateEnvelope plus
// the producer-only policies that a ref is present only when opt.ExportRef is set
// and that free-form Title/Formula are present only when opt.emitContent is set,
// under the configured profile. Both are producer knobs that are NOT on the wire,
// so receivers must use ValidateEnvelope/ValidateBatch instead — those do not
// depend on producer Options.
func Validate(env Envelope, opt Options) error {
	if opt.Profile != ProfileRedactedEnvelope {
		return fmt.Errorf("eventexport: unknown profile %d", opt.Profile)
	}
	if err := ValidateEnvelope(env); err != nil {
		return err
	}
	if env.Ref != "" && !opt.ExportRef {
		return errors.New("eventexport: ref present but ExportRef disabled")
	}
	// Symmetric with the ExportRef check: content must never be present unless the
	// producer opted into it. A populated Title/Formula with the content opt-in off
	// means the envelope was built outside ProjectEvent's content gate — fail the
	// self-check rather than ship content the operator never enabled.
	if (env.Title != "" || env.Formula != "") && !opt.emitContent {
		return errors.New("eventexport: title/formula present but content opt-in disabled")
	}
	return nil
}

// ErrSchemaMismatch reports a batch whose schema_version does not match this
// build's SchemaVersion. Receivers map it to their schema-mismatch handling so
// version skew fails loudly at the wire instead of silently at the redaction
// boundary.
var ErrSchemaMismatch = errors.New("eventexport: batch schema_version mismatch")

// ValidateBatch checks a received batch end to end: its schema_version must equal
// SchemaVersion (else it returns an error wrapping ErrSchemaMismatch), its
// city_hash must be the opaque 16-hex partition-key shape that schema v2 promises
// (rejecting empty, cleartext, or otherwise malformed values at the receiver trust
// boundary, the same shape gate ValidateEnvelope applies to actor_hash), then every
// envelope must pass ValidateEnvelope. Validation is fail-fast: it returns the
// FIRST failure with its row index, not an aggregate of all failures.
func ValidateBatch(b Batch) error {
	if b.SchemaVersion != SchemaVersion {
		return fmt.Errorf("eventexport: batch schema_version %d != %d: %w", b.SchemaVersion, SchemaVersion, ErrSchemaMismatch)
	}
	if !isHex16(b.CityHash) {
		return fmt.Errorf("eventexport: city_hash %q must be 16 hex chars", b.CityHash)
	}
	for i, env := range b.Events {
		if err := ValidateEnvelope(env); err != nil {
			return fmt.Errorf("eventexport: batch row %d: %w", i, err)
		}
	}
	return nil
}

// IsOpaqueRef reports whether s is a non-empty opaque lowercase id/slug (the
// shape safeRef accepts): the single importable definition every rail shares for
// an opaque correlation id. Values over 64 bytes are not opaque (dropped, not
// truncated).
func IsOpaqueRef(s string) bool { return s != "" && safeRef(s) == s }

// safeRef returns s iff it is an opaque lowercase id/slug: no path separators,
// uppercase, '@', whitespace, or other free-text markers, and no longer than 64
// bytes. This passes bead ids (mc-wisp-i6vz0e), convoy ids (gcg-4216) and order
// slugs (cascade-nudge-on-blocker-close); it rejects repo/path refs
// (gascity/codex-1) and anything over the length bound.
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

// capContent returns s iff it is non-empty and within the content length bound;
// an over-cap value is DROPPED (not truncated — a half-string is worse than
// absent). Unlike safeRef it does NOT restrict the charset: Title/Formula are
// free text by design (the content opt-in exception to the envelope-only contract).
func capContent(s string) string {
	if s == "" || len(s) > maxContentLen {
		return ""
	}
	return s
}

// isHex16 reports whether s is exactly 16 lowercase hex characters (the
// ActorHash and CityHash shape).
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
