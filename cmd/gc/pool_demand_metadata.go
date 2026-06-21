package main

const (
	poolDemandMetadataKey   = "gc.pool_demand"
	poolDemandMetadataValue = "order"
)

func poolDemandMetadataPair() map[string]string {
	return map[string]string{
		poolDemandMetadataKey: poolDemandMetadataValue,
	}
}
