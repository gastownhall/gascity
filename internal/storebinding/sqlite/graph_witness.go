package sqlite

// The Graph logical witness (the witness contract).
//
// The witness is the provider-neutral half of a migration proof: a digest over
// a canonical stream of the class's LOGICAL content, carrying no provider,
// format, path, schema version, or physical identity. Two stores holding the
// same graph produce the same digest whatever they are made of, which is what
// makes "the destination equals the source" checkable across a provider
// change. The physical half is the descriptor envelope, and it is never
// compared across providers.
//
// It is computed through the closed storebinding.GraphStore contract, not
// through SQL. That is the point rather than a convenience: a witness built
// from this provider's tables could only ever compare against itself, and an
// exporter reaching past the front door for rows would be the generic-store
// escape AC 2 forbids.
//
// Completeness, not non-emptiness, is what the stream has to prove. Three
// mechanisms do it, each independently blocking: every record family is
// emitted with its count even when the count is zero, so a destination that
// lost a whole family produces a different stream rather than a matching one;
// dependency edges are cross-checked in both directions, so a store that
// imported beads but lost edge state blocks instead of hashing equal; and the
// allocator floors are hashed as logical values, so a destination that
// re-mints from zero fails before it can collide.
//
// References carry a typed kind (the witness contract). The Graph class
// legitimately points outside itself — at Work and Sessions beads, and at
// declared-external targets — so "the endpoint is not in this store" is not by
// itself a fact about damage. Each edge is therefore classified from what its
// endpoints DECLARE, never from what happens to be resident, and the kind is
// hashed alongside the tuple. That ordering is what makes the digest an
// equality proof: an edge contributes the same bytes whether or not the class
// it points into was loaded, so two stores holding the same graph agree even
// when the rest of the StoreSet differs, while a change of kind is a change of
// graph and moves the digest. Resolution is still required where the class is
// the authority — an endpoint in Graph's own namespace must be present, or the
// witness blocks.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// ErrGraphWitnessDamagedState reports graph content that cannot be witnessed
// because it contradicts itself: a dependency edge that resolves in one
// direction but not the other, an edge pointing at a bead that is not there,
// or a duplicated label on one bead. A witness over damaged state would be a
// confident proof of the wrong thing, so it blocks.
var ErrGraphWitnessDamagedState = errors.New("SQLite Graph content cannot be witnessed")

const (
	graphWitnessFamilyBead      = "bead"
	graphWitnessFamilyDep       = "dependency"
	graphWitnessFamilyLiveRoots = "live_root_closure"
	graphWitnessFamilyAllocator = "allocator"

	graphWitnessAllocatorCrossClass = "cross_class_gcg_maximum"
	graphWitnessAllocatorPersisted  = "persisted_graph_floor"
)

// The typed reference kinds of the witness contract. These strings are hashed, so
// they are part of the canonical stream layout and cannot be renamed without a
// new algorithm version.
const (
	// graphRefKindInternal is an edge wholly inside the Graph class. Both
	// endpoints must resolve within Graph; one that does not is damage.
	graphRefKindInternal = "internal"
	// graphRefKindCrossClass is a required reference into another class's
	// namespace (Work, Sessions). Graph is not the authority for it, so the
	// witness records the kind and leaves resolution to the StoreSet-wide
	// fenced census (the fenced-census rule), which is the only reader that can see
	// the other class at all.
	graphRefKindCrossClass = "cross_class_required"
	// graphRefKindExternal is a declared-external or declared-optional
	// reference. It is valid without resolving anywhere — that is what
	// declaring it external means.
	graphRefKindExternal = "external_optional"
)

// graphWitnessExternalScheme is the deployed spelling of a declared-external
// dependency endpoint; the beads issue-lifecycle facade the native store writes
// through resolves only same-prefix dependency targets, so it already accepts
// one without resolving it, and the witness honors the same declaration rather
// than inventing a second convention.
const graphWitnessExternalScheme = "external:"

// GraphAllocatorFloors carries the two semantic allocator values of the Graph
// class. They are hashed as logical numbers; the graph.seqfloor file's bytes
// and identity are envelope facts and are deliberately not in the stream.
type GraphAllocatorFloors struct {
	// CrossClassMaximum is the global maximum plain gcg-N suffix found by the
	// genesis census across every class, not only the Graph file.
	CrossClassMaximum int64
	// PersistedFloor is the value the Graph allocator will actually honor on
	// its next reopen.
	PersistedFloor int64
}

