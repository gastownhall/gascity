// Package slingassert verifies the internal assertions Beads Team Server mints
// for a private Gas City sling command.
//
// It is the City-side half of a deliberate trust-boundary split. The public
// sling gateway (BTS operation slingCityWork, POST /api/v1/city/{city}/sling)
// terminates the caller's public credential — a human Bearer, a machine Bearer,
// or a same-origin browser session — resolves the tenant, and mints a
// short-lived assertion naming exactly one command. Gas City never sees the
// public credential and never trusts a caller-supplied tenant field: it accepts
// an assertion, and only an assertion, over a mutually authenticated transport.
//
// Like [citywriteauth] this package is verify-only. It ships no minter; BTS
// signs with an ed25519 private key and Gas City verifies against the
// corresponding public key(s). The two packages are siblings, not layers: the
// city-write grant authorizes an operator's config mutation, while an assertion
// here carries a resolved tenant across a service boundary. Their claim sets and
// audiences are disjoint on purpose so a token minted for one can never verify
// as the other. The single-use machinery is genuinely shared —
// [citywriteauth.ReplayGuard] and [citywriteauth.MemoryReplayGuard] are reused
// rather than reimplemented.
//
// # Wire format
//
// An assertion token is two base64url (no padding) segments joined by ".":
//
//	base64url(payload) "." base64url(signature)
//
// payload is the UTF-8 JSON encoding of an [Assertion]. signature is the
// ed25519 signature over the exact payload bytes (before base64url encoding);
// the minter MUST sign over the same bytes it transmits. This is byte-for-byte
// the citywriteauth token grammar, so both sides reuse one parser shape.
//
// # Exact match, no defaults
//
// Every claim is required and every claim is checked. There is no optional
// claim, no legacy audience, and no "absent means unconstrained" case: an
// assertion missing any claim is rejected before any equality check, so a
// stripped-claim token can never verify vacuously. Five claims are matched
// against request-derived facts the caller cannot forge ([Expect]): the mTLS
// workload identity, the HTTP method, the command, the body hash, and the city.
// The rest — org, workspace, principal, source, project, issue — are
// authoritative *outputs*: they are what the City dispatches on, and they have
// no caller-supplied counterpart to compare against, by design. A request that
// carries its own tenant fields is refused at the HTTP boundary, not
// reconciled here.
//
// # Registry
//
// The command claim is resolved through a registry supplied at construction
// ([Options.Commands]). An unregistered command is rejected outright, and a
// registered one must carry that command's exact policy and operation ids. The
// registry is the runtime rollback gate: withdrawing the sling command closes
// the private sling path and nothing else, and because dispatch is only ever
// reached through a registry hit there is no unregistered broker or service
// fallback to fall back to.
package slingassert

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// Audience is the assertion audience. It is disjoint from every citywriteauth
// audience so a city-write grant can never verify here and vice versa; the
// ".v1" suffix is this claim set's generation, bumped in lockstep with any
// claim addition so an older verifier cannot silently ignore a new claim.
const Audience = "gc-city-sling.v1"

// CommandSlingCityWork is the only command the City sling boundary accepts. It
// is the operationId of the frozen public gateway operation (POST
// /api/v1/city/{city}/sling), so the private command name and the public
// operation it descends from cannot drift apart.
const CommandSlingCityWork = "slingCityWork"

// PolicySlingCityWork is the public scope the gateway enforced before minting.
// The City re-checks it rather than trusting that the gateway did: a mint bug
// that stamped a different policy fails closed here.
const PolicySlingCityWork = "beads:city.sling"

