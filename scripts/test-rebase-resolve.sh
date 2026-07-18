#!/usr/bin/env bash
#
# test-rebase-resolve.sh — unit tests for the deployer's bounded self-rebase
# feature (bead ga-gcy0cd; architecture ga-h7hnpt FR-5/FR-6). Three layers:
#
#   1. Conflict-resolution logic (scripts/rebase-resolve-lib.sh) exercised
#      against real temp git repos: identical → take one, disjoint/one-side
#      addition → keep, both-add-tests → keep both, real code conflict →
#      refuse. These cases are PORTED from
#      packs/maintainer-pr-review/tests/test-rebase-resolve.sh because the
#      classifier functions under test
#      (is_additive_keepboth_path/resolve_conflict_markers_in_file/
#      attempt_trivial_conflict_resolution) are themselves a byte-identical
#      ported copy — see scripts/rebase-resolve-lib.sh's header.
#   2. attempt_bounded_self_rebase (new driver, not present in the mpr copy):
#      clean fast-forward → succeeds; trivial conflict shape → resolves and
#      succeeds; non-trivial conflict → refuses, branch left untouched;
#      guard rails (protected branch, wrong branch checked out, dirty tree,
#      already-ancestor no-op); a stale remote lease is rejected (proving
#      --force-with-lease semantics are actually active, not a bare
#      --force).
#   3. Static guards: the new file's push must use --force-with-lease and
#      must never use a bare --force.
#
# No network, no gh, no models. Pure git + the lib.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/rebase-resolve-lib.sh"

# shellcheck source=../scripts/rebase-resolve-lib.sh disable=SC1091
. "$LIB"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

# Deterministic, hermetic git identity for the temp repos.
export GIT_AUTHOR_NAME="Test Author" GIT_AUTHOR_EMAIL="author@example.com"
export GIT_COMMITTER_NAME="Test Deployer" GIT_COMMITTER_EMAIL="deployer@example.com"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

# new_repo: create an isolated git repo in a fresh tmpdir, print its path.
new_repo() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-test.XXXXXX")"
    git -C "$d" init -q -b main
    git -C "$d" config commit.gpgsign false
    printf '%s' "$d"
}

# make_conflict_repo <file> <base_content> <ours_content> <theirs_content>
#
# Builds main with <base_content> in <file>, branches `feature` from it, makes
# `feature` set the file to <ours_content>, advances main to <theirs_content>,
# then checks out feature and rebases it onto main — leaving the working tree
# mid-rebase with a conflict in <file>. Echoes the repo path.
make_conflict_repo() {
    local file="$1" base="$2" ours="$3" theirs="$4"
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        mkdir -p "$(dirname "$file")"
        printf '%s' "$base" > "$file"
        git add -A && git commit -qm "base"
        git checkout -q -b feature
        printf '%s' "$ours" > "$file"
        git add -A && git commit -qm "feature change"
        git checkout -q main
        printf '%s' "$theirs" > "$file"
        git add -A && git commit -qm "main change"
        git checkout -q feature
        git rebase main >/dev/null 2>&1 || true
    )
    printf '%s' "$d"
}

# in_conflict <repo>: 0 if the repo currently has unmerged paths.
in_conflict() {
    [[ -n "$(git -C "$1" ls-files --unmerged 2>/dev/null)" ]]
}

# has_markers <repo>: 0 if any tracked file still has conflict markers.
has_markers() {
    git -C "$1" -c core.pager=cat grep -lE '^(<<<<<<< |=======$|>>>>>>> )' -- . 2>/dev/null | grep -q .
}

# write_conflict_file <path> <ours_block> <theirs_block>
write_conflict_file() {
    local path="$1" ours="$2" theirs="$3"
    {
        echo "unchanged top"
        echo "<<<<<<< HEAD"
        [[ -n "$ours" ]] && printf '%s\n' "$ours"
        echo "======="
        [[ -n "$theirs" ]] && printf '%s\n' "$theirs"
        echo ">>>>>>> branch"
        echo "unchanged bottom"
    } > "$path"
}

# ---------------------------------------------------------------------------
# is_additive_keepboth_path classification (ported — see file header)
# ---------------------------------------------------------------------------

