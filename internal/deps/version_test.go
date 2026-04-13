package deps

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.2.0", "1.1.9", 1},
		{"0.58.0", "0.57.0", 1},
		{"0.57.0", "0.58.0", -1},
		{"1.83.1", "1.82.4", 1},
		{"1.82.4", "1.83.1", -1},
		{"2.0.0", "1.99.99", 1},

		// Normalized inputs.
		{"v1.2.3", "1.2.3", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"V2.0.0", "v1.99.99", 1},
		{"  1.2.3  ", "1.2.3", 0},
		{"1.2.3-rc.1", "1.2.3", 0},
		{"1.2.3-rc.1", "1.2.4", -1},
		{"1.2.3+build.5", "1.2.3", 0},
		{"v1.2.3-rc.1+build.5", "1.2.3", 0},
	}
	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.58.0", [3]int{0, 58, 0}},
		{"1.83", [3]int{1, 83, 0}},
		{"1", [3]int{1, 0, 0}},
		{"", [3]int{0, 0, 0}},
		{"abc", [3]int{0, 0, 0}},

		// Leading v/V prefix.
		{"v1.2.3", [3]int{1, 2, 3}},
		{"V0.58.0", [3]int{0, 58, 0}},

		// Pre-release / build metadata suffixes.
		{"1.2.3-rc.1", [3]int{1, 2, 3}},
		{"1.2.3-beta", [3]int{1, 2, 3}},
		{"1.2.3+build.5", [3]int{1, 2, 3}},
		{"1.2.3-rc.1+build.5", [3]int{1, 2, 3}},
		{"v1.2.3-rc.1", [3]int{1, 2, 3}},

		// Whitespace.
		{"  1.2.3\n", [3]int{1, 2, 3}},
		{"\tv1.2.3 ", [3]int{1, 2, 3}},
	}
	for _, tt := range tests {
		got := ParseVersion(tt.input)
		if got != tt.want {
			t.Errorf("ParseVersion(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
