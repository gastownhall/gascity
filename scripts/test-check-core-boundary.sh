#!/usr/bin/env bash
# test-check-core-boundary.sh — regression tests for
# scripts/check-core-boundary.sh, focused on the false-positive bug
# (gascity#4479): the (b) `org_` scan used to walk the whole working tree
# (grep -r --exclude-dir=vendor --exclude-dir=testdata), so any UNTRACKED
# in-tree Go build cache (GitLab CI's canonical $CI_PROJECT_DIR/.cache/go-mod
# layout, or any other in-tree cache) tripped false violations on
# third-party module sources. Scanning `git ls-files` instead means an
# untracked directory is invisible to the scan regardless of its name.
#
# Each test builds an isolated temp git repo, copies the real script in, and
# runs it there — real git, real grep, no fakes needed (this is a pure
# text-scanning guard, not a network/bd-dependent one like the
# push-ownership-guard tests it borrows its harness shape from).

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$TEST_DIR/check-core-boundary.sh"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

export GIT_AUTHOR_NAME="Test Author" GIT_AUTHOR_EMAIL="author@example.com"
export GIT_COMMITTER_NAME="Test Author" GIT_COMMITTER_EMAIL="author@example.com"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

# new_repo: an isolated git repo in a fresh tmpdir with a minimal go.mod
# (check (d) requires one to be present), prints its path.
new_repo() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-ccb-test.XXXXXX")"
    git -C "$d" init -q -b main
    git -C "$d" config commit.gpgsign false
    printf 'module example.com/testmod\n\ngo 1.22\n' > "$d/go.mod"
    git -C "$d" add go.mod
    git -C "$d" commit -qm base
    printf '%s' "$d"
}

# run_check <repo>: runs the real script inside <repo>, capturing exit code
# and combined output.
run_check() {
    local repo="$1" out ec
    out=$(cd "$repo" && bash "$SCRIPT" 2>&1)
    ec=$?
    printf '%s\x1e%s' "$ec" "$out"
}

# ---------------------------------------------------------------------------
# Test 1: an UNTRACKED in-tree cache directory carrying a third-party org_
# hit must NOT trip check (b). This is the exact bug: a project-local
# GOMODCACHE (or any other in-tree, gitignored cache) sits inside the
# working tree but is never `git add`ed.
# ---------------------------------------------------------------------------
test_untracked_cache_dir_ignored() {
    local repo result ec out
    repo="$(new_repo)"
    mkdir -p "$repo/.cache/go-mod/github.com/google/go-github@v1.2.3"
    cat > "$repo/.cache/go-mod/github.com/google/go-github@v1.2.3/orgs.go" <<'EOF'
package github

// ListOrgs lists org_id-scoped organizations for the authenticated user.
func ListOrgs() {}
EOF
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -eq 0 ]; then
        record_pass "untracked in-tree cache dir with org_ hit is ignored"
    else
        record_fail "untracked in-tree cache dir with org_ hit is ignored" "exit=$ec, expected 0
$out"
    fi
    rm -rf "$repo"
}

# ---------------------------------------------------------------------------
# Test 2: a genuine org_ token in a TRACKED core .go file must still trip
# check (b) — the fix must not blind the guard to real violations.
# ---------------------------------------------------------------------------
test_tracked_org_token_still_blocked() {
    local repo result ec out
    repo="$(new_repo)"
    cat > "$repo/tenant.go" <<'EOF'
package core

// resolveOrgID resolves the commercial org_id tenant key.
func resolveOrgID() string { return "" }
EOF
    git -C "$repo" add tenant.go
    git -C "$repo" commit -qm "add tracked violation"
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -ne 0 ] && printf '%s' "$out" | grep -q 'BLOCKED (b)'; then
        record_pass "tracked org_ token in core .go still blocks (b)"
    else
        record_fail "tracked org_ token in core .go still blocks (b)" "exit=$ec, expected nonzero with BLOCKED (b)
$out"
    fi
    rm -rf "$repo"
}

# ---------------------------------------------------------------------------
# Test 3: vendor/ and testdata/ stay excluded even though they are TRACKED
# (unlike the cache-dir case) — the switch to git ls-files must preserve the
# pre-existing directory exclusions, not just fix the untracked-dir bug.
# ---------------------------------------------------------------------------
test_tracked_vendor_and_testdata_still_excluded() {
    local repo result ec out
    repo="$(new_repo)"
    mkdir -p "$repo/vendor/github.com/example/pkg" "$repo/internal/foo/testdata"
    cat > "$repo/vendor/github.com/example/pkg/orgs.go" <<'EOF'
package pkg

func F() { _ = "org_id" }
EOF
    cat > "$repo/internal/foo/testdata/golden.go" <<'EOF'
package testdata

func F() { _ = "org_id" }
EOF
    git -C "$repo" add vendor internal
    git -C "$repo" commit -qm "add tracked vendor/testdata"
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -eq 0 ]; then
        record_pass "tracked vendor/ and testdata/ stay excluded"
    else
        record_fail "tracked vendor/ and testdata/ stay excluded" "exit=$ec, expected 0
