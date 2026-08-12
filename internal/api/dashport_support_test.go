//go:build integration

package api

import "testing"

func TestSingleCityPathResolverCities(t *testing.T) {
	r := singleCityPathResolver{name: "alpha", path: "/tmp/alpha"}

	got := r.Cities()
	if len(got) != 1 {
		t.Fatalf("Cities() len = %d, want 1", len(got))
	}
	if got[0].Name != "alpha" || got[0].Path != "/tmp/alpha" {
		t.Fatalf("Cities()[0] = %+v, want name=alpha path=/tmp/alpha", got[0])
	}
}
