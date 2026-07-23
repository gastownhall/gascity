#!/usr/bin/env bash
#
# test-push-ownership-guard.sh — unit tests for the pre-push bead
# ownership/staleness guard (bead ga-fip9ps.1; guards the race described in
# ga-fip9ps). Three layers:
#
#   1. assert_bead_still_claimed (scripts/push-ownership-guard.sh) exercised
#      directly against real temp git repos with a fake `bd` on PATH: allow
#      on a clean claim; block on closed/reassigned/rerouted/held; fail
#      closed when bd is unreachable or the read times out.
#   2. Bead-id resolution: branch name wins over the assignee fallback and
#      only warns (doesn't hard-fail) on disagreement; falls back to the
#      assignee lookup when the branch name doesn't match; allows when
#      neither resolves (nothing to check — required for Layer A to be
#      wired in unconditionally without blocking unrelated pushes). Also
#      pins a known limitation of that fallback — see the KNOWN LIMITATION
#      comment on _pog_resolve_bead_id in push-ownership-guard.sh.
#   3. Hook wiring: a real .githooks/pre-push, installed into a real temp
#      repo pushing to a real bare remote, actually rejects a stale-claim
#      push, actually allows a clean one, and `--no-verify` actually
#      bypasses the guard end to end.
#
# No network, no gh, no models — a fake `bd` stands in for the real one.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/push-ownership-guard.sh"
REPO_ROOT="$(cd "$TEST_DIR/.." && pwd)"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

# Deterministic, hermetic git identity for the temp repos.
export GIT_AUTHOR_NAME="Test Author" GIT_AUTHOR_EMAIL="author@example.com"
export GIT_COMMITTER_NAME="Test Pusher" GIT_COMMITTER_EMAIL="pusher@example.com"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

# ---------------------------------------------------------------------------
# Repo/remote helpers (mirrors scripts/test-rebase-resolve.sh's harness).
# ---------------------------------------------------------------------------

# new_repo: create an isolated git repo in a fresh tmpdir, print its path.
new_repo() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-test.XXXXXX")"
    git -C "$d" init -q -b main
    git -C "$d" config commit.gpgsign false
    printf '%s' "$d"
}

# new_repo_with_branch <branch>: a repo with one commit on main, then
# checked out to a fresh <branch>. Prints the repo path.
new_repo_with_branch() {
    local branch="$1"
    local d; d="$(new_repo)"
    (
        cd "$d" || exit 1
        printf 'base\n' > f.txt
        git add -A && git commit -qm base
        git checkout -q -b "$branch"
    )
    printf '%s' "$d"
}

# new_bare_remote: an isolated bare repo standing in for `origin`.
new_bare_remote() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-remote.XXXXXX")"
    git init -q --bare -b main "$d"
    printf '%s' "$d"
}

# remote_sha <remote> <ref>: current SHA of <ref> on the bare remote, or empty.
remote_sha() {
    git -C "$1" rev-parse --verify -q "$2" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Fake `bd`: behavior driven by state files, so each test writes exactly the
# response it needs without a combinatorial helper signature.
#
#   <dir>/fake-bd-state/show-json   -- `bd show <any-id> --json` echoes this
#                                       verbatim (exit 0).
#   <dir>/fake-bd-state/show-exit   -- if present, `bd show` exits with this
#                                       code instead (no output) — simulates
#                                       bd/Dolt unreachable.
#   <dir>/fake-bd-state/show-sleep  -- if present, `bd show` sleeps this many
#                                       seconds first — simulates a hung
#                                       read for timeout tests.
#   <dir>/fake-bd-state/list-json   -- response to `bd list ... --json`
#                                       (defaults to "[]").
# ---------------------------------------------------------------------------

write_fake_bd() {
    local dir="$1"
    mkdir -p "$dir/fake-bd-state"
    cat > "$dir/bd" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state="$(dirname "$0")/fake-bd-state"
case "$1" in
  show)
    if [ -f "$state/show-sleep" ]; then
      sleep "$(cat "$state/show-sleep")"
    fi
    if [ -f "$state/show-exit" ]; then
      exit "$(cat "$state/show-exit")"
    fi
    if [ -f "$state/show-json" ]; then
      cat "$state/show-json"
      exit 0
    fi
    exit 1
    ;;
  list)
    if [ -f "$state/list-json" ]; then
      cat "$state/list-json"
      exit 0
    fi
    printf '[]'
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
FAKE
    chmod +x "$dir/bd"
}

