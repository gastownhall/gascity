# Runbooks

## Dolt Recovery

```bash
# 1. Check if alive
pgrep -u pratham2 -f "dolt sql-server" || echo "DEAD"

# 2. Direct health check
dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q "SELECT 1"

# 3. If dead: restart
gt dolt start

# 4. If alive but gt hangs: check processes
ulimit -u && ps -u pratham2 --no-headers | wc -l
```

## Sling Work

```bash
gt sling <bead-id> <rig>                    # Auto-spawn polecat + convoy
gt sling <bead-id> <rig> --no-boot --no-convoy  # Lightweight
gt sling respawn-reset <bead-id>             # If respawn limit hit
gt sling <bead-id> <rig> --force             # Override limits
```

## Kill Stuck Agents

```bash
# Find hanging processes
ps -u pratham2 --no-headers -o pid,args | grep -E '(gt |bd )' | grep -v grep

# Kill specific
kill <pid>

# Nuclear: kill all witnesses
ps -u pratham2 --no-headers -o pid,args | grep witness | grep claude | awk '{print $1}' | xargs kill
```

## Process Budget

```bash
ulimit -u                     # Limit
ps -u pratham2 --no-headers | wc -l  # Usage
```

## Rebuild gt Binary

```bash
cd /tmp/gastown-patch && SKIP_UPDATE_CHECK=1 make install
```
