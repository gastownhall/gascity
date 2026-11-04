# Native input admission

`InputStores.Admission` owns the shared input-readiness decision for native
execution. Ready queries apply it before their limit; demand applies it to
candidate rows; hooks check before a new claim and revalidate before delivery;
ordinary native controls check after their existing lifecycle cleanup gate.
Keep those consumers on this domain predicate.

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