# write_show_json <fake-bd-dir> <id> <status> <assignee> <routed_to> [labels-json-array]
write_show_json() {
    local fbd="$1" id="$2" status="$3" assignee="$4" routed_to="$5" labels="${6:-[]}"
    mkdir -p "$fbd/fake-bd-state"
    printf '[{"id":"%s","status":"%s","assignee":"%s","metadata":{"gc.routed_to":"%s"},"labels":%s}]' \
        "$id" "$status" "$assignee" "$routed_to" "$labels" > "$fbd/fake-bd-state/show-json"
}

# run_guard <repo> <fake-bd-dir> <gc-agent> <gc-template> [pog-timeout-seconds] [gc-session-id] [gc-session-name]
#
# Runs assert_bead_still_claimed with cwd=<repo>, PATH prefixed with
# <fake-bd-dir>, and the given env. GC_SESSION_ID/GC_SESSION_NAME are
# optional (empty by default) so existing GC_AGENT-only callers are
# unchanged; supply them to exercise the session identity-set match.
# Combined stdout+stderr is the caller's to capture; the subshell's exit
# code is assert_bead_still_claimed's.
run_guard() {
    local repo="$1" fbd="$2" agent="$3" template="$4" pog_timeout="${5:-5}"
    local session_id="${6:-}" session_name="${7:-}"
    (
        cd "$repo" || exit 1
        PATH="$fbd:$PATH" GC_AGENT="$agent" GC_TEMPLATE="$template" \
            GC_SESSION_ID="$session_id" GC_SESSION_NAME="$session_name" \
            POG_TIMEOUT_SECONDS="$pog_timeout" LIB="$LIB" \
            bash -c '. "$LIB"; assert_bead_still_claimed'
    )
}

# ---------------------------------------------------------------------------
# assert_bead_still_claimed — direct tests.
# ---------------------------------------------------------------------------

test_allow_clean_claim() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "agent-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        record_pass "allow/clean-claim"
    else
        record_fail "allow/clean-claim" "expected rc=0, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_closed() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "closed" "agent-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -qi "status" <<<"$out" && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/closed (rc=$rc, names the failed check + mentions --no-verify)"
    else
        record_fail "block/closed" "expected non-zero rc mentioning status+--no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_reassigned() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    # assignee differs from EVERY supplied session identity
    # (GC_AGENT=agent-x, GC_SESSION_ID=sess-id-x, GC_SESSION_NAME=sess-name-x)
    # → the identity-set match finds no owner → still blocks.
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "someone-else" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 5 "sess-id-x" "sess-name-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -qi "assignee" <<<"$out" && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/reassigned (rc=$rc, names the failed check + mentions --no-verify)"
    else
        record_fail "block/reassigned" "expected non-zero rc mentioning assignee+--no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

# The claim path sets bead.assignee from the first non-empty of
# GC_SESSION_NAME/GC_SESSION_ID/GC_ALIAS/GC_AGENT, so a bead legitimately
# owned by this session can carry the session id (or name) as its assignee
# while GC_AGENT differs. These two tests pin that the guard accepts ANY
# current-session identity, not GC_AGENT alone (the false-block fixed here).
test_allow_when_assignee_is_session_id() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "sess-id-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 5 "sess-id-x" "sess-name-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        record_pass "allow/assignee-is-session-id (rc=0, GC_AGENT differs)"
    else
        record_fail "allow/assignee-is-session-id" "expected rc=0, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_allow_when_assignee_is_session_name() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "sess-name-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 5 "sess-id-x" "sess-name-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        record_pass "allow/assignee-is-session-name (rc=0, GC_AGENT differs)"
    else
        record_fail "allow/assignee-is-session-name" "expected rc=0, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_routed_to_changed() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "agent-x" "other-tmpl" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -qi "routed_to" <<<"$out" && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/routed-to-changed (rc=$rc, names the failed check + mentions --no-verify)"
    else
        record_fail "block/routed-to-changed" "expected non-zero rc mentioning routed_to+--no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_hold_mayor() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "agent-x" "tmpl-x" '["hold:mayor"]'
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -qi "hold:mayor" <<<"$out" && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/hold-mayor (rc=$rc, names the failed check + mentions --no-verify)"
    else
        record_fail "block/hold-mayor" "expected non-zero rc mentioning hold:mayor+--no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_hold_external() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "agent-x" "tmpl-x" '["hold:external"]'
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -qi "hold:external" <<<"$out" && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/hold-external (rc=$rc, names the failed check + mentions --no-verify)"
    else
        record_fail "block/hold-external" "expected non-zero rc mentioning hold:external+--no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_bd_unreachable() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    mkdir -p "$fbd/fake-bd-state"
    echo 1 > "$fbd/fake-bd-state/show-exit"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/bd-unreachable (rc=$rc, fails closed, mentions --no-verify)"
    else
        record_fail "block/bd-unreachable" "expected non-zero rc mentioning --no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_block_on_bd_timeout() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    mkdir -p "$fbd/fake-bd-state"
    echo 3 > "$fbd/fake-bd-state/show-sleep"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 1 2>&1)"; rc=$?  # POG_TIMEOUT_SECONDS=1, sleep=3
    if [[ $rc -ne 0 ]] && grep -q -- "--no-verify" <<<"$out"; then
        record_pass "block/bd-timeout (rc=$rc, bounded read killed the hang, mentions --no-verify)"
    else
        record_fail "block/bd-timeout" "expected non-zero rc mentioning --no-verify, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

