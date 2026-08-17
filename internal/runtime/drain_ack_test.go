package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestClearDrainAckMetadataRemovesOnlyAcknowledgementFamily(t *testing.T) {
	sp := NewFake()
	const name = "worker-a"
	values := map[string]string{
		DrainAckMetadataKey:           "1",
		DrainAckSourceMetadataKey:     "agent",
		DrainAckReasonMetadataKey:     "assigned-work-complete",
		DrainAckGenerationMetadataKey: "7",
		DrainAckAwakeEpochMetadataKey: "2026-08-06T12:00:00Z",
		"GC_DRAIN":                    "operator-request",
	}
	for key, value := range values {
		if err := sp.SetMeta(name, key, value); err != nil {
			t.Fatalf("SetMeta(%s): %v", key, err)
		}
	}

	if err := ClearDrainAckMetadata(sp, name); err != nil {
		t.Fatalf("ClearDrainAckMetadata: %v", err)
	}

	for _, key := range []string{
		DrainAckMetadataKey,
		DrainAckSourceMetadataKey,
		DrainAckReasonMetadataKey,
		DrainAckGenerationMetadataKey,
		DrainAckAwakeEpochMetadataKey,
	} {
		got, err := sp.GetMeta(name, key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != "" {
			t.Errorf("%s = %q, want empty", key, got)
		}
	}
	got, err := sp.GetMeta(name, "GC_DRAIN")
	if err != nil {
		t.Fatalf("GetMeta(GC_DRAIN): %v", err)
	}
	if got != "operator-request" {
		t.Fatalf("GC_DRAIN = %q, want preserved", got)
	}
	for _, call := range sp.Calls {
		if call.Method == "RemoveMeta" && call.Key == "GC_DRAIN" {
			t.Fatal("ClearDrainAckMetadata attempted to remove GC_DRAIN")
		}
	}
}

func TestClearDrainAckMetadataIgnoresOnlySessionGone(t *testing.T) {
	sp := NewFake()
	const name = "worker-b"
	sp.RemoveMetaErrors[name] = map[string]error{
		DrainAckMetadataKey:       errors.Join(ErrSessionNotFound, errors.New("box vanished")),
		DrainAckSourceMetadataKey: errors.New("metadata transport unavailable"),
	}

	err := ClearDrainAckMetadata(sp, name)
	if err == nil {
		t.Fatal("ClearDrainAckMetadata error = nil, want non-session-gone failure")
	}
	if strings.Contains(err.Error(), "box vanished") {
		t.Fatalf("error includes ignored session-gone failure: %v", err)
	}
	if !strings.Contains(err.Error(), DrainAckSourceMetadataKey) ||
		!strings.Contains(err.Error(), "metadata transport unavailable") {
		t.Fatalf("error = %v, want keyed non-session-gone failure", err)
	}
}