// GraphSemanticWitness computes the Graph class's provider-neutral semantic
// witness over one already-open graph front door.
//
// The caller supplies the allocator floors because their authority is outside
// the class: the cross-class maximum comes from the five-file genesis census,
// and the persisted floor is the value that survives a reopen.
func GraphSemanticWitness(ctx context.Context, graph storebinding.GraphStore, floors GraphAllocatorFloors) (storebinding.SemanticWitness, error) {
	if graph == nil {
		return storebinding.SemanticWitness{}, errors.New("computing Graph witness: graph front door is required")
	}
	if floors.CrossClassMaximum < 0 || floors.PersistedFloor < 0 {
		return storebinding.SemanticWitness{}, fmt.Errorf("computing Graph witness: negative allocator floor %+v", floors)
	}
	if err := ctx.Err(); err != nil {
		return storebinding.SemanticWitness{}, err
	}
	all, err := graph.List(beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return storebinding.SemanticWitness{}, fmt.Errorf("reading Graph beads for witness: %w", err)
	}
	sorted := append([]beads.Bead(nil), all...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	present := make(map[string]struct{}, len(sorted))
	for _, bead := range sorted {
		if _, duplicate := present[bead.ID]; duplicate {
			return storebinding.SemanticWitness{}, fmt.Errorf("%w: bead %s is listed twice", ErrGraphWitnessDamagedState, bead.ID)
		}
		present[bead.ID] = struct{}{}
	}
	deps, err := graphWitnessDependencies(ctx, graph, sorted, present)
	if err != nil {
		return storebinding.SemanticWitness{}, err
	}
	liveRoots := make([]string, 0, len(sorted))
	for _, bead := range sorted {
		if bead.Status != "closed" {
			liveRoots = append(liveRoots, bead.ID)
		}
	}

	stream := &canonicalStream{}
	stream.text(storebinding.SemanticWitnessAlgorithm)
	stream.text(coordclass.ClassGraph.String())
	stream.text(string(graphContract))

	stream.family(graphWitnessFamilyBead, len(sorted))
	for _, bead := range sorted {
		if err := encodeWitnessBead(stream, bead); err != nil {
			return storebinding.SemanticWitness{}, err
		}
	}
	stream.family(graphWitnessFamilyDep, len(deps))
	for _, edge := range deps {
		// The opaque graph-apply payload is part of the edge, so a destination
		// that moved the edges but dropped their payloads must not hash equal.
		// Absent and present-but-empty stay distinguishable.
		payload, carried, err := graph.DepMetadata(edge.dep.IssueID, edge.dep.DependsOnID)
		if err != nil {
			return storebinding.SemanticWitness{}, fmt.Errorf("reading edge metadata %s -> %s for witness: %w", edge.dep.IssueID, edge.dep.DependsOnID, err)
		}
		encodeWitnessDependency(stream, edge, payload, carried)
	}
	stream.family(graphWitnessFamilyLiveRoots, len(liveRoots))
	for _, id := range liveRoots {
		stream.text(id)
	}
	stream.family(graphWitnessFamilyAllocator, 2)
	stream.text(graphWitnessAllocatorCrossClass)
	stream.number(floors.CrossClassMaximum)
	stream.text(graphWitnessAllocatorPersisted)
	stream.number(floors.PersistedFloor)

	witness := storebinding.SemanticWitness{
		Version:   1,
		Class:     coordclass.ClassGraph,
		Contract:  graphContract,
		Algorithm: storebinding.SemanticWitnessAlgorithm,
		Digest:    stream.digest(),
		Families: []storebinding.WitnessFamilyCount{
			{Name: graphWitnessFamilyBead, Count: len(sorted)},
			{Name: graphWitnessFamilyDep, Count: len(deps)},
			{Name: graphWitnessFamilyLiveRoots, Count: len(liveRoots)},
			{Name: graphWitnessFamilyAllocator, Count: 2},
		},
	}
	if err := witness.Validate(); err != nil {
		return storebinding.SemanticWitness{}, err
	}
	return witness, nil
}

// graphWitnessEdge is one dependency edge together with the typed kind that
// says what the edge claims to be. The kind is derived from the endpoints'
// declared namespaces alone, so it is a property of the graph rather than of
// the reader's view of it.
type graphWitnessEdge struct {
	dep  beads.Dep
	kind string
}

// graphWitnessDependencies collects every edge from both sides and requires
// the two views to agree wherever both views can see it. Reading only the down
// direction would hash a store whose reverse index had lost rows as equal to a
// healthy one, and would miss entirely an inbound reference from outside the
// class.
//
// Only an edge with both endpoints resident is visible from both sides: the
// forward index is read per resident source and the reverse index per resident
// target. So the two-sided cross-check applies exactly to internal edges, and a
// one-sided sighting is required to be explained by an endpoint that lives
// outside Graph — never by a missing index row.
func graphWitnessDependencies(ctx context.Context, graph storebinding.GraphStore, all []beads.Bead, present map[string]struct{}) ([]graphWitnessEdge, error) {
	down := map[string]int{}
	up := map[string]int{}
	seen := map[string]beads.Dep{}
	record := func(dep beads.Dep, counts map[string]int) error {
		if err := requireResolvableGraphEndpoints(dep, present); err != nil {
			return err
		}
		key := witnessDepKey(dep)
		counts[key]++
		seen[key] = dep
		return nil
	}
	for _, bead := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outgoing, err := graph.DepList(bead.ID, "down")
		if err != nil {
			return nil, fmt.Errorf("listing dependencies of %s for witness: %w", bead.ID, err)
		}
		for _, dep := range outgoing {
			if err := record(dep, down); err != nil {
				return nil, err
			}
		}
		incoming, err := graph.DepList(bead.ID, "up")
		if err != nil {
			return nil, fmt.Errorf("listing dependents of %s for witness: %w", bead.ID, err)
		}
		for _, dep := range incoming {
			if err := record(dep, up); err != nil {
				return nil, err
			}
		}
	}
	var collected []graphWitnessEdge
	for key, dep := range seen {
		count, err := witnessEdgeMultiplicity(dep, present, down[key], up[key])
		if err != nil {
			return nil, err
		}
		kind := graphReferenceKind(dep)
		for repeat := 0; repeat < count; repeat++ {
			collected = append(collected, graphWitnessEdge{dep: dep, kind: kind})
		}
	}
	sort.Slice(collected, func(i, j int) bool {
		return witnessDepKey(collected[i].dep) < witnessDepKey(collected[j].dep)
	})
	return collected, nil
}

