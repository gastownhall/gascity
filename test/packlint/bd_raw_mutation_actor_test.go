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
//	--actor "${GC_ALIAS:-${GC_SESSION_NAME:-$GC_SESSION_ID}}"   (preferred)
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
			if rawBdMutationAllowlistFiles[filepath.ToSlash(rel)] {
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
				if bdActorGuardExemptRE.MatchString(line) {
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
