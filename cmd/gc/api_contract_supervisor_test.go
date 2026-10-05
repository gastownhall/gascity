package main

import (
	"net/http"
	"testing"
)

// contractSupervisorFamily covers supervisor-scope reads and the city-level
// read surface (status, health, readiness, config, usage).
func contractSupervisorFamily(t *testing.T, h *contractHarness) {
	c, ctx := h.client, h.ctx

	health, err := c.GetHealthWithResponse(ctx)
	expectStatus(t, "GET /health", health, err, http.StatusOK)

	cities, err := c.GetV0CitiesWithResponse(ctx)
	expectStatus(t, "GET /v0/cities", cities, err, http.StatusOK)
	list := mustJSON(t, "cities", cities.JSON200, cities)
	if list.Items == nil || len(*list.Items) != 1 || (*list.Items)[0].Name != contractCityName {
		t.Fatalf("cities = %s, want exactly %q", contractBody(cities), contractCityName)
	}

	readiness, err := c.GetV0ReadinessWithResponse(ctx, nil)
	expectStatus(t, "GET /v0/readiness", readiness, err, http.StatusOK)
	provReady, err := c.GetV0ProviderReadinessWithResponse(ctx, nil)
	expectStatus(t, "GET /v0/provider-readiness", provReady, err, http.StatusOK)

	city, err := c.GetV0CityByCityNameWithResponse(ctx, contractCityName)
	expectStatus(t, "GET city", city, err, http.StatusOK)
	status, err := c.GetV0CityByCityNameStatusWithResponse(ctx, contractCityName, nil)
	expectStatus(t, "GET city status", status, err, http.StatusOK)
	cityHealth, err := c.GetV0CityByCityNameHealthWithResponse(ctx, contractCityName)
	expectStatus(t, "GET city health", cityHealth, err, http.StatusOK)
	cityReady, err := c.GetV0CityByCityNameReadinessWithResponse(ctx, contractCityName, nil)
	expectStatus(t, "GET city readiness", cityReady, err, http.StatusOK)
	cityProvReady, err := c.GetV0CityByCityNameProviderReadinessWithResponse(ctx, contractCityName, nil)
	expectStatus(t, "GET city provider readiness", cityProvReady, err, http.StatusOK)

	cfg, err := c.GetV0CityByCityNameConfigWithResponse(ctx, contractCityName)
	expectStatus(t, "GET config", cfg, err, http.StatusOK)
	defaults, err := c.GetV0CityByCityNameConfigDefaultsWithResponse(ctx, contractCityName)
	expectStatus(t, "GET config defaults", defaults, err, http.StatusOK)
	explain, err := c.GetV0CityByCityNameConfigExplainWithResponse(ctx, contractCityName)
	expectStatus(t, "GET config explain", explain, err, http.StatusOK)
	validate, err := c.GetV0CityByCityNameConfigValidateWithResponse(ctx, contractCityName)
	expectStatus(t, "GET config validate", validate, err, http.StatusOK)

	usage, err := c.GetV0CityByCityNameUsageWithResponse(ctx, contractCityName, nil)
	expectStatus(t, "GET usage", usage, err, http.StatusOK)

	missing, err := c.GetV0CityByCityNameWithResponse(ctx, "no-such-city")
	expectStatus(t, "GET unknown city", missing, err, http.StatusNotFound)
}