// witnessEdgeMultiplicity reconciles one edge's forward and reverse sightings
// and returns how many times it belongs in the stream.
//
// Multiplicity is retained rather than collapsed (the comparison matrix): dropping
// one edge of a duplicated pair has to change the digest.
func witnessEdgeMultiplicity(dep beads.Dep, present map[string]struct{}, forward, reverse int) (int, error) {
	_, sourceResident := present[dep.IssueID]
	_, targetResident := present[dep.DependsOnID]
	if sourceResident && targetResident {
		if forward != reverse {
			return 0, fmt.Errorf("%w: edge %s appears %d times forward and %d times in reverse", ErrGraphWitnessDamagedState, witnessDepKey(dep), forward, reverse)
		}
		return forward, nil
	}
	if forward > 0 && reverse > 0 {
		// Both indexes claim an edge whose endpoint is not in this store. One
		// of the two must be describing a bead that is not the one it is keyed
		// under, which is index corruption, not a cross-class reference.
		return 0, fmt.Errorf("%w: edge %s is indexed in both directions but an endpoint is not resident", ErrGraphWitnessDamagedState, witnessDepKey(dep))
	}
	if forward > 0 {
		return forward, nil
	}
	return reverse, nil
}

// requireResolvableGraphEndpoints blocks on the references Graph is the
// authority for. An endpoint in Graph's own reserved namespace must be in this
// store: nothing else can vouch for it, so a missing one is damaged state and
// fails closed. An endpoint that declares another namespace, or declares
// itself external, is not something this class can resolve and is admitted
// with its kind instead of being misreported as dangling (the witness contract).
//
// An endpoint that declares no namespace at all is treated as internal. An
// unreadable ID is not a license to skip the check — the conservative reading
// is the one that still blocks.
func requireResolvableGraphEndpoints(dep beads.Dep, present map[string]struct{}) error {
	if graphEndpointKind(dep.IssueID) == graphRefKindInternal {
		if _, ok := present[dep.IssueID]; !ok {
			return fmt.Errorf("%w: absent Graph bead %s depends on %s", ErrGraphWitnessDamagedState, dep.IssueID, dep.DependsOnID)
		}
	}
	if graphEndpointKind(dep.DependsOnID) == graphRefKindInternal {
		if _, ok := present[dep.DependsOnID]; !ok {
			return fmt.Errorf("%w: %s depends on absent Graph bead %s", ErrGraphWitnessDamagedState, dep.IssueID, dep.DependsOnID)
		}
	}
	return nil
}

