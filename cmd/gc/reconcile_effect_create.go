package main

import "context"

// The create effect (CONTRACT v5 C1, C2, P3; D-21): a pool or named create
// runs as one more effect kind on the session executor, under the 60s
// deadline admission sets, keyed by the instance token the planner minted
// into its plan at submit. Its body is the allocator's guarded create
// (createEffects.run, allocator_create.go): the one v2 write outside a
// fenced writer, v5 R1's exception 1, under the identifier flock with a live
// re-census.

// createEffect is the create kind's effect: the pass's create inputs, and
// the plan the planner minted its token into.
func createEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		return p.creates.run(ctx, p.create, it.plan()).settlement()
	}
}
