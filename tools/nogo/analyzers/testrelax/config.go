package testrelax

const gascity = "github.com/gastownhall/gascity/"

// Analyzer is the lint as nogo runs it over the repository.
var Analyzer = New(Config{Guards: guards, Vars: vars, Env: env, Allowed: allowed})

// The reviewed guards. A production guard is anything that refuses or delays
// a destructive act or a takeover, or that keeps a process off a resource it
// must not touch: its setter, the variable that holds it, and the environment
// variable that breaks it open. A timing bound that gates a destructive act or
// a takeover (a kill once a deadline passes, a takeover once a lease
// expires) is a guard. A seam that only injects a collaborator (a clock, a
// jitter, a recorder, a sling index) is not, nor is a guard's installer that
// tightens it (GuardSessionKeys, which init arms to panic).

// guards are the guard setters. Each takes a testing.TB and restores on
// t.Cleanup, so no init or TestMain can call it.
var guards = map[string]string{
	gascity + "internal/runtime/proctable.SetScanRootForTesting": "moves the process-table scan off the live /proc, which liveScanGuard refuses under go test so an orphan sweep cannot reap a host's real agents (#2839)",
	gascity + "cmd/gc.setSessionCircuitBreakerForTest":           "replaces the session circuit breaker, which stops restarting a crash-looping session",
}

// vars are the variables that hold a guard. A test writes one only through
// its setter.
var vars = map[string]string{
	gascity + "internal/runtime/proctable.scanRoot":            "holds the procfs root liveScanGuard refuses to scan live under go test (#2839)",
	gascity + "cmd/gc.sessionCircuitBreakerSingleton":          "holds the session circuit breaker, which stops restarting a crash-looping session",
	gascity + "internal/workspacesvc.proxyProcessReadyTimeout": "holds the readiness bound after which a spawned service helper is killed as hung",
}

// env are the break-glass environment variables: each lets a process past a
// refusal.
var env = map[string]string{
	"GC_CITY_WRITE_ALLOW_UNVERIFIED":       "lets the API accept city writes from an unverified caller",
	"GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE": "lets a native beads store whose schema is behind its migrations open (the preflight schema gate)",
	"GC_BD_ALLOW_RELOCATED_CLASS_READ":     "lets `gc bd` read a bead class from the store it was relocated away from",
	"GC_BD_ALLOW_UNREAD_STORE_READ":        "lets `gc bd` read a store gc no longer reads (the unread-store guard)",
	"GC_ALLOW_PROD_DOLT_PORT_IN_TESTS":     "lets a test client reach the production Dolt port (ga-4c2ss6)",
}

// allowed are the package-global relaxations kept, each with its reason.
var allowed = map[string]string{
	gascity + "internal/workspacesvc:" + gascity + "internal/workspacesvc.proxyProcessReadyTimeout": "ga-bhct0w: a starved test host can take longer than the production bound to start a healthy helper. Reload and Tick return only once the helper answers, so the tests need a bound only a hung helper outlives; the helper processes the test binary re-executes itself as share it, which a per-test write cannot reach",
}
