# Recipe: Swarm Consensus with Fast Inference Providers

Swarm consensus runs the same question through multiple independent LLM
agents simultaneously and reduces their answers to a majority verdict.
Using Groq or Cerebras as the provider cuts per-voter latency from minutes
to seconds, making 5–9 voter quorums practical.

## Prerequisites

| Feature | PR |
|---------|----|
| Step-level `provider` field | #1193 |
| `tally` aggregation control | #1194 |

Enable `formula_v2` in your city config:

```toml
# city.toml
[daemon]
formula_v2 = true
```

## Formula layout

```
examples/swarm-vote/
├── mol-swarm-vote.toml   # orchestrator: fanout + tally
└── mol-ask-groq.toml     # voter fragment: one per fanout item
```

## How it works

```
vote ──► vote-fanout ──► voter 1 (mol-ask-groq)
             │        ──► voter 2
             │        ──► voter 3 …
         vote-tally ──► report
```

1. **`vote` step** builds a voter roster and closes with
   `gc.output_json = {"voters": [{"index":1}, …]}`.
2. **`vote-fanout` control** (injected automatically) reads the roster and
   spawns one `mol-ask-groq` molecule per entry — all in parallel.
3. Each **voter** independently answers the question and writes
   `gc.output_json = {"answer": "yes"}` before closing.
4. **`vote-tally` control** (injected automatically) waits for all voters,
   collects their `answer` fields, and computes the majority winner.
   - Majority (default): `>50 %` agreement required.
   - Unanimous: every voter must agree.
   - Any-pass: at least one voter outcome must be `"pass"`.
5. **`report` step** (automatically rewired to wait for `vote-tally`)
   reads `gc.tally_result` and writes a human-readable summary.

## Running the example

```bash
# From the gascity repo root
gc bd mol mol-swarm-vote \
  --formula-path examples/swarm-vote \
  --var question="Should we migrate the API gateway to gRPC?"
```

With 5 Groq voters the whole round typically completes in under 30 seconds.

## Tuning the vote

### Change voter count

```bash
gc bd mol mol-swarm-vote \
  --formula-path examples/swarm-vote \
  --var question="..." \
  --var voter_count=7
```

Use odd numbers (3, 5, 7, 9) to avoid ties.

### Switch aggregation mode

Edit `mol-swarm-vote.toml` and change the tally mode:

```toml
[steps.tally]
vote_field = "answer"
mode       = "unanimous"   # or "any-pass"
```

### Use a different vote value

If the question calls for more than yes/no, edit the voter fragment
(`mol-ask-groq.toml`) to produce a domain-specific answer:

```toml
# mol-ask-groq.toml — voter description excerpt
ANSWER="accept"  # or "reject", "escalate", etc.
```

Update `vote_field` and document the expected values in the formula.

### Use Cerebras instead of Groq

Change `provider = "groq"` to `provider = "cerebras"` in both formula
files, or override at dispatch time via `gc.provider` metadata.

## Provider routing

The `provider = "groq"` field in each step compiles to
`gc.provider = "groq"` in the recipe's step metadata. The dispatcher
uses this hint to select a matching agent. Ensure you have a Groq
(or Cerebras) agent registered in your city:

```toml
# city.toml
[[agents]]
id       = "groq-worker"
provider = "groq"
role     = "coder"
```

If no matching agent is available the dispatcher falls back to default
routing (the field is advisory, not mandatory).

## Implementation reference

| Component | Location |
|-----------|----------|
| `provider` field on steps | `internal/formula/types.go`, `compile.go` |
| `tally` block on steps | `internal/formula/types.go`, `graph.go` |
| `processTallyControl` | `internal/dispatch/tally.go` |
| Orchestrator formula | `examples/swarm-vote/mol-swarm-vote.toml` |
| Voter fragment formula | `examples/swarm-vote/mol-ask-groq.toml` |
