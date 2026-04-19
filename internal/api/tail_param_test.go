package api

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// TailParam must distinguish three wire states:
//   - absent (Tail == "")   → provided=false, so handler applies default
//   - "0"                   → provided=true, n=0 (return all segments)
//   - "N" where N>0         → provided=true, n=N
//
// A prior refactor typed Tail as int and conflated absent with 0, which
// silently broke the "tail=0 means return all" contract.
func TestTailParamCompactionsDistinguishesAbsentFromZero(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantN        int
		wantProvided bool
	}{
		{"absent", "", 0, false},
		{"zero", "0", 0, true},
		{"positive", "3", 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := TailParam{Tail: tc.raw}
			n, provided := tp.Compactions()
			if n != tc.wantN {
				t.Errorf("n = %d, want %d", n, tc.wantN)
			}
			if provided != tc.wantProvided {
				t.Errorf("provided = %v, want %v", provided, tc.wantProvided)
			}
		})
	}
}

// Resolve must reject malformed values at the Huma boundary so the
// handler never sees a bad Tail string.
func TestTailParamResolveRejectsGarbage(t *testing.T) {
	cases := []string{"abc", "-1", "1.5", " "}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			tp := TailParam{Tail: raw}
			errs := tp.Resolve(huma.Context(nil))
			if len(errs) == 0 {
				t.Errorf("Resolve(%q) accepted malformed value", raw)
			}
		})
	}
}

func TestTailParamResolveAcceptsValid(t *testing.T) {
	cases := []string{"", "0", "1", "10", "9999"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			tp := TailParam{Tail: raw}
			if errs := tp.Resolve(huma.Context(nil)); len(errs) != 0 {
				t.Errorf("Resolve(%q) rejected valid value: %v", raw, errs)
			}
		})
	}
}
