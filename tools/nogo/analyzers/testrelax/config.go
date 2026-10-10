package testrelax

// Analyzer is the lint as nogo runs it over the repository.
var Analyzer = New(Config{Guards: guards})

// guards are the production guard setters, reviewed: each weakens a
// production refusal so a test can pass. Setters that only inject a seam (a
// clock, a jitter, a timing bound, a recorder) are not guards. The
// session-key guard's GuardSessionKeys is not one either: init arms it to
// panic, which tightens it.
var guards = map[string]string{
	"github.com/gastownhall/gascity/internal/runtime/proctable.SetScanRootForTesting": "moves the process-table scan off the live /proc, which liveScanGuard refuses under go test so an orphan sweep cannot reap a host's real agents (#2839)",
	"github.com/gastownhall/gascity/cmd/gc.setSessionCircuitBreakerForTest":           "replaces the session circuit breaker, which stops restarting a crash-looping session",
}
