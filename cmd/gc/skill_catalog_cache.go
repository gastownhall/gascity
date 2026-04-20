package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

// Transient filesystem errors in loadSharedSkillCatalog used to silently
// drop skill entries from FingerprintExtra for one tick, which flipped
// CoreFingerprint hashes and drained every live session in a config-drift
// storm. The cache preserves the last successful catalog for each catalog
// input set so a failed load reuses the prior result instead of emitting a
// degraded fingerprint. Bootstrap-backed successful-empty loads get a
// single grace tick before the empty catalog propagates, which avoids
// transient drain storms without pinning stale skill sources forever.
var skillCatalogCache = struct {
	sync.Mutex
	city map[string]cachedSkillCatalog // input key -> cached catalog metadata
	rig  map[string]cachedSkillCatalog // input key -> cached catalog metadata
}{
	city: map[string]cachedSkillCatalog{},
	rig:  map[string]cachedSkillCatalog{},
}

type cachedSkillCatalog struct {
	Catalog            materialize.CityCatalog
	BootstrapInputs    []bootstrapSkillCacheInput
	PendingEmptyReuse  bool
	PendingEmptyInputs []bootstrapSkillCacheInput
}

type bootstrapCatalogState struct {
	Known          bool
	Inputs         []bootstrapSkillCacheInput
	AnyUnavailable bool
}

type sharedCatalogLoadMode int

const (
	sharedCatalogLoadDirect sharedCatalogLoadMode = iota
	sharedCatalogLoadCachedOnError
	sharedCatalogLoadCachedOnEmptyGrace
)

type sharedCatalogLoadResult struct {
	Catalog materialize.CityCatalog
	Mode    sharedCatalogLoadMode
	Err     error
}

// cachedCityCatalog returns the last successfully loaded city catalog entry
// for key, or (zero, false) if none has been cached yet.
func cachedCityCatalog(key string) (cachedSkillCatalog, bool) {
	skillCatalogCache.Lock()
	defer skillCatalogCache.Unlock()
	c, ok := skillCatalogCache.city[key]
	return cloneCachedSkillCatalog(c), ok
}

// setCachedCityCatalog stores the cached city catalog entry for key.
func setCachedCityCatalog(key string, cat cachedSkillCatalog) {
	skillCatalogCache.Lock()
	defer skillCatalogCache.Unlock()
	skillCatalogCache.city[key] = cloneCachedSkillCatalog(cat)
}

// cachedRigCatalog returns the last successfully loaded rig catalog entry,
// or (zero, false) if none has been cached yet for key.
func cachedRigCatalog(key string) (cachedSkillCatalog, bool) {
	skillCatalogCache.Lock()
	defer skillCatalogCache.Unlock()
	c, ok := skillCatalogCache.rig[key]
	return cloneCachedSkillCatalog(c), ok
}

// setCachedRigCatalog stores the cached rig catalog entry for key.
func setCachedRigCatalog(key string, cat cachedSkillCatalog) {
	skillCatalogCache.Lock()
	defer skillCatalogCache.Unlock()
	skillCatalogCache.rig[key] = cloneCachedSkillCatalog(cat)
}

func citySkillCatalogCacheKey(cityPath string, cfg *config.City) string {
	return skillCatalogCacheKey(cityPath, "", cfg)
}

func rigSkillCatalogCacheKey(cityPath, rigName string, cfg *config.City) string {
	return skillCatalogCacheKey(cityPath, rigName, cfg)
}

func skillCatalogCacheKey(cityPath, rigName string, cfg *config.City) string {
	var b strings.Builder
	writeCacheKeyPart(&b, cityPath)
	writeCacheKeyPart(&b, rigName)
	if cfg == nil {
		return b.String()
	}
	writeCacheKeyPart(&b, cfg.PackSkillsDir)
	writeCacheKeyPart(&b, os.Getenv("GC_HOME"))
	for _, catalog := range sharedSkillCatalogInputs(cfg, rigName) {
		writeCacheKeyPart(&b, catalog.SourceDir)
		writeCacheKeyPart(&b, catalog.BindingName)
		writeCacheKeyPart(&b, catalog.PackName)
	}
	return b.String()
}

func writeCacheKeyPart(b *strings.Builder, part string) {
	b.WriteString(part)
	b.WriteByte(0)
}

type bootstrapSkillCacheInput struct {
	Name string
	Dir  string
}