test_additive_path_classification() {
    local p
    for p in \
        "packs/foo/tests/test-bar.sh" \
        "src/widget_test.go" \
        "app/components/Button.test.tsx" \
        "spec/models/user_spec.rb" \
        "docs/usage.md" \
        "README.md" \
        "CHANGELOG.md" \
        "testdata/golden/out.txt" \
        "internal/fixtures/sample.json" \
        "x/__snapshots__/View.snap" \
        "pkg/foo_test.py" \
        "test_helpers.py" \
        "e2e/login.spec.ts"
    do
        if ! is_additive_keepboth_path "$p"; then
            record_fail "additive-path/$p" "expected additive, got non-additive"
            return
        fi
    done
    for p in \
        "src/main.go" \
        "internal/server/handler.py" \
        "app/components/Button.tsx" \
        "lib/parser.rb" \
        "cmd/gc/main.go" \
        "Makefile" \
        "scripts/deploy.sh"
    do
        if is_additive_keepboth_path "$p"; then
            record_fail "additive-path/$p" "expected NON-additive (source), got additive"
            return
        fi
    done
    record_pass "additive-path classification (tests/docs/fixtures yes; source no)"
}

# ---------------------------------------------------------------------------
# resolve_conflict_markers_in_file — direct unit cases (no git needed)
# ---------------------------------------------------------------------------

test_resolve_identical_takes_one() {
    local f; f="$(mktemp)"
    write_conflict_file "$f" "same line" "same line"
    if resolve_conflict_markers_in_file "$f" 0; then
        if [[ "$(grep -c '^same line$' "$f")" == "1" ]] && ! grep -q '^<<<<<<< ' "$f"; then
            record_pass "resolve/identical-takes-one"
        else
            record_fail "resolve/identical-takes-one" "content: $(tr '\n' '|' < "$f")"
        fi
    else
        record_fail "resolve/identical-takes-one" "returned non-zero (rc=$?)"
    fi
    rm -f "$f"
}

test_resolve_one_side_empty_takes_nonempty() {
    local f; f="$(mktemp)"
    write_conflict_file "$f" "" "added by theirs"
    if resolve_conflict_markers_in_file "$f" 0; then
        if grep -q '^added by theirs$' "$f" && ! grep -q '^<<<<<<< ' "$f"; then
            record_pass "resolve/one-side-empty-takes-nonempty"
        else
            record_fail "resolve/one-side-empty-takes-nonempty" "content: $(tr '\n' '|' < "$f")"
        fi
    else
        record_fail "resolve/one-side-empty-takes-nonempty" "returned non-zero (rc=$?)"
    fi
    rm -f "$f"
}

test_resolve_both_add_refused_when_not_allowed() {
    local f; f="$(mktemp)"
    write_conflict_file "$f" "ours_code()" "theirs_code()"
    if resolve_conflict_markers_in_file "$f" 0; then
        record_fail "resolve/both-add-refused-source" "should have refused, but resolved"
    else
        if grep -q '^<<<<<<< ' "$f"; then
            record_pass "resolve/both-add-refused-source (markers left intact)"
        else
            record_fail "resolve/both-add-refused-source" "refused but mutated file"
        fi
    fi
    rm -f "$f"
}

test_resolve_both_add_kept_when_allowed() {
    local f; f="$(mktemp)"
    write_conflict_file "$f" "func TestA(t *testing.T) {}" "func TestB(t *testing.T) {}"
    if resolve_conflict_markers_in_file "$f" 1; then
        if grep -q 'TestA' "$f" && grep -q 'TestB' "$f" && ! grep -q '^<<<<<<< ' "$f"; then
            record_pass "resolve/both-add-kept-when-allowed (union keeps both)"
        else
            record_fail "resolve/both-add-kept-when-allowed" "content: $(tr '\n' '|' < "$f")"
        fi
    else
        record_fail "resolve/both-add-kept-when-allowed" "returned non-zero (rc=$?)"
    fi
    rm -f "$f"
}

test_resolve_malformed_markers_refused() {
    local f; f="$(mktemp)"
    {
        echo "<<<<<<< HEAD"
        echo "ours"
        echo "======="
        echo "theirs"
    } > "$f"
    if resolve_conflict_markers_in_file "$f" 1; then
        record_fail "resolve/malformed-refused" "should have refused malformed markers"
    else
        record_pass "resolve/malformed-refused"
    fi
    rm -f "$f"
}