# ---------------------------------------------------------------------------
# Bead-id resolution.
# ---------------------------------------------------------------------------

test_bead_id_branch_wins_and_warns_on_disagreement() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "builder/ga-abc123.1-my-feature")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    # Branch resolves to ga-abc123.1; the assignee fallback disagrees.
    printf '[{"id":"ga-zzz999.2"}]' > "$fbd/fake-bd-state/list-json"
    write_show_json "$fbd" "ga-abc123.1" "in_progress" "agent-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]] && grep -q "ga-abc123.1" <<<"$out" && grep -q "ga-zzz999.2" <<<"$out"; then
        record_pass "resolve/branch-wins-warns-on-disagreement (rc=0, warning names both ids)"
    else
        record_fail "resolve/branch-wins-warns-on-disagreement" "expected rc=0 with both ids mentioned, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_bead_id_fallback_used_when_branch_no_match() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "chore/unrelated-cleanup")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    printf '[{"id":"ga-fallbk.3"}]' > "$fbd/fake-bd-state/list-json"
    write_show_json "$fbd" "ga-fallbk.3" "in_progress" "agent-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        record_pass "resolve/assignee-fallback-used-when-branch-no-match (rc=0)"
    else
        record_fail "resolve/assignee-fallback-used-when-branch-no-match" "expected rc=0, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

test_allow_when_no_bead_id_resolvable() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "chore/unrelated-cleanup")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    # No list-json configured -> fake `bd list` answers "[]"; no show-json
    # configured either, since nothing should ever call `bd show`.
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]]; then
        record_pass "resolve/allow-when-no-bead-id-resolvable (rc=0, nothing to check)"
    else
        record_fail "resolve/allow-when-no-bead-id-resolvable" "expected rc=0, got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

# Known limitation: the assignee-fallback query filters on
# --status=in_progress, so it cannot find a bead that has already left that
# status (e.g. closed by the exact mayor ruling this guard exists to catch)
# by the time the fallback runs. See the KNOWN LIMITATION comment on
# _pog_resolve_bead_id in push-ownership-guard.sh — this pins the current,
# spec-compliant behavior so a change to the fallback algorithm is a
# deliberate, visible decision, not a silent regression either direction.
# Confirmed against a real bd via manual repro (ga-fip9ps.1 bead notes);
# this test reproduces it hermetically. This gap only affects the fallback
# path (branch name doesn't encode the bead id) — this repo's real branch
# convention (builder/<bead-id>-<slug>) always encodes the id, so the
# primary resolution path is unaffected.
test_fallback_cannot_detect_staleness_after_status_leaves_in_progress() {
    local repo fbd out rc
    repo="$(new_repo_with_branch "chore/unrelated-cleanup")"
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    # The fallback's `bd list --status=in_progress` sees nothing, exactly as
    # a real bd would once this session's bead flipped to closed — the same
    # query that resolves the id is filtered on the very status the guard
    # exists to catch a bead leaving.
    printf '[]' > "$fbd/fake-bd-state/list-json"
    # Configured but must never be consulted: with no id resolved,
    # assert_bead_still_claimed's "nothing to check" branch returns before
    # ever calling `bd show`. If a future change to resolution makes it
    # less strict (e.g. caching the last-seen id), this would start hitting
    # show-json below and the second assertion below would catch it.
    write_show_json "$fbd" "ga-stale01" "closed" "agent-x" "tmpl-x" "[]"
    out="$(run_guard "$repo" "$fbd" "agent-x" "tmpl-x" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]] && ! grep -qi "BLOCKED" <<<"$out"; then
        record_pass "resolve/fallback-cannot-detect-staleness-after-status-leaves-in-progress (rc=0, known limitation pinned)"
    else
        record_fail "resolve/fallback-cannot-detect-staleness-after-status-leaves-in-progress" "expected rc=0 and no BLOCKED text (this documents a known gap — if it now fails because the gap was closed, update this test and the KNOWN LIMITATION comment together), got rc=$rc, output: $out"
    fi
    rm -rf "$repo" "$fbd"
}

