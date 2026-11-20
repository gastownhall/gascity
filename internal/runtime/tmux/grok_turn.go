package tmux

import (
	"slices"
	"strings"
)

// grokBusyHint is the exact footer string Grok Build's TUI shows while it
// processes a turn (and only then): "Ctrl+c:cancel". The match is exact and
// case-sensitive, so a near miss in another family's chrome (ctrl+c,
// CTRL+C, "Ctrl+C to cancel") never reads as grok's footer.
const grokBusyHint = "Ctrl+c:cancel"

// grokTurnRunning reports whether lines, the bottom of a Grok Build pane,
// show a turn in progress. Grok's live composer is a boxed "│ ❯ … │" row:
// the bottom-most line matching DefaultReadyPromptPrefix with its leading box
// border stripped (grok configures no prefix of its own). While grok works,
// that composer stays on screen and a footer carrying grokBusyHint sits
// strictly below it, so the pane is busy exactly when such a row exists
// below the composer — a hint above the live composer is transcript text an
// earlier turn printed and never counts. With no composer on screen the hint
// counts anywhere, mirroring paneShowsRunningTurn's codex fallback.
func grokTurnRunning(lines []string) bool {
	composer := bottomPromptLine(lines, DefaultReadyPromptPrefix)
	if composer < 0 {
		return slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, grokBusyHint)
		})
	}
	return slices.ContainsFunc(lines[composer+1:], func(line string) bool {
		return strings.Contains(line, grokBusyHint)
	})
}

// paneBusyReading is the leaf's busy reading for a pane of provider family
// family: the shared indicator scan every family gets
// (paneContainsBusyIndicator), plus grok's processing footer read relative to
// its live composer (grokTurnRunning). Only a grok pane consults the grok
// reader; every other family keeps the shared scan alone, so grok's footer
// string in another family's transcript never reads as busy.
func paneBusyReading(lines []string, family string) bool {
	return paneContainsBusyIndicator(lines) || (family == "grok" && grokTurnRunning(lines))
}
