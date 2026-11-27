package beadmeta

import "testing"

func TestGCExempt(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"no metadata", nil, false},
		{"unset", map[string]string{"other": "true"}, false},
		{"empty", map[string]string{GCExemptMetadataKey: ""}, false},
		{"true", map[string]string{GCExemptMetadataKey: "true"}, true},
		{"ParseBool one", map[string]string{GCExemptMetadataKey: "1"}, true},
		{"ParseBool T", map[string]string{GCExemptMetadataKey: "T"}, true},
		{"ParseBool TRUE", map[string]string{GCExemptMetadataKey: "TRUE"}, true},
		{"yes with surrounding space", map[string]string{GCExemptMetadataKey: " yes\n"}, true},
		{"ON in capitals", map[string]string{GCExemptMetadataKey: "ON"}, true},
		{"false", map[string]string{GCExemptMetadataKey: "false"}, false},
		{"zero", map[string]string{GCExemptMetadataKey: "0"}, false},
		{"no", map[string]string{GCExemptMetadataKey: "no"}, false},
		{"off", map[string]string{GCExemptMetadataKey: "off"}, false},
		{"unrecognized", map[string]string{GCExemptMetadataKey: "maybe"}, false},
	}
	for _, tc := range cases {
		if got := GCExempt(tc.meta); got != tc.want {
			t.Errorf("%s: GCExempt(%v) = %v, want %v", tc.name, tc.meta, got, tc.want)
		}
	}
}
