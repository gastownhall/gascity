package bdflags

import "testing"

func TestClaimFlag(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want string
	}{
		{"update", []string{"x", "--claim"}, "--claim"},
		{"update", []string{"x", "--claim=true"}, "--claim"},
		{"update", []string{"x", "--claim=false"}, ""},
		{"update", []string{"x", "--", "--claim"}, ""},
		{"ready", []string{"--claim"}, "--claim"},
		{"close", []string{"x", "--claim-next"}, "--claim-next"},
		{"close", []string{"x", "--continue"}, "--continue"},
		{"close", []string{"x", "--continue", "--no-auto"}, ""},
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
