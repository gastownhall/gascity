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
	GuardedReads: map[string][]string{"WorkLegs": {"plan", "work", "err"}},
	Mints: map[string]map[string]string{
		"cityWorkLegOf": {
			"CityRuntime.workLegs":                   "cr.cityWorkStore(): the store the controller registers as the city's work leg",
			"controllerState.WakeStartRefusal":       "cs.cityWorkStore(): the API's controller work store",
			"doSessionWake":                          "deps.store: cmdSessionWake's openCityStore",
			"cmdSessionClose":                        "openCityStore",
			"releaseUnexecutedClaimsForSessionStore": "openCityStoreAtWithConfig in releaseUnexecutedClaimsForSession",
			"doStartStandalone":                      "oneShotStore, the store the one-shot pass registers as the work leg",
			"gather":                                 "gatherEnv.WorkStore: cr.cityBeadStore (newPlannerHost)",
			"gatherEnv.externalReadsEnv":             "gatherEnv.WorkStore, as gather mints it",
		},
		"workLegsFromCensus": {
			"CityRuntime.workLegs":                   "over cityWorkLegOf(cr.cityWorkStore())",
			"wakeWillNotStart":                       "over wakeVerdictDeps.work, minted by its two callers",
			"cmdSessionClose":                        "over its cityWorkLegOf",
			"releaseUnexecutedClaimsForSessionStore": "over its cityWorkLegOf",
			"doStartStandalone":                      "over its cityWorkLegOf",
			"gather":                                 "over its cityWorkLegOf",
			"gatherEnv.externalReadsEnv":             "over its cityWorkLegOf",
		},
	},
	// The release primitives (R9 REL): every release path goes through
	// ReleaseClaims, whose body (releaseListed) alone names the two release
	// writes; the rest is their own layering.
	Releases: map[string]map[string]string{
		"workAssignment.ReleaseWorkBead": {"releaseListed": "the verb's body"},
		"releaseOrphanedPoolAssignment":  {"releaseListed": "the verb's body (the orphan write)"},
		"releasePoolAssignmentWithRecheck": {
			"releaseOrphanedPoolAssignment": "its fallback tier",
		},
		"releaseWorkAssignmentIfRevisionMatches": {
			"workAssignment.ReleaseWorkBead": "its revision-fenced tier",
			"releaseOrphanedPoolAssignment":  "its revision-fenced tier",
		},
		"releaseWorkAssignmentIfCurrent": {"workAssignment.ReleaseWorkBead": "its assignee-fenced tier"},
		"releaseAssignmentFenced": {
			"workAssignment.ReleaseWorkBead":   "its fenced tier",
			"workAssignment.ReassignWorkBead":  "a reassignment's fenced write, not a release",
			"releasePoolAssignmentWithRecheck": "its fenced write",
		},
	},
	Scope:        "workScope",
	ScopeImpls:   []string{"ReleaseScope", "RefuseScope"},
	WorkStore:    beads + ".WorkStore",
	SessionStore: beads + ".SessionStore",
})
