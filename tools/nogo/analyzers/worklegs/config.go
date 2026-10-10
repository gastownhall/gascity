package worklegs

const (
	gascity = "github.com/gastownhall/gascity/"
	beads   = gascity + "internal/beads"
)

// Analyzer is the lint as nogo runs it over the repository.
var Analyzer = New(Config{
	Package:      gascity + "cmd/gc",
	DefiningFile: "work_location.go",
	Guarded:      []string{"WorkLegs", "cityWorkLeg"},
	Mints: map[string]map[string]string{
		"cityWorkLegOf": {
			"CityRuntime.workLegs":                   "cr.cityWorkStore(): the store the controller registers as the city's work leg",
			"controllerState.WakeStartRefusal":       "cs.cityWorkStore(): the API's controller work store",
			"doSessionWake":                          "deps.store: cmdSessionWake's openCityStore",
			"cmdSessionClose":                        "openCityStore",
			"releaseUnexecutedClaimsForSessionStore": "openCityStoreAtWithConfig in releaseUnexecutedClaimsForSession",
			"doStartStandalone":                      "oneShotStore, the store the one-shot pass registers as the work leg",
			"gather":                                 "gatherEnv.WorkStore: cr.cityBeadStore (newPlannerHost)",
		},
		"workLegsFromCensus": {
			"CityRuntime.workLegs":                   "over cityWorkLegOf(cr.cityWorkStore())",
			"wakeWillNotStart":                       "over wakeVerdictDeps.work, minted by its two callers",
			"cmdSessionClose":                        "over its cityWorkLegOf",
			"releaseUnexecutedClaimsForSessionStore": "over its cityWorkLegOf",
			"doStartStandalone":                      "over its cityWorkLegOf",
			"gather":                                 "over its cityWorkLegOf",
		},
	},
	Scope:        "workScope",
	ScopeImpls:   []string{"ReleaseScope", "RefuseScope"},
	WorkStore:    beads + ".WorkStore",
	SessionStore: beads + ".SessionStore",
})
