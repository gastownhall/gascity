package storebinding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/coordclass"
)

const writerFenceRevalidationTimeout = 10 * time.Second

var (
	// ErrDuplicateProvider reports duplicate exact provider IDs in one registry.
	ErrDuplicateProvider = errors.New("duplicate storage provider")
	// ErrUnknownProvider reports an exact provider ID absent from a registry.
	ErrUnknownProvider = errors.New("unknown storage provider")
	// ErrProviderRegistryFrozen reports a registration attempted after Freeze.
	ErrProviderRegistryFrozen = errors.New("storage provider registry is frozen")
	// ErrProviderRegistryNotFrozen reports resolution attempted before Freeze.
	ErrProviderRegistryNotFrozen = errors.New("storage provider registry is not frozen")
	// ErrInvalidBindingSpec reports an invalid config-facing binding envelope.
	ErrInvalidBindingSpec = errors.New("invalid storage binding specification")
	// ErrInvalidDescriptor reports a malformed provider descriptor.
	ErrInvalidDescriptor = errors.New("invalid storage descriptor")
	// ErrDescriptorOverlap reports two named bindings that share a component.
	ErrDescriptorOverlap = errors.New("storage descriptor overlap")
	// ErrSecretMaterial reports a credential-bearing value at the provider boundary.
	ErrSecretMaterial = errors.New("storage provider value contains secret material")
	// ErrInvalidInspection reports an incomplete or incompatible inspection value.
	ErrInvalidInspection = errors.New("invalid storage inspection")
	// ErrInvalidFenceTarget reports a malformed mutation-free fence target.
	ErrInvalidFenceTarget = errors.New("invalid storage fence target")
	// ErrInvalidFence reports a fence incompatible with its request.
	ErrInvalidFence = errors.New("invalid storage writer fence")
	// ErrFenceNotHeld reports a fence that ceased to exclude writers.
	ErrFenceNotHeld = errors.New("storage writer fence is not held")
	// ErrFenceTargetMoved reports a component identity that changed after Inspect.
	ErrFenceTargetMoved = errors.New("storage fence target moved")
	// ErrUnsupportedClass reports a class not supported by a provider descriptor.
	ErrUnsupportedClass = errors.New("unsupported storage class")
	// ErrMissingCapability reports a required provider capability that is absent.
	ErrMissingCapability = errors.New("missing storage provider capability")
	// ErrWrongContract reports an incompatible semantic storage contract.
	ErrWrongContract = errors.New("wrong storage semantic contract")
	// ErrWrongFormat reports an incompatible physical storage format.
	ErrWrongFormat = errors.New("wrong storage physical format")
	// ErrProviderUnavailable reports a compiled provider whose implementation cannot run.
	ErrProviderUnavailable = errors.New("storage provider unavailable")
	// ErrInvalidProviderID reports a provider identifier that cannot safely be resolved.
	ErrInvalidProviderID = errors.New("invalid storage provider ID")
	// ErrProviderFactoryContract reports a factory that violates the resource-free construction contract.
	ErrProviderFactoryContract = errors.New("storage provider factory contract violation")
)

// ProviderID is an exact backend-owned provider identifier.
type ProviderID string

// BindingName is the configuration name of one storage binding.
type BindingName string

// ConfigRef is a provider-owned opaque configuration reference, never a credential.
type ConfigRef string

// ConfigRefDigest is the secret-free canonical digest of a ConfigRef.
type ConfigRefDigest string

// ContractVersion identifies the semantic contract implemented by a binding.
type ContractVersion string

// FormatID identifies one provider-owned physical storage format.
type FormatID string

// ComponentID identifies one physical component inside a binding.
type ComponentID string

// ComponentLocator is a secret-free canonical physical component locator.
type ComponentLocator string

// PhysicalIdentity is a provider-observed, secret-free physical component identity.
type PhysicalIdentity string

// BindingIdentity is a versioned digest of a complete binding descriptor.
type BindingIdentity string

// Generation identifies one durable storage generation.
type Generation uint64

// Valid reports whether a generation names a durable storage generation.
// Generation zero is the invalid/unset sentinel; genesis evidence is separate.
func (g Generation) Valid() bool { return g > 0 }

// AttemptID identifies one durable storage migration attempt.
type AttemptID string

// BindingSpec is the validated config-facing envelope for one named binding.
// Path is provider-owned configuration and ConfigRef is an opaque, secret-free reference.
type BindingSpec struct {
	Name      BindingName
	Provider  ProviderID
	Path      string
	ConfigRef ConfigRef
	// CityRoot is the absolute directory of the city this binding belongs to.
	//
	// It is here because a provider's configuration is relative to a city and
	// nothing else in this envelope says which one. Without it the only base a
	// provider can resolve against is the process working directory — which is
	// the city for a command run inside one, and is emphatically not the city
	// for a supervisor hosting many of them from one process. A provider that
	// resolves against the working directory therefore serves a different
	// binding depending on where the binary was started, and two cities can
	// silently land on the same location.
	//
	// Plan resolution stamps it. An empty value stays legal because a plan can
	// be resolved without one — a caller that knows no city, and every test
	// that only exercises planning — and a provider that needs it refuses when
	// it is absent rather than inventing a base.
	CityRoot string
	// URL is the http or https endpoint a binding's backing store answers on
	// when it does not live on this disk. Empty — the default — means the
	// binding's configuration resolves locally. The beads-workspace provider
	// reads it only to select the explicitly configured credential bridge; the
	// workspace's own configuration still owns the connection endpoint. It is
	// validated so no provider sees a value that smuggles a credential.
	URL string
	// Auth is a reference to the credential for URL, never the credential
	// itself. AuthCredentialProvider and the "env:NAME" form are the whole
	// accepted set.
	Auth string
}

// Validate verifies that a binding specification is safe for provider selection.
func (s BindingSpec) Validate() error {
	if err := validateIdentifier("binding name", string(s.Name)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
	}
	if err := validateIdentifier("provider ID", string(s.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
	}
	if s.Path != "" && s.ConfigRef != "" {
		return fmt.Errorf("%w: path and config reference are mutually exclusive", ErrInvalidBindingSpec)
	}
	if s.ConfigRef != "" {
		if err := validateSecretFree("binding config reference", string(s.ConfigRef)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
		}
		if err := validateIdentifier("binding config reference", string(s.ConfigRef)); err != nil {
			return fmt.Errorf("%w: non-canonical config reference", ErrInvalidBindingSpec)
		}
	}
	if err := validateSecretFree("binding path", s.Path); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
	}
	if s.CityRoot != "" {
		if err := validateSecretFree("binding city root", s.CityRoot); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
		}
		if !filepath.IsAbs(s.CityRoot) {
			// A relative city root is the defect this field exists to remove,
			// wearing the field's name: whatever resolved it would be back to
			// guessing a base from the working directory.
			return fmt.Errorf("%w: city root %q is not absolute", ErrInvalidBindingSpec, s.CityRoot)
		}
	}
	if err := validateEndpoint(s.URL, s.Auth); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
	}
	return nil
}

// ClassSet is an immutable typed set of StoreSet classes.
type ClassSet struct {
	work      bool
	graph     bool
	sessions  bool
	messaging bool
	orders    bool
	nudges    bool
}

// NewClassSet creates a typed class set and rejects unknown or duplicate classes.
func NewClassSet(classes ...coordclass.Class) (ClassSet, error) {
	var set ClassSet
	for _, class := range classes {
		if !isKnownClass(class) {
			return ClassSet{}, fmt.Errorf("%w: %s", ErrUnsupportedClass, class)
		}
		if set.Has(class) {
			return ClassSet{}, fmt.Errorf("duplicate storage class %s", class)
		}
		set = set.with(class)
	}
	return set, nil
}

