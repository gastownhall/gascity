# Deepwork Examples

Production examples from the Deepwork multi-agent swarm — a Gas Town running 6 rigs with 15+ agents, private wasteland federation, and external contributor onboarding.

## Packs

### `packs/deepwork-org/`

Full org config pack for running a multi-agent Gas Town with:

- **Private wasteland federation** — DoltHub-based task board with bidirectional sync
- **Deterministic effort estimation** — pattern-based task sizing (trivial → epic)
- **Self-evolving knowledge** — 19 knowledge files, auto-harvested from closed beads every 6h
- **Gitea → GitHub mirror** — hourly sync with auto-release creation
- **10 agent roles** — mayor, deacon, witness, refinery, polecat, crew, coordinator, planner, reviewer, worker
- **7 cron jobs** — thread guardrail, log rotation, knowledge evolution, GitHub mirror, wasteland push, pack update, auto-release
- **12 wasteland governance rules** — enforced via deepwork-governance.yaml

### Key Features

**Wasteland Integration in Formulas:**
- `mol-polecat-work` — polecats auto-detect wasteland items at start (3 cases: direct link, fuzzy match, auto-create), claim them, and complete on `gt done`
- `mol-witness-patrol` — witnesses check wasteland each patrol cycle and dispatch unclaimed work
- Smart internal filter ensures only externally meaningful work reaches wasteland

**External Contributor Onboarding:**
- Complete onboarding guide at `docs/wasteland/ONBOARDING.md`
- Task posting template at `docs/wasteland/POST_TEMPLATE.md`
- Effort/priority guide, reputation system, project catalog

### Prerequisites

This pack requires a [patched gt binary](https://github.com/gastownhall/gastown/pull/3493) that reads wasteland config from `mayor/wasteland.json` instead of hardcoding `hop/wl-commons`.

### Source

- GitHub: [masti-ai/deepwork-org-config-pack](https://github.com/masti-ai/deepwork-org-config-pack)
- Maintainer: [@pratham-bhatnagar](https://github.com/pratham-bhatnagar)
