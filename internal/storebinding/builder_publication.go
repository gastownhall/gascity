package storebinding

import (
	"errors"
	"fmt"
)

var (
	// ErrUnpublishedStoreSetInvalid reports a candidate that was never produced
	// by a successful build.
	ErrUnpublishedStoreSetInvalid = errors.New("invalid unpublished store set candidate")
	// ErrInvalidPublicationAuthority reports publication authority that does not
	// prove a durable active generation with a complete receipt set.
	ErrInvalidPublicationAuthority = errors.New("invalid store set publication authority")
)

// UnpublishedStoreSet is the built but unreachable StoreSet candidate. No
// accessor returns the underlying set: the only way out is Publish, and the
// only way to obtain publication authority is the unexported mint the
// activation path calls after the active manifest is durable. Because assembly
// produces this type and never a StoreSet, Publish is the sole producer of a
// populated set anywhere in the module.
type UnpublishedStoreSet struct {
	built       bool
	set         StoreSet
	generation  Generation
	descriptors map[BindingName]BindingIdentity
}

// storeSetFronts carries the six class fronts one candidate is assembled from.
// The type and every field are unexported, so no code outside this package can
// name it, let alone hand a set of fronts to the assembler below.
type storeSetFronts struct {
	work      WorkTopology
	graph     GraphStore
	sessions  SessionsStore
	messaging MessagingFrontDoors
	orders    OrdersStore
	nudges    NudgeFrontDoors
}

// validate rejects an incomplete or typed-nil front set.
func (f storeSetFronts) validate() error {
	if err := f.work.validate(); err != nil {
		return fmt.Errorf("store set requires valid Work topology: %w", err)
	}
	if nilInterface(f.graph) || nilInterface(f.sessions) || nilInterface(f.orders) ||
		nilInterface(f.messaging.Mail) || nilInterface(f.messaging.Bindings) ||
		nilInterface(f.messaging.DeliveryContexts) || nilInterface(f.messaging.Groups) ||
		nilInterface(f.messaging.Transcripts) ||
		nilInterface(f.nudges.Queue) || nilInterface(f.nudges.Shadows) {
		return errors.New("store set requires complete non-nil class fronts")
	}
	return nil
}

// newUnpublishedStoreSet is the module's only StoreSet assembly path, and it
// hands back the candidate rather than the set. That return type is what makes
// publication before activation structurally impossible rather than merely policed: any
// route to this function — an in-package call, a future consumer, or a linkname
// pull that ignores exportedness entirely — still yields a value
// whose sole method is Publish, and Publish demands authority nothing outside
// the sanctioned mint can produce.
func newUnpublishedStoreSet(fronts storeSetFronts, generation Generation, descriptors map[BindingName]BindingIdentity) (UnpublishedStoreSet, error) {
	if err := fronts.validate(); err != nil {
		return UnpublishedStoreSet{}, err
	}
	// Spelled as a literal on purpose, and NOT collapsed to StoreSet(fronts).
	// TestStoreSetHasOneProducer counts StoreSet literals in this file and
	// requires exactly one: the single assembly point is the whole guarantee
	// that no other route can mint a published set. A conversion satisfies the
	// compiler and reads as tidier, but it takes the census's anchor away and
	// the count drops to zero.
	//nolint:staticcheck // S1016: the literal is the single-assembly-point anchor.
	set := StoreSet{
		work:      fronts.work,
		graph:     fronts.graph,
		sessions:  fronts.sessions,
		messaging: fronts.messaging,
		orders:    fronts.orders,
		nudges:    fronts.nudges,
	}
	return UnpublishedStoreSet{built: true, set: set, generation: generation, descriptors: cloneBindingIdentities(descriptors)}, nil
}

// Publish exchanges the candidate for the immutable StoreSet consumers accept.
// It validates the authority against the candidate's generation and the exact
// descriptor identity of every opened binding.
func (u UnpublishedStoreSet) Publish(authority StoreSetPublicationAuthority) (StoreSet, error) {
	if !u.built || !u.generation.Valid() || len(u.descriptors) == 0 {
		return StoreSet{}, ErrUnpublishedStoreSetInvalid
	}
	if err := authority.validate(); err != nil {
		return StoreSet{}, err
	}
	if authority.generation != u.generation {
		return StoreSet{}, fmt.Errorf("%w: authority names generation %d for candidate generation %d", ErrInvalidPublicationAuthority, authority.generation, u.generation)
	}
	if len(authority.descriptors) != len(u.descriptors) {
		return StoreSet{}, fmt.Errorf("%w: authority names %d bindings for %d opened bindings", ErrInvalidPublicationAuthority, len(authority.descriptors), len(u.descriptors))
	}
	for binding, identity := range u.descriptors {
		if authority.descriptors[binding] != identity {
			return StoreSet{}, fmt.Errorf("%w: binding %q descriptor identity differs from the active manifest", ErrInvalidPublicationAuthority, binding)
		}
	}
	return u.set, nil
}

// StoreSetPublicationAuthority is the opaque proof that the migration saga made
// one active generation durable with a complete receipt set. Its fields are
// unexported and it has no public mint, so no code outside this package can
// construct one — which is what makes publication before activation impossible rather
// than merely discouraged.
type StoreSetPublicationAuthority struct {
	version          uint16
	attempt          AttemptID
	generation       Generation
	manifestDigest   string
	descriptors      map[BindingName]BindingIdentity
	receiptsComplete bool
}

// newStoreSetPublicationAuthority is the sole mint. The activation path
// calls it after ACTIVE_MANIFEST_DURABLE with a complete receipt bitmap; the
// census test pins its call sites.
func newStoreSetPublicationAuthority(attempt AttemptID, generation Generation, manifestDigest string, descriptors map[BindingName]BindingIdentity, receiptsComplete bool) (StoreSetPublicationAuthority, error) {
	authority := StoreSetPublicationAuthority{
		version:          1,
		attempt:          attempt,
		generation:       generation,
		manifestDigest:   manifestDigest,
		descriptors:      cloneBindingIdentities(descriptors),
		receiptsComplete: receiptsComplete,
	}
	if err := authority.validate(); err != nil {
		return StoreSetPublicationAuthority{}, err
	}
	return authority, nil
}

func (a StoreSetPublicationAuthority) validate() error {
	if a.version != 1 || !a.generation.Valid() {
		return fmt.Errorf("%w: missing version or generation", ErrInvalidPublicationAuthority)
	}
	if err := validateSecretFree("publication attempt", string(a.attempt)); err != nil {
		return err
	}
	if a.attempt == "" {
		return fmt.Errorf("%w: missing attempt", ErrInvalidPublicationAuthority)
	}
	if err := validateCanonicalSHA256Digest("active manifest digest", a.manifestDigest); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPublicationAuthority, err)
	}
	if len(a.descriptors) == 0 {
		return fmt.Errorf("%w: no active descriptors", ErrInvalidPublicationAuthority)
	}
	for binding, identity := range a.descriptors {
		if err := validateIdentifier("binding name", string(binding)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidPublicationAuthority, err)
		}
		if err := validateCanonicalSHA256Digest("active descriptor identity", string(identity)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidPublicationAuthority, err)
		}
	}
	if !a.receiptsComplete {
		return fmt.Errorf("%w: receipt bitmap is incomplete", ErrInvalidPublicationAuthority)
	}
	return nil
}

func cloneBindingIdentities(identities map[BindingName]BindingIdentity) map[BindingName]BindingIdentity {
	if identities == nil {
		return nil
	}
	out := make(map[BindingName]BindingIdentity, len(identities))
	for binding, identity := range identities {
		out[binding] = identity
	}
	return out
}