// Has reports whether a class belongs to the set.
func (s ClassSet) Has(class coordclass.Class) bool {
	switch class {
	case coordclass.ClassWork:
		return s.work
	case coordclass.ClassGraph:
		return s.graph
	case coordclass.ClassSessions:
		return s.sessions
	case coordclass.ClassMessaging:
		return s.messaging
	case coordclass.ClassOrders:
		return s.orders
	case coordclass.ClassNudges:
		return s.nudges
	default:
		return false
	}
}

// Classes returns the set in canonical class order.
func (s ClassSet) Classes() []coordclass.Class {
	classes := make([]coordclass.Class, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		if s.Has(class) {
			classes = append(classes, class)
		}
	}
	return classes
}

// Empty reports whether no class belongs to the set.
func (s ClassSet) Empty() bool { return len(s.Classes()) == 0 }

// Equal reports whether two typed class sets have identical members.
func (s ClassSet) Equal(other ClassSet) bool {
	return s.work == other.work && s.graph == other.graph && s.sessions == other.sessions &&
		s.messaging == other.messaging && s.orders == other.orders && s.nudges == other.nudges
}

// Contains reports whether s contains every member of other.
func (s ClassSet) Contains(other ClassSet) bool {
	for _, class := range other.Classes() {
		if !s.Has(class) {
			return false
		}
	}
	return true
}

func (s ClassSet) with(class coordclass.Class) ClassSet {
	switch class {
	case coordclass.ClassWork:
		s.work = true
	case coordclass.ClassGraph:
		s.graph = true
	case coordclass.ClassSessions:
		s.sessions = true
	case coordclass.ClassMessaging:
		s.messaging = true
	case coordclass.ClassOrders:
		s.orders = true
	case coordclass.ClassNudges:
		s.nudges = true
	}
	return s
}

func isKnownClass(class coordclass.Class) bool {
	for _, known := range coordclass.Classes() {
		if class == known {
			return true
		}
	}
	return false
}

// ClassCapability describes the capabilities available to one semantic class.
type ClassCapability struct {
	Available    bool
	Transactions bool
	Claims       bool
}

// ClassCapabilities contains typed class and binding capabilities.
// It deliberately has no string-keyed capability map.
type ClassCapabilities struct {
	Work      ClassCapability
	Graph     ClassCapability
	Sessions  ClassCapability
	Messaging ClassCapability
	Orders    ClassCapability
	Nudges    ClassCapability

	WriterFencing     bool
	GuardedActivation bool
}

// For returns the capability declaration for one semantic class.
func (c ClassCapabilities) For(class coordclass.Class) ClassCapability {
	switch class {
	case coordclass.ClassWork:
		return c.Work
	case coordclass.ClassGraph:
		return c.Graph
	case coordclass.ClassSessions:
		return c.Sessions
	case coordclass.ClassMessaging:
		return c.Messaging
	case coordclass.ClassOrders:
		return c.Orders
	case coordclass.ClassNudges:
		return c.Nudges
	default:
		return ClassCapability{}
	}
}

// Supports reports whether every requested class is available.
func (c ClassCapabilities) Supports(classes ClassSet) bool {
	for _, class := range classes.Classes() {
		if !c.For(class).Available {
			return false
		}
	}
	return true
}

// Equal reports whether two typed capability declarations have identical values.
func (c ClassCapabilities) Equal(other ClassCapabilities) bool { return c == other }

// ValidateRequired checks class support and explicitly requested binding capabilities.
func (c ClassCapabilities) ValidateRequired(classes ClassSet, requireTransactions, requireClaims, requireFence bool) error {
	for _, class := range classes.Classes() {
		capability := c.For(class)
		if !capability.Available {
			return fmt.Errorf("%w: %s", ErrMissingCapability, class)
		}
		if requireTransactions && !capability.Transactions {
			return fmt.Errorf("%w: transactions for %s", ErrMissingCapability, class)
		}
		if requireClaims && !capability.Claims {
			return fmt.Errorf("%w: claims for %s", ErrMissingCapability, class)
		}
	}
	if requireFence && !c.WriterFencing {
		return fmt.Errorf("%w: writer fencing", ErrMissingCapability)
	}
	return nil
}

func (c ClassCapabilities) validateDeclaredClasses(classes ClassSet) error {
	for _, class := range coordclass.Classes() {
		capability := c.For(class)
		if capability.Available != classes.Has(class) {
			if classes.Has(class) {
				return fmt.Errorf("%w: %s", ErrMissingCapability, class)
			}
			return fmt.Errorf("class capability does not match declared class %s", class)
		}
		if !capability.Available && (capability.Transactions || capability.Claims) {
			return fmt.Errorf("unavailable class %s declares transactions or claims", class)
		}
	}
	return nil
}

// MarkerState records the marker state observed for one physical component.
type MarkerState struct {
	Name    string
	Present bool
}

// ComponentDescriptor describes one physical component of a complete binding.
type ComponentDescriptor struct {
	ID               ComponentID
	Locator          ComponentLocator
	PhysicalIdentity PhysicalIdentity
	Classes          ClassSet
	Format           FormatID
	SchemaVersion    string
	ABIVersion       string
	Marker           MarkerState
}

// Descriptor is a complete, secret-free aggregate binding identity.
type Descriptor struct {
	Version                 uint16
	SemanticContractVersion ContractVersion
	Provider                ProviderID
	ImplementationVersion   string
	Components              []ComponentDescriptor
	Capabilities            ClassCapabilities
	ConfigRefDigest         ConfigRefDigest
	RetainedSource          *RetainedSourceRef
}

// NewDescriptor validates and defensively copies a complete descriptor.
func NewDescriptor(descriptor Descriptor) (Descriptor, error) {
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor.Clone(), nil
}

// Clone returns a detached descriptor value.
func (d Descriptor) Clone() Descriptor {
	out := d
	out.Components = append([]ComponentDescriptor(nil), d.Components...)
	if d.RetainedSource != nil {
		retained := d.RetainedSource.Clone()
		out.RetainedSource = &retained
	}
	return out
}

// Classes returns every class represented by the descriptor.
func (d Descriptor) Classes() ClassSet {
	var classes ClassSet
	for _, component := range d.Components {
		for _, class := range component.Classes.Classes() {
			classes = classes.with(class)
		}
	}
	return classes
}