// graphReferenceKind types one edge from its two endpoints. An edge is only
// internal when it is internal at both ends; a single foreign or external
// endpoint makes the whole reference cross the class boundary, and external
// wins over cross-class because a declared-external target is not something
// any census is expected to resolve.
func graphReferenceKind(dep beads.Dep) string {
	source := graphEndpointKind(dep.IssueID)
	target := graphEndpointKind(dep.DependsOnID)
	if source == graphRefKindExternal || target == graphRefKindExternal {
		return graphRefKindExternal
	}
	if source == graphRefKindCrossClass || target == graphRefKindCrossClass {
		return graphRefKindCrossClass
	}
	return graphRefKindInternal
}

// graphEndpointKind classifies one endpoint by what its ID declares. It reads
// only the ID, never the store, so the same endpoint types identically on both
// sides of a migration whatever each side happens to hold.
func graphEndpointKind(id string) string {
	normalized := strings.ToLower(strings.TrimSpace(id))
	if strings.HasPrefix(normalized, graphWitnessExternalScheme) {
		return graphRefKindExternal
	}
	namespace, _, split := strings.Cut(normalized, "-")
	if !split || namespace == "" || namespace == graphIDPrefix {
		return graphRefKindInternal
	}
	return graphRefKindCrossClass
}

func witnessDepKey(dep beads.Dep) string {
	return dep.IssueID + "\x00" + dep.DependsOnID + "\x00" + dep.Type
}

// encodeWitnessDependency writes one edge's logical projection: the tuple, the
// typed kind, and the opaque payload. The kind is hashed rather than merely
// consulted, so a reference that changes from internal to external is a
// different graph and produces a different digest.
func encodeWitnessDependency(stream *canonicalStream, edge graphWitnessEdge, payload string, carried bool) {
	stream.text(edge.dep.IssueID)
	stream.text(edge.dep.DependsOnID)
	stream.text(edge.dep.Type)
	stream.text(edge.kind)
	stream.optionalText(carried, payload)
}

// encodeWitnessBead writes one bead's logical projection.
//
// Two fields of beads.Bead are deliberately absent. Dependencies is the bead's
// own copy of edges the dependency family already carries authoritatively, and
// hashing both would let one store's decode choice change the digest.
// IsBlocked is a denormalized readiness mirror recomputed from those same
// edges — a projection, not state, and one backends legitimately populate
// differently.
func encodeWitnessBead(stream *canonicalStream, bead beads.Bead) error {
	labels, err := canonicalWitnessLabels(bead)
	if err != nil {
		return err
	}
	stream.text(bead.ID)
	stream.text(bead.Title)
	stream.text(bead.Status)
	stream.text(bead.Type)
	stream.optionalNumber(bead.Priority != nil, int64(intOrZero(bead.Priority)))
	stream.optionalTime(bead.CreatedAt)
	stream.optionalTime(bead.UpdatedAt)
	stream.text(bead.Assignee)
	stream.text(bead.From)
	stream.text(bead.ParentID)
	stream.text(bead.Ref)
	stream.text(bead.Description)
	stream.list("needs", bead.Needs)
	stream.list("labels", labels)
	stream.text(witnessStorageTier(bead))
	stream.optionalTime(timeOrZero(bead.DeferUntil))
	stream.number(bead.Revision)
	stream.number(bead.ClaimFence)
	keys := make([]string, 0, len(bead.Metadata))
	for key := range bead.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	stream.family("metadata", len(keys))
	for _, key := range keys {
		stream.text(key)
		stream.text(bead.Metadata[key])
	}
	return nil
}

