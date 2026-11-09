package beadmeta

import "testing"

func TestAwaitsHuman(t *testing.T) {
	const seat = "gcg-session-9110f395"
	cases := []struct {
		name  string
		asker string
		seat  string
		want  bool
	}{
		{"not asked", "", seat, false},
		{"this seat asked", seat, seat, true},
		{"surrounding space", " " + seat + "\n", " " + seat, true},
		// A claim released and taken by another seat is that seat's work:
		// the key names the seat that asked, not the step.
		{"another seat asked", "gcg-session-dead", seat, false},
		{"no seat to hold it", "", "", false},
		{"blank on both sides", " ", "\t", false},
	}
	for _, tc := range cases {
		if got := AwaitsHuman(tc.asker, tc.seat); got != tc.want {
			t.Errorf("%s: AwaitsHuman(%q, %q) = %v, want %v", tc.name, tc.asker, tc.seat, got, tc.want)
		}
	}
}
