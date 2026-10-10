package v2purity

// Default is the gascity configuration nogo runs.
var Default = Config{
	Package: "github.com/gastownhall/gascity/cmd/gc",
	Module:  "github.com/gastownhall/gascity/",
	ForbiddenPackages: []string{
		"os", "os/exec", "os/signal", "os/user", "log", "log/slog", "syscall", "net", "net/http", "io/ioutil",
		"sync", "sync/atomic", "github.com/gastownhall/gascity/internal/fsys",
	},
	ForbiddenFuncs: []string{
		"time.Now", "time.Since", "time.Until", "time.Sleep", "time.After", "time.Tick", "time.NewTimer",
		"time.NewTicker", "time.AfterFunc", "path/filepath.Abs", "path/filepath.EvalSymlinks",
		"path/filepath.Glob", "path/filepath.Walk", "path/filepath.WalkDir",
		"fmt.Print", "fmt.Printf", "fmt.Println", // os.Stdout
	},
	ForbiddenInterfaces: []string{
		"github.com/gastownhall/gascity/internal/beads.Store",
		"github.com/gastownhall/gascity/internal/runtime.Provider",
	},
	// Roots pins the //gc:pure roots: the decide, admission, the dirty
	// filters, every section's Decide, and every decide a probed section
	// is given.
	Roots: []string{
		"github.com/gastownhall/gascity/cmd/gc.decideAllocation", "github.com/gastownhall/gascity/cmd/gc.admit", "github.com/gastownhall/gascity/cmd/gc.decideRow",
		"github.com/gastownhall/gascity/cmd/gc.newRelevantSet", "github.com/gastownhall/gascity/cmd/gc.beadEventRelevant", "github.com/gastownhall/gascity/cmd/gc.inventoryChanged", "github.com/gastownhall/gascity/cmd/gc.healthChanged",
		"section.Decide", "probed(decide)",
	},
	Exempt: map[string]Exemption{
		"(*github.com/gastownhall/gascity/cmd/gc.SessionReconcilerTraceCycle).RecordDecision":   {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"(*github.com/gastownhall/gascity/cmd/gc.decidePass).computePoolDesired"}, Reason: "dead: computePoolDesired passes a nil trace (cmd/gc/allocator_plan.go:170), and RecordDecision returns on a nil receiver (cmd/gc/session_reconciler_trace_collector.go:686). It covers the trace recorder under it"},
		"(*github.com/gastownhall/gascity/cmd/gc.controlDispatcherRouteRepair).write":           {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.projectControlDispatcherRoutes"}, Reason: "dead: the projection repairs with a nil store (cmd/gc/allocator_demand_reads.go:299), and persist returns on a nil store before write (cmd/gc/build_desired_state.go:6908)"},
		"github.com/gastownhall/gascity/cmd/gc.normalizeNonExpandingPoolSessionInfo":            {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.normalizeNonExpandingPoolSessionInfoForSelection"}, Reason: "dead: the decide's build params are planOnly (cmd/gc/allocator_plan.go:270, copied into each realize view at cmd/gc/allocator_index.go:246), and selection returns before normalizing (cmd/gc/build_desired_state_pool_info.go:666)"},
		"github.com/gastownhall/gascity/cmd/gc.recordDeferredNonExpandingPoolAliasConflictInfo": {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.normalizeNonExpandingPoolSessionInfoForSelection"}, Reason: "dead: the decide's build params are planOnly (cmd/gc/allocator_plan.go:270), and selection returns before normalizing (cmd/gc/build_desired_state_pool_info.go:666)"},
		"github.com/gastownhall/gascity/cmd/gc.workerSessionTargetLastActivityWithConfig":       {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.sessionIdleReferenceInfoWithError"}, Reason: "dead: configSleepSuppressed passes a nil provider (cmd/gc/allocator_decide.go:562), and sessionIdleReferenceInfoWithError reads the last activity only under sp != nil (cmd/gc/session_sleep.go:294). It covers the worker factory and observation under it"},
		"github.com/gastownhall/gascity/cmd/gc.verifyPoolTriggerWorktree":                       {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.verifiedPoolTriggerWorkDir"}, Reason: "dead: the decide's build params are planOnly (cmd/gc/allocator_plan.go:270), and verifiedPoolTriggerWorkDir returns before it (cmd/gc/build_desired_state.go:4269)"},
		"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).OpenInfos":                {Class: "live", Bead: "mc-zndi7.88", Reason: "live: takes the pass's session snapshot's RWMutex (retainScaleCheckPartialPoolDesired)"},
		"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).LoadError":                {Class: "live", Bead: "mc-zndi7.88", Reason: "live: takes the pass's session snapshot's RWMutex (hasCompleteSessionSnapshot)"},
		"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).FindInfoByID":             {Class: "live", Bead: "mc-zndi7.88", Reason: "live: takes the pass's session snapshot's RWMutex (realizeRequest)"},
		"github.com/gastownhall/gascity/cmd/gc.applyTemplateOverridesToConfigInfo":              {Class: "live", Bead: "mc-zndi7.90", Reason: "live: logs an unhonored template option pin, from the row arms' drift key"},
		"github.com/gastownhall/gascity/cmd/gc.templateParamsToConfigWithDelivery":              {Class: "live", Bead: "mc-zndi7.90", Reason: "live: logs (slog) from the row arms' drift key; it covers logOversizedPromptDelivery"},
		"github.com/gastownhall/gascity/cmd/gc.recordNewDemandCapTrace":                         {Class: "live", Bead: "mc-zndi7.90", Reason: "live: logs a rig_resolution_error refusal unguarded by the trace"},
		"github.com/gastownhall/gascity/cmd/gc.reserveNestedCapFloors":                          {Class: "live", Bead: "mc-zndi7.90", Reason: "live: logs a rig_resolution_error floor refusal unguarded by the trace"},
		"github.com/gastownhall/gascity/internal/session.ProjectLifecycle":                      {Class: "live", Bead: "mc-zndi7.89", Reason: "live: projects a row's lifecycle at time.Now, from the awake input and the drain arms' live-runtime claim"},
		"(*github.com/gastownhall/gascity/internal/poolplan.CreateBudget).TryClaim":             {Class: "live", Bead: "mc-zndi7.88", Reason: "live: takes the create budget's mutex, from the realize plan and admission's create cap"},
		"(*github.com/gastownhall/gascity/internal/poolplan.CreateBudget).ConfigureFairShare":   {Class: "live", Bead: "mc-zndi7.88", Reason: "live: takes the create budget's mutex, from admission's create cap"},
		"(*github.com/gastownhall/gascity/internal/config.DaemonConfig).PatrolIntervalDuration": {Class: "live", Bead: "mc-zndi7.96", Reason: "live: clock.Backstop reads GC_BACKSTOP_SPEEDUP (os.LookupEnv) on every call, from admission's patrol interval"},
		"github.com/gastownhall/gascity/internal/config.NamedSessionRuntimeName":                {Class: "live", Bead: "mc-zndi7.90", Reason: "live: agent.SessionNameFor warns on os.Stderr, from the decide's named-session identity reads"},
		"github.com/gastownhall/gascity/internal/session.FindNamedSessionSpec":                  {Class: "live", Bead: "mc-zndi7.90", Reason: "live: agent.SessionNameFor warns on os.Stderr, from the decide's identity duplicates"},
		"github.com/gastownhall/gascity/internal/workdir.SessionQualifiedName":                  {Class: "live", Bead: "mc-zndi7.94", Reason: "live: pathutil.SamePath resolves relative paths with filepath.Abs (the working directory), from the overlay's qualified names"},
		"github.com/gastownhall/gascity/internal/workdir.ConfiguredRigName":                     {Class: "live", Bead: "mc-zndi7.94", Reason: "live: pathutil.SamePath resolves relative paths with filepath.Abs, from computePoolDesired's suspended-rig check"},
		"github.com/gastownhall/gascity/internal/workdir.ResolveTmuxAlias":                      {Class: "live", Bead: "mc-zndi7.94", Reason: "live: pathutil.SamePath resolves relative paths with filepath.Abs, from the plan's pool identifiers"},
		"github.com/gastownhall/gascity/internal/workdir.ResolveWorkDirPathStrict":              {Class: "live", Bead: "mc-zndi7.94", Reason: "live: pathutil.SamePath resolves relative paths with filepath.Abs, from the plan's pool work dir"},
		"github.com/gastownhall/gascity/cmd/gc.resolveConfiguredWorkDirPath":                    {Class: "dead", Bead: "mc-zndi7.91", Via: []string{"github.com/gastownhall/gascity/cmd/gc.poolTriggerWorkDir"}, Reason: "dead: the validating wrapper, the only one that names workdir.ValidateAncestorWorktreesNotStale (os.Lstat); poolTriggerWorkDir names it but runs resolveConfiguredWorkDirPathUnvalidated under the decide's planOnly params (cmd/gc/build_desired_state.go:4333)"},
	},
	Cutover: "v2EffectsReal",
}

// Analyzer runs the rules with Default.
var Analyzer = New(Default)
