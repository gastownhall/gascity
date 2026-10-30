package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// A config edit that references a provider missing from the city's provider
// catalog is rolled back and must surface as a client error, not a 500.
func TestMutationErrorMapsProviderCatalogErrorToBadRequest(t *testing.T) {
	catalogErr := &config.ProviderCatalogError{References: []config.ProviderReference{{Kind: "agent", Agent: "helper", Provider: "nope"}}}
	wrapped := fmt.Errorf("refreshing updated city config: %w", fmt.Errorf("loading updated city config: %w", catalogErr))

	var se huma.StatusError
	if !errors.As(mutationError(wrapped), &se) {
		t.Fatalf("mutationError(%v) is not a huma.StatusError", wrapped)
	}
	if se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", se.GetStatus(), http.StatusBadRequest)
	}

	var internal huma.StatusError
	if !errors.As(mutationError(errors.New("disk on fire")), &internal) || internal.GetStatus() != http.StatusInternalServerError {
		t.Fatalf("unclassified mutation error must stay 500")
	}
}