// Validate verifies that a descriptor is complete, non-overlapping, and secret-free.
func (d Descriptor) Validate() error {
	if d.Version != 1 || strings.TrimSpace(string(d.SemanticContractVersion)) == "" {
		return fmt.Errorf("%w: missing version or semantic contract", ErrInvalidDescriptor)
	}
	if err := validateSecretFree("descriptor semantic contract", string(d.SemanticContractVersion)); err != nil {
		return err
	}
	if err := validateIdentifier("provider ID", string(d.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if strings.TrimSpace(d.ImplementationVersion) == "" {
		return fmt.Errorf("%w: missing implementation version", ErrInvalidDescriptor)
	}
	if err := validateSecretFree("descriptor implementation version", d.ImplementationVersion); err != nil {
		return err
	}
	if strings.TrimSpace(string(d.ConfigRefDigest)) == "" {
		return fmt.Errorf("%w: missing config reference digest", ErrInvalidDescriptor)
	}
	if err := validateCanonicalSHA256Digest("descriptor config reference digest", string(d.ConfigRefDigest)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if len(d.Components) == 0 {
		return fmt.Errorf("%w: no components", ErrInvalidDescriptor)
	}
	ids := make(map[ComponentID]struct{}, len(d.Components))
	identities := make(map[PhysicalIdentity]struct{}, len(d.Components))
	locators := make(map[ComponentLocator]struct{}, len(d.Components))
	var allClasses ClassSet
	for _, component := range d.Components {
		if err := component.validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
		}
		if _, exists := ids[component.ID]; exists {
			return fmt.Errorf("%w: duplicate component ID %q", ErrInvalidDescriptor, component.ID)
		}
		if _, exists := identities[component.PhysicalIdentity]; exists {
			return fmt.Errorf("%w: duplicate physical component", ErrInvalidDescriptor)
		}
		if _, exists := locators[component.Locator]; exists {
			return fmt.Errorf("%w: duplicate component locator", ErrInvalidDescriptor)
		}
		for _, class := range component.Classes.Classes() {
			if allClasses.Has(class) {
				return fmt.Errorf("%w: overlapping class %s", ErrInvalidDescriptor, class)
			}
			allClasses = allClasses.with(class)
		}
		ids[component.ID] = struct{}{}
		identities[component.PhysicalIdentity] = struct{}{}
		locators[component.Locator] = struct{}{}
	}
	if err := d.Capabilities.validateDeclaredClasses(allClasses); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if d.RetainedSource != nil {
		if err := d.RetainedSource.Validate(); err != nil {
			return fmt.Errorf("%w: retained source: %w", ErrInvalidDescriptor, err)
		}
		if err := d.validateRetainedSource(*d.RetainedSource); err != nil {
			return fmt.Errorf("%w: retained source: %w", ErrInvalidDescriptor, err)
		}
	}
	return nil
}

func (d Descriptor) validateRetainedSource(source RetainedSourceRef) error {
	if source.Provider != d.Provider || source.ImplementationVersion != d.ImplementationVersion || source.SemanticContractVersion != d.SemanticContractVersion || source.ConfigRefDigest != d.ConfigRefDigest {
		return ErrInvalidRetainedSource
	}
	for _, component := range d.Components {
		if component.ID != source.Component {
			continue
		}
		if component.PhysicalIdentity != source.PhysicalIdentity || !component.Classes.Equal(source.Classes) || component.Format != source.Format || component.SchemaVersion != source.SchemaVersion || component.ABIVersion != source.ABIVersion {
			return ErrInvalidRetainedSource
		}
		return nil
	}
	return ErrInvalidRetainedSource
}

func (d ComponentDescriptor) validate() error {
	if err := validateIdentifier("component ID", string(d.ID)); err != nil {
		return err
	}
	if strings.TrimSpace(string(d.Locator)) == "" || strings.TrimSpace(string(d.PhysicalIdentity)) == "" {
		return errors.New("missing component locator or physical identity")
	}
	if err := validateSecretFree("component locator", string(d.Locator)); err != nil {
		return err
	}
	if err := validateSecretFree("component physical identity", string(d.PhysicalIdentity)); err != nil {
		return err
	}
	if d.Classes.Empty() {
		return errors.New("component has no classes")
	}
	if strings.TrimSpace(string(d.Format)) == "" {
		return errors.New("missing component format")
	}
	if strings.TrimSpace(d.SchemaVersion) == "" && strings.TrimSpace(d.ABIVersion) == "" {
		return errors.New("missing component schema or ABI version")
	}
	if err := validateSecretFree("component format", string(d.Format)); err != nil {
		return err
	}
	if err := validateSecretFree("component schema version", d.SchemaVersion); err != nil {
		return err
	}
	if err := validateSecretFree("component ABI version", d.ABIVersion); err != nil {
		return err
	}
	if strings.TrimSpace(d.Marker.Name) == "" {
		return errors.New("missing component marker state")
	}
	if err := validateSecretFree("component marker", d.Marker.Name); err != nil {
		return err
	}
	return nil
}

// Identity returns the versioned aggregate identity digest for a complete descriptor.
func (d Descriptor) Identity() (BindingIdentity, error) {
	payload, err := d.canonicalIdentityPayload()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return BindingIdentity("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Equal reports whether two complete descriptors carry the same canonical typed facts.
// Component declaration order is not part of descriptor identity.
func (d Descriptor) Equal(other Descriptor) bool {
	left, err := d.canonicalIdentityPayload()
	if err != nil {
		return false
	}
	right, err := other.canonicalIdentityPayload()
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func (d Descriptor) canonicalIdentityPayload() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	components := append([]ComponentDescriptor(nil), d.Components...)
	sort.Slice(components, func(i, j int) bool { return components[i].ID < components[j].ID })
	var encoded canonicalDescriptorEncoding
	encoded.string("gascity.storage-binding-descriptor.v2")
	encoded.uint16(d.Version)
	encoded.string(string(d.SemanticContractVersion))
	encoded.string(string(d.Provider))
	encoded.string(d.ImplementationVersion)
	encoded.string(string(d.ConfigRefDigest))
	encoded.uint64(uint64(len(components)))
	for _, component := range components {
		encoded.string(string(component.ID))
		encoded.string(string(component.Locator))
		encoded.string(string(component.PhysicalIdentity))
		encoded.classSet(component.Classes)
		encoded.string(string(component.Format))
		encoded.string(component.SchemaVersion)
		encoded.string(component.ABIVersion)
		encoded.string(component.Marker.Name)
		encoded.bool(component.Marker.Present)
	}
	for _, class := range coordclass.Classes() {
		capability := d.Capabilities.For(class)
		encoded.string(class.String())
		encoded.bool(capability.Available)
		encoded.bool(capability.Transactions)
		encoded.bool(capability.Claims)
	}
	encoded.bool(d.Capabilities.WriterFencing)
	encoded.bool(d.Capabilities.GuardedActivation)
	if d.RetainedSource == nil {
		encoded.bool(false)
		return encoded.bytes, nil
	}
	encoded.bool(true)
	encoded.retainedSource(*d.RetainedSource)
	return encoded.bytes, nil
}

type canonicalDescriptorEncoding struct{ bytes []byte }

func (e *canonicalDescriptorEncoding) uint16(value uint16) {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	e.bytes = append(e.bytes, encoded[:]...)
}

func (e *canonicalDescriptorEncoding) uint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	e.bytes = append(e.bytes, encoded[:]...)
}

func (e *canonicalDescriptorEncoding) bool(value bool) {
	if value {
		e.bytes = append(e.bytes, 1)
		return
	}
	e.bytes = append(e.bytes, 0)
}

func (e *canonicalDescriptorEncoding) string(value string) {
	e.uint64(uint64(len(value)))
	e.bytes = append(e.bytes, value...)
}

func (e *canonicalDescriptorEncoding) raw(value []byte) {
	e.uint64(uint64(len(value)))
	e.bytes = append(e.bytes, value...)
}

func (e *canonicalDescriptorEncoding) classSet(classes ClassSet) {
	classNames := classes.Classes()
	e.uint64(uint64(len(classNames)))
	for _, class := range classNames {
		e.string(class.String())
	}
}

func (e *canonicalDescriptorEncoding) retainedSource(source RetainedSourceRef) {
	e.uint16(source.Version)
	e.string(string(source.Provider))
	e.string(source.ImplementationVersion)
	e.string(string(source.Component))
	e.classSet(source.Classes)
	e.string(string(source.SemanticContractVersion))
	e.string(string(source.Format))
	e.string(source.SchemaVersion)
	e.string(source.ABIVersion)
	e.string(string(source.PhysicalIdentity))
	e.string(string(source.ConfigRefDigest))
	e.string(source.WitnessVersion)
	e.string(source.WitnessDigest)
	e.raw(source.ReopenData)
}

// NamedDescriptor associates a complete descriptor with a configured binding name.
type NamedDescriptor struct {
	Name       BindingName
	Descriptor Descriptor
}

// ValidateNoDescriptorOverlap rejects aliases across distinct binding names.
func ValidateNoDescriptorOverlap(descriptors []NamedDescriptor) error {
	byIdentity := make(map[PhysicalIdentity]BindingName)
	byLocator := make(map[ComponentLocator]BindingName)
	for _, named := range descriptors {
		if err := validateIdentifier("binding name", string(named.Name)); err != nil {
			return fmt.Errorf("%w: %w", ErrDescriptorOverlap, err)
		}
		if err := named.Descriptor.Validate(); err != nil {
			return err
		}
		for _, component := range named.Descriptor.Components {
			if other, exists := byIdentity[component.PhysicalIdentity]; exists && other != named.Name {
				return &DescriptorOverlapError{First: other, Second: named.Name, Component: component.ID}
			}
			if other, exists := byLocator[component.Locator]; exists && other != named.Name {
				return &DescriptorOverlapError{First: other, Second: named.Name, Component: component.ID}
			}
			byIdentity[component.PhysicalIdentity] = named.Name
			byLocator[component.Locator] = named.Name
		}
	}
	return nil
}

// DescriptorOverlapError reports named binding aliases without exposing a physical locator.
type DescriptorOverlapError struct {
	First     BindingName
	Second    BindingName
	Component ComponentID
}

// Error implements error.
func (e *DescriptorOverlapError) Error() string {
	return fmt.Sprintf("%s: bindings %q and %q share component %q", ErrDescriptorOverlap, e.First, e.Second, e.Component)
}

// Unwrap supports errors.Is.
func (e *DescriptorOverlapError) Unwrap() error { return ErrDescriptorOverlap }

// FenceComponentTarget identifies one component that may be fenced without opening it.
type FenceComponentTarget struct {
	ID               ComponentID
	Locator          ComponentLocator
	PhysicalIdentity PhysicalIdentity
	Classes          ClassSet
}

// FenceTarget is the mutation-free, secret-free target used to acquire a writer fence.
type FenceTarget struct {
	Version    uint16
	Provider   ProviderID
	Classes    ClassSet
	Components []FenceComponentTarget
}

// NewFenceTarget validates and defensively copies one mutation-free fence target.
func NewFenceTarget(provider ProviderID, classes ClassSet, components []FenceComponentTarget) (FenceTarget, error) {
	target := FenceTarget{Version: 1, Provider: provider, Classes: classes, Components: append([]FenceComponentTarget(nil), components...)}
	sort.Slice(target.Components, func(i, j int) bool { return target.Components[i].ID < target.Components[j].ID })
	if err := target.Validate(); err != nil {
		return FenceTarget{}, err
	}
	return target, nil
}

// Clone returns a detached fence target value.
func (t FenceTarget) Clone() FenceTarget {
	out := t
	out.Components = append([]FenceComponentTarget(nil), t.Components...)
	return out
}

// Equal reports whether two targets identify the exact same physical components.
func (t FenceTarget) Equal(other FenceTarget) bool {
	if t.Version != other.Version || t.Provider != other.Provider || !t.Classes.Equal(other.Classes) || len(t.Components) != len(other.Components) {
		return false
	}
	left := append([]FenceComponentTarget(nil), t.Components...)
	right := append([]FenceComponentTarget(nil), other.Components...)
	sort.Slice(left, func(i, j int) bool { return left[i].ID < left[j].ID })
	sort.Slice(right, func(i, j int) bool { return right[i].ID < right[j].ID })
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Locator != right[index].Locator || left[index].PhysicalIdentity != right[index].PhysicalIdentity || !left[index].Classes.Equal(right[index].Classes) {
			return false
		}
	}
	return true
}

// Validate checks that a fence target is sufficient to identify every expected component.
func (t FenceTarget) Validate() error {
	if t.Version != 1 {
		return fmt.Errorf("%w: missing version", ErrInvalidFenceTarget)
	}
	if err := validateIdentifier("provider ID", string(t.Provider)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidFenceTarget, err)
	}
	if t.Classes.Empty() || len(t.Components) == 0 {
		return fmt.Errorf("%w: missing classes or components", ErrInvalidFenceTarget)
	}
	ids := make(map[ComponentID]struct{}, len(t.Components))
	identities := make(map[PhysicalIdentity]struct{}, len(t.Components))
	locators := make(map[ComponentLocator]struct{}, len(t.Components))
	var allClasses ClassSet
	for _, component := range t.Components {
		if err := validateIdentifier("fence component ID", string(component.ID)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidFenceTarget, err)
		}
		if strings.TrimSpace(string(component.Locator)) == "" || strings.TrimSpace(string(component.PhysicalIdentity)) == "" {
			return fmt.Errorf("%w: missing component locator or physical identity", ErrInvalidFenceTarget)
		}
		if err := validateSecretFree("fence component locator", string(component.Locator)); err != nil {
			return err
		}
		if err := validateSecretFree("fence component physical identity", string(component.PhysicalIdentity)); err != nil {
			return err
		}
		if component.Classes.Empty() || !t.Classes.Contains(component.Classes) {
			return fmt.Errorf("%w: component has unsupported classes", ErrInvalidFenceTarget)
		}
		if _, exists := ids[component.ID]; exists {
			return fmt.Errorf("%w: duplicate component ID %q", ErrInvalidFenceTarget, component.ID)
		}
		if _, exists := identities[component.PhysicalIdentity]; exists {
			return fmt.Errorf("%w: duplicate physical component", ErrInvalidFenceTarget)
		}
		if _, exists := locators[component.Locator]; exists {
			return fmt.Errorf("%w: duplicate component locator", ErrInvalidFenceTarget)
		}
		for _, class := range component.Classes.Classes() {
			if allClasses.Has(class) {
				return fmt.Errorf("%w: overlapping class %s", ErrInvalidFenceTarget, class)
			}
			allClasses = allClasses.with(class)
		}
		ids[component.ID] = struct{}{}
		identities[component.PhysicalIdentity] = struct{}{}
		locators[component.Locator] = struct{}{}
	}
	if !allClasses.Equal(t.Classes) {
		return fmt.Errorf("%w: component classes do not equal target classes", ErrInvalidFenceTarget)
	}
	return nil
}

