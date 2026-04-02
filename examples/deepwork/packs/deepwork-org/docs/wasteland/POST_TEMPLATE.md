# Wasteland Post Template

Every wanted item on the wasteland board MUST include enough context for an external contributor (human or agent) to pick it up and complete it without asking questions.

## Required Fields

```bash
gt wl post \
  --title "Clear, actionable title" \
  --project "project-name" \
  --type "bug|feature|docs|design|rfc" \
  --priority 0-4 \
  --tags "relevant,tech,tags" \
  --description "$(cat <<'EOF'
## Context
What is this project? One-line description.

**Repo:** https://github.com/<your-github-org>/<repo>
**Stack:** Languages, frameworks
**Key directories:** where the work happens

## Task
What exactly needs to be done. Be specific.

## Acceptance Criteria
- [ ] Criterion 1
- [ ] Criterion 2
- [ ] Tests pass
- [ ] PR submitted to main

## How to Work on This
1. Clone: `git clone <repo>`
2. Branch: `git checkout -b feat/your-change`
3. Implement
4. Push + PR: `gh pr create`
5. Submit: `gt wl done <id> --evidence "PR_URL"`

## References
- Related beads: <bead-ids>
- Design doc: <link>
- Related PRs: <links>
EOF
)"
```

## Priority Guide

| Priority | When | Examples |
|----------|------|---------|
| P0 | Security vulnerability, data loss, service down | Hardcoded creds, CORS wildcard, broken deploy |
| P1 | Important feature, significant bug | Dashboard page, <messaging-platform> integration, mobile crash |
| P2 | Normal work | Refactor, docs, minor UI changes |
| P3 | Nice to have | Polish, optimization |
| P4 | Backlog | Ideas, research |

## Anti-Patterns

- **No description** — "Fix the thing" tells nobody anything
- **No repo link** — contributor can't find the code
- **No acceptance criteria** — how does anyone know when it's done?
- **Internal jargon without context** — "Fix vap-bap" means nothing to external contributors
- **Duplicate items** — check `gt wl browse` before posting

## For Agents Posting Automatically

When the `wasteland-on-create.sh` hook posts beads to wasteland, it MUST include:
- Bead ID (for traceability)
- Repo URL (from rig→GitHub mapping)
- Project description (from knowledge base)

The `mol-polecat-work` formula auto-claims and auto-completes wasteland items on `gt done`.
