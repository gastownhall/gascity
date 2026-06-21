package progname

import "testing"

func TestDefaultValue(t *testing.T) {
	// Reset to default for this test.
	old := name
	name = "gc"
	defer func() { name = old }()

	if got := Get(); got != "gc" {
		t.Errorf("Get() = %q, want %q", got, "gc")
	}
}

func TestSetChangesValue(t *testing.T) {
	old := name
	defer func() { name = old }()

	Set("gascity")
	if got := Get(); got != "gascity" {
		t.Errorf("after Set(%q), Get() = %q", "gascity", got)
	}
}

func TestSetThenGetRoundTrip(t *testing.T) {
	old := name
	defer func() { name = old }()

	for _, want := range []string{"gc", "gt", "my-binary", ""} {
		Set(want)
		if got := Get(); got != want {
			t.Errorf("Set(%q); Get() = %q", want, got)
		}
	}
}
