package main

import (
	"io"
	"testing"
)

// TestSeatHooksComposeTheCityConfigOncePerInvocation pins one composed city
// config per session-side hook invocation, as TestReadyLoadsCityConfigOnce and
// TestHookClaimComposesTheCityConfigOncePerInvocation pin it for `gc ready` and
// `gc hook`.
//
// Every fresh seat runs `gc prime --hook` at SessionStart and both
// `gc nudge drain --inject` and `gc mail check --inject` on every prompt. Each
// helper on those paths — the session proof, the provider-key write, the mail
// injection, the step reminder, every store open — loaded the city config on
// its own: 10-12 compositions per invocation on maintainer-city at 80-160 ms
// each, about 1-1.3 s of every hook (ga-ylpbsy.6).
//
// Not parallel: composedCityConfigLoads is process-wide.
func TestSeatHooksComposeTheCityConfigOncePerInvocation(t *testing.T) {
	none := func(*testing.T, hookBudgetFixture) {}
	for _, sc := range []struct {
		name string
		seed func(t *testing.T, f hookBudgetFixture)
		run  func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
	}{
		{name: "prime --hook/fresh", seed: none, run: primeHook},
		{name: "prime --hook/auto-handoff and mail", seed: func(t *testing.T, f hookBudgetFixture) {
			f.autoHandoff(t)
			f.sendMail(t, "status please")
		}, run: primeHook},
		{name: "prime --hook/active step", seed: func(t *testing.T, f hookBudgetFixture) { f.activeStep(t) }, run: primeHook},
		{name: "nudge drain --inject/empty", seed: none, run: nudgeDrainInject},
		{name: "nudge drain --inject/one nudge", seed: func(t *testing.T, f hookBudgetFixture) { f.queueNudge(t) }, run: nudgeDrainInject},
		{name: "nudge drain --inject/usage sample", seed: none, run: nudgeDrainInjectWithUsage},
		{name: "nudge drain --inject/active step", seed: func(t *testing.T, f hookBudgetFixture) { f.activeStep(t) }, run: nudgeDrainInject},
		{name: "mail check --inject/one mail", seed: func(t *testing.T, f hookBudgetFixture) { f.sendMail(t, "status please") }, run: mailCheckInject},
		{name: "mail check --inject/auto-handoff", seed: func(t *testing.T, f hookBudgetFixture) { f.autoHandoff(t) }, run: mailCheckInject},
	} {
		t.Run(sc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			sc.seed(t, f)
			before := composedCityConfigLoads.Load()
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			if grew := composedCityConfigLoads.Load() - before; grew != 1 {
				t.Fatalf("composed the city config %d times in one invocation, want exactly 1", grew)
			}
			if currentInvocationConfigMemo() != nil {
				t.Fatal("the hook left its invocation config memo open after returning")
			}
		})
	}
}