// Inspection is the mutation-free discovery result for a binding.
// Descriptor is nil when a fenced final census is required.
type Inspection struct {
	Target     FenceTarget
	Descriptor *Descriptor
}

// NewInspection validates and defensively copies an inspection result.
func NewInspection(target FenceTarget, descriptor *Descriptor) (Inspection, error) {
	inspection := Inspection{Target: target.Clone()}
	if descriptor != nil {
		cloned := descriptor.Clone()
		inspection.Descriptor = &cloned
	}
	if err := inspection.Validate(); err != nil {
		return Inspection{}, err
	}
	return inspection, nil
}

// Complete reports whether mutation-free inspection proved an aggregate descriptor.
func (i Inspection) Complete() bool { return i.Descriptor != nil }

// Validate checks the inspection and any complete descriptor against its target.
func (i Inspection) Validate() error {
	if err := i.Target.Validate(); err != nil {
		return err
	}
	if i.Descriptor == nil {
		return nil
	}
	if err := i.Descriptor.Validate(); err != nil {
		return err
	}
	if err := validateDescriptorForTarget(*i.Descriptor, i.Target); err != nil {
		return err
	}
	return nil
}

// FenceRole describes the closed protocol role of a writer fence.
type FenceRole uint8

const (
	// FenceRoleSource excludes writers from a populated source.
	FenceRoleSource FenceRole = iota + 1
	// FenceRolePopulatedDestination admits an existing populated destination.
	FenceRolePopulatedDestination
	// FenceRoleNewDestinationReservation reserves a pristine destination namespace.
	FenceRoleNewDestinationReservation
	// FenceRoleActiveVerification validates an active generation before open.
	FenceRoleActiveVerification
)

// Valid reports whether the role belongs to the closed fence-role set.
func (r FenceRole) Valid() bool {
	return r >= FenceRoleSource && r <= FenceRoleActiveVerification
}

// String returns a stable diagnostic fence-role name.
func (r FenceRole) String() string {
	switch r {
	case FenceRoleSource:
		return "source"
	case FenceRolePopulatedDestination:
		return "populated-destination"
	case FenceRoleNewDestinationReservation:
		return "new-destination-reservation"
	case FenceRoleActiveVerification:
		return "active-verification"
	default:
		return "unknown"
	}
}

// FenceRequest contains a mutation-free target, the expected city guard scope,
// its components, generation, and role.
type FenceRequest struct {
	Target             FenceTarget
	GuardScope         MigrationGuardScope
	ExpectedGeneration Generation
	Components         []ComponentID
	Role               FenceRole
}

// Clone returns a detached fence request suitable for a provider invocation.
func (r FenceRequest) Clone() FenceRequest {
	out := r
	out.Target = r.Target.Clone()
	out.Components = append([]ComponentID(nil), r.Components...)
	return out
}