func currentBootstrapCatalogState() bootstrapCatalogState {
	imports, _, err := config.ReadImplicitImports()
	if err != nil {
		return bootstrapCatalogState{}
	}
	gcHome := config.ImplicitGCHome()
	if gcHome == "" || len(imports) == 0 {
		return bootstrapCatalogState{Known: true}
	}
	bootstrapNames := make(map[string]struct{}, len(bootstrap.PackNames()))
	for _, name := range bootstrap.PackNames() {
		bootstrapNames[name] = struct{}{}
	}
	out := make([]bootstrapSkillCacheInput, 0, len(imports))
	anyUnavailable := false
	for name, imp := range imports {
		if _, ok := bootstrapNames[name]; !ok {
			continue
		}
		if strings.TrimSpace(imp.Commit) == "" {
			continue
		}
		dir := filepath.Join(config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit), "skills")
		out = append(out, bootstrapSkillCacheInput{
			Name: name,
			Dir:  dir,
		})
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			anyUnavailable = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return bootstrapCatalogState{
		Known:          true,
		Inputs:         out,
		AnyUnavailable: anyUnavailable,
	}
}

func newCachedSkillCatalog(cat materialize.CityCatalog, state bootstrapCatalogState) cachedSkillCatalog {
	return cachedSkillCatalog{
		Catalog:         cloneCityCatalog(cat),
		BootstrapInputs: cloneBootstrapInputs(state.Inputs),
	}
}

func shouldReuseCachedCatalogOnLoadError(cached cachedSkillCatalog, state bootstrapCatalogState) bool {
	if !state.Known {
		return true
	}
	return sameBootstrapInputs(state.Inputs, cached.BootstrapInputs)
}

func shouldReuseCachedCatalogOnSuccessfulEmptyLoad(current materialize.CityCatalog, cached cachedSkillCatalog, state bootstrapCatalogState) bool {
	if len(current.Entries) != 0 || len(cached.Catalog.Entries) == 0 || !state.Known {
		return false
	}
	needsGrace := state.AnyUnavailable || !sameBootstrapInputs(state.Inputs, cached.BootstrapInputs)
	if !needsGrace {
		return false
	}
	return !cached.PendingEmptyReuse || !sameBootstrapInputs(state.Inputs, cached.PendingEmptyInputs)
}

func markCachedCatalogPendingEmpty(cached cachedSkillCatalog, state bootstrapCatalogState) cachedSkillCatalog {
	next := cloneCachedSkillCatalog(cached)
	next.PendingEmptyReuse = true
	next.PendingEmptyInputs = cloneBootstrapInputs(state.Inputs)
	return next
}

func loadSharedSkillCatalogWithFallback(cityPath string, cfg *config.City, rigName string) sharedCatalogLoadResult {
	key := citySkillCatalogCacheKey(cityPath, cfg)
	loadCached := cachedCityCatalog
	storeCached := setCachedCityCatalog
	if rigName != "" {
		key = rigSkillCatalogCacheKey(cityPath, rigName, cfg)
		loadCached = cachedRigCatalog
		storeCached = setCachedRigCatalog
	}
	bootstrapState := currentBootstrapCatalogState()
	cat, err := loadSharedSkillCatalog(cfg, rigName)
	if err != nil {
		if cached, ok := loadCached(key); ok && shouldReuseCachedCatalogOnLoadError(cached, bootstrapState) {
			return sharedCatalogLoadResult{
				Catalog: cached.Catalog,
				Mode:    sharedCatalogLoadCachedOnError,
				Err:     err,
			}
		}
		return sharedCatalogLoadResult{Catalog: cat, Mode: sharedCatalogLoadDirect, Err: err}
	}
	if cached, ok := loadCached(key); ok && shouldReuseCachedCatalogOnSuccessfulEmptyLoad(cat, cached, bootstrapState) {
		storeCached(key, markCachedCatalogPendingEmpty(cached, bootstrapState))
		return sharedCatalogLoadResult{
			Catalog: cached.Catalog,
			Mode:    sharedCatalogLoadCachedOnEmptyGrace,
		}
	}
	storeCached(key, newCachedSkillCatalog(cat, bootstrapState))
	return sharedCatalogLoadResult{Catalog: cat, Mode: sharedCatalogLoadDirect}
}

func cloneCityCatalog(cat materialize.CityCatalog) materialize.CityCatalog {
	return materialize.CityCatalog{
		Entries:    append([]materialize.SkillEntry(nil), cat.Entries...),
		OwnedRoots: append([]string(nil), cat.OwnedRoots...),
		Shadowed:   append([]materialize.ShadowedEntry(nil), cat.Shadowed...),
	}
}

func cloneCachedSkillCatalog(cat cachedSkillCatalog) cachedSkillCatalog {
	return cachedSkillCatalog{
		Catalog:            cloneCityCatalog(cat.Catalog),
		BootstrapInputs:    cloneBootstrapInputs(cat.BootstrapInputs),
		PendingEmptyReuse:  cat.PendingEmptyReuse,
		PendingEmptyInputs: cloneBootstrapInputs(cat.PendingEmptyInputs),
	}
}

func cloneBootstrapInputs(inputs []bootstrapSkillCacheInput) []bootstrapSkillCacheInput {
	return append([]bootstrapSkillCacheInput(nil), inputs...)
}

func sameBootstrapInputs(a, b []bootstrapSkillCacheInput) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
