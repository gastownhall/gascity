package beads

// WithLogicalReadTier makes the cache answer every read that leaves its tier
// at the zero value (TierIssues) across BOTH tiers, the question a city's work
// stores have always answered.
//
// # Why a class binding needs it
//
// A work store sits behind cmd/gc's bead-policy layer, which rewrites a
// TierIssues read to TierBoth before it reaches anything (expandPolicyReadTier),
// so a work-store read has never been tier-scoped no matter what its caller
// asked. On a single-store city the graph, session and other class reads ARE
// work-store reads, so the code that issues them was written — and tested —
// against that answer. A split city serves those classes from a binding the
// storage routes hand out with no policy layer, so the very same read asks the
// binding the narrow question and the ephemeral tier drops out silently: an
// agent running a wisp step reads as not running on /status, /agents and the
// dashboard, and the deps of a wisp root come back without its ephemeral
// children (FederatedReadTier in query.go documents the same hole for the
// federated readers, which state their tier; these did not).
//
// Fixing each read would leave the next one to rediscover the hole. The
// controller puts this cache over the binding for every reader it serves
// (cmd/gc class_store_cache.go), so widening here, once, gives every one of
// them the answer a single-store city gives, and leaves an explicit TierWisps
// or TierBoth exactly as asked.
//
// It is an option rather than the cache's default because a cache over a work
// store sits under the policy layer, which already widens: the work path is
// unchanged either way, and a cache built directly over some other store keeps
// the tier it is asked for.
func WithLogicalReadTier() CachingStoreOption {
	return func(c *CachingStore) { c.logicalReadTier = true }
}

// readTier is the tier a read is answered at: TierIssues widened to TierBoth
// under WithLogicalReadTier, anything else as asked.
func (c *CachingStore) readTier(tier TierMode) TierMode {
	if c.logicalReadTier && tier == TierIssues {
		return TierBoth
	}
	return tier
}

// listTier applies readTier to a list query.
func (c *CachingStore) listTier(query ListQuery) ListQuery {
	query.TierMode = c.readTier(query.TierMode)
	return query
}

// readyTier applies readTier to a ready query.
func (c *CachingStore) readyTier(query ReadyQuery) ReadyQuery {
	query.TierMode = c.readTier(query.TierMode)
	return query
}

// readyArgsTier applies readTier to Ready's variadic query. A zero query stays
// zero unless the tier changed, so an unwidened cache keeps its argument-free
// fast path byte for byte.
func (c *CachingStore) readyArgsTier(query []ReadyQuery) []ReadyQuery {
	if !c.logicalReadTier {
		return query
	}
	return []ReadyQuery{c.readyTier(readyQueryFromArgs(query))}
}

// optsTier restates opts' tier as readTier answers it.
func (c *CachingStore) optsTier(opts []QueryOpt) []QueryOpt {
	if !c.logicalReadTier || TierModeFromOpts(opts) != TierIssues {
		return opts
	}
	return append(append([]QueryOpt(nil), opts...), WithBothTiers)
}
