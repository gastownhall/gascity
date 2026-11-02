package bdflags

import (
	"reflect"
	"testing"
)

func TestClaimFlag(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want string
	}{
		{"update", []string{"x", "--claim"}, "--claim"},
		{"update", []string{"x", "--claim=true"}, "--claim"},
		{"update", []string{"x", "--claim=false"}, ""},
		{"update", []string{"x", "--claim=0"}, ""},
		{"update", []string{"x", "--claim=maybe"}, "--claim"}, // unparsable: set
		{"update", []string{"x", "--claim", "--claim=false"}, ""},
		{"update", []string{"x", "--claim=false", "--claim"}, "--claim"},
		{"update", []string{"x", "--", "--claim"}, ""},
		{"update", []string{"x", "--append-notes", "--claim"}, ""}, // a value
		{"ready", []string{"--claim"}, "--claim"},
		{"close", []string{"x", "--claim-next"}, "--claim-next"},
		{"close", []string{"x", "--continue"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto"}, ""},
		{"close", []string{"x", "--continue", "--no-auto=true"}, ""},
		{"close", []string{"x", "--continue", "--no-auto=0"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=f"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=F"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto=nope"}, "--continue"}, // unparsable: unset
		{"close", []string{"x", "--continue", "--no-auto", "--no-auto=false"}, "--continue"},
		{"close", []string{"x", "--continue", "--reason", "--no-auto"}, "--continue"},
		{"close", []string{"x", "--continue=false", "--claim-next=0"}, ""},
		{"close", []string{"x"}, ""},
		{"show", []string{"--claim"}, ""},
	}
	for _, tc := range cases {
		got, ok := ClaimFlag(tc.sub, tc.args)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("ClaimFlag(%q, %v) = %q, %v; want %q", tc.sub, tc.args, got, ok, tc.want)
		}
	}
}

func TestBoolFlagValueAndFlagValues(t *testing.T) {
	vf := map[string]bool{"--append-notes": true}
	if p, on, v := BoolFlagValue([]string{"--help=false"}, vf, "--help", "-h"); !p || on || !v {
		t.Errorf("--help=false = %v %v %v", p, on, v)
	}
	if p, _, _ := BoolFlagValue([]string{"--append-notes", "--help"}, vf, "--help", "-h"); p {
		t.Error("a --help in a value position read as the flag")
	}
	if p, on, v := BoolFlagValue([]string{"-h", "-h=0"}, vf, "--help", "-h"); !p || on || !v {
		t.Errorf("last -h=0 = %v %v %v", p, on, v)
	}
	got := FlagValues([]string{"--actor", "a", "--append-notes", "--actor", "--actor=b", "--", "--actor", "c"}, vf, "--actor")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("FlagValues = %v", got)
	}
}