# ---------------------------------------------------------------------------
# Hook wiring — real .githooks/pre-push, real bare remote.
#
# install_guard_hook copies the REAL guard lib and the REAL
# .githooks/pre-push (not a re-implementation) into the temp repo, so these
# tests catch a future edit to either file breaking the wiring. A trivial
# Makefile stands in for the real one: pushing a brand-new branch makes the
# hook's `go_changed` gate trip (no remote counterpart to diff against) and
# fall through to `exec make test-fast-parallel`, which these tests don't
# want to actually run — only the ownership guard's wiring is under test
# here.
# ---------------------------------------------------------------------------

install_guard_hook() {
    local repo="$1"
    mkdir -p "$repo/scripts" "$repo/.githooks"
    cp "$LIB" "$repo/scripts/push-ownership-guard.sh"
    cp "$REPO_ROOT/.githooks/pre-push" "$repo/.githooks/pre-push"
    chmod +x "$repo/.githooks/pre-push"
    printf 'test-fast-parallel:\n\t@true\n' > "$repo/Makefile"
    git -C "$repo" config core.hooksPath .githooks
}

# setup_hook_push_scenario <status>: bare remote + a work clone with the
# guard hook installed and one commit queued on a fresh bead-shaped branch,
# plus a fake bd reporting the given status for that bead. Echoes
# "<remote-dir> <work-dir> <fake-bd-dir> <branch>" on one line.
setup_hook_push_scenario() {
    local status="$1"
    local remote work fbd branch="ga-abc123.1-my-feature"
    remote="$(new_bare_remote)"
    work="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-hookwork.XXXXXX")"
    git clone -q "$remote" "$work" 2>/dev/null
    (
        cd "$work" || exit 1
        git config commit.gpgsign false
        install_guard_hook "$work"
        printf 'base\n' > f.txt; git add -A; git commit -qm base
        git checkout -q -b "$branch"
        printf 'more\n' >> f.txt; git add -A; git commit -qm work
    )
    fbd="$(mktemp -d "${TMPDIR:-/tmp}/gc-pog-fakebd.XXXXXX")"
    write_fake_bd "$fbd"
    write_show_json "$fbd" "ga-abc123.1" "$status" "agent-x" "tmpl-x" "[]"
    printf '%s %s %s %s' "$remote" "$work" "$fbd" "$branch"
}

test_hook_blocks_push_on_stale_claim() {
    local remote work fbd branch out rc
    read -r remote work fbd branch <<<"$(setup_hook_push_scenario closed)"
    out="$(cd "$work" && PATH="$fbd:$PATH" GC_AGENT="agent-x" GC_TEMPLATE="tmpl-x" GIT_TERMINAL_PROMPT=0 git push origin "$branch" 2>&1)"; rc=$?
    if [[ $rc -ne 0 ]] && [[ -z "$(remote_sha "$remote" "refs/heads/$branch")" ]]; then
        record_pass "hook/blocks-push-on-stale-claim (rejected, remote untouched)"
    else
        record_fail "hook/blocks-push-on-stale-claim" "expected rejected push with remote untouched, got rc=$rc remote_sha=$(remote_sha "$remote" "refs/heads/$branch") output=$out"
    fi
    rm -rf "$remote" "$work" "$fbd"
}