# ---------------------------------------------------------------------------
# attempt_trivial_conflict_resolution — against real git rebases (ported)
# ---------------------------------------------------------------------------

test_git_disjoint_keepboth_source() {
    local d
    d="$(make_conflict_repo "src/app.go" \
        $'package app\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\n' \
        $'package app\n\n// added by feature\nfunc A() {}\nfunc B() {}\nfunc C() {}\n' \
        $'package app\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\n\n// added by main\n')"
    if in_conflict "$d"; then
        if ! ( cd "$d" && attempt_trivial_conflict_resolution ); then
            record_fail "git/disjoint-keepboth-source" "resolver refused a trivially-disjoint conflict (rc=$?)"
            rm -rf "$d"; return
        fi
    fi
    if ! has_markers "$d" \
       && grep -q 'added by feature' "$d/src/app.go" \
       && grep -q 'added by main' "$d/src/app.go"; then
        record_pass "git/disjoint-keepboth-source (both disjoint additions kept)"
    else
        record_fail "git/disjoint-keepboth-source" "markers or content wrong: $(tr '\n' '|' < "$d/src/app.go")"
    fi
    rm -rf "$d"
}

test_git_one_side_empty_resolves() {
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        printf 'package app\nfunc Keep() {}\n' > app.go
        git add -A && git commit -qm base
        git checkout -q -b feature
        printf 'package app\nfunc Keep() {}\n' > app.go
        git commit -q --allow-empty -am "feature no-op touch"
        git checkout -q main
        printf 'package app\nfunc Keep() {}\nfunc MainOnly() {}\n' > app.go
        git add -A && git commit -qm "main adds MainOnly"
        git checkout -q feature
        git rebase main >/dev/null 2>&1 || true
    )
    if in_conflict "$d"; then
        if ! ( cd "$d" && attempt_trivial_conflict_resolution ); then
            record_fail "git/one-side-empty-resolves" "resolver refused a one-side-empty conflict (rc=$?)"
            rm -rf "$d"; return
        fi
    fi
    if ! has_markers "$d" && grep -q 'MainOnly' "$d/app.go"; then
        record_pass "git/one-side-empty-resolves (non-empty side kept, no markers)"
    else
        record_fail "git/one-side-empty-resolves" "content: $(tr '\n' '|' < "$d/app.go")"
    fi
    rm -rf "$d"
}

test_git_both_add_tests_keepboth() {
    local d
    d="$(make_conflict_repo "pkg/widget_test.go" \
        $'package widget\n' \
        $'package widget\n\nfunc TestFeatureA(t *testing.T) { /* a */ }\n' \
        $'package widget\n\nfunc TestFeatureB(t *testing.T) { /* b */ }\n')"
    if ! in_conflict "$d"; then
        record_fail "git/both-add-tests-keepboth" "rebase did not produce a conflict to test"
        rm -rf "$d"; return
    fi
    if ( cd "$d" && attempt_trivial_conflict_resolution ); then
        if ! has_markers "$d" \
           && grep -q 'TestFeatureA' "$d/pkg/widget_test.go" \
           && grep -q 'TestFeatureB' "$d/pkg/widget_test.go"; then
            record_pass "git/both-add-tests-keepboth (test file → union keeps both)"
        else
            record_fail "git/both-add-tests-keepboth" "content: $(tr '\n' '|' < "$d/pkg/widget_test.go")"
        fi
    else
        record_fail "git/both-add-tests-keepboth" "resolver refused both-add on a test file (rc=$?)"
    fi
    rm -rf "$d"
}

test_git_identical_take_one() {
    local d
    d="$(make_conflict_repo "src/app.go" \
        $'package app\n\nconst X = 1\n' \
        $'package app\n\nconst X = 2\n' \
        $'package app\n\nconst X = 2\n')"
    if in_conflict "$d"; then
        if ( cd "$d" && attempt_trivial_conflict_resolution ) \
           && ! has_markers "$d" \
           && [[ "$(grep -c 'const X = 2' "$d/src/app.go")" == "1" ]]; then
            record_pass "git/identical-take-one (single copy, no markers)"
        else
            record_fail "git/identical-take-one" "content: $(tr '\n' '|' < "$d/src/app.go")"
        fi
    else
        record_pass "git/identical-take-one (git auto-merged identical change)"
    fi
    rm -rf "$d"
}

