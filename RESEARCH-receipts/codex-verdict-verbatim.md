## Result

Both requested capabilities are absent. The current checkout pins Beads v1.1.0, and `gc bd` delegates to the `bd` executable rather than adding notification behavior itself: [go.mod:25](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/go.mod:25), [cmd_bd.go:300](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/cmd/gc/cmd_bd.go:300).

### 1. Notify on human-gate creation

**VERDICT = ABSENT**

`bd gate create` constructs an open gate, records its await fields, adds the blocking dependency, and commits—there is no assignee/waiter recipient and no mail or nudge call: [gate.go:262](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:262), [gate.go:294](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:294), [gate.go:307](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:307). Formula compilation likewise only synthesizes a sibling gate bead and blocking edge, with no assignee or notification side effect; the materializer does not copy `RecipeGate` into the created bead: [compile.go:520](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/formula/compile.go:520), [molecule.go:1303](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/molecule/molecule.go:1303).

### 2. Staleness re-nudge

**VERDICT = ABSENT**

`bd gate check` explicitly skips unsupported types, including `human`, so neither age nor `Timeout` is evaluated for a human gate: [gate.go:554](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:554), [gate.go:568](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:568). `--escalate` applies only after GitHub checks return an escalated result; it shells out to external `gt escalate`, with no addressee mapping and no direct `gc mail` or `gc session nudge`: [gate.go:606](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:606), [gate.go:891](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:891).

### `gate-sweep` and configuration

**No: neither `bd gate check --escalate` nor `gate-sweep` mails or nudges the addressee of an aging open human gate.**

The bundled order runs every 30 seconds, but its script checks only `timer` and `gh` gates—never `human`: [gate-sweep.toml:4](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/bootstrap/packs/core/orders/gate-sweep.toml:4), [gate-sweep.sh:28](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/bootstrap/packs/core/assets/scripts/gate-sweep.sh:28). Timer gates resolve rather than escalate, while GitHub escalation is based on failure/closure, not staleness: [gate.go:855](/home/csauer/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go:855).

The formula schema has only `{type, id, timeout}`—no addressee, notification, repeat interval, or reminder policy—and the specification explicitly classifies gate types as accepted but inert with no bundled watcher: [formula-spec-v2.md:224](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/docs/reference/specs/formula-spec-v2.md:224), [formula-spec-v2.md:903](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/docs/reference/specs/formula-spec-v2.md:903).

Closest existing machinery:

- `gate-sweep` supplies a configurable order cadence, but has no human-gate path.
- `timeout` is stored on gates, but is inert for `human`.
- Core’s generic escalation script can mail a configurable recipient, but nothing in gate checking invokes it: [escalate.sh:49](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/bootstrap/packs/core/assets/scripts/escalate.sh:49).
- `nudge-on-route` nudges routed sessions once and explicitly deduplicates subsequent events, so it is neither gate-specific nor a re-nudge loop: [nudge-on-route.toml:10](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/bootstrap/packs/core/orders/nudge-on-route.toml:10), [nudge-on-route.sh:107](/home/csauer/gascity-work/sc-lcwsqf-notify-gate/internal/bootstrap/packs/core/assets/scripts/nudge-on-route.sh:107).

No existing config switch connects these pieces; both capabilities require new watcher/wiring code.