// Assertion is the claim set BTS mints for exactly one private City command.
//
// Every field is required. Kid/Aud/Nonce/IAT/Exp are the token envelope;
// Workload/Method/Command/BodyHash bind it to one request over one transport;
// PolicyID/OperationID name the authorization it descends from; and
// Org/Workspace/Principal/Source/City/Project/Issue are the resolved tenant and
// target the City dispatches on.
type Assertion struct {
	Kid   string `json:"kid"`
	Aud   string `json:"aud"`
	Nonce string `json:"nonce"`
	IAT   int64  `json:"iat"`
	Exp   int64  `json:"exp"`

	// Workload is the mTLS identity the gateway will present on the connection
	// carrying this assertion (a SPIFFE URI SAN). Binding it means a captured
	// assertion is useless to any workload but the one it was minted for, even
	// one holding a valid client certificate for a different service.
	Workload string `json:"workload"`
	// Method and Command are the HTTP method and the private command name.
	Method  string `json:"method"`
	Command string `json:"command"`
	// BodyHash is lowercase hex sha256 of the exact request body bytes.
	BodyHash string `json:"body_hash"`

	// PolicyID and OperationID name the public authorization this assertion
	// descends from: the scope the gateway enforced and the frozen operation id.
	PolicyID    string `json:"policy_id"`
	OperationID string `json:"operation_id"`

	// Resolved tenant. The City treats these as authoritative and refuses any
	// request that also supplies them.
	Org       string `json:"org"`
	Workspace string `json:"workspace"`
	Principal string `json:"principal"`
	Source    string `json:"source"`

	// Resolved target within the tenant.
	City    string `json:"city"`
	Project string `json:"project"`
	Issue   string `json:"issue"`
}

// Sentinel errors. Callers distinguish failures with errors.Is; every one is a
// rejection (fail-closed), never a pass-through. Callers MUST NOT surface which
// one fired to the requester — a distinguishable rejection is a tenant oracle.
var (
	ErrMalformed          = errors.New("slingassert: malformed assertion")
	ErrUnknownKey         = errors.New("slingassert: unknown kid")
	ErrBadSignature       = errors.New("slingassert: signature verification failed")
	ErrAudience           = errors.New("slingassert: audience mismatch")
	ErrExpired            = errors.New("slingassert: assertion expired")
	ErrNotYetValid        = errors.New("slingassert: assertion not yet valid")
	ErrBadWindow          = errors.New("slingassert: assertion validity window is non-positive")
	ErrTTLTooLong         = errors.New("slingassert: assertion ttl exceeds max")
	ErrMissingClaim       = errors.New("slingassert: assertion missing required claim")
	ErrMissingExpectation = errors.New("slingassert: request expectation incomplete")
	ErrUnknownCommand     = errors.New("slingassert: command not registered")
	ErrPolicyMismatch     = errors.New("slingassert: policy or operation id mismatch")
	ErrWorkloadMismatch   = errors.New("slingassert: mTLS workload mismatch")
	ErrMethodMismatch     = errors.New("slingassert: method mismatch")
	ErrCommandMismatch    = errors.New("slingassert: command mismatch")
	ErrBodyMismatch       = errors.New("slingassert: body hash mismatch")
	ErrCityMismatch       = errors.New("slingassert: city mismatch")
	ErrReplay             = errors.New("slingassert: replay detected")
	ErrReplayUnavailable  = errors.New("slingassert: replay guard unavailable")
)

// Command is a registry entry: the authorization a named private command must
// descend from. Both ids are required, and an assertion must carry both exactly.
type Command struct {
	PolicyID    string
	OperationID string
}

// Options configures a Verifier. The security-critical fields are validated by
// New so a misconfiguration fails loudly at construction rather than silently
// admitting dispatches.
type Options struct {
	// Keys maps a key id (kid) to the BTS minting key's ed25519 public half.
	// At least one is required.
	Keys map[string]ed25519.PublicKey
	// Commands is the runtime command registry. At least one entry is required;
	// an assertion naming any other command is rejected. Emptying an entry is
	// the rollback gate — see the package doc.
	Commands map[string]Command
	// MaxTTL bounds Exp-IAT. Required; a non-positive TTL is refused.
	MaxTTL time.Duration
	// Skew tolerates clock drift on the iat/exp window.
	Skew time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// Replay enforces single-use on the nonce; defaults to an in-memory guard.
	// Shared with citywriteauth so both boundaries can be pointed at one
	// durable store without two implementations drifting apart.
	Replay citywriteauth.ReplayGuard
}

// Verifier checks internal sling assertions. It is verify-only: it never mints.
type Verifier struct {
	keys     map[string]ed25519.PublicKey
	commands map[string]Command
	maxTTL   time.Duration
	skew     time.Duration
	now      func() time.Time
	replay   citywriteauth.ReplayGuard
}

