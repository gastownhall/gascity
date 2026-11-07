package session

import (
	"strconv"
	"strings"
)

// The stop request's row keys (CONTRACT v5 D1, D5, R1): new keys legacy
// ignores. The controller half: the v2 drain begin, signal, cancel and
// finalize write it. The request half: `gc runtime drain-ack` writes it by
// CAS in a v2 city only, DrainAckIncarnationKey holding the row's generation
// as the CLI read it and DrainAckAtKey the RFC 3339 time of the ack; v2's
// PreWake clears both halves, and so does Manager.ensureRunning's confirm of
// a runtime it started (restartStopRequestClears). cmd/gc's activeStop is
// the reconciler's reader, and cmd/gc's v2SignaledUnacked, through the same
// accessor, legacy's rollback guard's. ReadStopRequest is the content
// gate's (V2-18): it reads the controller half's reason and incarnation,
// writes nothing, and reads every request as residue under a controller that
// latched legacy. Only the files TestStopRequestKeysAreNewKeys
// admits call it. ClearStopRequestPatch is the clear that cmd/gc's residue
// void writes. The operator's drain (OperatorDrainKey) is a third half:
// `gc runtime drain` writes the row's generation there by CAS in a v2 city,
// legacy's GC_DRAIN on the row; it explains an agent's ack (A11) and nothing
// else reads it; `gc runtime undrain`, PreWake, the stop's finalize and
// Manager.ensureRunning's confirm clear it. No production file but this one,
// fields.go (the field registry), cmd/gc/reconcile_stop_request.go and
// cmd/gc/cmd_runtime_drain.go names them (TestStopRequestKeysAreNewKeys).
const (
	DrainIntentReasonKey      = "drain_intent_reason"
	DrainIntentAtKey          = "drain_intent_at"
	DrainIntentIncarnationKey = "drain_intent_incarnation"
	DrainAckIncarnationKey    = "drain_ack_incarnation"
	DrainAckAtKey             = "drain_ack_at"
	OperatorDrainKey          = "operator_drain"
)

// stopRequestPremiseKeys are the stop request's keys a premise compares
// (FactsBaseline): the controller half and the ack's incarnation.
var stopRequestPremiseKeys = []string{DrainIntentReasonKey, DrainIntentAtKey, DrainIntentIncarnationKey, DrainAckIncarnationKey}

// restartStopRequestClears is ClearStopRequestPatch narrowed to the keys
// meta holds: what a runtime start outside PreWake (Manager.ensureRunning's,
// which keeps the generation) clears in its confirming write, as PreWake
// clears both halves, so the restarted runtime is never stopped for its
// predecessor's request (#7635 review ruling), nor kept on the operator's
// drain of its predecessor (OperatorDrainKey: legacy's GC_DRAIN does not
// survive a new runtime either). Empty when meta holds none.
func restartStopRequestClears(meta map[string]string) MetadataPatch {
	out := MetadataPatch{}
	for k := range ClearStopRequestPatch() {
		if meta[k] != "" {
			out[k] = ""
		}
	}
	if meta[OperatorDrainKey] != "" {
		out[OperatorDrainKey] = ""
	}
	return out
}

// ClearStopRequestPatch clears both halves of a row's stop request.
func ClearStopRequestPatch() MetadataPatch {
	return MetadataPatch{
		DrainIntentReasonKey:      "",
		DrainIntentAtKey:          "",
		DrainIntentIncarnationKey: "",
		DrainAckIncarnationKey:    "",
		DrainAckAtKey:             "",
	}
}

// StopRequestReading is how a reader outside the v2 reconciler takes a row's
// stop request (V2-18). The keys are new keys legacy ignores (CONTRACT v5
// R1): a controller that latched legacy never carries a request out, so a
// request a rolled-back v2 run left on a row is residue to it. Only the
// controller latches the session reconciler, so every reader takes the
// reading of the controller's latched mode: its lane host from its own
// latch, and a reader outside it (the CLI's inline pass, the prompt hook)
// by asking the running controller, never from the on-disk config.
type StopRequestReading uint8

const (
	// StopRequestsLive takes the controller half as v2's live request: the
	// reading under a controller that latched v2, and the fail-safe reading
	// of a reader outside the controller that cannot learn the latched mode
	// (no controller answers, or its answer names no known mode): an unknown
	// mode honors the keys. It is the zero value, and ReadStopRequest takes
	// any reading but StopRequestsIgnored as it: a reader that names none
	// never starts content into a session v2 may be stopping. A reader
	// outside the controller only defers that content to the controller's
	// lane host, which reads by its latched mode.
	StopRequestsLive StopRequestReading = iota
	// StopRequestsIgnored takes it as residue: the reading under a
	// controller that latched legacy, which ignores the keys, so a request a
	// rolled-back v2 run left never holds content to a session legacy keeps
	// running, from the controller's lane host or from a reader outside it.
	StopRequestsIgnored
)

// StopRequest is a row's stop request as ReadStopRequest read it: whether
// the v2 controller has asked to stop the row's current incarnation. The
// zero value requests nothing; only ReadStopRequest builds one that does.
type StopRequest struct {
	requested bool
}

// Requested reports whether the v2 controller's stop request stands at the
// row's own generation.
func (r StopRequest) Requested() bool { return r.requested }

// ReadStopRequest is the stop request's reader outside the v2 reconciler
// (V2-18), for ContentGate alone. meta is a session row's persisted metadata;
// it requests a stop when its drain_intent_reason is set and its
// drain_intent_incarnation names the generation meta records. That is
// cmd/gc's activeStop rule for the controller half, read the same way: the
// two keys and the generation trimmed, and the generation parsed as the
// census parses it (strconv.ParseInt's value: 0 for an empty or malformed
// one). It reads neither drain_intent_at (v2 signals a request without one
// at once), nor the request half, nor the operator's drain, and applies none
// of activeStop's other rules: a row that claims no runtime takes no live
// content and v2 voids its residue, and a signaled row reads draining as
// well. Under StopRequestsIgnored it requests nothing. It writes nothing.
func ReadStopRequest(meta map[string]string, reading StopRequestReading) StopRequest {
	if reading == StopRequestsIgnored {
		return StopRequest{}
	}
	gen, _ := strconv.ParseInt(strings.TrimSpace(meta["generation"]), 10, 64)
	reason := strings.TrimSpace(meta[DrainIntentReasonKey])
	incarnation := strings.TrimSpace(meta[DrainIntentIncarnationKey])
	return StopRequest{requested: reason != "" && incarnation == strconv.FormatInt(gen, 10)}
}
