package sqlite

import (
	"context"

	"github.com/gastownhall/gascity/internal/storebinding"
)

var _ storebinding.WriterFenceAcquirer = (*capturingSQLiteFenceAcquirer)(nil)

type capturingSQLiteFenceAcquirer struct {
	delegate storebinding.WriterFenceAcquirer
	inner    storebinding.WriterFence
}

func (a *capturingSQLiteFenceAcquirer) AcquireFence(ctx context.Context, claim storebinding.MigrationGuardClaim, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	fence, err := a.delegate.AcquireFence(ctx, claim, request)
	a.inner = fence
	return fence, err
}
