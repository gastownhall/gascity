// Package cityneutral is Gas City's producer adapter for the platform-neutral
// run, session and transcript-record resources of the Beads Team Server v1 API.
//
// City is ONE OPTIONAL producer of those resources. A custom orchestrator with
// no City in the picture uploads the same chain on its own credential and gets
// records of the same shape, so nothing here may become a schema, an identity
// or an authority that a non-City producer would have to satisfy. Every neutral
// Team ID is minted by the server and read back off a response; this package
// never invents one, and City-shaped facts (city, rig, formula) are display
// only, gated, and never load-bearing.
//
// The layering is deliberate:
//
//   - [API] is the client seam. It carries one method per frozen operation this
//     adapter uses and NOTHING else. The adapter speaks no HTTP: no URL, no
//     header, no status code appears below this file's imports.
//   - [Mapper] turns a City chain into neutral request bodies and refuses any
//     mapping that would let a City fact reach a neutral authority field.
//   - [Producer] owns checkpointing: stable source identity for dedup, a
//     contiguous acknowledged frontier, and the refusals (changed replay, gap,
//     post-finalize mutation) that stop this adapter instead of corrupting a
//     neutral record.
//
// Rollback is per producer: stop calling Push and the acknowledged records stay
// exactly as the server accepted them, because this package never rewrites an
// accepted record and never sends a mutation for a finalized session.
package cityneutral

import "errors"

// Frozen operation IDs from operation_matrix_v6. They are spelled here so a
// reader can check the adapter's surface against the frozen matrix without
// leaving the package, and so the client seam cannot quietly grow a route that
// is not in the matrix.
const (
	OpUpsertRun              = "upsertRun"
	OpGetRun                 = "getRun"
	OpUpsertRunSession       = "upsertRunSession"
	OpGetSession             = "getSession"
	OpFinalizeSession        = "finalizeSession"
	OpCreateTranscriptRecord = "createTranscriptRecord"
	OpListTranscriptRecords  = "listTranscriptRecords"
	OpGetSessionTranscript   = "getSessionTranscript"
)

// Resource kinds in the server's idempotency namespace. A key minted for a run
// may not be replayed into a session, so the kind is part of the key preimage.
const (
	KindRun              = "run"
	KindSession          = "session"
	KindTranscriptRecord = "transcript_record"
	// kindSessionFinalize namespaces the finalize key. Finalize has no resource
	// kind of its own server-side, but a finalize key derived from the same
	// preimage as the session upsert would collide with it.
	kindSessionFinalize = "session_finalize"
)

// Bounds mirroring the server's validators, so an oversized field is refused
// here with a source field name attached rather than as a correlated 4xx.
const (
	maxSourceNativeIDLen  = 200
	maxDisplayTitleLength = 400
)

// Refusals. Every one of them stops this producer with its checkpoint intact:
// nothing has been advanced past the last acknowledged record, so an operator
// who fixes the source can restart and resume rather than reconcile.
var (
	// ErrChangedReplay is a record whose stable source identity was already
	// acknowledged but whose payload no longer matches. Sending it would ask
	// the server to rewrite accepted input, so the adapter stops instead.
	ErrChangedReplay = errors.New("cityneutral: source identity replayed with changed payload")
	// ErrGap is a record that would advance the frontier non-contiguously.
	// Uploading it would leave a hole no later read could distinguish from
	// data loss.
	ErrGap = errors.New("cityneutral: non-contiguous record would skip the acknowledged frontier")
	// ErrFinalized is any attempt to add to or mutate a session the server has
	// already finalized. Finalize is one-way (FIN-1) and this adapter is not
	// the component that gets to argue with it.
	ErrFinalized = errors.New("cityneutral: session is finalized; input cannot be rewritten")
	// ErrIdentityDrift is a server response binding a stable source identity to
	// a different neutral Team ID than the checkpoint holds. That is either a
	// wrong-tenant credential or a reset that was never declared; both are
	// louder than a retry.
	ErrIdentityDrift = errors.New("cityneutral: stable source identity resolved to a different neutral id")
	// ErrNeutralAuthority is a mapping that would let a City fact become
	// neutral authority.
	ErrNeutralAuthority = errors.New("cityneutral: City-shaped field cannot become neutral authority")
	// ErrInvalidChain is a City chain this adapter refuses to map at all.
	ErrInvalidChain = errors.New("cityneutral: invalid City chain")
	// ErrCredentialLeak is outbound bytes carrying credential evidence.
	ErrCredentialLeak = errors.New("cityneutral: outbound payload carries credential evidence")
	// ErrContentRoute is raw transcript content on a route not authorized to
	// carry it.
	ErrContentRoute = errors.New("cityneutral: raw content outside its authorized route")
)
