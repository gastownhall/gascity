#!/usr/bin/env bash
# Isolate a throwaway dolt sql-server for the coordstore baseline soak.
# Per handoff (ga-babhr / ga-w08fz caveat / ga-4rvpy/ga-vigx7): NEVER :28232 —
# that pollutes the supervisor's main dolt with orphan dbs.
set -euo pipefail
TS=${TS:-$(date -u +%Y%m%dT%H%M%SZ)}
DATA=/var/tmp/dolt-soak-baseline-$TS
PORT=39990
DB=coordstore_baseline
UNIT=dolt-soak-baseline-$TS.service

mkdir -p "$DATA"
cd "$DATA"
/usr/local/bin/dolt init --name "soak" --email "soak@local" 2>&1 || true
/usr/local/bin/dolt sql -q "CREATE DATABASE IF NOT EXISTS $DB;" 2>&1 || true

# Start dolt sql-server in its own systemd --user unit so it's bounded and stoppable.
systemd-run --user --unit="$UNIT" \
  --working-directory="$DATA" \
  --property=MemoryMax=32G \
  --property=MemorySwapMax=8G \
  /usr/local/bin/dolt sql-server \
    -H 127.0.0.1 \
    -P "$PORT" \
    --loglevel=warning \
    >/dev/null 2>&1

# Poll for ready.
for i in $(seq 1 30); do
  if (echo > /dev/tcp/127.0.0.1/$PORT) 2>/dev/null; then
    echo "dolt sql-server ready on :$PORT after ${i}s"
    break
  fi
  sleep 1
done

echo "unit:     $UNIT"
echo "data:     $DATA"
echo "port:     $PORT"
echo "db:       $DB"
echo "dsn:      root:@tcp(127.0.0.1:$PORT)/$DB?parseTime=true"