test_git_real_conflict_refused() {
    local d
    d="$(make_conflict_repo "src/app.go" \
        $'package app\n\nconst Timeout = 10\n' \
        $'package app\n\nconst Timeout = 30\n' \
        $'package app\n\nconst Timeout = 60\n')"
    if ! in_conflict "$d"; then
        record_fail "git/real-conflict-refused" "rebase did not produce a conflict to test"
        rm -rf "$d"; return
    fi
    if ( cd "$d" && attempt_trivial_conflict_resolution ); then
        record_fail "git/real-conflict-refused" "resolver wrongly resolved a real semantic conflict"
    else
        if has_markers "$d"; then
            record_pass "git/real-conflict-refused (refused; markers intact for abort)"
        else
            record_fail "git/real-conflict-refused" "refused but markers gone"
        fi
    fi
    rm -rf "$d"
}

test_git_delete_modify_refused() {
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        printf 'package app\nfunc Old() {}\n' > app.go
        git add -A && git commit -qm base
        git checkout -q -b feature
        printf 'package app\nfunc Old() { /* feature edit */ }\n' > app.go
        git add -A && git commit -qm "feature edits app.go"
        git checkout -q main
        git rm -q app.go && git commit -qm "main deletes app.go"
        git checkout -q feature
        git rebase main >/dev/null 2>&1 || true
    )
    if ! in_conflict "$d"; then
        record_fail "git/delete-modify-refused" "rebase did not produce a delete/modify conflict"
        rm -rf "$d"; return
    fi
    if ( cd "$d" && attempt_trivial_conflict_resolution ); then
        record_fail "git/delete-modify-refused" "resolver wrongly resolved a delete/modify conflict"
    else
        record_pass "git/delete-modify-refused (structural conflict routed)"
    fi
    rm -rf "$d"
}

# ---------------------------------------------------------------------------
# Static guards on THIS pack's copy of the lib (new file, new assertions).
# ---------------------------------------------------------------------------

test_push_never_bare_force() {
    local bad
    bad="$(grep -nE 'git push .*--force([^-]|$)' "$LIB" | grep -v -- '--force-with-lease' || true)"
    if [[ -n "$bad" ]]; then
        record_fail "push/no-bare-force" "bare --force push found: $bad"
    else
        record_pass "push/no-bare-force (no bare --force anywhere in the deployer copy)"
    fi
}

test_bounded_rebase_uses_force_with_lease() {
    # SC2016 intentional: literal-text search of rebase-resolve-lib.sh source.
    # shellcheck disable=SC2016
    if grep -qE 'git push --force-with-lease origin "\$branch"' "$LIB"; then
        record_pass "push/bounded-rebase-force-with-lease"
    else
        record_fail "push/bounded-rebase-force-with-lease" "attempt_bounded_self_rebase's push is not --force-with-lease"
    fi
}

# ---------------------------------------------------------------------------
# attempt_bounded_self_rebase — new deployer-specific driver.
#
# new_bare_remote: an isolated bare repo standing in for `origin`.
# new_clone_with_branches: clones <remote>, ensures both main and <branch>
# exist as local branches tracking origin, leaves <branch> checked out.
# ---------------------------------------------------------------------------

new_bare_remote() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-remote.XXXXXX")"
    git init -q --bare -b main "$d"
    printf '%s' "$d"
}

# remote_sha <remote> <ref>: current SHA of <ref> on the bare remote, or empty.
remote_sha() {
    git -C "$1" rev-parse --verify -q "$2" 2>/dev/null || true
}

test_bounded_rebase_protected_branch_refused() {
    local d; d="$(new_repo)"
    local rc
    ( cd "$d" && attempt_bounded_self_rebase main main >/dev/null 2>&1 ); rc=$?
    if [[ $rc -eq 10 ]]; then
        record_pass "bounded/protected-branch-refused (main refused, rc=10)"
    else
        record_fail "bounded/protected-branch-refused" "expected rc=10, got rc=$rc"
    fi
    rm -rf "$d"
}

