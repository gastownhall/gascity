package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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

// reservedPrefixRigAddRefusal is the refusal rig provisioning returns for a
// rig whose derived prefix (graphql-code-generator derives gcg) is in the
// reserved namespace of a split city, as every new city is.
func reservedPrefixRigAddRefusal(t *testing.T) error {
	t.Helper()
	next := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Rigs:      []config.Rig{{Name: "graphql-code-generator", Path: "/tmp/graphql-code-generator"}},
		Storage: &config.StorageConfig{
			Classes: config.StorageClasses{
				Work: config.StorageWorkBinding, Graph: "infra", Sessions: "infra",
				Messaging: "infra", Orders: "infra", Nudges: "infra",
			},
			Bindings: map[string]config.StorageBindingConfig{"infra": {Provider: config.StorageProviderSQLiteBeads}},
		},
	}
	refusal := config.ReservedPrefixSplitRefusalForRigAdd(fsys.OSFS{}, filepath.Join(t.TempDir(), "city.toml"), next, "graphql-code-generator")
	if refusal == nil {
		t.Fatal("no refusal for a gcg rig on a split city")
	}
	return refusal
}

// The reserved-prefix refusal (split-default R10) is the caller's error on
// both rig-create paths, refused before anything was written: 400 on the
// synchronous create and invalid_request on the asynchronous one, never a 500
// or provision_failed.
func TestReservedBeadPrefixRefusalIsTheCallersError(t *testing.T) {
	refusal := reservedPrefixRigAddRefusal(t)
	var se huma.StatusError
	if !errors.As(mutationError(refusal), &se) || se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("mutationError(%v) = %v, want 400", refusal, mutationError(refusal))
	}
	if got := rigProvisionFailureCode(refusal); got != "invalid_request" {
		t.Fatalf("rigProvisionFailureCode(%v) = %q, want invalid_request", refusal, got)
	}
}