// canonicalWitnessLabels sorts labels and rejects a duplicate rather than
// collapsing it: a store that lost label multiplicity is damaged, and a
// silently deduplicating witness would hash it equal to a healthy one.
func canonicalWitnessLabels(bead beads.Bead) ([]string, error) {
	labels := append([]string(nil), bead.Labels...)
	sort.Strings(labels)
	for index := 1; index < len(labels); index++ {
		if labels[index] == labels[index-1] {
			return nil, fmt.Errorf("%w: bead %s carries label %q twice", ErrGraphWitnessDamagedState, bead.ID, labels[index])
		}
	}
	return labels, nil
}

// witnessStorageTier renders the bead's logical storage tier. The tier is
// semantic — a wisp that arrives as a durable row is a different graph — while
// the physical table it lands in is a provider fact.
func witnessStorageTier(bead beads.Bead) string {
	switch {
	case bead.Ephemeral:
		return "ephemeral"
	case bead.NoHistory:
		return "no_history"
	default:
		return "history"
	}
}

func intOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// canonicalStream builds the hashed byte stream. Every value is written with
// an explicit type tag and an explicit length, so no two different logical
// contents can produce the same bytes by running together — the property that
// makes the digest an equality proof rather than a checksum.
type canonicalStream struct {
	started bool
	state   []byte
}

const (
	witnessTagText     byte = 't'
	witnessTagNumber   byte = 'n'
	witnessTagAbsent   byte = '0'
	witnessTagPresent  byte = '1'
	witnessTagFamily   byte = 'f'
	witnessTagListHead byte = 'l'
)

func (s *canonicalStream) write(tag byte, payload []byte) {
	if !s.started {
		s.state = append(s.state, storebinding.SemanticWitnessAlgorithm...)
		s.state = append(s.state, 0)
		s.started = true
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
	s.state = append(s.state, tag)
	s.state = append(s.state, length[:]...)
	s.state = append(s.state, payload...)
}

func (s *canonicalStream) text(value string) { s.write(witnessTagText, []byte(value)) }

func (s *canonicalStream) number(value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	s.write(witnessTagNumber, encoded[:])
}

// optionalNumber distinguishes absent from present-and-zero; an unset priority
// and a priority of zero are different logical states.
func (s *canonicalStream) optionalNumber(present bool, value int64) {
	if !present {
		s.write(witnessTagAbsent, nil)
		return
	}
	s.write(witnessTagPresent, nil)
	s.number(value)
}

// optionalText distinguishes absent from present-and-empty, which the witness
// encoding rules require as three distinct states alongside a set value.
func (s *canonicalStream) optionalText(present bool, value string) {
	if !present {
		s.write(witnessTagAbsent, nil)
		return
	}
	s.write(witnessTagPresent, nil)
	s.text(value)
}

// optionalTime normalizes to UTC nanoseconds and treats the zero time as
// absent, which is how the deployed stores spell "never set".
func (s *canonicalStream) optionalTime(value time.Time) {
	s.optionalNumber(!value.IsZero(), value.UTC().UnixNano())
}

// family opens a tagged section with its record count. The count is hashed, so
// a family that lost records — or a whole family an exporter skipped — changes
// the digest structurally rather than by coincidence.
func (s *canonicalStream) family(name string, count int) {
	s.write(witnessTagFamily, []byte(name))
	s.number(int64(count))
}

func (s *canonicalStream) list(name string, values []string) {
	s.write(witnessTagListHead, []byte(name))
	s.number(int64(len(values)))
	for _, value := range values {
		s.text(value)
	}
}

func (s *canonicalStream) digest() string {
	sum := sha256.Sum256(s.state)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GraphWitnessFamilies returns the record families a complete Graph witness
// always carries, in stream order. Status and doctor reads use it to say which
// family is missing from a witness rather than only that a digest differed.
func GraphWitnessFamilies() []string {
	return []string{
		graphWitnessFamilyBead,
		graphWitnessFamilyDep,
		graphWitnessFamilyLiveRoots,
		graphWitnessFamilyAllocator,
	}
}