test_hook_no_verify_bypasses_guard() {
    local remote work fbd branch out rc
    read -r remote work fbd branch <<<"$(setup_hook_push_scenario closed)"
    out="$(cd "$work" && PATH="$fbd:$PATH" GC_AGENT="agent-x" GC_TEMPLATE="tmpl-x" GIT_TERMINAL_PROMPT=0 git push --no-verify origin "$branch" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]] && [[ -n "$(remote_sha "$remote" "refs/heads/$branch")" ]]; then
        record_pass "hook/no-verify-bypasses-guard (push succeeded despite stale claim)"
    else
        record_fail "hook/no-verify-bypasses-guard" "expected successful push, got rc=$rc output=$out"
    fi
    rm -rf "$remote" "$work" "$fbd"
}

test_hook_allows_push_on_clean_claim() {
    local remote work fbd branch out rc
    read -r remote work fbd branch <<<"$(setup_hook_push_scenario in_progress)"
    out="$(cd "$work" && PATH="$fbd:$PATH" GC_AGENT="agent-x" GC_TEMPLATE="tmpl-x" GIT_TERMINAL_PROMPT=0 git push origin "$branch" 2>&1)"; rc=$?
    if [[ $rc -eq 0 ]] && [[ -n "$(remote_sha "$remote" "refs/heads/$branch")" ]]; then
        record_pass "hook/allows-push-on-clean-claim"
    else
        record_fail "hook/allows-push-on-clean-claim" "expected successful push, got rc=$rc output=$out"
    fi
    rm -rf "$remote" "$work" "$fbd"
}

# ---------------------------------------------------------------------------
# Static wiring checks — mirrors test-rebase-resolve.sh's style of grepping
# the real source files rather than re-deriving their behavior.
# ---------------------------------------------------------------------------

test_pre_push_hook_sources_and_calls_guard() {
    local hook="$REPO_ROOT/.githooks/pre-push"
    if grep -q "push-ownership-guard.sh" "$hook" 2>/dev/null && grep -q "assert_bead_still_claimed" "$hook" 2>/dev/null; then
        record_pass "wiring/pre-push-hook-sources-and-calls-guard"
    else
        record_fail "wiring/pre-push-hook-sources-and-calls-guard" "$hook does not source+call assert_bead_still_claimed"
    fi
}

test_rebase_lib_calls_guard_before_force_with_lease() {
    local rebase_lib="$REPO_ROOT/scripts/rebase-resolve-lib.sh"
    local guard_line lease_line push_line
    guard_line="$(grep -nF "assert_bead_still_claimed" "$rebase_lib" 2>/dev/null | tail -1 | cut -d: -f1)"
    # SC2016 intentional: literal-text search of rebase-resolve-lib.sh source.
    # shellcheck disable=SC2016
    lease_line="$(grep -nF 'lease_arg="--force-with-lease=$branch:$expected_remote_sha"' "$rebase_lib" 2>/dev/null | tail -1 | cut -d: -f1)"
    # shellcheck disable=SC2016
    push_line="$(grep -nF 'git push "$lease_arg" origin "$branch"' "$rebase_lib" 2>/dev/null | tail -1 | cut -d: -f1)"
    if [[ -n "$guard_line" && -n "$lease_line" && -n "$push_line" \
        && "$guard_line" -lt "$lease_line" && "$lease_line" -lt "$push_line" ]]; then
        record_pass "wiring/rebase-lib-calls-guard-before-force-with-lease"
    else
        record_fail "wiring/rebase-lib-calls-guard-before-force-with-lease" "guard_line=$guard_line lease_line=$lease_line push_line=$push_line (guard must precede lease construction and guarded push)"
    fi
}

# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------

run_all() {
    test_allow_clean_claim
    test_block_on_closed
    test_block_on_reassigned
    test_allow_when_assignee_is_session_id
    test_allow_when_assignee_is_session_name
    test_block_on_routed_to_changed
    test_block_on_hold_mayor
    test_block_on_hold_external
    test_block_on_bd_unreachable
    test_block_on_bd_timeout
    test_bead_id_branch_wins_and_warns_on_disagreement
    test_bead_id_fallback_used_when_branch_no_match
    test_allow_when_no_bead_id_resolvable
    test_fallback_cannot_detect_staleness_after_status_leaves_in_progress
    test_hook_blocks_push_on_stale_claim
    test_hook_no_verify_bypasses_guard
    test_hook_allows_push_on_clean_claim
    test_pre_push_hook_sources_and_calls_guard
    test_rebase_lib_calls_guard_before_force_with_lease

    echo
    echo "pass=$pass fail=$fail"
    [[ $fail -eq 0 ]]
}

run_all
