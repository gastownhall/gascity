package api

import "testing"

func TestReadyBeadsCityReadAuthoritative(t *testing.T) {
	cases := []struct {
		name   string
		ready  ReadyBeads
		wantOK bool
	}{
		{
			name:   "complete read is authoritative",
			ready:  ReadyBeads{Partial: false},
			wantOK: true,
		},
		{
			name:   "partial omitting city store is not authoritative",
			ready:  ReadyBeads{Partial: true, PartialErrors: []string{CityReadyPartialLabel + ": read ready: store slow"}},
			wantOK: false,
		},
		{
			name:   "partial losing only a rig store stays authoritative",
			ready:  ReadyBeads{Partial: true, PartialErrors: []string{"rig alpha: read ready: store slow"}},
			wantOK: true,
		},
		{
			name:   "partial flag without recorded errors stays authoritative",
			ready:  ReadyBeads{Partial: true},
			wantOK: true,
		},
		{
			name:   "city failure among several rigs is detected",
			ready:  ReadyBeads{Partial: true, PartialErrors: []string{"rig alpha: boom", CityReadyPartialLabel + ": boom"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ready.CityReadAuthoritative(); got != tc.wantOK {
				t.Fatalf("CityReadAuthoritative() = %v, want %v", got, tc.wantOK)
			}
		})
	}
}