// Validate checks that the request names a closed role and exact target components.
func (r FenceRequest) Validate() error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.ExpectedGeneration.Valid() {
		return fmt.Errorf("%w: invalid expected generation", ErrInvalidFence)
	}
	if !r.GuardScope.valid() {
		return fmt.Errorf("%w: missing or invalid city guard scope", ErrInvalidFence)
	}
	if !r.Role.Valid() {
		return fmt.Errorf("%w: unknown role", ErrInvalidFence)
	}
	if len(r.Components) == 0 {
		return fmt.Errorf("%w: no components", ErrInvalidFence)
	}
	targetComponents := make(map[ComponentID]struct{}, len(r.Target.Components))
	for _, component := range r.Target.Components {
		targetComponents[component.ID] = struct{}{}
	}
	seen := make(map[ComponentID]struct{}, len(r.Components))
	for _, component := range r.Components {
		if _, exists := targetComponents[component]; !exists {
			return fmt.Errorf("%w: component %q is not in target", ErrInvalidFence, component)
		}
		if _, exists := seen[component]; exists {
			return fmt.Errorf("%w: duplicate component %q", ErrInvalidFence, component)
		}
		seen[component] = struct{}{}
	}
	return nil
}

// WriterFence is a held writer-exclusion fence. Release must be idempotent.
type WriterFence interface {
	Target() FenceTarget
	Role() FenceRole
	Generation() Generation
	CoveredComponents() []ComponentID
	Held(context.Context) (bool, error)
	Release(context.Context) error
}

// FenceProjection identifies one provider-private operation that may run while
// a writer fence is held. It is not a provider ID: providers that share an ID
// still use distinct projections for distinct private fence implementations.
type FenceProjection string

// ProviderFenceOperation is inert provider-owned inspection data. It carries
// no writer-fence methods or cleanup authority. The generic managed fence
// evaluates FenceProjection before entering its held scope, then passes the
// operation only to the matching provider-private executor.
type ProviderFenceOperation interface {
	FenceProjection() FenceProjection
}

// providerFenceExecutor is implemented only by provider-owned inner fences.
// Its method is exported solely so implementations may live in provider
// packages; the managed outer fence deliberately does not implement it.
type providerFenceExecutor interface {
	ExecuteProviderFenceOperation(context.Context, FenceProjection, ProviderFenceOperation) error
}

// InspectProviderFence runs one inert provider-private operation through the
// exact managed fence returned by AcquireWriterFence. No provider fence or
// releasable capability is passed to or returned from the operation. The outer
// fence serializes the operation with Release, revalidates afterward, and
// remains the sole owner of inner-fence and migration-claim cleanup.
func InspectProviderFence(ctx context.Context, fence WriterFence, operation ProviderFenceOperation) error {
	if isNilInterface(operation) {
		return ErrInvalidFence
	}
	projection := operation.FenceProjection()
	if projection == "" {
		return ErrInvalidFence
	}
	baseline, err := snapshotWriterFence(ctx, fence)
	if err != nil {
		return err
	}
	managed, ok := fence.(*managedWriterFence)
	if !ok || managed == nil {
		return ErrInvalidFence
	}
	operationErr := managed.executeProviderFenceOperation(ctx, projection, operation)
	return providerFenceResult(ctx, "inspecting provider writer fence", operationErr, baseline)
}

// FencedInspectionRequest asks a provider to complete inspection under one held fence.
type FencedInspectionRequest struct {
	Target             FenceTarget
	Fence              WriterFence
	ExpectedGeneration Generation
}

// Clone returns a detached fenced inspection request suitable for provider invocation.
// The held fence itself remains caller-owned and is intentionally shared.
func (r FencedInspectionRequest) Clone() FencedInspectionRequest {
	out := r
	out.Target = r.Target.Clone()
	return out
}

// Validate checks that a held writer fence exactly matches the requested target.
func (r FencedInspectionRequest) Validate(ctx context.Context) error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.ExpectedGeneration.Valid() {
		return fmt.Errorf("%w: invalid expected generation", ErrInvalidFence)
	}
	if isNilInterface(r.Fence) {
		return fmt.Errorf("%w: missing writer fence", ErrInvalidFence)
	}
	if !r.Fence.Target().Equal(r.Target) || r.Fence.Generation() != r.ExpectedGeneration || !r.Fence.Role().Valid() {
		return fmt.Errorf("%w: fence does not match request", ErrInvalidFence)
	}
	if !sameComponentIDs(r.Fence.CoveredComponents(), fenceTargetComponentIDs(r.Target)) {
		return fmt.Errorf("%w: fence does not cover every target component", ErrInvalidFence)
	}
	held, err := r.Fence.Held(ctx)
	if err != nil {
		return fmt.Errorf("checking writer fence: %w", err)
	}
	if !held {
		return ErrFenceNotHeld
	}
	return nil
}

// writerFenceSnapshot preserves the exact caller-owned fence facts that must
// remain true while a provider operation is in flight.
type writerFenceSnapshot struct {
	fence      WriterFence
	target     FenceTarget
	role       FenceRole
	generation Generation
	components []ComponentID
}

func snapshotWriterFence(ctx context.Context, fence WriterFence) (writerFenceSnapshot, error) {
	if isNilInterface(fence) {
		return writerFenceSnapshot{}, ErrInvalidFence
	}
	snapshot := writerFenceSnapshot{
		fence:      fence,
		target:     fence.Target().Clone(),
		role:       fence.Role(),
		generation: fence.Generation(),
		components: append([]ComponentID(nil), fence.CoveredComponents()...),
	}
	if err := snapshot.Validate(ctx); err != nil {
		return writerFenceSnapshot{}, err
	}
	return snapshot, nil
}

// Validate confirms that the original fence remains held and retains every
// identity fact observed before the provider call.
func (s writerFenceSnapshot) Validate(ctx context.Context) error {
	if isNilInterface(s.fence) || !s.role.Valid() || !s.generation.Valid() || s.target.Validate() != nil {
		return ErrInvalidFence
	}
	if !s.fence.Target().Equal(s.target) || s.fence.Role() != s.role || s.fence.Generation() != s.generation || !sameComponentIDs(s.fence.CoveredComponents(), s.components) {
		return fmt.Errorf("%w: writer fence changed during provider operation", ErrInvalidFence)
	}
	held, err := s.fence.Held(ctx)
	if err != nil {
		return fmt.Errorf("checking writer fence after provider operation: %w", err)
	}
	if !held {
		return ErrFenceNotHeld
	}
	return nil
}

