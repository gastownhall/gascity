# Account Cycling — Handling Credit Limits Gracefully

When a Claude account hits its rate/credit limit, work must continue on another account.
This is routine, not an emergency. Handle it cleanly.

## Before You Hit the Limit

- Watch for "rate limit" or "credit limit" warnings in tool output
- When you see them: STOP new work, focus on preserving context

## Handoff Protocol

1. **Commit and push** all code changes immediately
2. **Close or update beads** with current status
3. **Write a handoff mail** to yourself:
   ```bash
   gt handoff -s "Account cycling: credit limit on <account>" -m "Current work: <summary>. Next steps: <what remains>."
   ```
4. The next session (on a fresh account) picks up from the handoff

## What Gets Lost

- Your in-memory conversation context (mitigated by handoff mail)
- Uncommitted code changes (mitigated by pushing before cycling)
- Running background processes (tunnels, dev servers — must restart)

## What Survives

- Git commits (pushed to remote)
- Bead state (in Dolt, persists across sessions)
- Hook state (`gt hook` persists)
- Memory files (in `.claude-accounts/` directory)
- Mesh mail (in DoltHub)

## Common Mistakes

- **Trying to finish "one more thing"** after seeing the limit warning — you'll lose it
- **Not pushing code** — the next session can't see uncommitted work
- **Not writing handoff context** — the next session starts blind
- **Forgetting to restart tunnels** — services go dark between sessions

## Account Rotation Order

Accounts are configured per-user. Check which account is active:
```bash
echo $CLAUDE_ACCOUNT  # or check tmux session name prefix
```

When one account is exhausted, the daemon or human rotates to the next.
Each account has independent rate limits that reset on their own schedule.
