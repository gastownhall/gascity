package session

// ApplyPatch returns a copy of info with a MetadataPatch applied to its
// metadata-derived fields. It is the typed "write-returns-Info" half of the
// session front door (front-door migration Step 6d): the reconciler applies a
// patch to the persisted bead via Store.ApplyPatch and, instead of re-reading
// and re-projecting the whole bead (the raw refreshSessionInfo path) or issuing
// a store Get, folds the SAME patch onto the coherent Info snapshot here.
//
// It is byte-identical to a full re-projection of the patched metadata:
//
//	info.ApplyPatch(p)  ==  InfoFromPersistedBead(bead{Status, Type, Title, ...,
//	                            Metadata: p.Apply(meta)})
//
// for the metadata-derived fields, where info == InfoFromPersistedBead(bead).
// Only fields whose source key appears in the patch are re-derived, from that
// key's raw patch value, using the same per-key logic as InfoFromPersistedBead;
// every other field carries forward unchanged. Bead-level fields (ID, Type,
// Title, Labels, CreatedAt) and the live runtime overlay (Attached, LastActive)
// are never touched by a metadata patch, so they carry forward. Status-derived
// facts (Closed, and the closed-blanking of State) are NOT reconstructable from
// a metadata patch — a status close is a separate refresh case (Store.Get) —
// so ApplyPatch reads the carried-forward Closed and never flips it.
//
// The fold shares one codec table with InfoFromPersistedBead (info_codec.go):
// each key's setter is the SAME closure both directions run, so fold ==
// re-projection by construction. TestInfoApplyPatchMatchesReprojection is kept
// as the equivalence oracle that gates the two against drift, exactly as
// TestSessionClassifierInfoEquivalence guards the classifier siblings.
func (info Info) ApplyPatch(patch MetadataPatch) Info {
	for key, v := range patch {
		// A key InfoFromPersistedBead projects folds through its shared codec
		// setter — the SAME closure the projection runs — so the fold is a
		// re-projection of that one key by construction. Keys the projection
		// does not read (e.g. live_hash, startup_dialog_verified, env.*) miss
		// the index and carry no Info field, keeping ApplyPatch byte-identical
		// to a full re-projection.
		if spec, ok := infoKeyIndex[key]; ok {
			spec.set(&info, v)
		}
	}
	return info
}

// MarkClosed returns a copy of info reflecting an in-memory status close. When
// the reconciler closes a session bead this tick it sets session.Status =
// "closed" on the working bead; the coherent Info snapshot must match without a
// store re-read. Status is the source of exactly the two facts
// InfoFromPersistedBead derives from b.Status == "closed": Closed becomes true,
// and State is blanked (a closed bead carries no runtime state). Every
// metadata-derived field is independent of Status, so it carries forward
// unchanged.
//
// MarkClosed is the status-close counterpart to ApplyPatch, which deliberately
// never flips Closed (a metadata patch cannot carry a status change). Together
// they are the write-returns-Info snapshot refresh (front-door migration Step
// 6d): ApplyPatch folds a metadata batch, MarkClosed folds an in-memory status
// close, each byte-identical to re-projecting the mutated bead — so the
// reconciler refreshes the snapshot from the mutation it just applied instead of
// re-projecting the raw working bead or issuing a store Get.
//
// TestInfoMarkClosedMatchesReprojection is the equivalence oracle: for any open
// bead b, InfoFromPersistedBead(b).MarkClosed() equals
// InfoFromPersistedBead(b with Status "closed").
func (info Info) MarkClosed() Info {
	info.Closed = true
	info.State = "" // closed beads have no runtime state
	return info
}
