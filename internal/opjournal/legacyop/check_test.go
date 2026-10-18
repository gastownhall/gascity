package legacyop

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// Check refuses, in require, what Admit would refuse without reading the
// conversation: a store that does not open, a payload over the inline limit
// and an invalid client key. It counts each refusal as Admit does and
// degrades a store that does not open; off and auto check nothing.
func TestCheckRefusesInRequireBeforeTheConversationIsKnown(t *testing.T) {
	outage := errors.New("transport: refused")
	for _, tc := range []struct {
		name    string
		req     Request
		open    error
		want    error
		reason  string
		degrade bool
	}{
		{name: "ok", req: msg("k1", "hello")},
		{name: "minted key ok", req: msg("", "hello")},
		{name: "store down", req: msg("k1", "hello"), open: outage, want: ErrUnavailable, reason: ReasonStoreUnavailable, degrade: true},
		{name: "payload too large", req: msg("k1", strings.Repeat("x", opjournal.MaxInlinePayload+1)), want: opjournal.ErrPayloadTooLarge, reason: ReasonPayloadTooLarge},
		{name: "invalid key", req: msg(strings.Repeat("k", 513), "hello"), want: opjournal.ErrInvalidRequest, reason: ReasonInvalidKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []gate.Mode{gate.Off, gate.Auto, gate.Require} {
				counts := captureCounts(t)
				e := newEnv(t)
				r := apiRecorder(e, mode)
				if tc.open != nil {
					r.Open = func() (*opjournal.Store, error) { return nil, tc.open }
				}
				var degrades []Degrade
				r.Degraded = func(d Degrade) { degrades = append(degrades, d) }
				err := r.Check(tc.req)
				if mode != gate.Require || tc.want == nil {
					if err != nil || len(degrades) != 0 || len(counts()) != 0 {
						t.Fatalf("%s: Check = %v, degrades %v, counts %v; want nothing", mode, err, degrades, counts())
					}
					continue
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("Check = %v, want %v", err, tc.want)
				}
				if got := counts(); len(got) != 1 || got[0] != (countedStep{"message", "api", StageAdmit, ResultRefused, tc.reason}) {
					t.Fatalf("counts = %+v", got)
				}
				if tc.degrade != (len(degrades) == 1) || tc.degrade && (degrades[0].Stage != StageOpen || degrades[0].Mode != gate.Require) {
					t.Fatalf("degrades = %+v, want degrade %v", degrades, tc.degrade)
				}
				if n := len(e.rec.Ops()); n != 0 {
					t.Fatalf("Check made %d store calls", n)
				}
			}
		})
	}
}
