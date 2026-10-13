package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// The stop request (CONTRACT v5 D1): one durable stop intent per row
// incarnation, with one reader. Its controller half is the three
// drain_intent_* keys, its request half E3's two drain_ack_* keys, and its
// signaled phase legacy's stop-pending projection. activeStop is the only
// reader of the keys, and the patch builders below are D1's only writers of
// them (I6, I14; TestStopRequestLintBansDirectKeyReads). Both halves name
// the row's generation only, so a rekey, which moves only the token, keeps
// them. An empty or unparseable generation is generation 0, as legacy's
// drain tracker reads it (strconv.Atoi).

// rawStopKeys are a row's raw stop-request keys, as the census read them.
type rawStopKeys struct {
	rawIntentReason, rawIntentAt, rawIntentIncarnation string
	rawAckIncarnation, rawAckAt                        string
	rawOperatorDrain                                   string
}

// readStopKeys is the census's projection of a row's metadata.
func readStopKeys(meta map[string]string) rawStopKeys {
	return rawStopKeys{
		rawIntentReason:      strings.TrimSpace(meta[session.DrainIntentReasonKey]),
		rawIntentAt:          strings.TrimSpace(meta[session.DrainIntentAtKey]),
		rawIntentIncarnation: strings.TrimSpace(meta[session.DrainIntentIncarnationKey]),
		rawAckIncarnation:    strings.TrimSpace(meta[session.DrainAckIncarnationKey]),
		rawAckAt:             strings.TrimSpace(meta[session.DrainAckAtKey]),
		rawOperatorDrain:     strings.TrimSpace(meta[session.OperatorDrainKey]),
	}
}

// stopPhase is how far a stop request has come.
type stopPhase uint8

const (
	stopRequested stopPhase = iota + 1 // drain begun, not yet signaled
	stopAcked                          // the agent or an operator acked: the signal is due (A11)
	stopSignaled                       // the stop-pending projection: the stop verb's (A4)
)

// stopRequest is a row's active stop request.
type stopRequest struct {
	Phase  stopPhase
	Reason string    // the drain reason; empty for a bare ack or a bare legacy stop-pending row
	At     time.Time // when the drain began; zero when unparseable or absent
	AckAt  time.Time // when an acked request's ack landed; zero when unparseable
	// Synthesized marks a bare legacy stop-pending row (rule 3).
	Synthesized bool
	// Operator marks an acked request that an operator's `gc runtime drain`
	// of the row's generation explains (session.OperatorDrainKey, legacy's
	// GC_DRAIN): A11 reads it, and nothing else does.
	Operator bool
	// Residue marks, on no request, a half of the row's generation that
	// rule 2 ended. The void clears both halves, so a runtime restarted
	// later at the same generation (Manager.ensureRunning bumps neither the
	// generation nor the token) is not stopped for its predecessor's request.
	Residue bool
}

// activeStop is a row's active stop request, by D1's rules in order:
//  1. a half whose incarnation is not the row's generation is no request;
//  2. a row that no longer claims a runtime and is not stop-pending has
//     none: an operator's suspend or kill ends the request;
//  3. a stop-pending row is signaled, synthesized when it carries no
//     current controller half;
//  4. an ack of the row's generation makes the signal due.
//
// Otherwise a current controller half is a request. A11 reads acks only
// through rule 4 (D5).
func activeStop(row censusRow) (stopRequest, bool) {
	k, gen := row.StopKeys, strconv.FormatInt(row.Incarnation, 10)
	intent := k.rawIntentReason != "" && k.rawIntentIncarnation == gen
	acked := k.rawAckIncarnation == gen
	operator := k.rawOperatorDrain == gen
	pending := isDrainAckStopPendingInfo(row.Info)
	if !pending && !sessionBeadClaimsLiveRuntime(row.Info) {
		return stopRequest{Residue: intent || acked}, false
	}
	var req stopRequest
	if intent {
		req.Reason = k.rawIntentReason
		req.At, _ = parseRFC3339Metadata(k.rawIntentAt)
	}
	switch {
	case pending:
		req.Phase, req.Synthesized = stopSignaled, !intent
	case acked:
		req.Phase = stopAcked
		req.AckAt, _ = parseRFC3339Metadata(k.rawAckAt)
		req.Operator = operator
	case intent:
		req.Phase = stopRequested
	default:
		return stopRequest{}, false
	}
	return req, true
}

// stopBeginPatch is none → requested (drain begin): the controller half at
// the row's generation. It never writes state (no BeginDrainPatch: legacy
// skips state=draining without stop-pending as an unknown state).
func stopBeginPatch(row censusRow, reason string, now time.Time) session.MetadataPatch {
	return session.MetadataPatch{
		session.DrainIntentReasonKey:      reason,
		session.DrainIntentAtKey:          now.UTC().Format(time.RFC3339),
		session.DrainIntentIncarnationKey: strconv.FormatInt(row.Incarnation, 10),
	}
}

// stopCancelPatch is requested → none (a lens, or lost authorization): the
// controller half cleared.
func stopCancelPatch() session.MetadataPatch {
	return session.MetadataPatch{session.DrainIntentReasonKey: "", session.DrainIntentAtKey: "", session.DrainIntentIncarnationKey: ""}
}

// cancelsStop reports a patch that writes the controller half's cancel
// (stopCancelPatch).
func cancelsStop(patch session.MetadataPatch) bool {
	reason, ok := patch[session.DrainIntentReasonKey]
	return ok && reason == ""
}

// stopAckVoidPatch clears the request half: A11's void of an ack older
// than the runtime it would stop.
func stopAckVoidPatch() session.MetadataPatch {
	return session.MetadataPatch{session.DrainAckIncarnationKey: "", session.DrainAckAtKey: ""}
}

// stopEndPatch is stopVoidResiduePatch and the operator's drain mark: what a
// row's PreWake and its stop's finalize clear, so neither the runtime they
// start nor the one they stopped keeps the operator's drain.
func stopEndPatch() session.MetadataPatch {
	p := stopVoidResiduePatch()
	p[session.OperatorDrainKey] = ""
	return p
}

// stopVoidResiduePatch clears both halves of a request rule 2 ended.
func stopVoidResiduePatch() session.MetadataPatch {
	return session.ClearStopRequestPatch()
}

// operatorDrainPatch is `gc runtime drain`'s mark on a v2 row: the row's
// generation as read, in its own key (session.OperatorDrainKey), so no
// controller request's begin, signal, cancel or close reads it. A mark of
// the row's generation already there returns nil.
func operatorDrainPatch(meta map[string]string) session.MetadataPatch {
	gen := strconv.FormatInt(rowGeneration(meta), 10)
	if readStopKeys(meta).rawOperatorDrain == gen {
		return nil
	}
	return session.MetadataPatch{session.OperatorDrainKey: gen}
}

// operatorUndrainPatch is `gc runtime undrain`'s clear of the operator's
// mark, or nil when the row carries none.
func operatorUndrainPatch(meta map[string]string) session.MetadataPatch {
	if readStopKeys(meta).rawOperatorDrain == "" {
		return nil
	}
	return session.MetadataPatch{session.OperatorDrainKey: ""}
}

// rowGeneration is a row's generation as the drain tracker reads it: an
// empty or unparseable one is 0.
func rowGeneration(meta map[string]string) int64 {
	gen, _ := strconv.ParseInt(strings.TrimSpace(meta["generation"]), 10, 64)
	return gen
}
