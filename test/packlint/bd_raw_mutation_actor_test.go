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

// TestBdMutationGuardRules pins the guard's rule set on synthetic commands
// (ga-u3g2r3, FR-4 re-based after #6324). The whole-repo walk above proves the
// shipped formulas pass; these cases prove the rules would catch a bad one.
//
// The rule: an actor-less mutation is fine (bd resolves it through
// $BEADS_ACTOR, which is what the session's claim was recorded under); an
// EXPLICIT --actor must be $BEADS_ACTOR or a GC_* fallback chain consistent
// with a claim-writer order; --force and "# guard-ack:<slug>" still exempt.
func TestBdMutationGuardRules(t *testing.T) {
	const (
		idFirst   = `"${GC_ALIAS:-${GC_SESSION_ID:-${GC_SESSION_NAME:-}}}"`
		nameFirst = `"${GC_ALIAS:-${GC_SESSION_NAME:-${GC_SESSION_ID:-}}}"`
		aliasLast = `"${GC_SESSION_NAME:-${GC_ALIAS:-}}"`
	)
	tests := []struct {
		name string
		cmd  string
		want int // violations
	}{
		{"actor-less raw bd close resolves through BEADS_ACTOR", `bd close "$ID" --reason r`, 0},
		{"actor-less gc bd close resolves through BEADS_ACTOR", `gc bd close "$ID" --reason r`, 0},
		{"alias-id-name chain is a claim-writer order", `gc bd close "$ID" --actor ` + idFirst, 0},
		{"alias-name-id chain is the manual claim-writer order", `gc bd close "$ID" --actor ` + nameFirst, 0},
		{"alias must lead any chain", `gc bd close "$ID" --actor ` + aliasLast, 1},
		{"BEADS_ACTOR is valid", `gc bd close "$ID" --actor "$BEADS_ACTOR"`, 0},
		{"BEADS_ACTOR in equals form is valid", `gc bd close "$ID" --actor=${BEADS_ACTOR}`, 0},
		{"a literal actor is not an identity the claim recorded", `gc bd close "$ID" --actor builder`, 1},
		{"an unrecognized variable is not valid", `gc bd close "$ID" --actor "$close_actor"`, 1},
		{"gc-prefixed invocations are scanned, not skipped", `gc bd update "$ID" --status closed --actor ` + aliasLast, 1},
		{"flags between bd and the verb do not hide the verb", `gc bd --city X close "$ID" --actor bogus`, 1},
		{"short flags between bd and the verb do not hide the verb", `bd -q update "$ID" --status=closed --actor bogus`, 1},
		{"a trailing && bd close is its own invocation", `gc bd update "$A" --actor "$BEADS_ACTOR" && bd close "$B" --actor bogus`, 1},
		{"a valid --actor on one invocation does not exempt the next", `gc bd close "$A" --actor "$BEADS_ACTOR" && gc bd close "$B" --actor bogus`, 1},
		{"two bad invocations are two violations", `gc bd close "$A" --actor x; gc bd close "$B" --actor y`, 2},
		{"--force exempts its own invocation", `gc bd close "$A" --actor bogus --force`, 0},
		{"--force on another invocation does not exempt this one", `gc bd close "$A" --force && gc bd close "$B" --actor bogus`, 1},
		{"guard-ack exempts the command", `bd close "$A" --actor bogus # guard-ack:reviewed`, 0},
		{"a non-mutating bd verb is out of scope", `gc bd show close --actor bogus`, 0},
		{"a separator inside quotes does not start a new invocation", `gc bd close "$A" --reason "done; bd close B --actor bogus"`, 0},
		{"a commented-out command is skipped", `# gc bd close "$A" --actor bogus`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bdCommandViolations(tc.cmd)
			if len(got) != tc.want {
				t.Fatalf("bdCommandViolations(%q) = %d violation(s) %q, want %d", tc.cmd, len(got), got, tc.want)
			}
		})
	}
}

// TestBdInvocationsParsing pins the tokenizer on the shapes that used to hide a
// mutation from the line-level scan.
func TestBdInvocationsParsing(t *testing.T) {
	tests := []struct {
		name  string
		cmd   string
		verbs []string
	}{
		{"plain", `bd close X`, []string{"close"}},
		{"gc wrapper", `gc bd close X`, []string{"close"}},
		{"valued flag between bd and verb", `gc bd --city X close Y`, []string{"close"}},
		{"equals flag between bd and verb", `gc bd --city=X heartbeat Y`, []string{"heartbeat"}},
		{"boolean flag before verb", `bd -v unclaim Y`, []string{"unclaim"}},
		{"chained with and", `gc bd update A --set-metadata k=v && bd close A`, []string{"update", "close"}},
		{"chained with semicolon and pipe", `bd close A; bd update B | cat`, []string{"close", "update"}},
		{"non-mutating verb", `gc bd show X`, nil},
		{"verb word inside another command", `echo close bd`, nil},
		{"identifier containing bd", `dry_run_bd close X`, nil},
		{"quoted separator", `gc bd close A --reason "x && bd close B"`, []string{"close"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, inv := range bdInvocations(tc.cmd) {
				got = append(got, inv.verb)
			}
			if strings.Join(got, ",") != strings.Join(tc.verbs, ",") {
				t.Fatalf("bdInvocations(%q) verbs = %v, want %v", tc.cmd, got, tc.verbs)
			}
		})
	}
}

// TestLogicalCommandsJoinContinuations: a backslash-continued command is one
// command, reported at the line it starts on, so an --actor on a continuation
// line belongs to its invocation and not to a line of its own.
func TestLogicalCommandsJoinContinuations(t *testing.T) {
	src := "echo start\n" +
		"gc bd close \"$ID\" \\\n" +
		"  --reason done \\\n" +
		"  --actor bogus\n" +
		"gc bd close \"$OTHER\"\n"
	got := logicalCommands(src)
	if len(got) != 3 {
		t.Fatalf("logicalCommands = %d commands %+v, want 3", len(got), got)
	}
	if got[1].line != 2 {
		t.Fatalf("continued command starts at line %d, want 2", got[1].line)
	}
	if v := bdCommandViolations(got[1].text); len(v) != 1 {
		t.Fatalf("continued command violations = %q, want the bogus --actor flagged once", v)
	}
	if v := bdCommandViolations(got[2].text); len(v) != 0 {
		t.Fatalf("following command violations = %q, want none", v)
	}
}

// RED-phase stubs (ga-u3g2r3): the rule implementation lands in the GREEN
// commit. These are inert so the package compiles and the rule tests above fail
// on behavior, not on a build error the pre-commit lint would reject.

// bdInvocation is one guarded bd invocation found in a command.
type bdInvocation struct {
	verb string
}

// logicalCommand is one shell command after backslash-continuation joining.
type logicalCommand struct {
	line int
	text string
}

func bdInvocations(_ string) []bdInvocation { return nil }

func logicalCommands(_ string) []logicalCommand { return nil }

func bdCommandViolations(_ string) []string { return nil }