test_bounded_rebase_wrong_branch_refused() {
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git checkout -q -b feature
    )
    local rc
    # cwd is checked out to `feature`, but we ask to rebase `other`.
    ( cd "$d" && attempt_bounded_self_rebase other main >/dev/null 2>&1 ); rc=$?
    if [[ $rc -eq 10 ]]; then
        record_pass "bounded/wrong-branch-refused (mismatched checkout, rc=10)"
    else
        record_fail "bounded/wrong-branch-refused" "expected rc=10, got rc=$rc"
    fi
    rm -rf "$d"
}

test_bounded_rebase_dirty_tree_refused() {
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git checkout -q -b feature
        echo "uncommitted" >> f.txt
    )
    local rc
    ( cd "$d" && attempt_bounded_self_rebase feature main >/dev/null 2>&1 ); rc=$?
    if [[ $rc -eq 10 ]]; then
        record_pass "bounded/dirty-tree-refused (rc=10)"
    else
        record_fail "bounded/dirty-tree-refused" "expected rc=10, got rc=$rc"
    fi
    rm -rf "$d"
}

test_bounded_rebase_noop_when_already_ancestor() {
    local remote work
    remote="$(new_bare_remote)"
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-work.XXXXXX")"
    (
        cd "$remote" || exit 1
        # populate main via a throwaway working clone (bare repos have no worktree).
        :
    )
    local seed; seed="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-seed.XXXXXX")"
    git clone -q "$remote" "$seed" 2>/dev/null
    (
        cd "$seed" || exit 1
        git config commit.gpgsign false
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git push -q origin main
    )
    git clone -q "$remote" "$work"
    (
        cd "$work" || exit 1
        git config commit.gpgsign false
        git checkout -q -b feature
        echo "feature-only" >> f.txt; git add -A; git commit -qm "feature ahead of main"
    )
    local output rc
    output="$(cd "$work" && attempt_bounded_self_rebase feature main 2>&1)"; rc=$?
    if [[ $rc -eq 20 ]]; then
        # No push should have happened: remote has no `feature` ref at all.
        if [[ -z "$(remote_sha "$remote" refs/heads/feature)" ]]; then
            record_pass "bounded/noop-already-ancestor (rc=20, no push attempted)"
        else
            record_fail "bounded/noop-already-ancestor" "rc=20 but remote gained a feature ref unexpectedly"
        fi
    else
        record_fail "bounded/noop-already-ancestor" "expected rc=20, got rc=$rc, output: $output"
    fi
    rm -rf "$remote" "$work" "$seed"
}

# Bead-required case 1: clean fast-forward (no conflict) -> rebase succeeds.
test_bounded_rebase_clean_fastforward_succeeds() {
    local remote work
    remote="$(new_bare_remote)"
    local seed; seed="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-seed.XXXXXX")"
    git clone -q "$remote" "$seed" 2>/dev/null
    (
        cd "$seed" || exit 1
        git config commit.gpgsign false
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git push -q origin main
        git checkout -q -b feature
        printf 'feature file\n' > feature.txt; git add -A; git commit -qm "feature adds feature.txt"
        git push -q origin feature
        git checkout -q main
        printf 'main file\n' > main-only.txt; git add -A; git commit -qm "main adds main-only.txt"
        git push -q origin main
    )
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-work.XXXXXX")"
    git clone -q "$remote" "$work"
    (
        cd "$work" || exit 1
        git config commit.gpgsign false
        git checkout -q feature
    )
    local before_local output rc
    before_local="$(git -C "$work" rev-parse HEAD)"
    output="$(cd "$work" && attempt_bounded_self_rebase feature main 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]]; then
        record_fail "bounded/clean-fastforward-succeeds" "expected rc=0, got rc=$rc, output: $output"
        rm -rf "$remote" "$work" "$seed"; return
    fi
    local before_sha after_sha
    before_sha="$(printf '%s\n' "$output" | sed -n 's/^BEFORE_SHA=//p')"
    after_sha="$(printf '%s\n' "$output" | sed -n 's/^AFTER_SHA=//p')"
    local remote_after local_after
    remote_after="$(remote_sha "$remote" refs/heads/feature)"
    local_after="$(git -C "$work" rev-parse HEAD)"
    if [[ "$before_sha" == "$before_local" \
          && -n "$after_sha" && "$after_sha" != "$before_sha" \
          && "$remote_after" == "$after_sha" \
          && "$local_after" == "$after_sha" \
          && -f "$work/main-only.txt" && -f "$work/feature.txt" ]]; then
        record_pass "bounded/clean-fastforward-succeeds (rc=0, pushed, both files present)"
    else
        record_fail "bounded/clean-fastforward-succeeds" \
            "before_sha=$before_sha before_local=$before_local after_sha=$after_sha remote_after=$remote_after local_after=$local_after"
    fi
    rm -rf "$remote" "$work" "$seed"
}

