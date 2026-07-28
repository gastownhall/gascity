package config

import (
	"strings"
	"testing"
)

func TestEventsScanBudgetConfigRoundTrip(t *testing.T) {
	maxBytes := int64(654321)
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Events: EventsConfig{
			Provider: "file",
			ScanBudget: EventsScanBudgetConfig{
				MaxArchiveBytesPerRequest: &maxBytes,
			},
		},
	}

	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"[events.scan_budget]",
		"max_archive_bytes_per_request = 654321",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("marshaled config missing %q:\n%s", want, text)
		}
	}

	decoded, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sb := decoded.Events.ScanBudget
	if sb.MaxArchiveBytesPerRequest == nil || *sb.MaxArchiveBytesPerRequest != 654321 {
		t.Fatalf("MaxArchiveBytesPerRequest = %v, want 654321", sb.MaxArchiveBytesPerRequest)
	}
}

func TestEventsScanBudgetConfigDefaults(t *testing.T) {
	var sb EventsScanBudgetConfig
	if got := sb.MaxArchiveBytesPerRequestOrDefault(); got != DefaultEventsScanBudgetMaxArchiveBytes {
		t.Fatalf("MaxArchiveBytesPerRequestOrDefault() = %d, want %d", got, DefaultEventsScanBudgetMaxArchiveBytes)
	}
}

func TestEventsScanBudgetConfigExplicitValueOverridesDefault(t *testing.T) {
	maxBytes := int64(1)
	sb := EventsScanBudgetConfig{MaxArchiveBytesPerRequest: &maxBytes}
	if got := sb.MaxArchiveBytesPerRequestOrDefault(); got != 1 {
		t.Fatalf("MaxArchiveBytesPerRequestOrDefault() = %d, want 1 (explicit override)", got)
	}
}

// Unlike its rotation-config siblings (MaxSize, ArchiveRetainAge), a
// non-positive scan budget must NOT mean "unlimited" — that would silently
// reintroduce the unbounded archive walk this knob exists to bound. It
// falls back to the safe default instead.
func TestEventsScanBudgetConfigNonPositiveValueFallsBackToDefault(t *testing.T) {
	for _, maxBytes := range []int64{0, -1, -134217728} {
		sb := EventsScanBudgetConfig{MaxArchiveBytesPerRequest: &maxBytes}
		if got := sb.MaxArchiveBytesPerRequestOrDefault(); got != DefaultEventsScanBudgetMaxArchiveBytes {
			t.Fatalf("MaxArchiveBytesPerRequestOrDefault() with configured %d = %d, want default %d", maxBytes, got, DefaultEventsScanBudgetMaxArchiveBytes)
		}
	}
}
