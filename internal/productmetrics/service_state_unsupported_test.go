//go:build !((linux && !android) || (darwin && !ios))

package productmetrics

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
)

func TestProductionServiceIsFailClosedAndNonCreatingOnUnsupportedPlatforms(t *testing.T) {
	service, err := OpenProduction(ProductionOptions{Home: gchome.ResolveReadOnly(), Release: CurrentReleaseIdentity()})
	if err != nil {
		t.Fatalf("OpenProduction() error = %v", err)
	}
	status := service.Status(context.Background())
	if status.State != StateFailClosed {
		t.Fatalf("Status().State = %q, want %q", status.State, StateFailClosed)
	}
	if permit := service.RecordingPermit(InvocationContext{Recordable: true, NoticeEligible: true}); permit.Valid() {
		t.Fatalf("unsupported service returned permit: %#v", permit)
	}
}