// checkWriterFenceSnapshots verifies every caller-owned fence after a provider
// call. It deliberately checks all snapshots so an error path cannot conceal a
// second lost exclusion boundary.
func checkWriterFenceSnapshots(ctx context.Context, snapshots ...writerFenceSnapshot) error {
	revalidationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writerFenceRevalidationTimeout)
	defer cancel()
	errs := make([]error, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := snapshot.Validate(revalidationCtx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// providerFenceResult combines a provider result with post-call fence checks.
// Provider errors never skip those checks: callers must learn both that the
// operation failed and that their exclusion boundary may no longer be valid.
func providerFenceResult(ctx context.Context, operation string, providerErr error, snapshots ...writerFenceSnapshot) error {
	fenceErr := checkWriterFenceSnapshots(ctx, snapshots...)
	if providerErr == nil {
		return fenceErr
	}
	return errors.Join(fmt.Errorf("%s: %w", operation, providerErr), fenceErr)
}

// FenceTargetMovedError reports an exact component that changed after Inspect.
type FenceTargetMovedError struct{ Component ComponentID }

// Error implements error without exposing physical identities.
func (e *FenceTargetMovedError) Error() string {
	return fmt.Sprintf("%s: component %q", ErrFenceTargetMoved, e.Component)
}

// Unwrap supports errors.Is.
func (e *FenceTargetMovedError) Unwrap() error { return ErrFenceTargetMoved }

// InspectBinding runs exactly the provider's mutation-free inspection operation.
func InspectBinding(ctx context.Context, provider Provider, spec BindingSpec) (Inspection, error) {
	if err := spec.Validate(); err != nil {
		return Inspection{}, err
	}
	if isNilInterface(provider) {
		return Inspection{}, fmt.Errorf("%w: provider is nil", ErrProviderUnavailable)
	}
	inspection, err := provider.Inspect(ctx, spec)
	if err != nil {
		return Inspection{}, fmt.Errorf("inspecting storage binding %q: %w", spec.Name, err)
	}
	if inspection.Target.Provider != spec.Provider {
		return Inspection{}, fmt.Errorf("%w: inspection provider does not match binding", ErrInvalidInspection)
	}
	if err := inspection.Validate(); err != nil {
		return Inspection{}, fmt.Errorf("%w: %w", ErrInvalidInspection, err)
	}
	return NewInspection(inspection.Target, inspection.Descriptor)
}

// AcquireWriterFence claims the city migration guard, delegates the provider
// lock acquisition, and validates the returned provider-owned fence. The
// returned managed fence releases the provider's inner fence first and then
// releases the claim if the provider did not do so itself.
func AcquireWriterFence(ctx context.Context, guard MigrationGuard, provider WriterFenceAcquirer, request FenceRequest) (WriterFence, error) {
	baseline := request.Clone()
	if err := baseline.Validate(); err != nil {
		return nil, err
	}
	if isNilInterface(provider) {
		return nil, fmt.Errorf("%w: provider is nil", ErrProviderUnavailable)
	}
	claim, err := guard.claim(ctx)
	if err != nil {
		return nil, fmt.Errorf("claiming storage migration guard: %w", err)
	}
	identity, err := claim.Identity()
	if err != nil {
		return nil, releaseAcquisitionClaim(claim, fmt.Errorf("reading storage migration guard claim: %w", err))
	}
	if identity.Generation() != baseline.ExpectedGeneration {
		return nil, releaseAcquisitionClaim(claim, fmt.Errorf("%w: guard generation does not match fence request", ErrInvalidFence))
	}
	if !identity.matchesScope(baseline.GuardScope) {
		return nil, releaseAcquisitionClaim(claim, fmt.Errorf("%w: acquired city guard does not match fence request", ErrMigrationGuardScopeMismatch))
	}
	if err := claim.validateLiveDirectory(); err != nil {
		return nil, releaseAcquisitionClaim(claim, fmt.Errorf("validating storage migration guard directory: %w", err))
	}
	fence, err := provider.AcquireFence(ctx, claim, baseline.Clone())
	if err != nil {
		rejected := fmt.Errorf("acquiring storage writer fence: %w", err)
		if !isNilInterface(fence) {
			return nil, rejectAcquiredFence(ctx, fence, claim, rejected)
		}
		return nil, releaseAcquisitionClaim(claim, rejected)
	}
	if isNilInterface(fence) {
		return nil, releaseAcquisitionClaim(claim, fmt.Errorf("%w: provider returned nil fence", ErrInvalidFence))
	}
	held, err := fence.Held(ctx)
	if err != nil {
		return nil, rejectAcquiredFence(ctx, fence, claim, fmt.Errorf("checking acquired storage writer fence: %w", err))
	}
	if !held {
		return nil, rejectAcquiredFence(ctx, fence, claim, ErrFenceNotHeld)
	}
	if !fence.Target().Equal(baseline.Target) || fence.Role() != baseline.Role || fence.Generation() != baseline.ExpectedGeneration {
		return nil, rejectAcquiredFence(ctx, fence, claim, fmt.Errorf("%w: provider returned mismatched fence", ErrInvalidFence))
	}
	if !sameComponentIDs(fence.CoveredComponents(), baseline.Components) {
		return nil, rejectAcquiredFence(ctx, fence, claim, fmt.Errorf("%w: provider returned mismatched fence coverage", ErrInvalidFence))
	}
	if !claim.Held() {
		return nil, rejectAcquiredFence(ctx, fence, claim, fmt.Errorf("%w: provider released migration guard claim before returning fence", ErrInvalidFence))
	}
	return newManagedWriterFence(fence, claim, baseline), nil
}

// managedWriterFence retains generic cleanup ownership around a provider
// fence. Providers receive the claim during acquisition for provider-specific
// composition, but a broken provider cannot strand it after reporting that its
// inner fence released successfully.
type managedWriterFence struct {
	inner      WriterFence
	claim      MigrationGuardClaim
	target     FenceTarget
	role       FenceRole
	generation Generation
	components []ComponentID

	mu            sync.Mutex
	innerReleased bool
	claimReleased bool
}

func newManagedWriterFence(inner WriterFence, claim MigrationGuardClaim, request FenceRequest) WriterFence {
	return &managedWriterFence{
		inner:      inner,
		claim:      claim,
		target:     request.Target.Clone(),
		role:       request.Role,
		generation: request.ExpectedGeneration,
		components: append([]ComponentID(nil), request.Components...),
	}
}

func (f *managedWriterFence) Target() FenceTarget { return f.target.Clone() }

func (f *managedWriterFence) Role() FenceRole { return f.role }

func (f *managedWriterFence) Generation() Generation { return f.generation }

func (f *managedWriterFence) CoveredComponents() []ComponentID {
	return append([]ComponentID(nil), f.components...)
}

func (f *managedWriterFence) Held(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.innerReleased || f.claimReleased {
		return false, nil
	}
	if isNilInterface(f.inner) || !f.inner.Target().Equal(f.target) || f.inner.Role() != f.role || f.inner.Generation() != f.generation || !sameComponentIDs(f.inner.CoveredComponents(), f.components) {
		return false, ErrInvalidFence
	}
	held, err := f.inner.Held(ctx)
	if err != nil || !held {
		return held, err
	}
	return f.claim.Held(), nil
}

func (f *managedWriterFence) executeProviderFenceOperation(ctx context.Context, projection FenceProjection, operation ProviderFenceOperation) error {
	if f == nil || projection == "" || isNilInterface(operation) {
		return ErrInvalidFence
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.innerReleased || f.claimReleased {
		return ErrFenceNotHeld
	}
	if isNilInterface(f.inner) || !f.inner.Target().Equal(f.target) || f.inner.Role() != f.role || f.inner.Generation() != f.generation || !sameComponentIDs(f.inner.CoveredComponents(), f.components) {
		return ErrInvalidFence
	}
	held, err := f.inner.Held(ctx)
	if err != nil {
		return fmt.Errorf("checking projected provider writer fence: %w", err)
	}
	if !held || !f.claim.Held() {
		return ErrFenceNotHeld
	}
	executor, ok := f.inner.(providerFenceExecutor)
	if !ok || isNilInterface(executor) {
		return ErrInvalidFence
	}
	return executor.ExecuteProviderFenceOperation(ctx, projection, operation)
}

func (f *managedWriterFence) Release(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.innerReleased {
		if err := f.inner.Release(ctx); err != nil {
			return fmt.Errorf("releasing provider storage writer fence: %w", err)
		}
		f.innerReleased = true
	}
	if !f.claimReleased {
		if f.claim.owned() {
			if err := f.claim.Release(); err != nil {
				return fmt.Errorf("releasing storage migration guard claim after writer fence: %w", err)
			}
		}
		f.claimReleased = true
	}
	return nil
}

func releaseAcquisitionClaim(claim MigrationGuardClaim, rejected error) error {
	if err := claim.Release(); err != nil {
		return errors.Join(rejected, fmt.Errorf("releasing rejected storage migration guard claim: %w", err))
	}
	return rejected
}

func rejectAcquiredFence(ctx context.Context, fence WriterFence, claim MigrationGuardClaim, rejected error) error {
	if err := fence.Release(ctx); err != nil {
		return &RejectedWriterFenceCleanupError{
			rejected:   rejected,
			releaseErr: fmt.Errorf("releasing rejected storage writer fence: %w", err),
			fence:      fence,
			claim:      claim,
		}
	}
	if claim.owned() {
		return releaseAcquisitionClaim(claim, rejected)
	}
	return rejected
}

// RejectedWriterFenceCleanupError retains exclusive cleanup ownership when a
// rejected provider fence cannot release its inner resource. The migration
// claim remains live until RetryCleanup succeeds, preventing another writer
// from entering an unknown partial-acquisition state.
type RejectedWriterFenceCleanupError struct {
	rejected   error
	releaseErr error
	fence      WriterFence
	claim      MigrationGuardClaim
	mu         sync.Mutex
	cleaned    bool
}

// Error reports the primary rejection and the failed inner cleanup without
// exposing the cleanup-capable fence as a usable writer fence.
func (e *RejectedWriterFenceCleanupError) Error() string {
	if e == nil {
		return "rejected storage writer fence cleanup"
	}
	return errors.Join(e.rejected, e.releaseErr).Error()
}

// Unwrap exposes both the primary rejection and inner-release failure for
// errors.Is and errors.As.
func (e *RejectedWriterFenceCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.rejected, e.releaseErr}
}

// RetryCleanup retries only cleanup. On a successful inner release it drops
// the retained migration claim, in that order; a repeated successful call is
// a no-op.
func (e *RejectedWriterFenceCleanupError) RetryCleanup(ctx context.Context) error {
	if e == nil || isNilInterface(e.fence) {
		return ErrInvalidFence
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleaned {
		return nil
	}
	if err := e.fence.Release(ctx); err != nil {
		return fmt.Errorf("retrying rejected storage writer fence cleanup: %w", err)
	}
	if e.claim.owned() {
		if err := e.claim.Release(); err != nil {
			return fmt.Errorf("releasing storage migration guard claim after fence cleanup: %w", err)
		}
	}
	e.cleaned = true
	return nil
}

func fenceTargetComponentIDs(target FenceTarget) []ComponentID {
	components := make([]ComponentID, len(target.Components))
	for index, component := range target.Components {
		components[index] = component.ID
	}
	return components
}

func sameComponentIDs(left, right []ComponentID) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet := make(map[ComponentID]struct{}, len(left))
	for _, component := range left {
		if _, exists := leftSet[component]; exists {
			return false
		}
		leftSet[component] = struct{}{}
	}
	for _, component := range right {
		if _, exists := leftSet[component]; !exists {
			return false
		}
	}
	return true
}

// InspectFenced completes descriptor discovery under a held writer fence before Open.
func InspectFenced(ctx context.Context, provider Provider, request FencedInspectionRequest) (Descriptor, error) {
	baseline := request.Clone()
	if err := baseline.Validate(ctx); err != nil {
		return Descriptor{}, err
	}
	if isNilInterface(provider) {
		return Descriptor{}, fmt.Errorf("%w: provider is nil", ErrProviderUnavailable)
	}
	fence, err := snapshotWriterFence(ctx, baseline.Fence)
	if err != nil {
		return Descriptor{}, err
	}
	descriptor, providerErr := provider.InspectFenced(ctx, baseline.Clone())
	if err := providerFenceResult(ctx, "performing fenced storage inspection", providerErr, fence); err != nil {
		return Descriptor{}, err
	}
	descriptor = descriptor.Clone()
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	if err := validateDescriptorForTarget(descriptor, baseline.Target); err != nil {
		return Descriptor{}, err
	}
	return descriptor.Clone(), nil
}

func validateDescriptorForTarget(descriptor Descriptor, target FenceTarget) error {
	if descriptor.Provider != target.Provider {
		return fmt.Errorf("%w: provider changed", ErrFenceTargetMoved)
	}
	if !descriptor.Classes().Equal(target.Classes) {
		return fmt.Errorf("%w: descriptor classes changed", ErrFenceTargetMoved)
	}
	if len(descriptor.Components) != len(target.Components) {
		return fmt.Errorf("%w: descriptor component census changed", ErrFenceTargetMoved)
	}
	byID := make(map[ComponentID]ComponentDescriptor, len(descriptor.Components))
	for _, component := range descriptor.Components {
		byID[component.ID] = component
	}
	for _, component := range target.Components {
		actual, found := byID[component.ID]
		if !found || actual.Locator != component.Locator || actual.PhysicalIdentity != component.PhysicalIdentity || !actual.Classes.Equal(component.Classes) {
			return &FenceTargetMovedError{Component: component.ID}
		}
	}
	return nil
}

// WriterFenceAcquirer is the narrow provider-owned mutation boundary used to
// acquire one writer fence under a validated migration-guard claim.
type WriterFenceAcquirer interface {
	AcquireFence(context.Context, MigrationGuardClaim, FenceRequest) (WriterFence, error)
}

// Provider is the complete provider lifecycle contract exposed to storage composition.
type Provider interface {
	WriterFenceAcquirer
	Inspect(context.Context, BindingSpec) (Inspection, error)
	InspectFenced(context.Context, FencedInspectionRequest) (Descriptor, error)
	RetainedGuards() (RetainedGuardLifecycle, bool)
	BindingMigration() (BindingMigrationLifecycle, bool)
	WorkMigration() (WorkMigrationLifecycle, bool)
	Open(context.Context, OpenRequest) (OpenedBinding, error)
}

// ProviderFactory constructs a resource-free immutable provider facade for one
// exact provider ID. New must not open files, sockets, databases, pools, or
// goroutines: operations either scope temporary resources to their call or
// transfer durable ownership through WriterFence, OpenedBinding, or retained
// Work handles.
type ProviderFactory interface {
	ID() ProviderID
	New(BindingSpec) (Provider, error)
}

// ProviderRegistry is an explicit, freezeable, non-global registry of compiled providers.
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[ProviderID]ProviderFactory
	frozen    bool
}

// NewProviderRegistry creates an empty explicit provider registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[ProviderID]ProviderFactory)}
}

