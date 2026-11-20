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
// family. A grok pane reads its own processing footer relative to its live
// composer (grokTurnRunning) and nothing else, as the codex turn guard reads
// only codex's own indicator (paneShowsRunningTurn): grok never draws the
// other families' indicators, so a transcript row shaped like one (a tool's
// "(2s · 3.1 MB)" reads as Claude's spinner) is not its turn. Every other
// family keeps the shared indicator scan (paneContainsBusyIndicator), so
// grok's footer string in another family's transcript never reads as busy.
func paneBusyReading(lines []string, family string) bool {
	if family == "grok" {
		return grokTurnRunning(lines)
	}
	return paneContainsBusyIndicator(lines)
}

// paneReadingNeedsFamily reports whether the pane's provider family decides
// lines' reading: a shared-scan indicator is busy for every family but grok,
// and trusted only for some (busyReadingTrusted), and grok's footer below its
// live composer is busy only on a grok pane. A capture showing neither reads
// the same for every family, so it needs no family read.
func paneReadingNeedsFamily(lines []string) bool {
	return paneContainsBusyIndicator(lines) || grokTurnRunning(lines)
}
