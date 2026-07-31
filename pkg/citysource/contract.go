package citysource

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PolicyKind distinguishes the two independently signed policies. They are
// separate artifacts because they are separately revocable: a content-policy
// rotation must not silently re-authorize a stale source contract, and vice
// versa.
type PolicyKind string

// The two independently signed policy kinds.
const (
	PolicySource  PolicyKind = "source_contract"
	PolicyContent PolicyKind = "content_policy"
)

// Reset reasons. The set is closed; anything else is an invalid declaration.
const (
	ReasonSourceRebuild = "source_rebuild"
	ReasonDataLoss      = "data_loss"
	ReasonMigration     = "migration"
)

// MaxResetFutureSkew bounds how far ahead of now a reset declaration may set
// its epoch start.
const MaxResetFutureSkew = 5 * time.Minute

// Reset issuers.
const (
	IssuerSourcePrincipal    = "enrolled_source_principal"
	IssuerOperatorBreakglass = "operator_breakglass"
)

// SignedPolicy is one signed policy row as the producer holds it.
//
// KeyID is INTERNAL EVIDENCE. It selects the verification key and is recorded
// in local logs; it appears in no wire DTO, which is why citytransport.Upload
// has no field for it. Signature covers the canonical bytes of everything else.
type SignedPolicy struct {
	Kind   PolicyKind `json:"kind"`
	Digest string     `json:"digest"`
	// ContentPermitted is meaningful only on a content policy. It is the ONLY
	// thing that may turn on free-form title/formula egress; the default with no
	// signed content policy is off, and there is no unsigned path to on.
	ContentPermitted bool      `json:"content_permitted"`
	Issuer           string    `json:"issuer"`
	NotBefore        time.Time `json:"not_before"`
	NotAfter         time.Time `json:"not_after"`
	KeyID            string    `json:"key_id"`
	Signature        []byte    `json:"signature"`
}

// signingBytes is the canonical preimage: every field except the signature.
func (p SignedPolicy) signingBytes() ([]byte, error) {
	unsigned := p
	unsigned.Signature = nil
	return canonical(unsigned)
}

