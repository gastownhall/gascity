package runtime

import "testing"

type scannerlessProvider struct{ Provider }

type conditionalScanner struct {
	*Fake
	can bool
}

func (c conditionalScanner) CanScanProcessTable() bool { return c.can }

func TestAsProcessTableScanner(t *testing.T) {
	cases := []struct {
		name string
		sp   Provider
		want bool
	}{
		{"scanner", NewFake(), true},
		{"no scanner", scannerlessProvider{NewFake()}, false},
		{"conditional can scan", conditionalScanner{Fake: NewFake(), can: true}, true},
		{"conditional cannot scan", conditionalScanner{Fake: NewFake(), can: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scanner, ok := AsProcessTableScanner(tc.sp)
			if ok != tc.want || (scanner != nil) != tc.want {
				t.Fatalf("AsProcessTableScanner = (%v, %v), want ok=%v", scanner, ok, tc.want)
			}
		})
	}
}