$out"
    fi
    rm -rf "$repo"
}

# ---------------------------------------------------------------------------
# Test 4: a `// boundary:allow org_` annotated line stays suppressed on a
# tracked file (unrelated to the cache-dir bug, but a regression guard that
# the switch to git ls-files didn't disturb the existing suppression path).
# ---------------------------------------------------------------------------
test_boundary_allow_annotation_still_suppresses() {
    local repo result ec out
    repo="$(new_repo)"
    cat > "$repo/otel.go" <<'EOF'
package core

const orgAttrKey = "org_id" // boundary:allow org_
EOF
    git -C "$repo" add otel.go
    git -C "$repo" commit -qm "add annotated line"
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -eq 0 ]; then
        record_pass "boundary:allow org_ annotation still suppresses"
    else
        record_fail "boundary:allow org_ annotation still suppresses" "exit=$ec, expected 0
$out"
    fi
    rm -rf "$repo"
}

# ---------------------------------------------------------------------------
# Test 5: outside a git work tree the tracked surface cannot be enumerated at
# all, so the scan must fail CLOSED — the header contract is explicit that a
# check which cannot evaluate is a violation, not a pass. Without this the
# scan silently reports OK on a directory carrying a real violation.
# ---------------------------------------------------------------------------
test_non_git_dir_fails_closed() {
    local dir result ec out
    dir="$(mktemp -d "${TMPDIR:-/tmp}/gc-ccb-test.XXXXXX")"
    printf 'module example.com/testmod\n\ngo 1.22\n' > "$dir/go.mod"
    cat > "$dir/tenant.go" <<'EOF'
package core

// resolveOrgID resolves the commercial org_id tenant key.
func resolveOrgID() string { return "" }
EOF
    result="$(run_check "$dir")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -ne 0 ] && printf '%s' "$out" | grep -q 'BLOCKED'; then
        record_pass "non-git directory fails closed"
    else
        record_fail "non-git directory fails closed" "exit=$ec, expected nonzero with BLOCKED
$out"
    fi
    rm -rf "$dir"
}

# ---------------------------------------------------------------------------
# Test 6: a tracked path containing whitespace must still be scanned. The file
# list is handed to grep NUL-delimited precisely so that a directory named
# `dir space` cannot split into two nonexistent paths and silently drop a real
# violation.
# ---------------------------------------------------------------------------
test_tracked_path_with_space_still_blocked() {
    local repo result ec out
    repo="$(new_repo)"
    mkdir -p "$repo/dir space"
    cat > "$repo/dir space/tenant.go" <<'EOF'
package core

// resolveOrgID resolves the commercial org_id tenant key.
func resolveOrgID() string { return "" }
EOF
    git -C "$repo" add "dir space/tenant.go"
    git -C "$repo" commit -qm "add tracked violation under a path with a space"
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -ne 0 ] && printf '%s' "$out" | grep -q 'BLOCKED (b)'; then
        record_pass "tracked path containing whitespace still blocks (b)"
    else
        record_fail "tracked path containing whitespace still blocks (b)" "exit=$ec, expected nonzero with BLOCKED (b)
$out"
    fi
    rm -rf "$repo"
}

# ---------------------------------------------------------------------------
# Test 7: a repo whose ONLY tracked .go file is a _test.go must not block —
# test files are outside the core surface. This is the case a filename-less
# grep gets wrong: with a single-file batch and no -H, grep omits the path and
# no downstream `_test\.go:` filter can recognize the hit, producing a FALSE
# BLOCKED on a required check.
# ---------------------------------------------------------------------------
test_lone_tracked_test_file_not_blocked() {
    local repo result ec out
    repo="$(new_repo)"
    cat > "$repo/tenant_test.go" <<'EOF'
package core

import "testing"

// TestOrgID references the commercial org_id key from a test file.
func TestOrgID(t *testing.T) { _ = "org_id" }
EOF
    git -C "$repo" add tenant_test.go
    git -C "$repo" commit -qm "add tracked test-only file"
    result="$(run_check "$repo")"
    ec="${result%%$'\x1e'*}"; out="${result#*$'\x1e'}"
    if [ "$ec" -eq 0 ]; then
        record_pass "lone tracked _test.go does not false-block"
    else
        record_fail "lone tracked _test.go does not false-block" "exit=$ec, expected 0
$out"
    fi
    rm -rf "$repo"
}

test_untracked_cache_dir_ignored
test_tracked_org_token_still_blocked
test_tracked_vendor_and_testdata_still_excluded
test_boundary_allow_annotation_still_suppresses
test_non_git_dir_fails_closed
test_tracked_path_with_space_still_blocked
test_lone_tracked_test_file_not_blocked

echo "----"
echo "test-check-core-boundary.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
