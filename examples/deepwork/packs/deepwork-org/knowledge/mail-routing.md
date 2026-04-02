# Mail Routing Configuration

The mesh mail handler dynamically routes incoming messages to the right local agent
based on keyword matching against the message subject and body.

## How It Works

1. Every 2-minute sync checks for unread mail
2. For each message, subject + body are scanned for keywords
3. First matching route wins — message is nudged to that agent's tmux session
4. If the target agent is dead, falls back to the next alive agent
5. P0/P1 messages always trigger a nudge; P2+ are logged for session pickup

## Default Routing Table

| Priority | Keywords | Target |
|----------|----------|--------|
| 1 | planogram, vap-, ai-planogram, villa_ai_planogram | vap-crew-manager |
| 2 | alc, vaa-, alc-ai, villa_alc | vaa-crew-manager |
| 3 | arcade, gta-, gt_arcade | gta-crew-manager |
| 4 | mesh, config, pack, improve, sync, invite, peer, gtconfig | hq-mayor |
| 5 | task, bead, issue, pr, review, deploy, release | hq-mayor |
| fallback | (no match) | first alive: mayor > vap > vaa > gta |

## Extending the Routing Table

Edit `scripts/mesh-mail-handler.sh` and add entries to the `ROUTE_TABLE` array:

```bash
ROUTE_TABLE=(
  "my-keyword|another-keyword:target-session-name"
  # ... existing entries
)
```

More specific patterns should come first (first match wins).

## Auto-Replies

Messages with subjects matching `status`, `ping`, `alive`, or `heartbeat` get
an automatic reply with: online status, peer count, active agent count, last sync time.

## File Locations

- Handler script: `scripts/mesh-mail-handler.sh`
- Pending log: `$GT_ROOT/.mesh-inbox-pending.log`
- Triggered by: `scripts/mesh-sync.sh` (when unread > 0)
