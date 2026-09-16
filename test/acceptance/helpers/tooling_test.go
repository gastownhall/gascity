package acceptancehelpers

import "testing"

func TestRequireSwitchOnReadsTheEnvironment(t *testing.T) {
	const name = "GC_TEST_REQUIRE_SWITCH"
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: " ", want: false},
		{value: "0", want: false},
		{value: "1", want: true},
		{value: "true", want: true},
		{value: " 1 ", want: true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(name, tc.value)
			if got := requireSwitchOn(name); got != tc.want {
				t.Fatalf("requireSwitchOn(%q=%q) = %t, want %t", name, tc.value, got, tc.want)
			}
		})
	}
}
