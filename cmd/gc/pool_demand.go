// Copyright (c) Gas City contributors. SPDX-License-Identifier: Apache-2.0

package main

// poolDemandMetadataKey is the bead-metadata flag set on a pooled order
// wisp at creation time and read by the supervisor's default scale_check
// path. It exists because PR #1154 added "molecule" and "step" to
// readyExcludeTypes for queue hygiene (workflow containers and formula
// scaffolding stay out of bd ready), leaving the supervisor's
// defaultScaleCheckCounts with no Ready surface for cron-fired pool
// orders. Rather than relax the type filter, the order-side writer
// stamps this key on the wisp and the supervisor takes a second
// metadata-filtered list as a separate demand source.
//
// Writers: doOrderRunWithJSON (cmd_order.go) and
// memoryOrderDispatcher.dispatchOne (order_dispatch.go).
// Reader: defaultScaleCheckCounts (build_desired_state.go).
const poolDemandMetadataKey = "gc.pool_demand"

// poolDemandMetadataValue is the literal value the writers stamp and the
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
// bd round-trip lossless and (b) update poolDemandMetadataPair()'s
// returned map so writers and the equality-match reader stay in sync.
const poolDemandMetadataValue = "order"

// poolDemandMetadataPair returns the metadata map a pool-order writer
// must merge into its UpdateOpts.Metadata alongside the existing
// gc.routed_to write. Writers compose with the routing key:
//
//	if a.Pool != "" {
//	    update.Metadata = map[string]string{"gc.routed_to": pool}
//	    for k, v := range poolDemandMetadataPair() {
//	        update.Metadata[k] = v
//	    }
//	}
//
// The helper exists so adding a second flag in the future (e.g., a
// per-trigger discriminator) does not require auditing every writer.
func poolDemandMetadataPair() map[string]string {
	return map[string]string{poolDemandMetadataKey: poolDemandMetadataValue}
}
