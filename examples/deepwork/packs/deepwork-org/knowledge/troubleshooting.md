# Troubleshooting — Common Issues and Fixes

## Dolt Issues

**"database is read only"**
The dolt clone was created without write permissions. Fix:
```bash
rm -rf /tmp/mesh-sync-clone
GT_ROOT=/path/to/gt bash .gt-mesh/scripts/mesh-sync.sh
```

**"permission denied" on dolt clone**
Dolt credentials not configured or expired. Fix:
```bash
dolt creds ls          # Check active credential
dolt creds use <id>    # Switch to valid credential
```

**"merge conflict" on dolt pull**
Multiple GTs wrote to the same table simultaneously. Fix:
```bash
cd /tmp/mesh-sync-clone
dolt conflicts resolve --ours .   # Keep local changes
dolt add . && dolt commit -m "resolve merge conflict"
dolt push
```

## Git Issues

**"push rejected" (non-fast-forward)**
Someone else pushed while you were working. Fix:
```bash
git pull --rebase && git push
```

**Polecat branch has no commits**
Polecat died before committing. Check for uncommitted files:
```bash
cd /path/to/polecat/workspace
git status --short
git ls-files --others --exclude-standard
```
Rescue files manually, commit from crew workspace.

## Tunnel Issues

**Backend/frontend not responding**
Cloudflare tunnels expire. Restart:
```bash
# Check if process is running
pgrep -f cloudflared
# If not, restart the tunnel
cloudflared tunnel --url http://localhost:PORT
```

**"python: command not found"**
Use `python3` not `python` on Ubuntu/Debian systems.

## Mesh Issues

**Messages not delivering**
Check sync is running:
```bash
GT_ROOT=/path/to/gt bash .gt-mesh/scripts/mesh-sync.sh
```
Then verify message exists:
```bash
cd /tmp/mesh-sync-clone
dolt sql -q "SELECT id, subject FROM messages WHERE to_gt='target' ORDER BY created_at DESC LIMIT 3;" -r csv
```

**Peer shows stale last_seen**
The peer's sync daemon may have stopped. Nudge them or check their tmux session.

## Polecat Issues

**"NEEDS_RECOVERY" but bead is closed**
False positive — work already landed on dev through another path. Safe to nuke:
```bash
gt polecat nuke rig/name --force
```

**Polecat session "not running" but state is "working"**
Session crashed or hit credit limit. Check for uncommitted work, rescue if needed, then nuke.

## Bead Issues

**"unknown flag" errors with bd/gt commands**
Check the command syntax — flags changed between versions:
```bash
gt polecat list villa_alc_ai    # positional arg, not --rig flag
gt polecat nuke rig/name        # requires rig/name format
```
