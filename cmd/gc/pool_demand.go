// Copyright (c) Gas City contributors. SPDX-License-Identifier: Apache-2.0

package main

import "github.com/gastownhall/gascity/internal/sling"

// The pool-demand sentinel is defined canonically in internal/sling
// (pool_demand.go) so that slingFormula — a third writer alongside the
// two gc order paths in this package — can stamp the same pair without
// an upward import. These aliases keep the cmd/gc writers and the
// defaultScaleCheckCounts reader on the shared definition; see the
// internal/sling doc comments for the key/value rationale (including
// why the value must stay non-numeric).
const (
	poolDemandMetadataKey   = sling.PoolDemandMetadataKey
	poolDemandMetadataValue = sling.PoolDemandMetadataValue
)

// poolDemandMetadataPair forwards to sling.PoolDemandMetadataPair, the
// canonical writer-side helper.
func poolDemandMetadataPair() map[string]string {
	return sling.PoolDemandMetadataPair()
}
