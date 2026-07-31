// Package cityartifact is Gas City's producer adapter for the Artifact
// resources of the Beads Team Server v1 API.
//
// City is ONE OPTIONAL producer of artifacts. A custom producer with no City in
// the picture creates, uploads and finalizes an artifact on its own credential
// and gets a record of exactly the same public shape, so nothing here may
// become a schema, an identity or an authority that a non-City producer would
// have to satisfy. Artifact IDs are minted by the server and read back off a
// response; this package never invents one, and City-shaped facts (city, rig,
// formula) are display-only breadcrumbs the adapter refuses to place anywhere
// in an outbound body.
//
// The layering matches the neutral run/session producer:
//
//   - [API] is the client seam. It carries one method per frozen artifact
//     operation and NOTHING else. The adapter speaks no HTTP: no URL, no
//     header, no status code appears anywhere below this file's imports.
//   - [Mapper] turns a City artifact into request bodies and refuses any
//     mapping that would let a City fact, a foreign link or a raw upstream
//     field reach the server.
//   - [Producer] owns the multi-part lifecycle: create, parts, finalize, with
//     stable source idempotency, a contiguous acknowledged part frontier, and
//     the refusals (changed manifest, gap, post-finalize write) that stop this
//     adapter instead of corrupting an artifact.
//
// The four public categories — metadata, evidence, references, content — stay
// separate here for the same reason they are separate server-side: they are
// four reads behind four scopes, and this package never routes one through
// another. Metadata calls carry no bytes and content calls carry no metadata.
//
// Rollback is per producer: stop calling Push and every acknowledged artifact
// stays exactly as the server accepted it. This package never rewrites an
// accepted part and never sends a mutation for a finalized artifact, and its
// failures are its own — an artifact refusal cannot reach into the run,
// session, transcript or event adapters, which hold their own checkpoints.
package cityartifact

import "errors"

// Frozen operation IDs from operation_matrix_v6. They are spelled here so a
// reader can check this adapter's surface against the frozen matrix without
// leaving the package, and so the client seam cannot quietly grow a route that
// is not in the matrix.
const (
	OpCreateArtifact        = "createArtifact"
	OpListArtifacts         = "listArtifacts"
	OpGetArtifact           = "getArtifact"
	OpGetArtifactEvidence   = "getArtifactEvidence"
	OpGetArtifactReferences = "getArtifactReferences"
	OpUploadArtifactContent = "uploadArtifactContent"
	OpFinalizeArtifact      = "finalizeArtifact"
	OpGetArtifactContent    = "getArtifactContent"
)

// Category is the public category an operation reads. The four are pairwise
// disjoint server-side; naming them here keeps the adapter honest about which
// one each call touches.
type Category string

// The four categories. There is no fifth and no "all".
const (
	CategoryMetadata   Category = "metadata"
	CategoryEvidence   Category = "evidence"
	CategoryReferences Category = "references"
	CategoryContent    Category = "content"
)

// CategoryOf reports the category of a frozen artifact operation. An unknown
// operation has no category rather than a default one.
func CategoryOf(operationID string) (Category, bool) {
	switch operationID {
	case OpCreateArtifact, OpListArtifacts, OpGetArtifact, OpFinalizeArtifact:
		return CategoryMetadata, true
	case OpGetArtifactEvidence:
		return CategoryEvidence, true
	case OpGetArtifactReferences:
		return CategoryReferences, true
	case OpUploadArtifactContent, OpGetArtifactContent:
		return CategoryContent, true
	}
	return "", false
}

// Resource kinds in the derived idempotency preimage. A key minted for a create
// may not be replayed into a part upload, so the kind is part of the preimage.
const (
	kindArtifact = "artifact"
	kindPart     = "artifact_part"
	kindFinalize = "artifact_finalize"
)

// Bounds mirroring the server's validators, so an oversized field is refused
// here with a City field name attached rather than as a correlated 4xx.
const (
	maxSourceNativeIDLen = 200
	maxLinkIDLen         = 200
	maxPartBytes         = 8 << 20
)

// Refusals. Every one of them stops this producer with its checkpoint intact:
// nothing has advanced past the last acknowledged part, so an operator who
// fixes the source can restart and resume rather than reconcile.
var (
	// ErrChangedManifest is an artifact whose already-acknowledged parts no
	// longer hash to what was accepted. Sending it would ask the server to
	// rewrite accepted content, so the adapter stops instead.
	ErrChangedManifest = errors.New("cityartifact: manifest changed under an acknowledged part")
	// ErrGap is a part that would advance the frontier non-contiguously.
	// Uploading it would leave a hole in the content no later read could tell
	// apart from data loss.
	ErrGap = errors.New("cityartifact: non-contiguous part would skip the acknowledged frontier")
	// ErrFinalized is any attempt to add to or rewrite an artifact the server
	// has already finalized. Finalize is one-way and this adapter is not the
	// component that gets to argue with it.
	ErrFinalized = errors.New("cityartifact: artifact is finalized; content cannot be rewritten")
	// ErrIdentityDrift is a server response binding a stable City artifact
	// identity to a different server-minted Artifact ID than the checkpoint
	// holds, or an acknowledgement against the wrong artifact. Both are louder
	// than a retry.
	ErrIdentityDrift = errors.New("cityartifact: stable source identity resolved to a different artifact id")
	// ErrCityAuthority is a mapping that would let a City-shaped fact become
	// artifact authority: a City-minted artifact ID, a rig or formula name in a
	// link slot, or a producer/source field set from the body.
	ErrCityAuthority = errors.New("cityartifact: City-shaped field cannot become artifact authority")
	// ErrInvalidArtifact is a City artifact this adapter refuses to map at all.
	ErrInvalidArtifact = errors.New("cityartifact: invalid City artifact")
	// ErrCredentialLeak is an outbound payload carrying credential evidence or
	// a signed URL.
	ErrCredentialLeak = errors.New("cityartifact: outbound payload carries credential evidence")
	// ErrContentRoute is content bytes on a route not authorized to carry them.
	// Metadata bodies never carry bytes; only the content route does.
	ErrContentRoute = errors.New("cityartifact: content bytes outside the content route")
	// ErrContentDenied is the server refusing content: a policy denial, a size
	// refusal or a quarantine. It is terminal for this artifact and inert for
	// every other adapter.
	ErrContentDenied = errors.New("cityartifact: server denied artifact content")
	// ErrUpstream is any other server failure, already stripped of its body.
	// The upstream text never travels with it.
	ErrUpstream = errors.New("cityartifact: upstream call failed")
	// ErrNotReadable is an artifact that is not readable: absent, another
	// tenant's, another producer's, or not yet finalized. The server answers
	// all four the same way and so does this package.
	ErrNotReadable = errors.New("cityartifact: artifact is not readable")
)
