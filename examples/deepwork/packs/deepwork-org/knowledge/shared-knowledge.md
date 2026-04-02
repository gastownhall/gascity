# Shared Knowledge — Deepwork-AI Mesh

## GitHub Labels (Multi-GT Coordination)

| Label | When to Use |
|-------|-------------|
| `gt-task` | Any cross-GT work assignment |
| `gt-from:<id>` | Which GT created the task |
| `gt-to:<id>` | Which GT should pick it up |
| `gt-status:pending` | Waiting for pickup |
| `gt-status:claimed` | Work in progress |
| `gt-status:done` | Delivered (PR created) |
| `needs-review` | PR needs parent review |
| `priority:p0/p1/p2` | Urgency levels |

## Branch Naming

```
gt/<instance-id>/<issue-number>-<short-description>
```

## PR Body Template

```markdown
Closes #<issue-number>

## Changes
- Bullet points

## Testing
- How you tested
```

## Pro Tips

1. Always `gh auth status` before GitHub work
2. Claim issues before starting (`gt-status:claimed`)
3. Mark done after PR (`gt-status:done`)
4. Never push directly to dev/main
5. Use `gt prime` after session start for full context
6. Update memory files when you learn something new
7. When idle, ask: "Any beads or work I should pick up?"
8. Use `gt mesh improve report` when you hit ANY friction — the loop makes everything better
9. Check `gt mesh inbox` at session start — messages may be waiting
10. Coordinate via mesh mail, not through the human — you have the tools

## Communication Style

- Short, direct messages
- No fluff or unnecessary context
- Code speaks louder than descriptions
- When delegating: send instructions via mesh mail, don't report to user what someone else "needs to do"

## Mesh Mail Behavior

- Incoming mail is auto-routed to the right agent based on content keywords
- P0/P1 messages nudge agents immediately
- Status/ping requests get auto-replies
- If you need something from another GT, use `gt mesh send` — don't wait for a human session
- Check inbox at every session start: `gt-mesh inbox`

## Self-Improving Loop

Every time you use the mesh and hit a problem:
```bash
# Report the finding
gt mesh improve report "description" --category bug --command sync --fix "how to fix it"

# After fixing, graduate to shared knowledge
gt mesh improve graduate <improvement-id>
```
This makes the mesh smarter for ALL nodes, not just your session.
