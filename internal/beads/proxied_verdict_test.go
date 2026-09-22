package beads

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProxiedVerdictErrorSurvivesDoubleWrapForErrorsAs is the load-bearing test
// of the type.
//
// The proxied wrapper decides whether to demote by asking "did this read fail
// with a verdict?". It never sees the verdict error as the top-level value:
// withReadRetry wraps a failed read's cause into reconnect context with %w and
// then re-wraps THAT into budget context with %w, so by the time the wrapper
// looks, the verdict is two fmt.wrapError layers down. A type assertion would
// find nothing and the handle would keep serving reads against a database it
// had already been told not to trust.
//
// The wrapping here is copied from the production shape rather than invented:
// native_dolt_store.go builds "native Dolt reconnect after transient read
// error (%w): %w" and hands it to nativeReadRetryBudgetError, which builds
// "native Dolt read retry budget exhausted (%w), last error: %w".
func TestProxiedVerdictErrorSurvivesDoubleWrapForErrorsAs(t *testing.T) {
	skew := NewSchemaSkewVerdictError(ProxiedSkewLaneIgnored, ProxiedSkewDirBehind, "main=66 ignored=25")

	// Layer 1: reconnect context, exactly as withReadRetry builds it.
	reconnectErr := fmt.Errorf("native Dolt reconnect after transient read error (%w): %w",
		errors.New("invalid connection"), skew)
	// Layer 2: budget context, exactly as nativeReadRetryBudgetError builds it.
	wrapped := nativeReadRetryBudgetError(context.DeadlineExceeded, reconnectErr)

	got, ok := ProxiedVerdictOf(wrapped)
	if !ok {
		t.Fatalf("ProxiedVerdictOf found no verdict in %v", wrapped)
	}
	if got.Verdict != ProxiedVerdictSchemaSkew {
		t.Errorf("verdict = %q, want %q", got.Verdict, ProxiedVerdictSchemaSkew)
	}
	if got.Lane != ProxiedSkewLaneIgnored || got.Dir != ProxiedSkewDirBehind {
		t.Errorf("lane/dir = %q/%q, want %q/%q", got.Lane, got.Dir, ProxiedSkewLaneIgnored, ProxiedSkewDirBehind)
	}
	if !got.Terminal() {
		t.Error("schema_skew must be terminal: a retry cannot move a migration cursor")
	}

	// errors.Is against a verdict-shaped target is the other spelling callers
	// use, and it must reach through the same two layers.
	if !errors.Is(wrapped, &ProxiedVerdictError{Verdict: ProxiedVerdictSchemaSkew}) {
		t.Error("errors.Is did not match the schema_skew target through two %w layers")
	}
	if errors.Is(wrapped, &ProxiedVerdictError{Verdict: ProxiedVerdictProxyGone}) {
		t.Error("errors.Is matched proxy_gone against a schema_skew error")
	}
	// A bare target asks "is this a proxied refusal at all".
	if !errors.Is(wrapped, &ProxiedVerdictError{}) {
		t.Error("errors.Is with an unqualified target did not match a verdict error")
	}
	// Lane and direction narrow the match, so a caller can ask about one lane.
	if errors.Is(wrapped, &ProxiedVerdictError{Verdict: ProxiedVerdictSchemaSkew, Lane: ProxiedSkewLaneMain}) {
		t.Error("errors.Is matched the main lane against an ignored-lane skew")
	}

	// The underlying cause stays reachable: the verdict adds a layer, it does
	// not replace the chain.
	cause := errors.New("boom")
	if !errors.Is(NewProxiedVerdictError(ProxiedVerdictDatabaseGone, "db=gc", cause), cause) {
		t.Error("Unwrap did not expose the cause")
	}
}

