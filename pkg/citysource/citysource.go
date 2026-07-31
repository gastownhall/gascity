// Package citysource is the Gas City signed event producer: it maps City events
// onto the api/v1 ingest wire under two signed policy digests, detects a source
// log reset LOUDLY, and advances a durable checkpoint only across acknowledged
// contiguous events.
//
// # The hazard this package exists to fix
//
// The City event source is a per-city append-only .gc/events.jsonl carrying a
// durable monotonic uint64 seq. The seq is resynced from disk before every
// write, so it survives restarts and rotation. What it does NOT have is any
// epoch, generation, or reset marker: deleting the log and its archives restarts
// seq at 1 with no signal at all.
//
// Every consumer of that log resumes on a `Seq > cursor` filter. So after a
// reset, a holder of a stale higher cursor SILENTLY STARVES — it sees a head
// below its cursor, matches nothing, and reports no error. Worse, once the
// rebuilt log climbs back past the stale cursor, the consumer quietly resumes
// mid-stream on records it never saw the start of, and no `head < cursor` check
// would ever fire again. Nothing in the source detects either case.
//
// This package makes both cases loud, with two independent checks:
//
//  1. SEQ REGRESSION — the observed head is below our watermark. Catches the
//     window right after a reset.
//  2. LINEAGE BREAK — the event still sitting at our watermark seq no longer
//     hashes to what we acknowledged there. Catches the case regression cannot:
//     a rebuilt log that has already grown past the stale cursor. This is the
//     silent-starvation case, and the anchor check is the only thing that sees
//     it.
//
// Either check trips a durable per-city FAULT. A faulted city exports nothing
// and its checkpoint cannot advance, until an operator or the enrolled source
// principal presents a SIGNED reset declaration. Epoch is a control-plane field
// minted by that declaration; it is never an engine column and the producer
// never mints one on its own.
//
// # What must be signed
//
// Two digests ride on every upload and both are required: the source-contract
// digest binds the schema/order/time semantics the events were mapped under, and
// the content-policy digest binds whether free-form content was permitted. They
// are separately signed and separately revocable. Missing, stale, unequal, or
// unknown — any of the four, on either digest — stops export AND stops
// checkpoint advancement. There is no unsigned default.
//
// # What credential rotation is not
//
// Rotation replaces the bearer token and nothing else. It preserves the source
// id, the epoch, the watermark, and the idempotency identity. A reset
// declaration that cites rotation as its reason is rejected outright.
package citysource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/pkg/citytransport"
)

// Errors returned by this package. Every one of them halts export for the city
// it concerns; none of them is recoverable by retrying the same upload.
var (
	// ErrPolicyMissing means a required signed digest was absent.
	ErrPolicyMissing = errors.New("citysource: signed policy digest missing")
	// ErrPolicyStale means a signed policy is past its validity bound.
	ErrPolicyStale = errors.New("citysource: signed policy stale")
	// ErrPolicyMismatch means an offered digest is not the one the enrollment
	// pins.
	ErrPolicyMismatch = errors.New("citysource: signed policy digest mismatch")
	// ErrPolicyUnknown means a policy's signature or signing key did not verify.
	ErrPolicyUnknown = errors.New("citysource: signed policy unknown or unverifiable")

	// ErrSeqRegression means the source head fell below our watermark: the log
	// was reset or truncated.
	ErrSeqRegression = errors.New("citysource: source seq regressed below watermark")
	// ErrLineageBreak means the event at our watermark seq no longer matches
	// what we acknowledged there: the log was rebuilt.
	ErrLineageBreak = errors.New("citysource: source lineage broken at watermark anchor")
	// ErrFaulted means the city is halted pending a signed reset declaration.
	ErrFaulted = errors.New("citysource: city halted pending signed reset declaration")

	// ErrInvalidReset means a reset declaration was malformed, misordered,
	// unsigned, or cited credential rotation as its reason.
	ErrInvalidReset = errors.New("citysource: invalid reset declaration")
	// ErrEpochMismatch means a server acknowledgement referenced a different
	// epoch than the one we uploaded under.
	ErrEpochMismatch = errors.New("citysource: acknowledgement epoch mismatch")
	// ErrUnsolicitedAck means the server acknowledged a seq we did not offer.
	ErrUnsolicitedAck = errors.New("citysource: acknowledgement references an unoffered seq")
)

