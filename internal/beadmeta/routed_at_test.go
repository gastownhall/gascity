package beadmeta

import (
	"testing"
	"time"
)

func TestRoutedAtRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 19, 21, 4, 5, 0, time.FixedZone("x", 3600))
	v := FormatRoutedAt(at)
	if v != "2026-11-19T20:04:05Z" {
		t.Fatalf("FormatRoutedAt = %q, want UTC RFC3339", v)
	}
	got, ok := ParseRoutedAt("  " + v + " ")
	if !ok || !got.Equal(at) {
		t.Fatalf("ParseRoutedAt(%q) = %v, %v; want %v, true", v, got, ok, at)
	}
	for _, bad := range []string{"", "   ", "yesterday", "2026-11-19"} {
		if _, ok := ParseRoutedAt(bad); ok {
			t.Fatalf("ParseRoutedAt(%q) ok, want false", bad)
		}
	}
}
