#!/usr/bin/env bash
# rbe-hot-precheck.sh: the B3 pre-check of the hot tier's edge (README "Hot
# blobs on R2"), run from a Blacksmith job (the WAF admits Blacksmith's range
# alone) once B0 and B1 are live and before RBE_FORK_HOT_URL is set. Every
# go/no-go GET reads the way rbe-hotread does: the object URL plus a query
# string of its own, "?r=" and 16 hex digits (8 random bytes), fresh per GET.
#   1. a key that cannot exist, GET twice: 404 both times, the second not an
#      edge HIT;
#   2. DIGEST's key, GET before it is promoted: 404 (an object already there
#      aborts: pick another DIGEST), then promote it (the fork worker
#      certificate), then GET it again at once: 200, not a 404;
#   3. those bytes decode to DIGEST, and zstd accepts the whole stream (R2
#      ignores the query string).
# Information only, never a FAIL (no reader in the design uses the plain
# URL; there is no cache rule): the same keys without a query string, so a
# missing key's second plain GET and a plain GET after the promotion show
# whether Cloudflare keeps h/ 404s at the edge.
# Usage (env):
#   DIGEST=<sha256>-<size>   a blob the fork tier holds (fork-big or cas-big,
#                            64 MiB or more; e.g. from rbe-cas-big-census)
#   CERT, KEY                the fork pool's worker certificate and key (PEM files)
#   HOT_URL                  default https://rbe-warm.ops.gascity.com/h/fork
#   PROMOTE_URL              default https://rbe-fork-zread.ops.gascity.com:8444/rbe.hot.v1.Hot/Promote
# Prints one line per check and "precheck: ok" or "precheck: FAIL (n)"; exit 1 on a failure.
# The promotion is real: one PUT of about 0.23 x the blob's size of egress.
set -uo pipefail
HOT_URL=${HOT_URL:-https://rbe-warm.ops.gascity.com/h/fork}
PROMOTE_URL=${PROMOTE_URL:-https://rbe-fork-zread.ops.gascity.com:8444/rbe.hot.v1.Hot/Promote}
: "${DIGEST:?DIGEST=<sha256>-<size>}" "${CERT:?}" "${KEY:?}"
[[ $DIGEST =~ ^([0-9a-f]{64})-([0-9]+)$ ]] || { echo "DIGEST must be <sha256>-<size>" >&2; exit 2; }
hash=${BASH_REMATCH[1]} size=${BASH_REMATCH[2]}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
check() { # check <ok?> <line>
	if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; fails=$((fails + 1)); fi
}
get() { # get <url> <out>: prints "<status> <cf-cache-status>"
	local code
	code=$(curl -4 -sS -o "$2" -D "$tmp/h" -w '%{http_code}' "$1" 2>/dev/null) || code=000
	echo "$code $(tr -d '\r' <"$tmp/h" 2>/dev/null | awk -F': ' 'tolower($1) == "cf-cache-status" { print $2 }' | tail -1)"
}
rand() { head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
q() { echo "?r=$(rand)"; } # rbe-hotread's query string: fresh per GET
info() { echo "info  $1"; }

# 1. A missing key, twice (and twice plain, information only).
miss="$HOT_URL/$(rand)$(rand)$(rand)$(rand)-1.zst"
read -r c1 _ <<<"$(get "$miss$(q)" /dev/null)"
read -r c2 s2 <<<"$(get "$miss$(q)" /dev/null)"
[ "$c1" = 404 ] && [ "$c2" = 404 ]; check $? "missing key, ?r=: $c1 $c2 (want 404 404)"
[ "${s2:-}" != HIT ]; check $? "missing key, ?r=, second GET cf-cache-status=${s2:--} (want not HIT)"
read -r p1 _ <<<"$(get "$miss" /dev/null)"
read -r p2 ps2 <<<"$(get "$miss" /dev/null)"
if [ "$p2" = 404 ] && [ "${ps2:-}" = HIT ]; then edge404=yes; else edge404=no; fi
info "missing key, plain URL: $p1 $p2, second cf-cache-status=${ps2:--}: edge-cached 404: $edge404"

# 2. DIGEST's key: before (and twice plain, to seed any edge copy), promote,
# after.
key="$HOT_URL/$hash-$size.zst"
read -r b1 bs1 <<<"$(get "$key$(q)" /dev/null)"
# The check needs a 404 to seed a cache with: an object already there
# proves nothing about a cached 404.
if [ "$b1" != 404 ]; then
	echo "FAIL  before promotion, ?r=: $b1 ${bs1:--} (want 404: already promoted? pick another DIGEST)"
	echo "precheck: FAIL (aborted)"
	exit 1
fi
echo "ok    before promotion, ?r=: 404 ${bs1:--}"
read -r bp1 bps1 <<<"$(get "$key" /dev/null)"
read -r bp2 bps2 <<<"$(get "$key" /dev/null)"
info "before promotion, plain URL: $bp1 ${bps1:--}, $bp2 ${bps2:--}"
ans=$(curl -4 -sS --http2 --cert "$CERT" --key "$KEY" -X POST -H 'Content-Type: application/json' -m 120 \
	--data "{\"hash\":\"$hash\",\"size\":$size,\"wait_ms\":60000,\"seen_404\":true}" -w ' %{http_code}' "$PROMOTE_URL" 2>&1)
case "$ans" in *'"key": "h/'*' 200') r=0 ;; *) r=1 ;; esac
check $r "promote: $ans"
read -r a1 as1 <<<"$(get "$key$(q)" "$tmp/q.zst")"
[ "$a1" = 200 ]; check $? "after promotion, ?r= at once: $a1 cf-cache-status=${as1:--} (want 200)"
read -r ap1 aps1 <<<"$(get "$key" /dev/null)"
if [ "$ap1" = 404 ]; then stale=yes; else stale=no; fi
info "after promotion, plain URL at once: $ap1 cf-cache-status=${aps1:--}: stale 404: $stale"

# 3. The bytes rbe-hotread would read decode to DIGEST.
if [ "$a1" = 200 ]; then
	got=$(zstd -q -dc "$tmp/q.zst" | tee >(wc -c >"$tmp/n") | sha256sum | cut -d' ' -f1)
	dec=$? # pipefail: zstd's own failure, even after it wrote every byte (a valid frame, then garbage)
	sleep 0.2
	[ "$dec" = 0 ] && [ "$got" = "$hash" ] && [ "$(tr -d ' ' <"$tmp/n")" = "$size" ]; check $? "after promotion, ?r=: the object decodes to $hash-$size (zstd exit $dec)"
fi
info "plain URL (no reader uses it; no cache rule): edge-cached 404: $edge404; stale 404 after promotion: $stale"
if [ "$fails" = 0 ]; then echo "precheck: ok"; else echo "precheck: FAIL ($fails)"; exit 1; fi