// Verify checks the policy's signature against a trusted key set and its
// validity window against now.
//
// The failure classes are distinct on purpose — an operator needs to tell a
// clock problem from a revoked key from a forged row — but every one of them is
// equally fatal to export.
func (p SignedPolicy) Verify(now time.Time, trusted map[string]ed25519.PublicKey) error {
	if strings.TrimSpace(p.Digest) == "" {
		return fmt.Errorf("%w: %s has no digest", ErrPolicyMissing, p.Kind)
	}
	if len(p.Signature) == 0 {
		return fmt.Errorf("%w: %s is unsigned", ErrPolicyMissing, p.Kind)
	}
	if p.Issuer == "" {
		return fmt.Errorf("%w: %s has no issuer", ErrPolicyMissing, p.Kind)
	}
	key, ok := trusted[p.KeyID]
	if !ok {
		return fmt.Errorf("%w: %s signed by unknown key_id %q", ErrPolicyUnknown, p.Kind, p.KeyID)
	}
	pre, err := p.signingBytes()
	if err != nil {
		return fmt.Errorf("%w: %s canonicalization: %w", ErrPolicyUnknown, p.Kind, err)
	}
	if !ed25519.Verify(key, pre, p.Signature) {
		return fmt.Errorf("%w: %s signature does not verify under key_id %q", ErrPolicyUnknown, p.Kind, p.KeyID)
	}
	// Validity is checked AFTER the signature so an attacker cannot learn
	// anything from a timing difference on an unverified row, and so a stale
	// but genuine policy reports staleness rather than forgery.
	if !p.NotBefore.IsZero() && now.Before(p.NotBefore) {
		return fmt.Errorf("%w: %s is not valid until %s", ErrPolicyStale, p.Kind, p.NotBefore.UTC().Format(time.RFC3339))
	}
	if p.NotAfter.IsZero() {
		return fmt.Errorf("%w: %s has no expiry; an unbounded policy cannot be current", ErrPolicyMissing, p.Kind)
	}
	if now.After(p.NotAfter) {
		return fmt.Errorf("%w: %s expired at %s", ErrPolicyStale, p.Kind, p.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

// Enrollment pins what this producer is allowed to export under: its stable
// identity, the trusted signing keys, and the two digests the server currently
// expects. The pinned digests are what make "unequal" detectable at the
// producer rather than only at the server.
type Enrollment struct {
	Identity Identity
	// PinnedSourceDigest and PinnedContentDigest are the digests the server
	// published as current. A held policy whose digest differs is unequal —
	// export stops rather than shipping under a superseded contract.
	PinnedSourceDigest  string
	PinnedContentDigest string
	TrustedKeys         map[string]ed25519.PublicKey
}

// PolicySet is the pair of signed policies a producer must hold to export.
type PolicySet struct {
	Source  SignedPolicy
	Content SignedPolicy
}

// Current verifies both policies against the enrollment and returns the pair
// that may stamp an upload.
//
// S-006: BOTH digests are required and recorded. Missing, stale, unequal, or
// unknown — on EITHER policy — stops export and, because the caller never
// reaches an upload, stops checkpoint advancement with it. There is no partial
// success: a valid source contract with a stale content policy exports nothing,
// not "everything except content".
func (e Enrollment) Current(now time.Time, set PolicySet) (PolicySet, error) {
	if err := e.Identity.Validate(); err != nil {
		return PolicySet{}, err
	}
	if set.Source.Kind != PolicySource {
		return PolicySet{}, fmt.Errorf("%w: source slot holds a %q policy", ErrPolicyMissing, set.Source.Kind)
	}
	if set.Content.Kind != PolicyContent {
		return PolicySet{}, fmt.Errorf("%w: content slot holds a %q policy", ErrPolicyMissing, set.Content.Kind)
	}
	if err := set.Source.Verify(now, e.TrustedKeys); err != nil {
		return PolicySet{}, err
	}
	if err := set.Content.Verify(now, e.TrustedKeys); err != nil {
		return PolicySet{}, err
	}
	if e.PinnedSourceDigest == "" || e.PinnedContentDigest == "" {
		return PolicySet{}, fmt.Errorf("%w: enrollment pins no digest pair", ErrPolicyMissing)
	}
	if set.Source.Digest != e.PinnedSourceDigest {
		return PolicySet{}, fmt.Errorf("%w: source contract %s, enrollment pins %s",
			ErrPolicyMismatch, set.Source.Digest, e.PinnedSourceDigest)
	}
	if set.Content.Digest != e.PinnedContentDigest {
		return PolicySet{}, fmt.Errorf("%w: content policy %s, enrollment pins %s",
			ErrPolicyMismatch, set.Content.Digest, e.PinnedContentDigest)
	}
	return set, nil
}

// ResetDeclaration is the signed artifact that moves a city to a new epoch. It
// is the ONLY thing that clears a fault, and the only thing that changes an
// epoch.
type ResetDeclaration struct {
	SourceID        string    `json:"source_id"`
	OldEpoch        uint64    `json:"old_epoch"`
	NewEpoch        uint64    `json:"new_epoch"`
	Reason          string    `json:"reason"`
	DeclaredStartTS time.Time `json:"declared_start_ts"`
	Issuer          string    `json:"issuer"`
	ContractDigest  string    `json:"contract_digest"`
	KeyID           string    `json:"key_id"`
	Signature       []byte    `json:"signature"`
}

func (d ResetDeclaration) signingBytes() ([]byte, error) {
	unsigned := d
	unsigned.Signature = nil
	return canonical(unsigned)
}

// SignReset produces the signature for a declaration. It exists so the fixture
// and drill tests mint declarations exactly the way a real issuer would, rather
// than through a test-only shortcut that could drift from the verifier.
func SignReset(d ResetDeclaration, keyID string, priv ed25519.PrivateKey) (ResetDeclaration, error) {
	d.KeyID = keyID
	pre, err := d.signingBytes()
	if err != nil {
		return ResetDeclaration{}, err
	}
	d.Signature = ed25519.Sign(priv, pre)
	return d, nil
}

// SignPolicy produces the signature for a policy row, for the same reason.
func SignPolicy(p SignedPolicy, keyID string, priv ed25519.PrivateKey) (SignedPolicy, error) {
	p.KeyID = keyID
	pre, err := p.signingBytes()
	if err != nil {
		return SignedPolicy{}, err
	}
	p.Signature = ed25519.Sign(priv, pre)
	return p, nil
}

// Verify checks a reset declaration against the enrollment and the state it
// claims to supersede.
//
// The rotation rule is categorical, not advisory: a declaration whose reason
// mentions credential rotation is invalid no matter who signed it. Rotation
// changes a credential and nothing else — it never constitutes, authorizes, or
// requires a reset. Letting rotation mint an epoch would hand anyone who can
// rotate a token the power to zero a watermark and re-deliver history.
func (d ResetDeclaration) Verify(now time.Time, e Enrollment, st CityState) error {
	if d.SourceID != e.Identity.SourceID {
		return fmt.Errorf("%w: declaration names source %q, enrollment is %q",
			ErrInvalidReset, d.SourceID, e.Identity.SourceID)
	}
	switch d.Reason {
	case ReasonSourceRebuild, ReasonDataLoss, ReasonMigration:
	default:
		// Anything outside the closed set — including every phrasing of
		// "rotation" — lands here.
		return fmt.Errorf("%w: reason %q is not one of %s/%s/%s (credential rotation is never a reset reason)",
			ErrInvalidReset, d.Reason, ReasonSourceRebuild, ReasonDataLoss, ReasonMigration)
	}
	switch d.Issuer {
	case IssuerSourcePrincipal, IssuerOperatorBreakglass:
	default:
		return fmt.Errorf("%w: issuer %q is not an enrolled source principal or operator breakglass",
			ErrInvalidReset, d.Issuer)
	}
	if d.OldEpoch != st.Epoch {
		return fmt.Errorf("%w: declaration supersedes epoch %d, city is at epoch %d",
			ErrInvalidReset, d.OldEpoch, st.Epoch)
	}
	if d.NewEpoch != st.Epoch+1 {
		return fmt.Errorf("%w: new epoch must be old+1 (%d), got %d",
			ErrInvalidReset, st.Epoch+1, d.NewEpoch)
	}
	if d.ContractDigest == "" || d.ContractDigest != e.PinnedSourceDigest {
		return fmt.Errorf("%w: declaration cites contract %q, enrollment pins %q",
			ErrInvalidReset, d.ContractDigest, e.PinnedSourceDigest)
	}
	if d.DeclaredStartTS.IsZero() {
		return fmt.Errorf("%w: declared_start_ts is required (it is the new epoch's time floor)", ErrInvalidReset)
	}
	// The declared start is the new epoch's time floor, so a far-future value
	// would silently quarantine every record the rebuilt log produces until the
	// clock caught up — a reset that looks applied but exports nothing. Bound it.
	if skew := d.DeclaredStartTS.Sub(now); skew > MaxResetFutureSkew {
		return fmt.Errorf("%w: declared_start_ts is %s in the future (max %s)",
			ErrInvalidReset, skew.Truncate(time.Second), MaxResetFutureSkew)
	}
	key, ok := e.TrustedKeys[d.KeyID]
	if !ok {
		return fmt.Errorf("%w: signed by unknown key_id %q", ErrInvalidReset, d.KeyID)
	}
	pre, err := d.signingBytes()
	if err != nil {
		return fmt.Errorf("%w: canonicalization: %w", ErrInvalidReset, err)
	}
	if !ed25519.Verify(key, pre, d.Signature) {
		return fmt.Errorf("%w: signature does not verify under key_id %q", ErrInvalidReset, d.KeyID)
	}
	return nil
}

// canonical renders a value as compact JSON with no HTML escaping, so a
// signature preimage is reproducible byte-for-byte by any verifier.
func canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
