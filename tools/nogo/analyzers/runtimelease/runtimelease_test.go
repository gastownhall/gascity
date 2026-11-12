package runtimelease_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/runtimelease"
)

const pkg = "example.com/p"

// src declares the runtime package's shapes in the linted package itself:
// analyzertest packages cannot import.
const src = `package p

type Context interface{}

func background() Context { return nil }

type Config struct{}

type Provider interface {
	Start(ctx Context, name string, cfg Config) error
	Stop(name string) error
}

type ProcessTableScanner interface{ TerminateRuntime(name string) error }

type RelaunchProvider interface {
	Relaunch(ctx Context, name string, cfg Config) error
}

type UnattendedSessionStopper interface {
	StopUnattendedSession(name, expectedToken string) error
}

type SessionObjectKiller interface {
	KillCorpseObject(name, objectID, created string) (int, error)
	KillZombieObject(name, objectID, created, panePID string) (int, error)
}

type ServerLifecycleProvider interface{ TeardownServer() error }

type stopper interface{ Stop(name string) error }

type timer struct{}

func (timer) Stop() bool { return true }

func StopForCleanup(p Provider, name string) error { return p.Stop(name) }

type tmux struct{}

func (tmux) Start(Context, string, Config) error { return nil }
func (tmux) Stop(string) error                            { return nil }
func (tmux) TerminateRuntime(string) error                { return nil }

type ActorKind int

const (
	ActorOperator ActorKind = 1
	ActorSweep    ActorKind = 5
)

type CityDir struct{ abs string }

type Actor struct {
	Kind ActorKind
	City CityDir
}

func killTarget(city, name string) error { return nil }
func killCtx(ctx Context, by Actor, city, name string) error { return nil }

func unleased(sp Provider) { _ = sp.Stop("a") } // L26
func unleasedConcrete(t tmux) { _ = t.Start(background(), "a", Config{}) } // L27
func unleasedKill(s ProcessTableScanner) { _ = s.TerminateRuntime("a") } // L28
func unleasedCleanup(sp Provider) { _ = StopForCleanup(sp, "a") } // L29
func narrow(s stopper) { _ = s.Stop("a") } // LN1
func methodValue(sp Provider) func(string) error { return sp.Stop } // LN2
func cleanupValue() func(Provider, string) error { return StopForCleanup } // LN3
func relaunch(r RelaunchProvider) { _ = r.Relaunch(background(), "a", Config{}) } // LN4
func unattended(u UnattendedSessionStopper) { _ = u.StopUnattendedSession("a", "t") } // LN5
func objectKill(k SessionObjectKiller) { _, _ = k.KillCorpseObject("a", "o", "c") } // LN6
func teardown(l ServerLifecycleProvider) { _ = l.TeardownServer() } // LN7
func otherStop(t timer) { _ = t.Stop() }

func underCallersLease(sp Provider) { _ = sp.Stop("a") }
func leaseHolder(sp Provider) { underCallersLease(sp) }
func stranger(sp Provider) { underCallersLease(sp) } // L33
func entryPoint(sp Provider) { _ = sp.Stop("a") }

const noCity = ""

func helpers(ctx Context, city string) {
	_ = killTarget("", "a") // L39
	_ = killTarget(noCity, "a") // L40
	empty := ""
	_ = killTarget(empty, "a") // L42
	_ = killTarget(city, "a")
	_ = killCtx(ctx, Actor{Kind: ActorOperator, City: CityDir{"/c"}}, "", "a") // L45
}

func sweeper(ctx Context) { _ = killCtx(ctx, Actor{Kind: ActorSweep}, "", "a") }

func notASweeper(ctx Context) { _ = killCtx(ctx, Actor{Kind: ActorSweep}, "/c", "a") } // LS1

func cityless(ctx Context) { _ = killCtx(ctx, Actor{Kind: ActorOperator}, "/c", "a") } // LS2
`

func analyzer(allowed map[string]runtimelease.Allowance) *runtimelease.Config {
	return &runtimelease.Config{
		Packages:    []string{pkg},
		RuntimePkg:  pkg,
		CityHelpers: map[string]int{"killTarget": 0, "killCtx": 2},
		Sweep:       pkg + ".ActorSweep",
		SweepSites:  []string{pkg + ":sweeper"},
		Actor:       pkg + ".Actor",
		Allowed:     allowed,
	}
}

func run(t *testing.T, cfg *runtimelease.Config) []string {
	t.Helper()
	var got []string
	for _, d := range analyzertest.Run(t, runtimelease.New(*cfg), pkg, map[string]string{"p.go": src}) {
		got = append(got, fmt.Sprintf("%d %s", d.Line, d.Message))
	}
	return got
}

