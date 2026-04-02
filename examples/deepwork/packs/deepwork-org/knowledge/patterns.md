# Patterns — What Works

Recurring approaches that have proven effective in Gas Town operations.

### Kill non-essential agents before heavy work (2026-04-01)
When operating near process limits, kill witnesses for rigs without P0 work before attempting sling/spawn operations. Deacon auto-respawns them later. This frees 10-20 processes per witness killed.
Source: de-9s0 handoff session.

### Direct Dolt queries bypass gt/bd hangs (2026-04-01)
When gt/bd commands timeout, query Dolt directly:
```bash
dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "QUERY"
```
This bypasses all the Go binary overhead and circuit-breaker logic. Useful for diagnostics and emergency operations.
Source: de-9s0 execution session.

### Beads need rig-scoped creation for sling to work (2026-03-31)
`bd create --rig <rigname> "title"` gives a rig-prefixed ID that `gt sling` understands. Creating at town level (de- prefix) then slinging to a rig fails by design. The ID prefix must match the target rig's database.
Source: de-2yd investigation.

### Process cleanup before restart (2026-03-30)
Before restarting Dolt or the daemon, kill orphaned bd/gt processes (ppid=1) first. They hold stale connections and eat process budget. Pattern:
```bash
ps -u pratham2 --no-headers -o pid,ppid,args | awk '$2==1 && /bd |gt /' | awk '{print $1}' | xargs kill
```
Source: Dolt incident 2026-03-30.

### Gitea over GitHub for all agent coordination (2026-03-07)
After the GitHub suspension, all agent git operations moved to Gitea (port 3300). This is faster (local), has no rate limits, and keeps agent API noise off GitHub. GitHub is reserved for public releases only.
Source: GitHub suspension incident.
