package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// TestGenClientRoundTripCitiesList exercises the supervisor-scope
// "list cities" operation through the generated client against a
// real httptest server. Catches method-name drift, request encoding,
// status-code drift, and decoded-body shape.
func TestGenClientRoundTripCitiesList(t *testing.T) {
	client, _ := newRoundTripClient(t)

	resp, err := client.GetV0CitiesWithResponse(context.Background())
	if err != nil {
		t.Fatalf("GetV0Cities: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 is nil; raw body: %s", string(resp.Body))
	}
	// The fake city's name should appear exactly once.
	if resp.JSON200.Items == nil {
		t.Fatalf("cities list Items is nil; body: %s", string(resp.Body))
	}
	var found bool
	for _, c := range *resp.JSON200.Items {
		if c.Name == "test-city" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cities list does not contain test-city; got %+v", *resp.JSON200.Items)
	}
}

func TestGenClientRoundTripAgentList(t *testing.T) {
	client, state := newRoundTripClient(t)

	resp, err := client.GetV0CityByCityNameAgentsWithResponse(context.Background(), state.CityName(), nil)
	if err != nil {
		t.Fatalf("GetV0CityByCityNameAgents: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 is nil; raw body: %s", string(resp.Body))
	}
	if resp.JSON200.Items == nil || len(*resp.JSON200.Items) == 0 {
		t.Fatalf("agent list is empty; expected at least 'worker' from fake city")
	}
}

func TestGenClientRoundTripBeadCreate(t *testing.T) {
	client, state := newRoundTripClient(t)

	title := "roundtrip-bead"
	rig := "myrig"
	body := genclient.CreateBeadJSONRequestBody{
		Title: title,
		Rig:   &rig,
	}
	resp, err := client.CreateBeadWithResponse(context.Background(), state.CityName(), nil, body)
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if resp.JSON201 == nil {
		t.Fatalf("JSON201 is nil; raw body: %s", string(resp.Body))
	}
	if resp.JSON201.Id == "" {
		t.Fatalf("created bead has empty ID; body: %s", string(resp.Body))
	}
	if resp.JSON201.Title != title {
		t.Fatalf("created bead title = %q, want %q", resp.JSON201.Title, title)
	}
}

// newRoundTripClient spins up a supervisor + fake city behind an
// httptest.Server and returns a generated client pointed at it. The
// client and fake state are owned by the test — the cleanup hook
// tears down the server when the test finishes.
func newRoundTripClient(t *testing.T) (*genclient.ClientWithResponses, *fakeState) {
	t.Helper()
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// The supervisor's CSRF middleware requires X-GC-Request on every
	// mutation. Attach it as a default request editor so tests don't
	// have to repeat it per call.
	addCSRF := func(_ context.Context, req *http.Request) error {
		req.Header.Set("X-GC-Request", "true")
		return nil
	}
	client, err := genclient.NewClientWithResponses(ts.URL, genclient.WithRequestEditorFn(addCSRF))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, state
}
