# Operations — Debug Shortcuts and Tricks

Practical operational knowledge for running Gas Town day-to-day.

### Raise ulimit in existing Claude sessions (2026-04-01)
Claude Code inherits the shell's ulimit. If limits.d is updated but session is old:
```bash
ulimit -Su 4096  # Raise soft to hard limit (doesn't require re-login)
```
Full 16384 requires a new SSH session where PAM applies limits.d.

### Quick Dolt health check without gt (2026-04-01)
```bash
dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "SELECT 1"
```
If this works but gt commands hang, the issue is in the gt binary (process spawn, circuit breaker, or Dolt query complexity), not Dolt itself.

### List all P0 beads across rigs via raw SQL (2026-04-01)
```sql
SELECT CONCAT(db, '-', id) as bead, title FROM (
  SELECT 'of' as db, id, title FROM officeworld.issues WHERE status='open' AND priority=0
  UNION ALL
  SELECT 'ds' as db, id, title FROM deepwork_site.issues WHERE status='open' AND priority=0
  UNION ALL
  SELECT 'vaa' as db, id, title FROM villa_alc_ai.issues WHERE status='open' AND priority=0
  UNION ALL
  SELECT 'vap' as db, id, title FROM villa_ai_planogram.issues WHERE status='open' AND priority=0
) t ORDER BY db, id
```

### Check what's consuming process budget (2026-04-01)
```bash
# Total processes
ps -u pratham2 --no-headers | wc -l

# Thread-heavy consumers
ps -u pratham2 --no-headers -o pid,nlwp,args --sort=-nlwp | head -10

# Orphaned processes (ppid=1, stuck)
ps -u pratham2 --no-headers -o pid,ppid,args | awk '$2==1'

# Claude instances eating budget
ps -u pratham2 --no-headers -o pid,args | grep claude | grep -v grep
```

### Emergency agent cleanup (2026-03-30)
When process ceiling is hit and nothing works:
1. Kill all witnesses: they auto-respawn and aren't critical
2. Kill orphaned bd/gt (ppid=1)
3. Kill the Go compile if one is running in /tmp/gastown-patch
4. Check ulimit -u and raise if possible
5. Then retry the operation

### tmux session naming convention
- `hq-*` — HQ agents (mayor, deacon, boot, dog)
- `<rig>-witness` — Per-rig witness
- `<rig>-refinery` — Per-rig refinery
- `<rig>-polecat-<name>` — Named polecats
- `<rig>-crew-<name>` — Crew members
