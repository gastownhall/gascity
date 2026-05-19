package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// parkedSessionReasonInputPrompt is the reason returned by the parked-session
// classifier when a session matches the rule: alive pane, two identical
// captures, typed-but-unsubmitted input on the ❯ prompt line, and no
// model-thinking indicator visible.
const parkedSessionReasonInputPrompt = "typed-but-unsubmitted input + no thinking indicator"

// parkedSessionDefaultSampleGap is the default time between the two pane
// captures used to confirm "no motion." 20s is long enough that a model
// turn boundary that is actually still printing tokens will change the
// pane bytes between the two captures, but short enough that operators
// running `gc session doctor` interactively don't lose patience.
const parkedSessionDefaultSampleGap = 20 * time.Second

// parkedSessionFixConfirmGap is the wait between sending Enter (--fix) and
// re-sampling the pane to confirm the typed input has been consumed.
const parkedSessionFixConfirmGap = 10 * time.Second

// parkedSessionPeekLines is the number of pane lines captured per sample.
// 80 is enough to capture the full Claude input box + any thinking
// indicator above it without bloating the byte-equal comparison.
const parkedSessionPeekLines = 80

// thinkingIndicatorPatterns lists the Claude Code "thinking/output"
// markers that mean the model is mid-turn — not parked. If any of these
// appear in EITHER pane capture we treat the session as making forward
// progress regardless of the input-line state. Kept in one place so the
// list is easy to extend as Claude adds new indicators.
var thinkingIndicatorPatterns = []string{
	"Blanching…",
	"Seasoning…",
	"Sautéing…",
	"Sautéed",
	"Forging…",
	"Ionizing…",
	"Baking…",
	"Baked",
	"Churned",
	"Channeling…",
	"Fiddle-faddling…",
	"Cooked",
	"Brewed",
	"Worked for",
	"Sautéed for",
}

// promptInputLineRE matches a Claude Code prompt line that has unsubmitted
// typed text after the cursor glyph. The cursor glyph "❯ " is followed by
// at least one non-whitespace character — bare "❯ " (idle prompt) does
// not match.
var promptInputLineRE = regexp.MustCompile(`(?m)^❯ +\S`)

// classifyParkedSession is the pure classifier used by `gc session doctor
// parked-sessions`. It takes two pane captures and the bead state and
// returns (parked, reason). The reason is human-readable and stable enough
// to grep against in tests.
//
// Detection rule (in this order so the most-precise reason wins):
//
//  1. State must be active or awake. asleep/suspended/closed/etc. never
//     park because the session has no live runtime to be waiting on.
//  2. The two captures must be byte-for-byte identical. Any change means
//     the model or terminal is making progress (tokens streaming,
//     status spinners, etc.).
//  3. At least one line in the capture must match the prompt-input regex
//     (typed text sitting after the ❯ cursor).
//  4. NEITHER capture may contain a thinking indicator. If one is
//     present the model is mid-turn and the input line is just queued
//     to be submitted after the turn boundary.
//
// Returns ("", "") when not parked. Returns (true, <reason>) when parked.
// Reason describes the specific signal that classified the session, not
// the whole rule set, so callers can surface it in `gc session doctor`
// output and operators can correlate quickly.
func classifyParkedSession(before, after string, state session.State) (bool, string) {
	if !parkedEligibleState(state) {
		return false, ""
	}
	if before != after {
		return false, ""
	}
	if !promptInputLineRE.MatchString(before) {
		return false, ""
	}
	if containsThinkingIndicator(before) || containsThinkingIndicator(after) {
		return false, ""
	}
	return true, parkedSessionReasonInputPrompt
}

// parkedEligibleState reports whether a session in this state can be
// considered "parked." We only flag states with a live runtime — anything
// asleep/suspended/closed is by definition not waiting on Enter.
func parkedEligibleState(state session.State) bool {
	switch state {
	case session.StateActive, session.StateAwake:
		return true
	default:
		return false
	}
}

