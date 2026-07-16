#!/bin/sh
# Unit test for the CONN_MAX derivation in mol-dog-doctor.sh.
#
# Verifies that CONN_MAX is read from @@GLOBAL.max_connections at runtime
# rather than defaulting to the legacy hardcoded 50, which produced false
# "near capacity" advisories when the server's real cap was 256.
#
# Run: sh test/dolt/conn_max_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
DOCTOR_SCRIPT="${DOCTOR_SCRIPT:-$HERE/../../examples/bd/dolt/assets/scripts/mol-dog-doctor.sh}"

if [ ! -f "$DOCTOR_SCRIPT" ]; then
  echo "FAIL: doctor script not found at $DOCTOR_SCRIPT"
  exit 1
fi

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

# Extract the CONN_MAX derivation block from the real script so the test
# exercises the shipped logic (with dolt_sql mocked) rather than a copy.
CONN_MAX_BLOCK=$(sed -n '/^# CONN_MAX:/,/^fi$/p' "$DOCTOR_SCRIPT")
case "$CONN_MAX_BLOCK" in
  *'@@GLOBAL.max_connections'*) : ;;
  *)
    echo "FAIL: could not extract CONN_MAX derivation block from $DOCTOR_SCRIPT"
    exit 1
    ;;
esac

eval_conn_max() {
  # $1 = mock return value for @@GLOBAL.max_connections ("" means query fails)
  # $2 = optional GC_DOCTOR_CONN_MAX override
  mock_server_max="$1"
  override="${2:-}"

  (
    dolt_sql() {
      if [ -n "$mock_server_max" ]; then
        printf '%s\n' "@@GLOBAL.max_connections"
        printf '%s\n' "$mock_server_max"
      else
        return 1
      fi
    }
    if [ -n "$override" ]; then
      GC_DOCTOR_CONN_MAX="$override"
    else
      unset GC_DOCTOR_CONN_MAX 2>/dev/null || true
    fi
    eval "$CONN_MAX_BLOCK"
    printf '%s\n' "$CONN_MAX"
  )
}

# Server reports 256 -> CONN_MAX must be 256, not the legacy 50.
result=$(eval_conn_max "256")
if [ "$result" = "256" ]; then
  pass "server returns 256 -> CONN_MAX=256"
else
  bad "server returns 256 -> expected CONN_MAX=256, got $result"
fi

# Server reports 512 -> CONN_MAX must reflect the server value.
result=$(eval_conn_max "512")
if [ "$result" = "512" ]; then
  pass "server returns 512 -> CONN_MAX=512"
else
  bad "server returns 512 -> expected CONN_MAX=512, got $result"
fi

# Server query fails -> fall back to 256.
result=$(eval_conn_max "")
if [ "$result" = "256" ]; then
  pass "server query fails -> CONN_MAX=256 (fallback)"
else
  bad "server query fails -> expected CONN_MAX=256 (fallback), got $result"
fi

# Explicit GC_DOCTOR_CONN_MAX override takes precedence over server value.
result=$(eval_conn_max "256" "100")
if [ "$result" = "100" ]; then
  pass "GC_DOCTOR_CONN_MAX=100 overrides server 256 -> CONN_MAX=100"
else
  bad "GC_DOCTOR_CONN_MAX=100 override -> expected CONN_MAX=100, got $result"
fi

# Legacy 50 is NOT the default anymore.
result=$(eval_conn_max "256")
if [ "$result" != "50" ]; then
  pass "CONN_MAX is not the legacy default 50 when server reports 256"
else
  bad "CONN_MAX must not default to 50; got $result"
fi

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
