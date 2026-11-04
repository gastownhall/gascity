package readywork

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

type inputAdmissionGate struct{ legs []Leg }

func (g *inputAdmissionGate) check(ctx context.Context, own int, b beads.Bead) (sourceworkflow.InputAdmission, error) {
	if g == nil {
		return sourceworkflow.InputAdmission{Ready: true}, nil
	}
	if own < 0 || own >= len(g.legs) {
		return sourceworkflow.InputAdmission{}, fmt.Errorf("input admission: candidate %s has no serving leg", b.ID)
	}
	stores := sourceworkflow.InputStores{Graph: g.legs[own].Graph}
	for _, leg := range g.legs {
		if !beads.IsNilStore(leg.Work) {
			stores.Work = append(stores.Work, leg.Work)
		}
	}
	return stores.Admission(ctx, b)
}

func (g *inputAdmissionGate) filter(ctx context.Context, own int, rows []beads.Bead) ([]beads.Bead, error) {
	if g == nil {
		return rows, nil
	}
	var out []beads.Bead
	for _, row := range rows {
		admission, err := g.check(ctx, own, row)
		if err != nil {
			return nil, err
		}
		if admission.Ready {
			out = append(out, row)
		}
	}
	return out, nil
}
