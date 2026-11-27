# Native input admission

`InputStores.Admission` owns the shared input-readiness decision for native
execution. Ready queries apply it before their limit; demand applies it to
candidate rows; hooks check before a new claim and revalidate before delivery;
ordinary native controls check after their existing lifecycle cleanup gate.
Keep those consumers on this domain predicate.

Use `beads.LiveFor` for authoritative reads through typed class handles;
`HandlesFor(handle).Live` can fall back to a cached logical reader. The demand
lane keeps unified exact-store candidates on their existing cached tier, then
applies live input admission off the planner pass. Preserve the split-store
and non-exact live candidate reads. Moving only Ready ahead of the cached claim
census can hide a new claim and turn a canceled respawn into an idle stop.

An exact class binding may refresh graph-only demand from its cache. Rows
carrying `gc.root_bead_id` or `gc.input_convoy_id` retain the off-pass gated
recording: the cache's local blocker verdict does not include input admission.
Never reintroduce a withheld native row through that cache optimization or
read live input stores from the demand pass.

Drain and fanout only expand/reconcile graphs: they validate input relations
without requiring descendant members to be execution-ready. The input convoy's
own prerequisites and parks, root/control parks, and existing graph dependency,
ownership and reservation gates still apply. Discovery must expose these
structural controls so a ready A can be materialized while B depends on A.
Generated item work uses its actual unit convoy; executable steps and effectful
checks require every current input in that scope. Never blanket-exempt controls
or use missing class handles as a permissive fixture fallback.

Read the root's current `gc.input_convoy_id`, the convoy's actual `tracks`
relations, and current blocking prerequisites through named Work and Graph
handles from the residency composition edge. Check explicit blocked/deferred
state, future deferrals and canonical dispatch holds. Every nonterminal input
is required. Terminal members use convoy terminal semantics; a closed input
convoy still exposes its current nonterminal members for admission. Read errors,
missing targets, duplicate residence, cycles and exhausted walk bounds withhold
new execution with an explicit error. Keep relation reads live and bounded.

An input stays open while its workflow produces its result. Do not infer
readiness from an input's route or assignee, copy prerequisites into a snapshot,
invent graph topology, reassign inputs, or add role-specific rules. Ownership,
route eligibility, exclusions and claim CAS remain independent constraints.

Finalizers, teardown and existing terminal/cancel cleanup must remain able to
settle workflows. Releasing a claim whose input changed is limited to a new,
undelivered claim through the existing conditional unwind protocol. Existing
obligations retain their owners; admission does not abort running work.

Recorded terminal drains (`succeeded`/`failed`) and spawned fanouts only settle
existing graph state. Their shared admission keeps active root/control parks,
but never opens input Work scopes or traverses input relations. Expanded or
completing drains can advance shared rows and must still validate live inputs.
Keep this operation/state distinction in the shared predicate so discovery and
execution agree; never exempt every drain or fanout from input admission.