# Bead-required case 2: trivial conflict shape -> resolves and succeeds.
test_bounded_rebase_trivial_conflict_resolves_and_succeeds() {
    local remote work seed
    remote="$(new_bare_remote)"
    seed="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-seed.XXXXXX")"
    git clone -q "$remote" "$seed" 2>/dev/null
    (
        cd "$seed" || exit 1
        git config commit.gpgsign false
        mkdir -p pkg
        printf 'package widget\n' > pkg/widget_test.go
        git add -A && git commit -qm base
        git push -q origin main
        git checkout -q -b feature
        printf 'package widget\n\nfunc TestFeatureA(t *testing.T) { /* a */ }\n' > pkg/widget_test.go
        git add -A && git commit -qm "feature adds TestFeatureA"
        git push -q origin feature
        git checkout -q main
        printf 'package widget\n\nfunc TestFeatureB(t *testing.T) { /* b */ }\n' > pkg/widget_test.go
        git add -A && git commit -qm "main adds TestFeatureB"
        git push -q origin main
    )
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-work.XXXXXX")"
    git clone -q "$remote" "$work"
    ( cd "$work" && git config commit.gpgsign false && git checkout -q feature )
    local output rc
    output="$(cd "$work" && attempt_bounded_self_rebase feature main 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]]; then
        record_fail "bounded/trivial-conflict-resolves" "expected rc=0, got rc=$rc, output: $output"
        rm -rf "$remote" "$work" "$seed"; return
    fi
    local remote_after local_after
    remote_after="$(remote_sha "$remote" refs/heads/feature)"
    local_after="$(git -C "$work" rev-parse HEAD)"
    if [[ "$remote_after" == "$local_after" ]] \
       && ! has_markers "$work" \
       && grep -q 'TestFeatureA' "$work/pkg/widget_test.go" \
       && grep -q 'TestFeatureB' "$work/pkg/widget_test.go"; then
        record_pass "bounded/trivial-conflict-resolves (both-add union resolved, pushed)"
    else
        record_fail "bounded/trivial-conflict-resolves" \
            "remote_after=$remote_after local_after=$local_after content: $(tr '\n' '|' < "$work/pkg/widget_test.go")"
    fi
    rm -rf "$remote" "$work" "$seed"
}

# Bead-required case 3: non-trivial/real conflict -> classifier correctly
# refuses, falls back to route-to-builder untouched (no push, branch
# restored to its pre-call state).
test_bounded_rebase_real_conflict_refused_untouched() {
    local remote work seed
    remote="$(new_bare_remote)"
    seed="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-seed.XXXXXX")"
    git clone -q "$remote" "$seed" 2>/dev/null
    (
        cd "$seed" || exit 1
        git config commit.gpgsign false
        printf 'package app\nconst Timeout = 10\n' > app.go
        git add -A && git commit -qm base
        git push -q origin main
        git checkout -q -b feature
        printf 'package app\nconst Timeout = 30\n' > app.go
        git add -A && git commit -qm "feature: 30"
        git push -q origin feature
        git checkout -q main
        printf 'package app\nconst Timeout = 60\n' > app.go
        git add -A && git commit -qm "main: 60"
        git push -q origin main
    )
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-work.XXXXXX")"
    git clone -q "$remote" "$work"
    ( cd "$work" && git config commit.gpgsign false && git checkout -q feature )
    local before_local remote_before
    before_local="$(git -C "$work" rev-parse HEAD)"
    remote_before="$(remote_sha "$remote" refs/heads/feature)"

    local output rc
    output="$(cd "$work" && attempt_bounded_self_rebase feature main 2>&1)"; rc=$?

    local after_local remote_after
    after_local="$(git -C "$work" rev-parse HEAD)"
    remote_after="$(remote_sha "$remote" refs/heads/feature)"

    if [[ $rc -eq 12 \
          && "$after_local" == "$before_local" \
          && "$remote_after" == "$remote_before" \
          && -z "$(git -C "$work" status --porcelain 2>/dev/null)" ]] \
       && ! has_markers "$work"; then
        record_pass "bounded/real-conflict-refused-untouched (rc=12, branch and remote unchanged, no markers)"
    else
        record_fail "bounded/real-conflict-refused-untouched" \
            "rc=$rc before_local=$before_local after_local=$after_local remote_before=$remote_before remote_after=$remote_after output=$output"
    fi
    rm -rf "$remote" "$work" "$seed"
}

