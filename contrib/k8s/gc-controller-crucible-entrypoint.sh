#!/usr/bin/env bash
# Entrypoint for the Model-B controller launched as a Crucible gVisor sandbox by the autonomous
# city-provisioner (B6). Unlike the K8s copy-in controller (Dockerfile.controller), this is ENV-DRIVEN
# and wires the city to its HOSTED beads ledger (never embedded dolt):
#
#   GC_CITY_NAME    the city / workspace name (slug)
#   GC_PACK         the starting pack: gascity | gastown | minimal | custom  (gc init --template)
#   GC_DOLT_HOST    the hosted beads dolt gateway host:port (one gateway serves all projects)
#   GC_DOLT_DATABASE the city's project database on that gateway (bd_prj_<id>)
#   GC_WORKSPACE_ID  optional pre-created workspace id
#
# CREDENTIAL BOOTSTRAP (the one piece that needs the live stack — see B6 spec): bd authenticates to
# the hosted gateway with a short-lived beads EIA as the dolt username (EIA-as-username). The
# controller mints it from its orchestrator SP key (delivered to the sandbox out of band, OpenBao/
# runtime) via STS. Because the EIA is ~90s, bd must RE-MINT on connect — wired here as a dolt
# credential command (the eia-helper). Until the eia-helper is installed + the orchestrator key is
# mounted, set GC_DOLT_CRED_CMD to that helper; this script fails closed if neither a credential
# command nor a static GC_DOLT_USER is present.
set -euo pipefail

CITY_DIR="${CITY_DIR:-/city}"

require() { # name
  if [ -z "${!1:-}" ]; then
    echo "city-controller: missing required env $1" >&2
    exit 2
  fi
}
require GC_CITY_NAME
require GC_DOLT_HOST
require GC_DOLT_DATABASE
PACK="${GC_PACK:-gascity}"

# 1. Initialize the city non-interactively from the chosen pack (idempotent: a persistent volume
#    survives restarts, so skip if already initialized).
if [ ! -d "${CITY_DIR}/.gc" ]; then
  gc init --template "${PACK}" --name "${GC_CITY_NAME}" "${CITY_DIR}"
fi

# 2. Point beads at the HOSTED dolt gateway. The canonical source gc resolves the connection from is
#    .beads/metadata.json (backend=dolt, dolt_mode=server, dolt_database); host/port come from the
#    GC_DOLT_* env (dolt_host in metadata is deprecated). Written after init so it overrides any
#    embedded-dolt default the pack would otherwise use.
mkdir -p "${CITY_DIR}/.beads"
cat > "${CITY_DIR}/.beads/metadata.json" <<JSON
{
  "database": "${GC_CITY_NAME}",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "${GC_DOLT_DATABASE}"
}
JSON

# Split GC_DOLT_HOST (host:port) into the host + port env gc projects onto bd.
RAW_HOST="${GC_DOLT_HOST}"
export GC_DOLT_HOST="${RAW_HOST%%:*}"
case "${RAW_HOST}" in
  *:*) export GC_DOLT_PORT="${RAW_HOST##*:}" ;;
  *)   export GC_DOLT_PORT="${GC_DOLT_PORT:-3306}" ;;
esac
export GC_BEADS_BACKEND="dolt"
export BEADS_BACKEND="dolt"

# 3. Credential: a re-minting dolt credential command (EIA-as-username) OR a static user. Fail closed.
if [ -n "${GC_DOLT_CRED_CMD:-}" ]; then
  export BEADS_DOLT_CREDENTIAL_COMMAND="${GC_DOLT_CRED_CMD}"
elif [ -n "${GC_DOLT_USER:-}" ]; then
  : # static credential already in env (dev / short-lived test)
else
  echo "city-controller: no beads credential — set GC_DOLT_CRED_CMD (eia-helper) or GC_DOLT_USER" >&2
  exit 3
fi

# 4. Run the controller in the foreground (PID 1 of the sandbox).
exec gc start --foreground "${CITY_DIR}"
