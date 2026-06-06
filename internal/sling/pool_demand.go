// Copyright (c) Gas City contributors. SPDX-License-Identifier: Apache-2.0

package sling

// PoolDemandMetadataKey is the bead-metadata flag set on a pool-routed
// wisp at creation time and read by the supervisor's default scale_check
// path. It exists because PR #1154 added "molecule" and "step" to
// readyExcludeTypes for queue hygiene (workflow containers and formula
// scaffolding stay out of bd ready), leaving the supervisor's
// defaultScaleCheckCounts with no Ready surface for pool-routed wisps.
// Rather than relax the type filter, each wisp writer stamps this key
// and the supervisor takes a second metadata-filtered list as a
// separate demand source.
//
// Writers: doOrderRunWithJSON (cmd/gc/cmd_order.go),
// memoryOrderDispatcher.dispatchOne (cmd/gc/order_dispatch.go), and
// slingFormula (sling_core.go) for `gc sling --formula` pool targets.
// Reader: defaultScaleCheckCounts (cmd/gc/build_desired_state.go).
const PoolDemandMetadataKey = "gc.pool_demand"

// PoolDemandMetadataValue is the literal value the writers stamp and the
// reader's ListQuery.Metadata equality match looks up.
//
// The value is a stable non-numeric sentinel, not "1", because bd's
// --set-metadata write path infers JSON type from the string — a
// numeric-looking value like "1" lands in the SQL metadata column as
// the JSON integer 1, and the cache's matchesMetadata (caching_store_reads.go)
// does strict string equality, so a "1" writer paired with a "1" reader
// silently misses every bead. Verified empirically when the first
// iteration of this fix shipped with "1": post-build dolt rows showed
// gc.pool_demand stored as INTEGER and scale_check_counts stayed at 0.
//
// Any future change to this value must (a) stay non-numeric to keep the
// bd round-trip lossless and (b) update PoolDemandMetadataPair()'s
// returned map so writers and the equality-match reader stay in sync.
const PoolDemandMetadataValue = "order"

// PoolDemandMetadataPair returns the metadata map a pool-routed wisp
// writer must merge into its UpdateOpts.Metadata alongside the existing
// gc.routed_to write. Writers compose with the routing key:
//
//	if a.Pool != "" {
//	    update.Metadata = map[string]string{"gc.routed_to": pool}
//	    for k, v := range sling.PoolDemandMetadataPair() {
//	        update.Metadata[k] = v
//	    }
//	}
//
// The helper exists so adding a second flag in the future (e.g., a
// per-trigger discriminator) does not require auditing every writer.
func PoolDemandMetadataPair() map[string]string {
	return map[string]string{PoolDemandMetadataKey: PoolDemandMetadataValue}
}