# Bead-required case 4 (dynamic half): confirm --force-with-lease (not
# --force) is what's actually invoked, by proving the lease's staleness
# protection is live — a concurrent push to the remote after our clone must
# cause our own push to be REJECTED (rc=13) and must NOT be clobbered. A bare
# --force would have silently destroyed the concurrent commit; this test
# would not catch that with a static grep alone, so it exercises the real
# git behavior end-to-end.
test_bounded_rebase_stale_lease_returns_13() {
    local remote work seed intruder
    remote="$(new_bare_remote)"
    seed="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-seed.XXXXXX")"
    git clone -q "$remote" "$seed" 2>/dev/null
    (
        cd "$seed" || exit 1
        git config commit.gpgsign false
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git push -q origin main
        git checkout -q -b feature
        printf 'feature file\n' > feature.txt; git add -A; git commit -qm "feature adds feature.txt"
        git push -q origin feature
        git checkout -q main
        printf 'main file\n' > main-only.txt; git add -A; git commit -qm "main adds main-only.txt"
        git push -q origin main
    )

    # Our worktree clones now — its refs/remotes/origin/feature snapshot is
    # taken here and will go stale the moment the intruder pushes below.
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-work.XXXXXX")"
    git clone -q "$remote" "$work"
    ( cd "$work" && git config commit.gpgsign false && git checkout -q feature )

    # A concurrent actor advances `feature` on the remote after our clone.
    intruder="$(mktemp -d "${TMPDIR:-/tmp}/gc-deployer-rebase-intruder.XXXXXX")"
    git clone -q "$remote" "$intruder"
    (
        cd "$intruder" || exit 1
        git config commit.gpgsign false
        git checkout -q feature
        printf 'intruder file\n' > intruder.txt; git add -A; git commit -qm "concurrent push to feature"
        git push -q origin feature
    )
    local remote_intruded; remote_intruded="$(remote_sha "$remote" refs/heads/feature)"

    local output rc
    output="$(cd "$work" && attempt_bounded_self_rebase feature main 2>&1)"; rc=$?

    local remote_final; remote_final="$(remote_sha "$remote" refs/heads/feature)"
    if [[ $rc -eq 13 && "$remote_final" == "$remote_intruded" ]]; then
        record_pass "bounded/stale-lease-returns-13 (rejected push, intruder commit preserved)"
    else
        record_fail "bounded/stale-lease-returns-13" \
            "rc=$rc remote_intruded=$remote_intruded remote_final=$remote_final output=$output"
    fi
    rm -rf "$remote" "$work" "$seed" "$intruder"
}

# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------

run_all() {
    test_additive_path_classification
    test_resolve_identical_takes_one
    test_resolve_one_side_empty_takes_nonempty
    test_resolve_both_add_refused_when_not_allowed
    test_resolve_both_add_kept_when_allowed
    test_resolve_malformed_markers_refused
    test_git_disjoint_keepboth_source
    test_git_one_side_empty_resolves
    test_git_both_add_tests_keepboth
    test_git_identical_take_one
    test_git_real_conflict_refused
    test_git_delete_modify_refused
    test_push_never_bare_force
    test_bounded_rebase_uses_force_with_lease
    test_bounded_rebase_protected_branch_refused
    test_bounded_rebase_wrong_branch_refused
    test_bounded_rebase_dirty_tree_refused
    test_bounded_rebase_noop_when_already_ancestor
    test_bounded_rebase_clean_fastforward_succeeds
    test_bounded_rebase_trivial_conflict_resolves_and_succeeds
    test_bounded_rebase_real_conflict_refused_untouched
    test_bounded_rebase_stale_lease_returns_13

    echo
    echo "pass=$pass fail=$fail"
    [[ $fail -eq 0 ]]
}

run_all