// TestProxiedVerdictTerminalTable pins the terminality split, because it is
// what decides whether a handle keeps asking. A verdict quietly flipping from
// non-terminal to terminal would turn a recoverable blip into a permanent
// demotion to the bd CLI with no diagnostic saying so.
func TestProxiedVerdictTerminalTable(t *testing.T) {
	nonTerminal := []ProxiedVerdict{
		ProxiedVerdictNone,
		ProxiedVerdictProxyGone,
		ProxiedVerdictDraining,
		ProxiedVerdictBackendUnreachable,
		ProxiedVerdictCircuitOpen,
		ProxiedVerdictBudgetExhausted,
		// head_moved is a fact about the DATABASE, which would ordinarily be
		// terminal — it is here because gc cannot attribute the commit to its
		// own open, and another bd client's ordinary write must not pin a scope
		// to the bd front door for the process. See ProxiedHeadUnmoved.
		ProxiedVerdictHeadMoved,
	}
	terminal := []ProxiedVerdict{
		ProxiedVerdictSchemaSkew,
		ProxiedVerdictProxyZombie,
		ProxiedVerdictDatabaseGone,
		ProxiedVerdictNotOurs,
		ProxiedVerdictNoOwnershipRecord,
		ProxiedVerdictLegacySchema,
		ProxiedVerdictIdlePolicyFinite,
		ProxiedVerdictPrefixMismatch,
		ProxiedVerdictAccessDenied,
		ProxiedVerdictWriteIndeterminate,
	}
	for _, v := range nonTerminal {
		if v.Terminal() {
			t.Errorf("verdict %q is terminal, want retryable", v)
		}
	}
	for _, v := range terminal {
		if !v.Terminal() {
			t.Errorf("verdict %q is retryable, want terminal", v)
		}
	}
	// The exhaustiveness guard, and it must observe the PRODUCTION enum.
	//
	// It used to be `len(nonTerminal)+len(terminal) != 16`, over two slices
	// declared twenty lines above it — a checksum of this file. Adding a
	// seventeenth ProxiedVerdict and touching nothing else left the sum at 16,
	// the "add the new one here" message never fired, and Terminal()'s
	// `default: return true` silently made the new verdict a PERMANENT
	// demotion: exactly the hazard the doc comment above says this test exists
	// to prevent (council C-F7).
	//
	// The constants are read out of proxied_verdict.go's own AST rather than
	// counted, because Go exposes no enumeration of a named string type and a
	// number is not a set.
	declared := declaredProxiedVerdicts(t)
	covered := map[ProxiedVerdict]bool{}
	for _, v := range append(append([]ProxiedVerdict(nil), nonTerminal...), terminal...) {
		if covered[v] {
			t.Errorf("verdict %q appears twice in the terminality table", v)
		}
		covered[v] = true
	}
	for name, verdict := range declared {
		if !covered[verdict] {
			t.Errorf("%s (%q) is declared in proxied_verdict.go and is in neither half of this table.\n"+
				"Add it, and decide its terminality deliberately: Terminal()'s default arm returns TRUE, "+
				"so a verdict nobody classified becomes a permanent demotion to the bd CLI.", name, verdict)
		}
		delete(covered, verdict)
	}
	for verdict := range covered {
		t.Errorf("the table classifies %q, which proxied_verdict.go no longer declares", verdict)
	}

	// The one override the design needs: admission's bounded drain expiring
	// returns draining as a fact about the CLOCK, not about the endpoint, and
	// the caller must demote for this open only. Proven against a verdict whose
	// table entry is terminal so the override cannot be reading the table.
	forced := NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyZombie, "budget spent mid-ladder", nil)
	if forced.Terminal() {
		t.Error("NewNonTerminalProxiedVerdictError did not override the terminality table")
	}
	if !strings.Contains(forced.Error(), "terminal=false") {
		t.Errorf("a non-terminal refusal must say so in its message; got %q", forced.Error())
	}
}

// TestProxiedNativeFlagDefaultOffAndForceFallbackWins pins the two properties
// the rollout depends on.
//
// Default off is the whole safety story of PR2: with the variable unset, every
// proxied scope keeps the store it has today. And GC_BEADS_FORCE_FALLBACK stays
// the escape hatch — it is read by forceNativeFallback in OpenStoreAtForCity
// BEFORE the proxied arm is reached, so an operator who sets it gets BdStore
// even on a box where the proxied lane is enabled. The ordering itself is
// asserted end to end by the factory test in P2-07; what this pins is that the
// two predicates are independent, so setting the native flag cannot make
// forceNativeFallback stop reporting the hatch.
func TestProxiedNativeFlagDefaultOffAndForceFallbackWins(t *testing.T) {
	t.Setenv(proxiedNativeEnv, "")
	t.Setenv(nativeForceFallbackEnv, "")
	if proxiedNativeEnabled() {
		t.Fatal("the proxied native lane must be off when GC_BEADS_PROXIED_NATIVE is unset")
	}

	for _, on := range []string{"1", "true", "TRUE", " true "} {
		t.Setenv(proxiedNativeEnv, on)
		if !proxiedNativeEnabled() {
			t.Errorf("GC_BEADS_PROXIED_NATIVE=%q did not enable the lane", on)
		}
	}
	// The same spellings forceNativeFallback rejects, rejected here: an
	// operator who wrote "yes" must get the lane OFF, not a silent half-on.
	for _, off := range []string{"0", "false", "yes", "on", "enabled"} {
		t.Setenv(proxiedNativeEnv, off)
		if proxiedNativeEnabled() {
			t.Errorf("GC_BEADS_PROXIED_NATIVE=%q enabled the lane; only 1/true do", off)
		}
	}

	t.Setenv(proxiedNativeEnv, "1")
	t.Setenv(nativeForceFallbackEnv, "1")
	if !forceNativeFallback() {
		t.Error("GC_BEADS_FORCE_FALLBACK=1 must still report the escape hatch with the proxied lane on")
	}
	if !proxiedNativeEnabled() {
		t.Error("the two flags are independent predicates; ordering is the factory's job, not this one's")
	}
}

