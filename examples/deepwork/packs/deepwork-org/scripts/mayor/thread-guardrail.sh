#!/bin/bash
# Thread Guardrail v2 for pratham2
# Prevents thread exhaustion by: killing orphan node processes, capping thread-heavy
# non-essential processes, and escalating when approaching limits.
# Cron: * * * * * /home/pratham2/gt/mayor/thread-guardrail.sh >> /tmp/thread-guardrail.log 2>&1

set -euo pipefail

USER="pratham2"
LOG_PREFIX="[guardrail $(date +%Y-%m-%dT%H:%M:%S)]"

# Hard limit for pratham2 (ulimit -u = 3072)
LIMIT=3072

# Thresholds (tight — 3072 is very low)
ORPHAN_ALWAYS=1      # Always kill orphan node processes regardless of thread count
WARN_PCT=40          # 40% = ~1,229 threads — start logging top consumers
ACTION_PCT=60        # 60% = ~1,843 threads — kill non-essential processes
CRITICAL_PCT=75      # 75% = ~2,304 threads — kill aggressively + alert

# Count current threads
CURRENT=$(ps -Lu "$USER" --no-headers 2>/dev/null | wc -l)
PCT=$(( CURRENT * 100 / LIMIT ))

echo "$LOG_PREFIX threads=$CURRENT limit=$LIMIT usage=${PCT}%"

# ============================================================
# ALWAYS: Kill orphan node/next/npm processes
# These are the #1 cause of thread exhaustion. A "next dev" server
# spawns 20+ threads and persists after the polecat that started it dies.
# ============================================================
kill_orphan_nodes() {
    local killed=0
    # Find node processes owned by pratham2 whose parent is init (ppid=1) or
    # whose parent tmux session no longer exists
    while IFS= read -r line; do
        pid=$(echo "$line" | awk '{print $1}')
        ppid=$(echo "$line" | awk '{print $2}')
        threads=$(echo "$line" | awk '{print $3}')
        etime=$(echo "$line" | awk '{print $4}')
        cmd=$(echo "$line" | cut -d' ' -f5-)

        # Skip if PID is gone
        [ -d "/proc/$pid" ] || continue

        # Orphan = parent is PID 1 (reparented after parent died)
        # or parent is not a tmux/claude/bash process
        if [ "$ppid" -eq 1 ]; then
            echo "$LOG_PREFIX ORPHAN node (ppid=1): PID $pid, ${threads} threads, age=$etime — killing"
            kill -TERM "$pid" 2>/dev/null || true
            killed=$((killed + 1))
        fi
    done < <(ps -u "$USER" --no-headers -o pid,ppid,nlwp,etime,args 2>/dev/null | grep -E '[/]node\b|next-server|next dev|npm run|npx ' | grep -v grep || true)

    # Also kill any node_modules/.bin processes that are orphaned
    while IFS= read -r line; do
        pid=$(echo "$line" | awk '{print $1}')
        ppid=$(echo "$line" | awk '{print $2}')
        threads=$(echo "$line" | awk '{print $3}')
        if [ "$ppid" -eq 1 ] && [ -d "/proc/$pid" ]; then
            echo "$LOG_PREFIX ORPHAN node_modules process: PID $pid, ${threads} threads — killing"
            kill -TERM "$pid" 2>/dev/null || true
            killed=$((killed + 1))
        fi
    done < <(ps -u "$USER" --no-headers -o pid,ppid,nlwp,args 2>/dev/null | grep 'node_modules/.bin' | grep -v grep || true)

    if [ "$killed" -gt 0 ]; then
        echo "$LOG_PREFIX Killed $killed orphan node processes"
    fi
}

# Always run orphan cleanup
kill_orphan_nodes

# Also always kill dolt send-metrics (telemetry, ~100 threads, never needed)
METRICS_PIDS=$(pgrep -u "$USER" -f "dolt send-metrics" 2>/dev/null || true)
if [ -n "$METRICS_PIDS" ]; then
    echo "$LOG_PREFIX Killing dolt send-metrics ($METRICS_PIDS)"
    echo "$METRICS_PIDS" | xargs kill 2>/dev/null || true
fi

