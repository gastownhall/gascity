package beadmeta

import "testing"

func TestAwaitsHuman(t *testing.T) {
	const seat = "gcg-session-9110f395"
	cases := []struct {
		name string
		meta map[string]string
		seat string
		want bool
	}{
		{"no metadata", nil, seat, false},
		{"not asked", map[string]string{"pr_review.approval_mail_id": "m-1"}, seat, false},
		{"this seat asked", map[string]string{AwaitingHumanMetadataKey: seat}, seat, true},
		{"surrounding space", map[string]string{AwaitingHumanMetadataKey: " " + seat + "\n"}, " " + seat, true},
		// A claim released and taken by another seat is that seat's work:
		// the key names the seat that asked, not the step.
		{"another seat asked", map[string]string{AwaitingHumanMetadataKey: "gcg-session-dead"}, seat, false},
		{"cleared", map[string]string{AwaitingHumanMetadataKey: ""}, seat, false},
		{"no seat to hold it", map[string]string{AwaitingHumanMetadataKey: ""}, "", false},
	}
	for _, tc := range cases {
		if got := AwaitsHuman(tc.meta, tc.seat); got != tc.want {
			t.Errorf("%s: AwaitsHuman(%v, %q) = %v, want %v", tc.name, tc.meta, tc.seat, got, tc.want)
		}
	}
}
