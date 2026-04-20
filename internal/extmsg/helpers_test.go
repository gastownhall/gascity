package extmsg

import (
	"math"
	"testing"
)

func TestEncodedMetadataFieldCapacity(t *testing.T) {
	tests := []struct {
		name       string
		fieldCount int
		metaCount  int
		want       int
	}{
		{
			name:       "adds safe metadata capacity",
			fieldCount: 2,
			metaCount:  3,
			want:       5,
		},
		{
			name:       "skips addition when it would overflow",
			fieldCount: math.MaxInt,
			metaCount:  1,
			want:       math.MaxInt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encodedMetadataFieldCapacity(tt.fieldCount, tt.metaCount); got != tt.want {
				t.Fatalf("encodedMetadataFieldCapacity(%d, %d) = %d, want %d", tt.fieldCount, tt.metaCount, got, tt.want)
			}
		})
	}
}

func TestEncodeMetadataFieldsPrefixesMetadataAndSkipsBlankFieldKeys(t *testing.T) {
	meta := map[string]string{
		"channel": "alerts",
	}
	fields := map[string]string{
		"":       "ignored",
		"status": "open",
	}

	got := encodeMetadataFields(meta, fields)

	if got["status"] != "open" {
		t.Fatalf("status = %q, want open", got["status"])
	}
	if got[""] != "" {
		t.Fatalf("blank field key should be omitted, got %q", got[""])
	}
	if got[metadataPrefix+"channel"] != "alerts" {
		t.Fatalf("prefixed metadata = %q, want alerts", got[metadataPrefix+"channel"])
	}
	if got["channel"] != "" {
		t.Fatalf("unprefixed metadata should be omitted, got %q", got["channel"])
	}
	if meta["channel"] != "alerts" {
		t.Fatalf("encodeMetadataFields mutated input metadata: %#v", meta)
	}
}