// Fault codes recorded on a halted city. They are stable strings because they
// are persisted and surfaced to operators.
const (
	FaultNone          = ""
	FaultSeqRegression = "seq_regression"
	FaultLineageBreak  = "lineage_break"
	FaultEpochMismatch = "epoch_mismatch"
)

// Identity is the stable, credential-independent identity of one enrolled City
// source.
//
// SourceID comes from enrollment and is the dedup and ordering key on the
// server. It is deliberately NOT derived from the credential: rotating the
// bearer token must leave it byte-identical, or every rotation would read as a
// new source and re-deliver the whole history. CityHash is the salted partition
// key; the cleartext city name is never carried here or on the wire.
type Identity struct {
	SourceID string
	CityHash string
	// City is the local cleartext name, kept for logs and durable state keys
	// only. It never reaches the wire — Upload carries SourceID, and the path
	// segment carries CityHash.
	City string
}

// Validate checks an Identity is usable.
func (id Identity) Validate() error {
	if strings.TrimSpace(id.SourceID) == "" {
		return errors.New("citysource: SourceID is required (enrollment-minted, rotation-stable)")
	}
	if strings.TrimSpace(id.CityHash) == "" {
		return errors.New("citysource: CityHash is required")
	}
	if strings.TrimSpace(id.City) == "" {
		return errors.New("citysource: City is required for durable state keying")
	}
	return nil
}

// CityState is the durable per-city producer state. It is the whole of what a
// restart needs, and every field in it is load-bearing:
//
//   - Epoch is control-plane-owned and changes only by a signed reset.
//   - Watermark is the highest CONTIGUOUSLY ACKNOWLEDGED seq within Epoch. It is
//     not a high-water mark of what we sent, or of what we read — only of what
//     the server confirmed it holds, with no hole beneath it.
//   - AnchorSeq/AnchorHash pin the identity of the log we acknowledged, so a
//     rebuilt log is detectable even after it grows past Watermark.
//   - Fault, when set, means this city exports nothing until a signed reset
//     clears it.
type CityState struct {
	SourceID    string `json:"source_id"`
	Epoch       uint64 `json:"epoch"`
	Watermark   uint64 `json:"watermark"`
	AnchorSeq   uint64 `json:"anchor_seq"`
	AnchorHash  string `json:"anchor_hash"`
	Fault       string `json:"fault,omitempty"`
	FaultDetail string `json:"fault_detail,omitempty"`
}

// Faulted reports whether the city is halted.
func (s CityState) Faulted() bool { return s.Fault != FaultNone }

// LineageProbe reads the source log back by seq. The producer needs read-back,
// not just a forward stream, because the anchor check is what distinguishes a
// rebuilt log from a quiet one.
type LineageProbe interface {
	// Head returns the highest seq the city's log currently holds. A city with
	// no log yet returns 0.
	Head(city string) (uint64, error)
	// OfferAt returns the mapped offer at seq. ok is false when the log holds
	// no such seq — which, for a seq we previously acknowledged, is itself
	// evidence of a rebuild.
	OfferAt(city string, seq uint64) (citytransport.Offer, bool, error)
}

// SemanticHash is the content-identity of a mapped offer: sha256 over its
// canonical bytes with the hash field itself zeroed.
//
// It is what lets the server distinguish a benign duplicate (same seq, same
// hash -> ACK_DUPLICATE) from a changed duplicate (same seq, different hash ->
// QUARANTINE_CONFLICT), and it is what the producer's own anchor check compares.
// It covers the content fields too: an offer that gained a title under a
// different content policy is not the same record.
func SemanticHash(o citytransport.Offer) string {
	o.SemanticHash = ""
	b, err := json.Marshal(o)
	if err != nil {
		// Offer is a closed struct of strings and uint64s; marshaling it cannot
		// fail. Panicking on the impossible beats returning a hash that silently
		// collides.
		panic(fmt.Sprintf("citysource: marshaling a closed Offer failed: %v", err))
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