// Expect carries the request-derived facts a valid assertion must be bound to.
// Every field is required; a zero value is a rejection, not a wildcard.
type Expect struct {
	// Workload is the identity extracted from the verified mTLS peer chain.
	Workload string
	// Method is the uppercase HTTP method of the private request.
	Method string
	// Command is the command segment of the private request path.
	Command string
	// BodyHash is BodyHash(body) over the exact bytes the handler will parse.
	BodyHash string
	// City is the city segment of the private request path.
	City string
}

// New builds a Verifier, validating that the security-critical options are set.
func New(opts Options) (*Verifier, error) {
	if len(opts.Keys) == 0 {
		return nil, errors.New("slingassert: at least one verifying key is required")
	}
	if len(opts.Commands) == 0 {
		return nil, errors.New("slingassert: at least one registered command is required")
	}
	if opts.MaxTTL <= 0 {
		return nil, errors.New("slingassert: MaxTTL must be positive")
	}
	keys := make(map[string]ed25519.PublicKey, len(opts.Keys))
	for kid, pub := range opts.Keys {
		if kid == "" {
			return nil, errors.New("slingassert: empty kid in key set")
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("slingassert: key %q has wrong size", kid)
		}
		// ed25519.PublicKey is a mutable []byte, so copying only the map would
		// alias the caller's backing array and let a later mutation change the
		// verifier's trust root after construction.
		keys[kid] = append(ed25519.PublicKey(nil), pub...)
	}
	commands := make(map[string]Command, len(opts.Commands))
	for name, cmd := range opts.Commands {
		if name == "" {
			return nil, errors.New("slingassert: empty command name in registry")
		}
		if cmd.PolicyID == "" || cmd.OperationID == "" {
			return nil, fmt.Errorf("slingassert: command %q needs both a policy id and an operation id", name)
		}
		commands[name] = cmd
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	replay := opts.Replay
	if replay == nil {
		replay = citywriteauth.NewMemoryReplayGuard()
	}
	return &Verifier{
		keys:     keys,
		commands: commands,
		maxTTL:   opts.MaxTTL,
		skew:     opts.Skew,
		now:      now,
		replay:   replay,
	}, nil
}

// Registered reports whether name is a currently registered command. It is the
// rollback gate's read side: a caller can refuse a request before reading its
// body when the command has been withdrawn.
func (v *Verifier) Registered(name string) bool {
	_, ok := v.commands[name]
	return ok
}

// Verify authenticates token and binds it to expect. On success it returns the
// authenticated assertion and consumes its nonce (single-use). Every check runs
// before the nonce is consumed, so a failed verification never burns a
// legitimate assertion.
//
// The one case that returns both an assertion and an error is ErrReplay: the
// signature, the registry, the temporal window, and every binding already
// passed, and only single-use failed. The returned assertion is therefore fully
// authenticated, which is what lets a caller answer a duplicate internal
// delivery from its result store keyed by the authentic nonce rather than
// trusting an unverified payload. On every other error the assertion is nil.
func (v *Verifier) Verify(token string, expect Expect) (*Assertion, error) {
	payload, sig, err := splitToken(token)
	if err != nil {
		return nil, err
	}
	var a Assertion
	if err := json.Unmarshal(payload, &a); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	// Select the key by the (still-untrusted) kid, then let the signature check
	// authenticate the whole payload. A tampered kid simply fails verification.
	pub, ok := v.keys[a.Kid]
	if !ok {
		return nil, ErrUnknownKey
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, ErrBadSignature
	}
	// From here the claims are authentic.

	// Reject empty claims and incomplete expectations before any equality check.
	// Without this an assertion with an empty org or workload would satisfy
	// every comparison whenever the integration supplied a zero-value Expect,
	// defeating fail-closed binding.
	if err := requireComplete(a, expect); err != nil {
		return nil, err
	}
	if a.Aud != Audience {
		return nil, ErrAudience
	}

	// Registry gate ahead of the bindings: an assertion naming a withdrawn or
	// never-registered command is refused whatever else it says, and a
	// registered one must descend from that command's exact authorization.
	cmd, ok := v.commands[a.Command]
	if !ok {
		return nil, ErrUnknownCommand
	}
	if a.PolicyID != cmd.PolicyID || a.OperationID != cmd.OperationID {
		return nil, ErrPolicyMismatch
	}

	// Temporal contract. exp is reused below to bound replay retention.
	exp, err := v.checkFreshness(a)
	if err != nil {
		return nil, err
	}

	// Request bindings. Constant-time on the body hash: it is the one binding an
	// attacker can grind byte-by-byte by mutating the body they control.
	if a.Workload != expect.Workload {
		return nil, ErrWorkloadMismatch
	}
	if a.Method != expect.Method {
		return nil, ErrMethodMismatch
	}
	if a.Command != expect.Command {
		return nil, ErrCommandMismatch
	}
	if subtle.ConstantTimeCompare([]byte(a.BodyHash), []byte(expect.BodyHash)) != 1 {
		return nil, ErrBodyMismatch
	}
	if a.City != expect.City {
		return nil, ErrCityMismatch
	}

	// Consume the nonce last so a failed check above never invalidates a real
	// assertion. Retain it until this verifier's own acceptance deadline
	// (exp+skew), not bare exp: a guard evicting at exp could drop the record
	// while Verify still accepts the assertion during the skew window,
	// reopening single-use.
	if err := v.replay.Use(a.Nonce, exp.Add(v.skew)); err != nil {
		if errors.Is(err, citywriteauth.ErrReplay) {
			// A genuine duplicate nonce. The assertion is authentic and fully
			// bound, so hand it back for duplicate-delivery resolution.
			return &a, ErrReplay
		}
		// A durable or shared guard can fail for storage or network reasons.
		// That is not a replay, so surface it under a distinct sentinel and
		// return no assertion: the caller must fail closed rather than mistake
		// guard unavailability for a duplicate.
		return nil, fmt.Errorf("%w: %w", ErrReplayUnavailable, err)
	}
	return &a, nil
}

// checkFreshness validates the temporal contract: a well-formed iat/exp window,
// a ttl within MaxTTL, and the current time inside the skew-tolerant window. It
// returns the parsed exp so the caller can bound replay retention to the same
// acceptance deadline (exp+skew) instead of recomputing it.
func (v *Verifier) checkFreshness(a Assertion) (time.Time, error) {
	iat := time.Unix(a.IAT, 0)
	exp := time.Unix(a.Exp, 0)
	if !exp.After(iat) {
		return exp, ErrBadWindow
	}
	if exp.Sub(iat) > v.maxTTL {
		return exp, ErrTTLTooLong
	}
	now := v.now()
	if now.After(exp.Add(v.skew)) {
		return exp, ErrExpired
	}
	if now.Before(iat.Add(-v.skew)) {
		return exp, ErrNotYetValid
	}
	return exp, nil
}

// requireComplete rejects an assertion with any empty claim and an expectation
// with any empty field. Both halves matter: an empty claim would compare equal
// to an empty expectation, and an empty expectation would admit any claim.
func requireComplete(a Assertion, expect Expect) error {
	for _, claim := range []string{
		a.Kid, a.Aud, a.Nonce,
		a.Workload, a.Method, a.Command, a.BodyHash,
		a.PolicyID, a.OperationID,
		a.Org, a.Workspace, a.Principal, a.Source,
		a.City, a.Project, a.Issue,
	} {
		if claim == "" {
			return ErrMissingClaim
		}
	}
	if a.IAT == 0 || a.Exp == 0 {
		return ErrMissingClaim
	}
	for _, want := range []string{expect.Workload, expect.Method, expect.Command, expect.BodyHash, expect.City} {
		if want == "" {
			return ErrMissingExpectation
		}
	}
	return nil
}

func splitToken(token string) (payload, sig []byte, err error) {
	p, s, ok := strings.Cut(token, ".")
	if !ok || p == "" || s == "" {
		return nil, nil, ErrMalformed
	}
	payload, err = base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: payload: %w", ErrMalformed, err)
	}
	sig, err = base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: signature: %w", ErrMalformed, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, nil, fmt.Errorf("%w: signature size", ErrMalformed)
	}
	return payload, sig, nil
}

// BodyHash is the lowercase hex sha256 of the exact request body bytes, the
// value both the minter and the verifier fold into the assertion. It is a bare
// body digest rather than citywriteauth's composite request digest because the
// method, command and city are separate claims here — the City checks each one
// by name instead of collapsing them into one opaque hash it cannot audit.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