// TestProxiedKnobsDefaultAndFloor pins that a bad knob cannot turn a working
// city off: an unparseable or non-positive value falls back to the default
// rather than yielding a zero budget, which would make every read fail
// instantly.
func TestProxiedKnobsDefaultAndFloor(t *testing.T) {
	t.Setenv(proxiedGuardIntervalEnv, "")
	t.Setenv(proxiedReadBudgetEnv, "")
	if got := proxiedGuardInterval(); got != proxiedGuardIntervalDefault {
		t.Errorf("default guard interval = %v, want %v", got, proxiedGuardIntervalDefault)
	}
	if got := ProxiedReadBudget(); got != proxiedReadBudgetDefault {
		t.Errorf("default read budget = %v, want %v", got, proxiedReadBudgetDefault)
	}
	if proxiedReadBudgetDefault >= nativeReadRetryBudget {
		t.Fatalf("the proxied read budget (%v) must be well under the direct lane's %v: gc cannot restart a bd-owned proxy, so waiting out its rebind buys nothing",
			proxiedReadBudgetDefault, nativeReadRetryBudget)
	}

	cases := []struct {
		raw  string
		want time.Duration
	}{
		{raw: "30s", want: 30 * time.Second},
		{raw: "10ms", want: proxiedKnobFloor},
		{raw: "0", want: proxiedGuardIntervalDefault},
		{raw: "-5s", want: proxiedGuardIntervalDefault},
		{raw: "sometimes", want: proxiedGuardIntervalDefault},
	}
	for _, tc := range cases {
		t.Setenv(proxiedGuardIntervalEnv, tc.raw)
		if got := proxiedGuardInterval(); got != tc.want {
			t.Errorf("GC_BEADS_PROXIED_GUARD_INTERVAL=%q -> %v, want %v", tc.raw, got, tc.want)
		}
	}

	t.Setenv(proxiedReadBudgetEnv, "3s")
	if got := ProxiedReadBudget(); got != 3*time.Second {
		t.Errorf("GC_BEADS_PROXIED_READ_BUDGET=3s -> %v, want 3s", got)
	}
	t.Setenv(proxiedReadBudgetEnv, "nope")
	if got := ProxiedReadBudget(); got != proxiedReadBudgetDefault {
		t.Errorf("an unparseable read budget -> %v, want the default %v", got, proxiedReadBudgetDefault)
	}
}

// declaredProxiedVerdicts reads every ProxiedVerdict constant out of
// proxied_verdict.go, keyed by its Go name.
//
// It parses the source rather than listing the constants, because a list is the
// thing the guard above is trying not to be: Go has no enumeration for a named
// string type, so the only source of truth that cannot drift from production is
// production's own declaration.
func declaredProxiedVerdicts(t *testing.T) map[string]ProxiedVerdict {
	t.Helper()
	const source = "proxied_verdict.go"
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, source, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}

	declared := map[string]ProxiedVerdict{}
	for _, decl := range parsed.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		// A const block states its type once, on the first spec; the rest
		// inherit it. Tracking it is what keeps the sibling blocks in this file
		// (ProxiedSkewLane*, which are untyped strings) out of the set.
		typeName := ""
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); ok {
				typeName = ident.Name
			} else if value.Type != nil {
				typeName = ""
			}
			if typeName != "ProxiedVerdict" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s: %s is not a string literal; this guard cannot read it", source, name.Name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("%s: %s has an unreadable literal %s: %v", source, name.Name, literal.Value, err)
				}
				declared[name.Name] = ProxiedVerdict(unquoted)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("%s declared no ProxiedVerdict constants; the guard is broken, not satisfied", source)
	}
	return declared
}
