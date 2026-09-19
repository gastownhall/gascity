package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRawBdMutationRequiresActorOrForce guards the REVISION 2 fix for
// ga-b15hwm. A raw (non-`gc`-prefixed) `bd close` / `bd unclaim` /
// `bd heartbeat` / `bd update --status closed` line in a formula or pack
// script falls back to bd's own actor-resolution chain (--actor >
// $BEADS_ACTOR > git config user.name > $USER) instead of the session/wisp
// identity `gc hook --claim` stamped into the bead's assignee. When those two
// identities differ, the ownership guard on close/heartbeat/unclaim rejects
// the call — upstream, validation.AssigneeMatches for close's CLI preflight,
// and the issueops-level actorMatches check inside HeartbeatIssueInTx /
// UnclaimIssueInTx for the other two — which is exactly what
// mol-dog-stale-db.toml:320 hit. This is an identity-MODEL mismatch, not a
// cwd issue: `gc bd`'s only real mechanistic effect is forcing cmd.Dir
// before exec, so prefixing a call with `gc` does not change which identity
// chain gets consulted and must not be treated as a pass condition here —
// that was the disproven REVISION 1 fix, tried live twice against this same
// formula.
//
// `bd update --status closed` (without an accompanying -a/assignee edit in
// the same call) is the one exception: upstream, EnforceClosePolicyInTx only
// checks open children and live blockers — no assignee/actor comparison — so
// a missing --actor there cannot reproduce this guard failure. It stays in
// scope anyway because actor still feeds the audit trail
// (audit.LogFieldChange), so an unset --actor still misattributes who closed
// the issue even though the write itself would succeed.
//
// Fix a violation by adding one of, on the SAME line as the bd invocation:
//
//	--actor "${GC_ALIAS:-${GC_SESSION_ID:-${GC_SESSION_NAME:-}}}"   (preferred)
//	--force                                                     (admin/reaper use)
//	# guard-ack:<slug>                                          (reviewed exception)
//
// Scope is deliberately runnable content only (.toml formula bodies, .sh
// pack scripts) — not .md, which is full of prose examples like
// "`bd close <id>`" that document the CLI but never execute it.
func TestRawBdMutationRequiresActorOrForce(t *testing.T) {
	root := repoRoot()
	scanRoots := []string{
		filepath.Join(root, "examples"),
		filepath.Join(root, "internal", "bootstrap", "packs"),
	}

	var violations []string
	for _, dir := range scanRoots {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !rawBdMutationScanExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(rel)
			if rawBdMutationAllowlistFiles[relSlash] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNo, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				verb := rawBdMutationVerb(line)
				if verb == "" {
					continue
				}
				if verb == "update" && !bdUpdateStatusClosedRE.MatchString(line) {
					continue
				}
				if bdActorGuardExempt(relSlash, line) {
					continue
				}
				violations = append(violations, fmt.Sprintf("%s:%d: %s", rel, lineNo+1, strings.TrimSpace(line)))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	if len(violations) > 0 {
		t.Errorf("found %d raw bd close/unclaim/heartbeat/update(--status closed) call(s) "+
			"with no --actor, --force, or # guard-ack:<slug> on the same line (ga-b15hwm "+
			"REVISION 2: this is an identity-model mismatch between the session/wisp identity "+
			"gc hook --claim stamps into assignee and bd's own actor-resolution fallback, NOT "+
			"a cwd issue — gc-prefixing alone does not fix it):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// rawBdMutationScanExts restricts the walk to runnable content: formula
// bodies (.toml) and pack scripts (.sh). Deliberately excludes .md — doc
// prose routinely quotes `bd close <id>` as an illustrative example that
// never executes, and papers like examples/gastown/SDK-ROADMAP.md do this
// throughout.
var rawBdMutationScanExts = map[string]bool{
	".toml": true,
	".sh":   true,
}

// rawBdMutationAllowlistFiles is the escape hatch for a file-wide exception
// (mirrors gc_nudge_form_test.go's nudgeAllowlistFiles). Empty until a real
// exception is needed.
var rawBdMutationAllowlistFiles = map[string]bool{}

// bdActorOrderCheckFiles opts a file into resolved-order validation of its
// --actor "${...}" fallback chains, on top of the baseline presence check
// above (AMENDED FR-4 DESIGN, ga-gskond REVISION 3). Inverted shape of
// rawBdMutationAllowlistFiles: presence here means "hold to the stronger
// invariant," not "skip."
//
// This is deliberately an opt-in allowlist rather than a codebase-wide rule
// because correctness here is claim-path-dependent, not universal.
// mol-dog-stale-db.toml is exclusively claimed via `gc hook --claim`, so its
// assignee is always hookClaimAssigneeIdentity's alias>sessionID>...
// >sessionName order (cmd/gc/cmd_hook.go) collapsed to
// GC_ALIAS>GC_SESSION_ID>GC_SESSION_NAME for a formula shell script. A
// formula that instead self-claims with a bare `bd update --claim` stamps
// BEADS_ACTOR, which resolves to the session NAME (not ID) in an unaliased
// pool session — e.g.
// packs/actual/deployer/formulas/mol-deployer-gate.formula.toml:97 in
// gc-management — so forcing ID-first there would turn a close that works
// today into a guard failure. Extend this list only after confirming a new
// file's claim path the way this ruling confirmed mol-dog-stale-db.toml's.
var bdActorOrderCheckFiles = map[string]bool{
	"examples/bd/dolt/formulas/mol-dog-stale-db.toml": true,
}

// actorFallbackTokenRE finds a recognized identity var immediately following
// a `${` inside a --actor value. Matching left-to-right over the value
// yields the tokens in nesting order, which for a bash `${VAR:-${VAR2:-...}}`
// chain is also fallback-precedence order: bash tries the outermost VAR
// before ever evaluating its own default expression.
var actorFallbackTokenRE = regexp.MustCompile(`\$\{(GC_ALIAS|GC_SESSION_ID|GC_SESSION_NAME)\b`)

// actorFallbackOrder returns the GC_ALIAS/GC_SESSION_ID/GC_SESSION_NAME
// tokens referenced in line's --actor value, in first-seen (left-to-right)
// order, or nil if line has no --actor flag or its value references none of
// the three recognized vars (e.g. a literal string, $BEADS_ACTOR alone, or
// the dynamic current-assignee shape noted on ga-gskond — out of scope
// here; no gascity formula uses it today).
func actorFallbackOrder(line string) []string {
	idx := strings.Index(line, "--actor")
	if idx < 0 {
		return nil
	}
	var order []string
	for _, m := range actorFallbackTokenRE.FindAllStringSubmatch(line[idx:], -1) {
		order = append(order, m[1])
	}
	return order
}

// actorFallbackOrderViolation reports a human-readable defect if order (as
// returned by actorFallbackOrder) does not resolve GC_ALIAS before
// GC_SESSION_ID before GC_SESSION_NAME, or "" if order respects it (which is
// vacuously true when fewer than two of the three tokens appear at all).
func actorFallbackOrderViolation(order []string) string {
	pos := make(map[string]int, len(order))
	for i, tok := range order {
		if _, seen := pos[tok]; !seen {
			pos[tok] = i
		}
	}
	aliasPos, hasAlias := pos["GC_ALIAS"]
	idPos, hasID := pos["GC_SESSION_ID"]
	namePos, hasName := pos["GC_SESSION_NAME"]

	if hasID && hasName && idPos > namePos {
		return "GC_SESSION_ID must precede GC_SESSION_NAME"
	}
	if hasAlias && hasID && aliasPos > idPos {
		return "GC_ALIAS must precede GC_SESSION_ID"
	}
	if hasAlias && hasName && aliasPos > namePos {
		return "GC_ALIAS must precede GC_SESSION_NAME"
	}
	return ""
}

// bdActorGuardExempt reports whether line is exempt from the raw-bd-mutation
// actor guard for the file at rel (repo-relative, slash-separated). For a
// file not in bdActorOrderCheckFiles this is the original presence-only
// check. For a checked file, presence of --force or a guard-ack still
// exempts unconditionally, but presence of --actor is necessary and no
// longer sufficient: its resolved fallback order must also pass
// actorFallbackOrderViolation.
func bdActorGuardExempt(rel, line string) bool {
	if !bdActorGuardExemptRE.MatchString(line) {
		return false
	}
	if !bdActorOrderCheckFiles[rel] {
		return true
	}
	order := actorFallbackOrder(line)
	if order == nil {
		return true
	}
	return actorFallbackOrderViolation(order) == ""
}

// bdMutationVerbRE finds a raw `bd <verb>` command-token occurrence for one
// of the four guarded verbs. \b before "bd" already rules out it being a
// substring of a larger identifier (e.g. "dry_run_bd_close"); the "gc bd"
// exclusion is handled separately by precededByGc since Go's RE2 engine has
// no lookbehind to fold that into the same expression.
var bdMutationVerbRE = regexp.MustCompile(`\bbd\s+(close|unclaim|heartbeat|update)\b`)

// bdUpdateStatusClosedRE requires --status closed (or -s closed — updateCmd
// registers "status" with shorthand "s") on the same line, in either
// space- or equals-separated form, so the update branch stays scoped to
// status-closed updates. This correctly excludes
// mol-dog-stale-db.toml:108's `bd update --append-notes`:
// validateIssueUpdatable calls only NotTemplate(), never AssigneeMatches, so
// that line needs no --actor.
var bdUpdateStatusClosedRE = regexp.MustCompile(`(?:--status|-s)[= ]closed\b`)

// bdActorGuardExemptRE matches any of the three sanctioned exemptions
// (--actor, --force, or a guard-ack) anywhere on the line. The explicit
// [= ] separator (rather than \b) avoids a flag like a hypothetical
// --actor-list or --force-all being mistaken for the real flag.
var bdActorGuardExemptRE = regexp.MustCompile(`--actor[= ]|--force(?:\s|$)|#\s*guard-ack:\S+`)

// rawBdMutationVerb reports the guarded verb ("close", "unclaim",
// "heartbeat", or "update") of the first raw (non-`gc`-prefixed) bd
// invocation on line, or "" if line has no such invocation.
func rawBdMutationVerb(line string) string {
	for _, m := range bdMutationVerbRE.FindAllStringSubmatchIndex(line, -1) {
		start, verbStart, verbEnd := m[0], m[2], m[3]
		if precededByGc(line[:start]) {
			continue
		}
		return line[verbStart:verbEnd]
	}
	return ""
}

// precededByGc reports whether the last whitespace-separated token in
// before is "gc" — i.e. whether the bd invocation this prefix leads into is
// actually `gc bd ...`, which routes through gc's own cmd.Dir-forcing
// wrapper and is a different, already out-of-scope code path for this
// guard (gc-prefixing does not change which actor-identity chain is
// consulted, so it is not treated as a pass condition — see the doc
// comment on TestRawBdMutationRequiresActorOrForce).
func precededByGc(before string) bool {
	fields := strings.Fields(before)
	if len(fields) == 0 {
		return false
	}
	// Trim both sides: a markdown/shell delimiter can abut "gc" on either
	// side with no space (e.g. the opening backtick in "`gc bd heartbeat
	// ...`" prose), not just trail it.
	last := strings.Trim(fields[len(fields)-1], "`\"';&|()")
	return last == "gc"
}

// TestBdActorGuardExemptOrderCheck is the isolated unit test for the
// order-validation helper (AMENDED FR-4 DESIGN, ga-gskond REVISION 3): it
// exercises bdActorGuardExempt directly against synthetic rel/line pairs,
// not the whole-repo file walk TestRawBdMutationRequiresActorOrForce
// performs.
func TestBdActorGuardExemptOrderCheck(t *testing.T) {
	const checkedFile = "examples/bd/dolt/formulas/mol-dog-stale-db.toml"
	cases := []struct {
		name string
		rel  string
		line string
		want bool
	}{
		{
			name: "checked file with alias-id-name order is exempt",
			rel:  checkedFile,
			line: `bd close "$WORK_BEAD" --actor "${GC_ALIAS:-${GC_SESSION_ID:-${GC_SESSION_NAME:-}}}"`,
			want: true,
		},
		{
			name: "checked file with name-before-id order is NOT exempt",
			rel:  checkedFile,
			line: `bd close "$WORK_BEAD" --actor "${GC_ALIAS:-${GC_SESSION_NAME:-${GC_SESSION_ID:-}}}"`,
			want: false,
		},
		{
			name: "unlisted file with the same wrong order is exempt on presence alone",
			rel:  "packs/actual/deployer/formulas/mol-deployer-gate.formula.toml",
			line: `bd close "$WORK_BEAD" --actor "${GC_ALIAS:-${GC_SESSION_NAME:-${GC_SESSION_ID:-}}}"`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdActorGuardExempt(tc.rel, tc.line); got != tc.want {
				t.Errorf("bdActorGuardExempt(%q, %q) = %v, want %v", tc.rel, tc.line, got, tc.want)
			}
		})
	}
}
