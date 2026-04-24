package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
)

func resetSkillCatalogCache() {
	skillCatalogCache.Lock()
	defer skillCatalogCache.Unlock()
	skillCatalogCache.city = map[string]cachedSkillCatalog{}
	skillCatalogCache.rig = map[string]cachedSkillCatalog{}
}

func TestShouldReuseCachedCatalogOnLoadErrorWithUnknownBootstrapState(t *testing.T) {
	cached := cachedSkillCatalog{
		Catalog: materialize.CityCatalog{
			Entries: []materialize.SkillEntry{{Name: "plan"}},
		},
		BootstrapInputs: []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/a"}},
	}
	if !shouldReuseCachedCatalogOnLoadError(cached, bootstrapCatalogState{}) {
		t.Fatal("unknown bootstrap state should still reuse the last-good cache on load error")
	}
	if shouldReuseCachedCatalogOnLoadError(cached, bootstrapCatalogState{
		Known:  true,
		Inputs: []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/b"}},
	}) {
		t.Fatal("known bootstrap state change should not reuse a cache built from different inputs")
	}
}

func TestShouldReuseCachedCatalogOnLoadErrorReusesAcrossRepeatedFailures(t *testing.T) {
	cached := cachedSkillCatalog{
		Catalog: materialize.CityCatalog{
			Entries: []materialize.SkillEntry{{Name: "plan"}},
		},
		BootstrapInputs: []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/a"}},
	}
	state := bootstrapCatalogState{
		Known:  true,
		Inputs: []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/a"}},
	}
	if !shouldReuseCachedCatalogOnLoadError(cached, state) {
		t.Fatal("first repeated shared-root error should reuse the last-good cache")
	}
	if !shouldReuseCachedCatalogOnLoadError(cached, state) {
		t.Fatal("second repeated shared-root error should still reuse the last-good cache")
	}
}

func TestShouldReuseCachedCatalogOnSuccessfulEmptyLoadOnlyOncePerBootstrapState(t *testing.T) {
	current := materialize.CityCatalog{}
	cached := cachedSkillCatalog{
		Catalog: materialize.CityCatalog{
			Entries: []materialize.SkillEntry{{Name: "plan"}},
		},
		BootstrapInputs: []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/a"}},
	}
	state := bootstrapCatalogState{
		Known:          true,
		Inputs:         []bootstrapSkillCacheInput{{Name: "core.gc-agents", Dir: "/cache/a"}},
		AnyUnavailable: true,
	}
	if !shouldReuseCachedCatalogOnSuccessfulEmptyLoad(current, cached, state) {
		t.Fatal("first successful empty load with unavailable bootstrap inputs should reuse the cached catalog")
	}
	reused := markCachedCatalogPendingEmpty(cached, state)
	if shouldReuseCachedCatalogOnSuccessfulEmptyLoad(current, reused, state) {
		t.Fatal("second successful empty load for the same bootstrap state should stop reusing the cached catalog")
	}
}