# Recount after orphan cleanup
CURRENT=$(ps -Lu "$USER" --no-headers 2>/dev/null | wc -l)
PCT=$(( CURRENT * 100 / LIMIT ))

if [ "$PCT" -lt "$WARN_PCT" ]; then
    exit 0
fi

# ============================================================
# WARNING ZONE (30%+) — log top consumers
# ============================================================
echo "$LOG_PREFIX WARNING: ${PCT}% thread usage ($CURRENT/$LIMIT)"
echo "$LOG_PREFIX Top thread consumers:"
ps -u "$USER" --no-headers -o pid,nlwp,etime,comm 2>/dev/null | sort -k2 -rn | head -10 | while read pid nlwp etime comm; do
    echo "$LOG_PREFIX   PID=$pid threads=$nlwp age=$etime cmd=$comm"
done

if [ "$PCT" -lt "$ACTION_PCT" ]; then
    exit 0
fi

# ============================================================
# ACTION ZONE (50%+) — kill non-essential heavy processes
# ============================================================
echo "$LOG_PREFIX ACTION: ${PCT}% — killing non-essential processes"

# Kill ALL node/next/npm processes (not just orphans) — dev servers can restart
echo "$LOG_PREFIX Killing all node/next/npm processes..."
pkill -u "$USER" -f "next-server" 2>/dev/null || true
pkill -u "$USER" -f "next dev" 2>/dev/null || true
pkill -u "$USER" -f "npm run dev" 2>/dev/null || true
pkill -u "$USER" -f "npx " 2>/dev/null || true
# Be more careful with generic "node" — only kill if high thread count
ps -u "$USER" --no-headers -o pid,nlwp,args 2>/dev/null | grep '[/]node\b' | grep -v 'claude\|gt\|bd' | while read pid nlwp cmd; do
    if [ "$nlwp" -gt 10 ]; then
        echo "$LOG_PREFIX Killing node PID $pid ($nlwp threads): $(echo "$cmd" | head -c 60)"
        kill -TERM "$pid" 2>/dev/null || true
    fi
done

# Kill vite/webpack dev servers
pkill -u "$USER" -f "vite" 2>/dev/null || true
pkill -u "$USER" -f "webpack.*serve" 2>/dev/null || true

sleep 2
CURRENT=$(ps -Lu "$USER" --no-headers 2>/dev/null | wc -l)
PCT=$(( CURRENT * 100 / LIMIT ))
echo "$LOG_PREFIX After action: threads=$CURRENT usage=${PCT}%"

if [ "$PCT" -lt "$CRITICAL_PCT" ]; then
    exit 0
fi

# ============================================================
# CRITICAL ZONE (70%+) — aggressive cleanup
# ============================================================
echo "$LOG_PREFIX CRITICAL: ${PCT}% — aggressive cleanup"

# Kill non-essential Claude sessions (not mayor, deacon, witness, refinery)
ps -u "$USER" --no-headers -o pid,etimes,args 2>/dev/null | grep 'claude' | \
    grep -v 'mayor\|deacon\|witness\|refinery' | \
    sort -k2 -rn | head -5 | while read pid etime cmd; do
    threads=$(ls /proc/$pid/task 2>/dev/null | wc -l)
    echo "$LOG_PREFIX Killing Claude session PID $pid ($threads threads, age=${etime}s)"
    kill "$pid" 2>/dev/null || true
done

sleep 2
CURRENT=$(ps -Lu "$USER" --no-headers 2>/dev/null | wc -l)
PCT=$(( CURRENT * 100 / LIMIT ))
echo "$LOG_PREFIX Final: threads=$CURRENT usage=${PCT}%"

if [ "$PCT" -ge "$CRITICAL_PCT" ]; then
    echo "$LOG_PREFIX STILL CRITICAL — manual intervention needed"
    /home/pratham2/go/bin/gt mail send mayor/ \
        -s "[CRITICAL] Thread limit ${PCT}% — manual action needed" \
        -m "Guardrail killed node+metrics+claude but still at ${CURRENT}/${LIMIT}. Top consumers: $(ps -u $USER --no-headers -o nlwp,comm | sort -rn | head -5 | tr '\n' '; ')" \
        2>/dev/null || true
fi
