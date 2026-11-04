package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRawBdMutationRequiresActorOrForce is the FR-4 guard for ga-b15hwm
// (rule maintained under ga-u3g2r3).
//
// The failure it prevents: a `bd close` / `bd unclaim` / `bd heartbeat` /
// `bd update --status closed` in a formula or pack script ran under an actor
// that differed from the identity `gc hook --claim` recorded in the bead's
// assignee, so bd's ownership guard (validation.AssigneeMatches on close, the
// actorMatches check in HeartbeatIssueInTx / UnclaimIssueInTx) rejected it.
// mol-dog-stale-db.toml:320 hit exactly that.
//
// A session's runtime environment exports BEADS_ACTOR from
// session.AssigneeIdentifier, the same string the claim is recorded under
// (hookClaimAssigneeIdentity), so an ACTOR-LESS mutation is correct by
// construction: bd resolves its actor through $BEADS_ACTOR. The guard
// therefore does not demand an --actor. It catches an EXPLICIT --actor that can
// disagree with the recorded claim, which is how the failure above is
// reintroduced by hand:
//
//   - `--actor "$BEADS_ACTOR"` is always valid.
//   - A GC_* fallback chain is valid when its variables appear in a claim-writer
//     order (see actorChainOrders). Two orders exist because the writer is
//     origin-aware: a manual unaliased session claims under session_name, a
//     pool or ephemeral one under the session bead id, and a script cannot know
//     which it is running in. Either order is accepted; prefer $BEADS_ACTOR,
//     which is right for both.
//   - A literal name or any other variable is a violation.
//
// `gc bd ...` is scanned like raw `bd ...`: the wrapper only forces cmd.Dir, it
// does not change which actor is used, so it earns no exemption.
//
// Exemptions: `--force` among the invocation's OWN arguments, and a
// `# guard-ack:<slug>` comment on the command (a reviewed exception).
//
// Parsing is per shell command, not per physical line: backslash continuations
// are joined, a command is split on && || ; | & outside quotes, and each bd
// invocation is judged on its own arguments, so one invocation's valid --actor
// or --force never exempts the next. Known limit: a bd call inside a `$(...)`
// within double quotes is not seen as its own invocation.
//
// Scope is deliberately runnable content only (.toml formula bodies, .sh pack
// scripts) — not .md, which is full of prose examples like "`bd close <id>`"
// that document the CLI but never execute it.
func TestRawBdMutationRequiresActorOrForce(t *testing.T) {
	root := repoRoot()
	scanRoots := []string{
		filepath.Join(root, "examples"),
		filepath.Join(root, "internal", "bootstrap", "packs"),
	}

	var violations []string
	for _, dir := range scanRoots {
		err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !rawBdMutationScanExts[strings.ToLower(filepath.Ext(file))] {
				return nil
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			if rawBdMutationAllowlistFiles[filepath.ToSlash(rel)] {
				return nil
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("reading %s: %w", file, err)
			}
			for _, cmd := range logicalCommands(string(data)) {
				for _, reason := range bdCommandViolations(cmd.text) {
					violations = append(violations, fmt.Sprintf("%s:%d: %s: %s", rel, cmd.line, reason, strings.TrimSpace(cmd.text)))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	if len(violations) > 0 {
		t.Errorf("found %d bd close/unclaim/heartbeat/update call(s) with an --actor that can "+
			"disagree with the identity the claim was recorded under (ga-b15hwm, re-based by "+
			"ga-u3g2r3: drop the --actor so bd resolves $BEADS_ACTOR, or pass --actor \"$BEADS_ACTOR\"; "+
			"--force or a `# guard-ack:<slug>` comment exempts a reviewed exception):\n  %s",
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

// bdGuardedVerbs are the bd verbs whose ownership check the guard protects.
var bdGuardedVerbs = map[string]bool{
	"close":     true,
	"unclaim":   true,
	"heartbeat": true,
	"update":    true,
}

// bdGuardAckRE matches the reviewed-exception comment anywhere in a command.
var bdGuardAckRE = regexp.MustCompile(`#\s*guard-ack:\S+`)

// actorVarRE finds each identity variable referenced in an --actor value, in
// left-to-right order, which for a bash `${A:-${B:-...}}` chain is also its
// fallback precedence: bash evaluates the outermost variable before its own
// default expression.
var actorVarRE = regexp.MustCompile(`\$\{?(BEADS_ACTOR|GC_[A-Z_]+)`)

// actorChainOrders are the claim-writer precedence orders a GC_* chain may
// follow. hookClaimAssigneeIdentity records a claim under alias first, then:
//
//   - pool, ephemeral, or unknown origin: session id, agent, session name;
//   - manual origin: session name, session id, agent.
//
// A script cannot know its session's origin, so a chain is accepted when it
// follows either order.
var actorChainOrders = [][]string{
	{"GC_ALIAS", "GC_SESSION_ID", "GC_AGENT", "GC_SESSION_NAME"},
	{"GC_ALIAS", "GC_SESSION_NAME", "GC_SESSION_ID", "GC_AGENT"},
}

// bdInvocation is one guarded bd invocation: its verb, and every word after the
// `bd` token up to the end of its own shell command (flags before the verb
// included).
type bdInvocation struct {
	verb string
	args []string
}

// logicalCommand is one shell command after backslash-continuation joining,
// with the 1-based physical line it starts on.
type logicalCommand struct {
	line int
	text string
}

// logicalCommands splits src into logical commands: a line ending in a
// backslash is joined with the next, so an --actor on a continuation line
// belongs to the invocation it continues. Blank lines are dropped, and a comment
// line never continues.
func logicalCommands(src string) []logicalCommand {
	var out []logicalCommand
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		start := i
		text := lines[i]
		for !strings.HasPrefix(strings.TrimSpace(text), "#") && i+1 < len(lines) {
			trimmed := strings.TrimRight(text, " \t")
			if !strings.HasSuffix(trimmed, `\`) {
				break
			}
			text = strings.TrimSuffix(trimmed, `\`) + " " + strings.TrimSpace(lines[i+1])
			i++
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, logicalCommand{line: start + 1, text: text})
	}
	return out
}

// shellSegments splits cmd into shell words grouped by command: it separates on
// && || ; | & outside quotes, strips quoting from words, treats a `#` at the
// start of a word as a comment to the end, and leaves `>&` / `&>` redirections
// inside their word.
func shellSegments(cmd string) [][]string {
	var (
		segments [][]string
		words    []string
		cur      strings.Builder
		inWord   bool
		quote    rune
	)
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endSegment := func() {
		endWord()
		if len(words) > 0 {
			segments = append(segments, words)
			words = nil
		}
	}
	runes := []rune(cmd)
scan:
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"' && i+1 < len(runes):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '#' && !inWord:
			break scan
		case r == ' ' || r == '\t' || r == '\n':
			endWord()
		case r == ';' || r == '|':
			endSegment()
		case r == '&':
			redirect := (i > 0 && runes[i-1] == '>') || (i+1 < len(runes) && runes[i+1] == '>')
			if redirect {
				cur.WriteRune(r)
				inWord = true
			} else {
				endSegment()
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	endSegment()
	return segments
}

// isBdWord reports whether w is the bd command word, bare or by path, once any
// subshell or backtick wrapper is peeled off.
func isBdWord(w string) bool {
	w = strings.TrimRight(strings.TrimLeft(w, "$(`{"), ")`}")
	return path.Base(w) == "bd"
}

// bdInvocations returns every guarded bd invocation in cmd. After the `bd`
// word it skips global flags (a valued `--city X` consumes its value, a `-v` or
// `--flag=value` stands alone) and accepts the invocation only when the first
// non-flag word is one of the guarded verbs.
func bdInvocations(cmd string) []bdInvocation {
	var out []bdInvocation
	for _, words := range shellSegments(cmd) {
		for i, w := range words {
			if !isBdWord(w) {
				continue
			}
			for j := i + 1; j < len(words); j++ {
				word := words[j]
				if bdGuardedVerbs[word] {
					out = append(out, bdInvocation{verb: word, args: words[i+1:]})
					break
				}
				if !strings.HasPrefix(word, "-") {
					break
				}
				if !strings.Contains(word, "=") && j+1 < len(words) &&
					!strings.HasPrefix(words[j+1], "-") && !bdGuardedVerbs[words[j+1]] {
					j++
				}
			}
		}
	}
	return out
}

// actorFlagValue returns the value of an explicit --actor among args, in either
// `--actor V` or `--actor=V` form, and whether the flag is present at all.
func actorFlagValue(args []string) (string, bool) {
	for i, a := range args {
		if a == "--actor" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if v, ok := strings.CutPrefix(a, "--actor="); ok {
			return v, true
		}
	}
	return "", false
}

// actorValueViolation reports why an explicit --actor value can disagree with
// the identity the claim was recorded under, or "" when it cannot.
func actorValueViolation(value string) string {
	var chain []string
	for _, m := range actorVarRE.FindAllStringSubmatch(value, -1) {
		if m[1] == "BEADS_ACTOR" {
			if len(chain) == 0 {
				return ""
			}
			continue
		}
		chain = append(chain, m[1])
	}
	if len(chain) == 0 {
		return fmt.Sprintf("--actor %q is not $BEADS_ACTOR or a GC_* identity chain", value)
	}
	for _, order := range actorChainOrders {
		if chainFollowsOrder(chain, order) {
			return ""
		}
	}
	return fmt.Sprintf("--actor %q resolves %s in an order no claim writer uses (alias first, then session id or session name)",
		value, strings.Join(chain, " > "))
}

// chainFollowsOrder reports whether every variable in chain is in order and
// their positions in order strictly increase left to right.
func chainFollowsOrder(chain, order []string) bool {
	prev := -1
	for _, v := range chain {
		pos := -1
		for i, o := range order {
			if o == v {
				pos = i
			}
		}
		if pos <= prev {
			return false
		}
		prev = pos
	}
	return true
}

// bdInvocationViolation judges one invocation on its own arguments.
func bdInvocationViolation(inv bdInvocation) string {
	for _, a := range inv.args {
		if a == "--force" {
			return ""
		}
	}
	value, present := actorFlagValue(inv.args)
	if !present {
		return ""
	}
	return actorValueViolation(value)
}

// bdCommandViolations returns one reason per guarded bd invocation in cmd whose
// explicit --actor can disagree with the recorded claim. A `# guard-ack:<slug>`
// comment exempts the whole command.
func bdCommandViolations(cmd string) []string {
	if bdGuardAckRE.MatchString(cmd) {
		return nil
	}
	var out []string
	for _, inv := range bdInvocations(cmd) {
		if reason := bdInvocationViolation(inv); reason != "" {
			out = append(out, reason)
		}
	}
	return out
}

// TestBdMutationGuardRules pins the guard's rule set on synthetic commands
// (FR-4, ga-u3g2r3). The whole-repo walk above proves the
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

// TestBdInvocationsParsing pins the tokenizer on the shapes a line-level scan
// would miss: global flags between bd and the verb, chained commands, and
// per-invocation --actor/--force.
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