// containsThinkingIndicator reports whether s contains any of the
// thinkingIndicatorPatterns. Plain substring match (no regex) because the
// patterns are unique enough that false positives in user input are not a
// realistic concern.
func containsThinkingIndicator(s string) bool {
	for _, pat := range thinkingIndicatorPatterns {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// parkedSessionResult is the per-session record emitted by `gc session
// doctor parked-sessions`. Used both for text output and the --json path.
type parkedSessionResult struct {
	Alias       string `json:"alias"`
	SessionName string `json:"session_name"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
	// Fixed is true when --fix sent Enter to this session and the
	// post-fix re-sample no longer matched the parked rule.
	Fixed bool `json:"fixed,omitempty"`
	// FixSent is true when --fix sent Enter, regardless of whether the
	// follow-up confirmation succeeded.
	FixSent bool `json:"fix_sent,omitempty"`
}

func newSessionDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	var sampleGap time.Duration
	var fix bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Detect sessions parked on unsubmitted input",
		Long: `Detect interactive sessions whose tmux pane has typed-but-unsubmitted
input AND no model-thinking indicator AND the model is idle on the last
turn boundary.

This is the recurring "mayor came back from restart but didn't press
Enter on the queued prompt" failure mode. Takes two pane samples
--sample-gap apart (default 20s); flags sessions whose pane is
byte-identical AND has typed text after the ❯ cursor AND shows no
thinking indicator.

Exit code: 0 if no sessions parked, 2 if at least one is parked.

Use --fix to send Enter to each parked session and re-sample 10s later
to confirm the input was consumed.`,
		Example: `  gc session doctor
  gc session doctor --sample-gap=10s
  gc session doctor --fix
  gc session doctor --json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdSessionDoctor(sampleGap, fix, jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().DurationVar(&sampleGap, "sample-gap", parkedSessionDefaultSampleGap, "wait between the two pane captures used to detect motion")
	cmd.Flags().BoolVar(&fix, "fix", false, "send Enter to each parked session and re-sample to confirm unblock")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "machine-readable JSON output")
	return cmd
}

// cmdSessionDoctor is the CLI entry point. Exits 0 if no sessions parked,
// 2 if at least one is parked (after the optional --fix re-sample).
func cmdSessionDoctor(sampleGap time.Duration, fix, jsonOutput bool, stdout, stderr io.Writer) int {
	if sampleGap <= 0 {
		fmt.Fprintf(stderr, "gc session doctor: --sample-gap must be positive (got %s)\n", sampleGap) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, code := openCityStore(stderr, "gc session doctor")
	if store == nil {
		return code
	}

	providerCtx := loadSessionProviderContext()
	allSessionBeads, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session doctor: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sessionBeads := newSessionBeadSnapshot(allSessionBeads)
	sp := newSessionProviderFromContext(providerCtx, sessionBeads)
	catalog, err := workerSessionCatalogWithConfig("", store, sp, providerCtx.cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc session doctor: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	listResult := catalog.ListFullFromBeads(allSessionBeads, "", "")

	candidates := filterParkedCandidates(listResult.Sessions)
	if len(candidates) == 0 {
		if jsonOutput {
			_ = json.NewEncoder(stdout).Encode([]parkedSessionResult{}) //nolint:errcheck
		} else {
			fmt.Fprintln(stdout, "No live sessions to check.") //nolint:errcheck // best-effort stdout
		}
		return 0
	}

	parked := detectParkedSessions(sp, candidates, sampleGap, stderr)
	if fix && len(parked) > 0 {
		applyParkedSessionFix(sp, parked, stderr)
	}

	if jsonOutput {
		_ = json.NewEncoder(stdout).Encode(parked) //nolint:errcheck
	} else {
		emitParkedSessionText(stdout, parked, fix)
	}

	if len(parked) == 0 {
		return 0
	}
	if fix && allParkedFixed(parked) {
		return 0
	}
	return 2
}

// filterParkedCandidates returns the subset of sessions whose state makes
// them eligible for the parked check (active/awake only).
func filterParkedCandidates(infos []session.Info) []session.Info {
	var out []session.Info
	for _, s := range infos {
		if s.SessionName == "" {
			continue
		}
		if !parkedEligibleState(s.State) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// detectParkedSessions takes two pane captures sampleGap apart and runs
// the classifier on each candidate. Returns the parked subset. Captures
// happen in two phases (all first, then all second) so the time between
// samples is bounded by sampleGap regardless of how many sessions are
// being checked. Capture errors are reported to stderr but treated as
// "not parked" (we can't classify what we can't see).
func detectParkedSessions(sp runtime.Provider, candidates []session.Info, sampleGap time.Duration, stderr io.Writer) []parkedSessionResult {
	type sample struct {
		alive  bool
		before string
		after  string
	}
	samples := make(map[string]*sample, len(candidates))
	deadChecker, _ := sp.(runtime.DeadRuntimeSessionChecker)

	for _, s := range candidates {
		alive := true
		if deadChecker != nil {
			dead, err := deadChecker.IsDeadRuntimeSession(s.SessionName)
			if err != nil {
				fmt.Fprintf(stderr, "gc session doctor: %s liveness check: %v\n", s.SessionName, err) //nolint:errcheck // best-effort stderr
			}
			alive = !dead
		}
		st := &sample{alive: alive}
		if alive {
			before, err := sp.Peek(s.SessionName, parkedSessionPeekLines)
			if err != nil {
				fmt.Fprintf(stderr, "gc session doctor: %s first capture: %v\n", s.SessionName, err) //nolint:errcheck // best-effort stderr
				st.alive = false
			} else {
				st.before = before
			}
		}
		samples[s.SessionName] = st
	}

	time.Sleep(sampleGap)

	var parked []parkedSessionResult
	for _, s := range candidates {
		st := samples[s.SessionName]
		if st == nil || !st.alive {
			continue
		}
		after, err := sp.Peek(s.SessionName, parkedSessionPeekLines)
		if err != nil {
			fmt.Fprintf(stderr, "gc session doctor: %s second capture: %v\n", s.SessionName, err) //nolint:errcheck // best-effort stderr
			continue
		}
		st.after = after
		isParked, reason := classifyParkedSession(st.before, st.after, s.State)
		if !isParked {
			continue
		}
		parked = append(parked, parkedSessionResult{
			Alias:       parkedSessionAlias(s),
			SessionName: s.SessionName,
			State:       string(s.State),
			Reason:      reason,
		})
	}
	return parked
}

// applyParkedSessionFix sends Enter to each parked session and re-samples
// after parkedSessionFixConfirmGap to confirm the input was consumed. The
// re-sample updates each result's Fixed and FixSent flags in place.
func applyParkedSessionFix(sp runtime.Provider, parked []parkedSessionResult, stderr io.Writer) {
	preFix := make([]string, len(parked))
	for i, p := range parked {
		fmt.Fprintf(stderr, "[parked] sending Enter to %s\n", p.Alias) //nolint:errcheck // best-effort stderr
		// Capture the pre-fix pane so we can confirm motion below.
		before, err := sp.Peek(p.SessionName, parkedSessionPeekLines)
		if err != nil {
			fmt.Fprintf(stderr, "gc session doctor: %s pre-fix capture: %v\n", p.SessionName, err) //nolint:errcheck // best-effort stderr
		}
		preFix[i] = before
		if err := sp.SendKeys(p.SessionName, "Enter"); err != nil {
			fmt.Fprintf(stderr, "gc session doctor: %s send Enter: %v\n", p.SessionName, err) //nolint:errcheck // best-effort stderr
			continue
		}
		parked[i].FixSent = true
	}

	time.Sleep(parkedSessionFixConfirmGap)

	for i, p := range parked {
		if !parked[i].FixSent {
			continue
		}
		after, err := sp.Peek(p.SessionName, parkedSessionPeekLines)
		if err != nil {
			fmt.Fprintf(stderr, "gc session doctor: %s post-fix capture: %v\n", p.SessionName, err) //nolint:errcheck // best-effort stderr
			continue
		}
		// "Fixed" = the pane is no longer parked: either the bytes
		// changed (model is now thinking/printing) or the prompt line
		// no longer has unsubmitted input.
		if after != preFix[i] || !promptInputLineRE.MatchString(after) || containsThinkingIndicator(after) {
			parked[i].Fixed = true
		}
	}
}

// allParkedFixed reports whether every parked session was successfully
// unblocked by --fix. Used to flip the exit code back to 0 when the
// remediation path fully cleared the queue.
func allParkedFixed(parked []parkedSessionResult) bool {
	if len(parked) == 0 {
		return true
	}
	for _, p := range parked {
		if !p.Fixed {
			return false
		}
	}
	return true
}

// emitParkedSessionText writes the human-readable summary to stdout.
func emitParkedSessionText(stdout io.Writer, parked []parkedSessionResult, fix bool) {
	if len(parked) == 0 {
		fmt.Fprintln(stdout, "No parked sessions detected.") //nolint:errcheck // best-effort stdout
		return
	}
	for _, p := range parked {
		status := ""
		if fix {
			switch {
			case p.Fixed:
				status = " (fixed)"
			case p.FixSent:
				status = " (fix sent, still parked)"
			default:
				status = " (fix skipped)"
			}
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s%s\n", p.Alias, p.SessionName, p.Reason, status) //nolint:errcheck // best-effort stdout
	}
}

// parkedSessionAlias returns the best human label for a session. Prefers
// the operator-visible alias ("mayor") and falls back to the tmux session
// name when no alias is recorded.
func parkedSessionAlias(s session.Info) string {
	if strings.TrimSpace(s.Alias) != "" {
		return s.Alias
	}
	return s.SessionName
}