func TestRuntimeLeaseLint(t *testing.T) {
	allowed := map[string]runtimelease.Allowance{
		pkg + ":StopForCleanup":    {Reason: "the runtime package's own"},
		pkg + ":underCallersLease": {Reason: "its caller's lease", Callers: []string{"leaseHolder"}},
		pkg + ":entryPoint":        {Reason: "an entry point"},
		pkg + ":gone":              {Reason: "a stale entry"},
	}
	got := run(t, analyzer(allowed))
	want := map[string]string{
		"L26": "unleased calls a provider's Stop",
		"L27": "unleasedConcrete calls a provider's Start",
		"L28": "unleasedKill calls a provider's TerminateRuntime",
		"L29": "unleasedCleanup calls a provider's StopForCleanup",
		"LN1": "narrow calls a provider's Stop",
		"LN2": "methodValue calls a provider's Stop",
		"LN3": "cleanupValue calls a provider's StopForCleanup",
		"LN4": "relaunch calls a provider's Relaunch",
		"LN5": "unattended calls a provider's StopUnattendedSession",
		"LN6": "objectKill calls a provider's KillCorpseObject",
		"LN7": "teardown calls a provider's TeardownServer",
		"L33": "stranger is not one of its callers",
		"L39": "killTarget is handed no city path",
		"L40": "killTarget is handed no city path",
		"L42": "killTarget is handed no city path",
		"L45": "killCtx is handed no city path",
		"LS1": "notASweeper names the stop sweep's actor kind",
		"LS2": "cityless builds an actor with no City",
		"":    "allowlisted gone calls no provider",
	}
	for marker, msg := range want {
		if line := markerLine(marker); !slicesContain(got, fmt.Sprintf("%d ", line), msg) {
			t.Errorf("no finding %q on line %d", msg, line)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestRuntimeLeaseLintOutOfScope(t *testing.T) {
	cfg := analyzer(nil)
	cfg.Packages = []string{"example.com/other"}
	if got := run(t, cfg); len(got) != 0 {
		t.Fatalf("an unlinted package reported %v", got)
	}
}

func slicesContain(got []string, prefix, msg string) bool {
	for _, g := range got {
		if strings.HasPrefix(g, prefix) && strings.Contains(g, msg) {
			return true
		}
	}
	return false
}

// markerLine is the source line carrying "// marker", or 1 (the package
// clause, where stale entries are reported) for "".
func markerLine(marker string) int {
	if marker == "" {
		return 1
	}
	for i, line := range strings.Split(src, "\n") {
		if strings.HasSuffix(line, "// "+marker) {
			return i + 1
		}
	}
	return -1
}

// skippedSrc is a file another lint covers (SkipFiles): its runtime verbs
// are not this lint's, but the actor rules still hold in it.
const skippedSrc = `package p

func effectStop(sp Provider) { _ = sp.Stop("a") }

func effectActor(ctx Context) { _ = killCtx(ctx, Actor{Kind: ActorOperator}, "/c", "a") } // LK1
`

// actorSrc holds the actor rules' other shapes: a package-level use of the
// sweep's kind, and an actor built field by field.
const actorSrc = `package p

const sweepKind = ActorSweep // LA1

func fieldBuilt(ctx Context) {
	var by Actor // LA2
	by.Kind = ActorOperator
	_ = killCtx(ctx, by, "/c", "a")
}

func fieldBuiltWithCity(ctx Context) {
	var by Actor
	by.Kind = ActorOperator
	by.City = CityDir{"/c"}
	_ = killCtx(ctx, by, "/c", "a")
}

func assignedWhole(ctx Context, from Actor) {
	var by Actor
	by = from
	_ = killCtx(ctx, by, "/c", "a")
}
`

// TestRuntimeLeaseActorRulesEverywhere: the actor rules hold in a skipped
// file and at package scope, and catch an actor built by field assignment
// with no City; a skipped file's verbs stay unreported.
func TestRuntimeLeaseActorRulesEverywhere(t *testing.T) {
	cfg := analyzer(map[string]runtimelease.Allowance{pkg + ":StopForCleanup": {Reason: "the runtime package's own"}})
	cfg.SkipFiles = []string{"reconcile_effect"}
	diags := analyzertest.Run(t, runtimelease.New(*cfg), pkg, map[string]string{"p.go": src, "reconcile_effect_x.go": skippedSrc, "actors.go": actorSrc})
	got := map[string][]string{}
	for _, d := range diags {
		got[d.File] = append(got[d.File], fmt.Sprintf("%d %s", d.Line, d.Message))
	}
	want := map[string]map[int]string{
		"reconcile_effect_x.go": {lineOf(skippedSrc, "LK1"): "effectActor builds an actor with no City"},
		"actors.go": {
			lineOf(actorSrc, "LA1"): "package scope names the stop sweep's actor kind",
			lineOf(actorSrc, "LA2"): "fieldBuilt declares an actor it never gives a City",
		},
	}
	for file, lines := range want {
		if len(got[file]) != len(lines) {
			t.Errorf("%s: findings %v, want %d", file, got[file], len(lines))
		}
		for line, msg := range lines {
			if !slicesContain(got[file], fmt.Sprintf("%d ", line), msg) {
				t.Errorf("%s: no finding %q on line %d; got %v", file, msg, line, got[file])
			}
		}
	}
}

// lineOf is the line of text carrying "// marker".
func lineOf(text, marker string) int {
	for i, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(line, "// "+marker) {
			return i + 1
		}
	}
	return -1
}
