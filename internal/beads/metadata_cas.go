// The narrow metadata value-CAS capability seam.
//
// ConditionalWriter (beads.go) bundles four methods: the revision-CAS trio
// (UpdateIfMatch/CloseIfMatch/DeleteIfMatch) plus CompareAndSetMetadataKey.
// The trio needs a backend fence token — a revision that advances on every
// mutation and is never reused. The beads v1.1.0 schema cannot supply one:
// types.Issue carries no revision field (Get().Revision is 0 and never
// advances), the issues DDL has no version column, and updated_at is
// second-granularity — so two same-second writes yield an EQUAL token and a
// stale fence SILENTLY SUCCEEDS, which is the lost update a fence exists to
// prevent. Label mutations never touch updated_at at all. A store-maintained
// counter is not a fence either: the Dolt database is multi-writer (the bd
// CLI, other gascity processes, graph-apply), and a counter only the fencer
// maintains fences nothing.
//
// CompareAndSetMetadataKey needs no such token — it guards on the key's own
// current value — so it is soundly implementable today on stores where the
// trio is not. MetadataCASWriter is that half, split out so a store can
// declare the capability it actually has. Declaring the whole of
// ConditionalWriter just to expose the CAS method would make
// ResolveConditionalWriter RESOLVE under require mode and hand the trio's
// callers a silently-wrong fence — converting today's loud typed refusal into
// exactly the silent legacy write under require that the seam exists to make
// inexpressible. See the condWritesStamp comment in native_dolt_store.go.
//
// Upstream beads #4697 (claim_fence) is the missing backend primitive. When it
// lands, a store can implement the trio soundly and declare ConditionalWriter;
// until then a narrow-only store declares MetadataCASWriter and nothing more.
//
// Resolution is deliberately SEPARATE from ResolveConditionalWriter: this is a
// capability lookup, not the operator-policy seam. It carries no
// conditional_writes mode, because its consumers (target_scope member
// declaration, the D3/D5 lease/claim_generation lane) have no legacy
// unconditional path to fall back to — the CAS is their only correct
// implementation, so gating it on a rollout flag would leave them with nothing
// under the default off. Nothing here can raise enforcement on the trio: a
// narrow writer never satisfies ConditionalWriter, so the trio's callers stay
// exactly as refused as they are today.

package beads

// MetadataCASWriter is the metadata value-CAS half of ConditionalWriter,
// declared on its own so stores that cannot soundly fence on a revision can
// still offer a sound single-key compare-and-set.
//
// The contract is identical to ConditionalWriter.CompareAndSetMetadataKey and
// is verified by the same assertions (beadstest.RunMetadataCASConformance):
// expected == "" matches a key that is absent OR present with the empty value
// (the two states are indistinguishable to callers; release paths write "" to
// clear). Returns (true, nil) on swap, (false, nil) on a genuine value
// mismatch — a lost race is NOT an error — and (false, err) for everything
// else. A store whose conditional writes are disabled at the instance level
// reports ErrConditionalWriteUnsupported from the call, matching how the trio
// behaves on those stores.
//
// Every ConditionalWriter satisfies MetadataCASWriter structurally, so a fully
// capable store serves narrow callers unchanged. The converse cannot hold: Go
// interface satisfaction needs all four methods, which is precisely the
// property that keeps a narrow store out of ConditionalWriter resolution.
type MetadataCASWriter interface {
	CompareAndSetMetadataKey(id, key, expected, next string) (bool, error)
}

// MetadataCASWriterHandleProvider exposes a metadata-CAS handle for stores
// whose capability depends on wrapped runtime state. It mirrors
// ConditionalWriterHandleProvider: a wrapper can delegate the capability
// without claiming the interface globally.
type MetadataCASWriterHandleProvider interface {
	MetadataCASWriterHandle() (MetadataCASWriter, bool)
}

// MetadataCASWriterFor returns the metadata value-CAS capability for store
// when one is available.
//
// It follows wrapper-declared resolution targets (ConditionalWritesResolveTargeter)
// for the same reason ResolveConditionalWriter does: interface-embedding
// wrappers — the cmd/gc policy store, the typed class wrappers in
// class_store.go — do not promote optional capabilities, so a direct assertion
// through them fails and the capability would look absent. As with
// ConditionalWriterFor, this does NOT guess at unwrapping: a wrapper
// participates only by declaring its target.
//
// This is a pure capability lookup and applies no operator policy — see the
// file comment for why the narrow CAS is not mode-gated.
func MetadataCASWriterFor(store Store) (MetadataCASWriter, bool) {
	if store == nil {
		return nil, false
	}
	store = followConditionalWritesResolveTarget(store)
	if writer, ok := store.(MetadataCASWriter); ok {
		return writer, true
	}
	if provider, ok := store.(MetadataCASWriterHandleProvider); ok {
		return provider.MetadataCASWriterHandle()
	}
	return nil, false
}