// Register adds one factory before the registry is frozen.
func (r *ProviderRegistry) Register(factory ProviderFactory) error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrProviderUnavailable)
	}
	if isNilInterface(factory) {
		return fmt.Errorf("%w: nil provider factory", ErrProviderUnavailable)
	}
	id := factory.ID()
	if err := validateIdentifier("provider ID", string(id)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingSpec, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return ErrProviderRegistryFrozen
	}
	if r.factories == nil {
		r.factories = make(map[ProviderID]ProviderFactory)
	}
	if _, exists := r.factories[id]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateProvider, id)
	}
	r.factories[id] = factory
	return nil
}

// Freeze prevents further registration and enables exact provider resolution.
func (r *ProviderRegistry) Freeze() error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrProviderUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
	return nil
}

// Lookup returns exactly one registered factory by its exact provider ID.
func (r *ProviderRegistry) Lookup(id ProviderID) (ProviderFactory, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: registry is nil", ErrProviderUnavailable)
	}
	if err := validateIdentifier("provider ID", string(id)); err != nil {
		return nil, ErrInvalidProviderID
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.frozen {
		return nil, ErrProviderRegistryNotFrozen
	}
	factory, ok := r.factories[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q (compiled in: %s)", ErrUnknownProvider, id, strings.Join(r.registeredIDsLocked(), ", "))
	}
	return factory, nil
}

// registeredIDsLocked lists the provider IDs compiled into this registry so a
// refusal can enumerate them. "Provider not found" is only actionable next to
// the list it was not found in — otherwise the operator cannot tell a typo from
// a build that never carried the provider.
func (r *ProviderRegistry) registeredIDsLocked() []string {
	ids := make([]string, 0, len(r.factories))
	for id := range r.factories {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []string{"none"}
	}
	return ids
}

