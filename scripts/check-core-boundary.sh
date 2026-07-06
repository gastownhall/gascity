#!/usr/bin/env bash
# check-core-boundary.sh
#
# Structural guard for the open-core boundary (infra docs:
# core-vs-commercial-boundary-v0.md, identity-tenancy-fabric-v0.md Decision 5).
#
# gascity is the MIT open-core. Multi-tenant / commercial concerns — org_id
# tenancy, the hosted control plane — live in a SEPARATE module
# (github.com/gastownhall/gascity-hosted), never in this one. The OSS module
# must build and run standalone with no commercial dependency. This check fails
# if the OSS module starts to leak commercial coupling:
#
#   (a) a .go file imports a known commercial module;
#   (b) the literal token `org_` appears in core .go — that is the COMMERCIAL
#       tenant key; the OSS core uses `tenant` / `TenantSlug`. A genuinely benign
#       hit (e.g. an OTel attribute key, a doc URL) is suppressed with a trailing
#       `// boundary:allow org_` marker on the line;
#   (c) `TenantSlug` (the OSS publication-routing label) appears on the same line
#       as an external-identity token (oidc/keycloak/jwt/eia/workos/org_id) — the
#       label must never be joined to a commercial identity (the exact collapse
#       identity-tenancy-fabric-v0 Decision 5 names as THE leak vector);
#   (d) go.mod lists a commercial module as a dependency (direct or // indirect);
#   (e) a non-empty OpenFeature EvaluationContext is constructed in core .go —
#       per-tenant flag targeting is a HOSTED concern; the core must always
#       evaluate flags with an empty context (feature-flag-system-v0 §5). This is
#       the check the `org_` grep (b) cannot see: a workspace-id / user-id / email
#       context threaded into core would sail past (b). INERT until the OpenFeature
#       dependency lands in core (Phase 1) — wired now so it bites the moment it does.
#
# FAILS CLOSED: if a check cannot evaluate (e.g. go.mod is unreadable), that is a
# violation, not a pass. A guard that silently passes when it cannot evaluate
# manufactures false confidence.
#
# REQUIRED / BLOCKING as of the Phase-0 promotion (feature-flag ADR Resolved
# decision 1; operator-authorized 2026-07-05). The baseline was verified clean on
# origin/main before the flip. It runs as a blocking step in preflight-static — a
# violation fails the check and blocks the merge.
set -uo pipefail # intentionally NOT -e: run every check and aggregate.

# Known commercial module paths that must never appear in the OSS module.
# Space-separated; add new commercial modules here as they are created.
COMMERCIAL_MODULES="github.com/gastownhall/gascity-hosted"

failed=0
note() { echo "check-core-boundary: $*" >&2; }

# Non-test core .go surface (whole module tree minus vendor/testdata/tests).
scan() {
	grep -rn --include='*.go' --exclude-dir=vendor --exclude-dir=testdata "$1" . 2>/dev/null \
		| grep -v '_test\.go:'
}

# (a) source-level import of a commercial module. This is a source-level HINT
# (it matches the quoted import path); check (d) (go.mod) is the authoritative
# backstop for an actual module dependency.
for cm in $COMMERCIAL_MODULES; do
	hits=$(scan "\"$cm")
	if [ -n "$hits" ]; then
		note "BLOCKED (a) — OSS module imports commercial module ${cm}:"
		printf '%s\n' "$hits" >&2
		failed=1
	fi
done

# (b) the commercial `org_` tenant key in core .go (allow with // boundary:allow org_).
# `\borg_` anchors to a token boundary so mid-identifier hits (e.g. morgue_org_tag)
# do not false-positive.
orgs=$(scan '\borg_' | grep -v 'boundary:allow org_')
if [ -n "$orgs" ]; then
	note "BLOCKED (b) — 'org_' token in the OSS core (the commercial tenant key)."
	note "  Use tenant/TenantSlug, or annotate a genuinely benign line with '// boundary:allow org_':"
	printf '%s\n' "$orgs" >&2
	failed=1
fi

# (c) TenantSlug joined to a commercial identity token on the same line
joins=$(scan 'TenantSlug' | grep -iE '\b(oidc|keycloak|jwt|eia|workos)\b|org_?id')
if [ -n "$joins" ]; then
	note "BLOCKED (c) — TenantSlug (publication-routing label) joined to a commercial identity:"
	printf '%s\n' "$joins" >&2
	failed=1
fi

# (d) go.mod must list no commercial module as a dependency. go.mod is the
# authoritative, network-free dependency manifest: a commercial package imported
# anywhere in this module appears here (direct, or pruned-in as // indirect).
# Deliberately NOT `go list -m all` — that mutates go.sum / needs the network.
# Fail CLOSED if go.mod is unreadable.
if [ ! -f go.mod ]; then
	note "BLOCKED (d) — go.mod not found; cannot verify the dependency set (fail-closed)"
	failed=1
else
	for cm in $COMMERCIAL_MODULES; do
		if grep -qE "(^|[[:space:]])${cm}([[:space:]]|/|\$)" go.mod; then
			note "BLOCKED (d) — commercial module ${cm} is a dependency in go.mod"
			failed=1
		fi
	done
fi

# (e) non-empty OpenFeature EvaluationContext in core .go. Core must evaluate with
# an EMPTY context (empty targeting key, no attributes); a non-empty context is the
# per-tenant-targeting leak that (b) cannot catch. Tripwire, not a proof:
#  - a NewEvaluationContext(...) call whose args are not the empty form
#    (NewEvaluationContext("", nil) / NewEvaluationContext("", map[string]interface{}{})),
#  - or an openfeature.EvaluationContext{...} struct literal with any field.
# Annotate a genuinely benign line with `// boundary:allow evalctx`. Matches nothing
# until OpenFeature is imported in core (Phase 1).
evalctx=$(scan 'NewEvaluationContext(' \
	| grep -vE 'NewEvaluationContext\([[:space:]]*""[[:space:]]*,[[:space:]]*(nil|map\[string\]interface\{\}\{\})[[:space:]]*\)' \
	| grep -v 'boundary:allow evalctx')
evalctx_lit=$(scan 'EvaluationContext{[^}]' | grep -v 'boundary:allow evalctx')
if [ -n "$evalctx$evalctx_lit" ]; then
	note "BLOCKED (e) — non-empty OpenFeature EvaluationContext in the OSS core."
	note "  Core must evaluate flags with an EMPTY context; per-tenant targeting is hosted-only."
	note "  Use NewEvaluationContext(\"\", nil), or annotate a benign line with '// boundary:allow evalctx':"
	printf '%s\n' "$evalctx" "$evalctx_lit" | grep -v '^$' >&2
	failed=1
fi

if [ "$failed" -ne 0 ]; then
	note "open-core boundary violations found (see above)."
	exit 1
fi
echo "check-core-boundary: OK (no commercial coupling in the OSS module)"
