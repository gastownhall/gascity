package runtime

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// DrainAckMetadataKey records that a runtime acknowledged its drain request.
	DrainAckMetadataKey = "GC_DRAIN_ACK"
	// DrainAckSourceMetadataKey records who authored the drain acknowledgement.
	DrainAckSourceMetadataKey = "GC_DRAIN_ACK_SOURCE"
	// DrainAckReasonMetadataKey records the lifecycle reason for an acknowledgement.
	DrainAckReasonMetadataKey = "GC_DRAIN_REASON"
	// DrainAckGenerationMetadataKey pins an acknowledgement to a session generation.
	DrainAckGenerationMetadataKey = "GC_DRAIN_GENERATION"
	// DrainAckAwakeEpochMetadataKey pins an acknowledgement to an awake interval.
	DrainAckAwakeEpochMetadataKey = "GC_DRAIN_AWAKE_EPOCH"
)

func drainAckMetadataKeys() [5]string {
	return [5]string{
		DrainAckMetadataKey,
		DrainAckSourceMetadataKey,
		DrainAckReasonMetadataKey,
		DrainAckGenerationMetadataKey,
		DrainAckAwakeEpochMetadataKey,
	}
}

// ClearDrainAckMetadata removes the complete acknowledgement/provenance family
// for name. It deliberately does not remove GC_DRAIN, whose operator request
// has a separate lifetime. A missing session is already clean; every other
// metadata failure is returned so a caller can fail closed before launching a
// replacement runtime.
func ClearDrainAckMetadata(meta MetaStore, name string) error {
	if meta == nil {
		return fmt.Errorf("runtime metadata store is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("runtime session name is empty")
	}

	var errs []error
	for _, key := range drainAckMetadataKeys() {
		if err := meta.RemoveMeta(name, key); err != nil && !IsSessionGone(err) {
			errs = append(errs, fmt.Errorf("removing %s for session %q: %w", key, name, err))
		}
	}
	return errors.Join(errs...)
}