// New validates a binding and constructs only its exact registered provider.
func (r *ProviderRegistry) New(spec BindingSpec) (Provider, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	factory, err := r.Lookup(spec.Provider)
	if err != nil {
		return nil, err
	}
	provider, err := factory.New(spec)
	if err != nil {
		if !isNilInterface(provider) {
			return nil, fmt.Errorf("%w: factory returned a provider with an error", ErrProviderFactoryContract)
		}
		return nil, fmt.Errorf("constructing storage provider %q: %w", spec.Provider, err)
	}
	if isNilInterface(provider) {
		return nil, fmt.Errorf("%w: provider %q returned nil", ErrProviderUnavailable, spec.Provider)
	}
	return provider, nil
}

func validateIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", field)
	}
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '-' || runeValue == '_' || runeValue == '.' {
			continue
		}
		return fmt.Errorf("%s has invalid characters", field)
	}
	return nil
}

func validateSecretFree(field, value string) error {
	if value == "" {
		return nil
	}
	if containsCredentialMaterial(value) {
		return newSecretMaterialError(field)
	}
	return nil
}

func containsCredentialMaterial(value string) bool {
	return containsCredentialMaterialAtDepth(value, 0)
}

const maxCredentialDecodeDepth = 4

func containsCredentialMaterialAtDepth(value string, depth int) bool {
	if containsPrivateKeyPEM(value) {
		return true
	}
	if jsonCandidate(value) {
		containsSecret, err := jsonContainsCredentialKey(value)
		return err != nil || containsSecret
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return hasHierarchicalURLPrefix(value) || containsURLPathCredentialAssignment(value)
	}
	if containsURLPathCredentialAssignment(parsed.Path) {
		return true
	}
	if parsed.Scheme != "" {
		if isCredentialKey(parsed.Scheme) || parsed.User != nil || containsOpaqueURLCredentials(parsed.Opaque) || containsURLPathCredentialAssignment(parsed.Opaque) {
			return true
		}
		if depth < maxCredentialDecodeDepth && (containsCredentialMaterialAtDepth(parsed.Opaque, depth+1) || containsCredentialMaterialAtDepth(parsed.Fragment, depth+1)) {
			return true
		}
		if parsed.RawQuery == "" {
			return false
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return true
		}
		for key, values := range query {
			if isCredentialKey(key) {
				return true
			}
			for _, queryValue := range values {
				if containsCredentialMaterialAtDepth(queryValue, depth+1) {
					return true
				}
			}
		}
		return false
	}
	if depth < maxCredentialDecodeDepth {
		decoded, err := url.QueryUnescape(value)
		if err == nil && decoded != value && containsCredentialMaterialAtDepth(decoded, depth+1) {
			return true
		}
	}
	return containsCredentialAssignment(value)
}

func hasHierarchicalURLPrefix(value string) bool {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || colon+1 >= len(value) || value[colon+1] != '/' {
		return false
	}
	for index := 0; index < colon; index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (index > 0 && ((character >= '0' && character <= '9') || character == '+' || character == '-' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func containsOpaqueURLCredentials(opaque string) bool {
	authority := opaque
	if slash := strings.IndexByte(authority, '/'); slash >= 0 {
		authority = authority[:slash]
	}
	at := strings.LastIndexByte(authority, '@')
	return at > 0
}

func containsURLPathCredentialAssignment(path string) bool {
	for depth := 0; depth < maxCredentialDecodeDepth; depth++ {
		for _, segment := range strings.Split(path, "/") {
			if containsCredentialAssignment(segment) {
				return true
			}
		}
		decoded, changed := unescapeValidURLPathBytes(path)
		if !changed {
			return false
		}
		path = decoded
	}
	return false
}

func unescapeValidURLPathBytes(path string) (string, bool) {
	decoded, err := url.PathUnescape(path)
	if err == nil {
		return decoded, decoded != path
	}

	var builder strings.Builder
	builder.Grow(len(path))
	changed := false
	for index := 0; index < len(path); index++ {
		if path[index] != '%' || index+2 >= len(path) {
			builder.WriteByte(path[index])
			continue
		}
		high, highOK := urlHexNibble(path[index+1])
		low, lowOK := urlHexNibble(path[index+2])
		if !highOK || !lowOK {
			builder.WriteByte(path[index])
			continue
		}
		builder.WriteByte(high<<4 | low)
		index += 2
		changed = true
	}
	if !changed {
		return path, false
	}
	return builder.String(), true
}

func urlHexNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func containsPrivateKeyPEM(value string) bool {
	upper := strings.ToUpper(value)
	for {
		begin := strings.Index(upper, "-----BEGIN ")
		if begin < 0 {
			return false
		}
		label := upper[begin+len("-----BEGIN "):]
		end := strings.Index(label, "-----")
		if end < 0 {
			return false
		}
		if strings.Contains(label[:end], "PRIVATE KEY") {
			return true
		}
		upper = label[end+len("-----"):]
	}
}

func validateCanonicalSHA256Digest(field, value string) error {
	if err := validateSecretFree(field, value); err != nil {
		return err
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return fmt.Errorf("%s is not a canonical sha256 digest", field)
	}
	for _, character := range value[len(prefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("%s is not a canonical sha256 digest", field)
		}
	}
	return nil
}

func containsCredentialAssignment(value string) bool {
	for index := 0; index < len(value); {
		if !assignmentBoundary(value, index) || !assignmentKeyCharacter(value[index]) {
			index++
			continue
		}
		start := index
		for index < len(value) && assignmentKeyCharacter(value[index]) {
			index++
		}
		key := value[start:index]
		for index < len(value) && isSpace(value[index]) {
			index++
		}
		if index < len(value) && (value[index] == '=' || value[index] == ':') && isCredentialKey(key) {
			return true
		}
	}
	return false
}

func assignmentBoundary(value string, index int) bool {
	if index == 0 {
		return true
	}
	switch value[index-1] {
	case ' ', '\t', '\r', '\n', '&', ';', ',', '?', '#', '(', '[', '{':
		return true
	default:
		return false
	}
}

func assignmentKeyCharacter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-'
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\r' || character == '\n'
}

func jsonCandidate(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

func jsonContainsCredentialKey(value string) (bool, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	containsSecret, err := scanJSONValue(decoder)
	if err != nil {
		return false, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("trailing JSON value")
		}
		return false, err
	}
	return containsSecret, nil
}

func scanJSONValue(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		stringValue, ok := token.(string)
		return ok && containsCredentialMaterial(stringValue), nil
	}
	switch delimiter {
	case '{':
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return false, errors.New("JSON object key is not a string")
			}
			if isCredentialKey(key) {
				return true, nil
			}
			containsSecret, err := scanJSONValue(decoder)
			if err != nil || containsSecret {
				return containsSecret, err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return false, err
		}
		if end != json.Delim('}') {
			return false, errors.New("JSON object is not closed")
		}
		return false, nil
	case '[':
		for decoder.More() {
			containsSecret, err := scanJSONValue(decoder)
			if err != nil || containsSecret {
				return containsSecret, err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return false, err
		}
		if end != json.Delim(']') {
			return false, errors.New("JSON array is not closed")
		}
		return false, nil
	default:
		return false, errors.New("unexpected JSON delimiter")
	}
}

func isCredentialKey(key string) bool {
	var normalized strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(key)) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			normalized.WriteRune(character)
		}
	}
	switch normalized.String() {
	case "password", "passwd", "token", "secret", "apikey", "accesstoken", "authtoken", "credential", "credentials",
		"authorization", "clientsecret", "secretkey", "privatekey", "secretaccesskey", "awssecretaccesskey", "awssecretkey", "awssessiontoken":
		return true
	default:
		return false
	}
}

// SecretMaterialError reports a rejected credential-bearing provider value without echoing it.
// Its implementation deliberately retains no caller-controlled context because errors often
// cross logging boundaries outside the storage package.
type SecretMaterialError struct{}

func newSecretMaterialError(string) *SecretMaterialError { return &SecretMaterialError{} }

// Error implements error.
func (e *SecretMaterialError) Error() string {
	return ErrSecretMaterial.Error()
}

// Unwrap supports errors.Is.
func (e *SecretMaterialError) Unwrap() error { return ErrSecretMaterial }

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
