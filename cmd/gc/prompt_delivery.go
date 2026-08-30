package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// promptDeliveryResult is the resolved plan for delivering a rendered startup
// prompt to a freshly launched session: which argv suffix / flag / nudge
// carries it, and whether a delivery mechanism was selected for it. This is a
// pure routing decision — no I/O happens here — so Delivered=true means "a
// mechanism exists to carry this prompt," not "the runtime received or the
// agent consumed it."
type promptDeliveryResult struct {
	PromptSuffix string
	PromptFlag   string
	Nudge        string
	// Delivered reports whether the startup prompt reached a first-turn
	// delivery mechanism (routing/selection only). Callers stamp the
	// GC_STARTUP_PROMPT_DELIVERED marker from this so observers can
	// distinguish "primed" from "live but never primed" — but "primed" itself
	// means delivery was selected and attempted for this runtime incarnation,
	// not that the agent consumed or began acting on the prompt; a live
	// worker can still be idle if the provider drops submission after this
	// point. The durable signal that work began is the trigger bead becoming
	// assigned/in-progress (gastownhall/gascity#5236).
	Delivered bool
}

// promptDelivery decides how a rendered startup prompt is delivered for a
// session launch. It is the single pure statement of the priming policy that
// was previously duplicated across the launch, ACP, and prompt-mode branches.
//
// Delivery mechanism, in precedence order:
//   - ACP or a "none" prompt-mode provider: prepend the prompt to the nudge.
//   - flag prompt-mode with a configured flag: pass the prompt via argv suffix
//     plus the provider's prompt flag.
//   - default: pass the prompt as a quoted argv suffix.
//
// An empty prompt delivers nothing. Judgment about prompt *content* stays in
// templates; this function only routes an already-rendered prompt and reports
// delivered/not-delivered.
func promptDelivery(prompt string, isACP bool, rp *config.ResolvedProvider, nudge string) promptDeliveryResult {
	res := promptDeliveryResult{Nudge: nudge}
	if prompt == "" {
		return res
	}
	switch {
	case isACP:
		res.Nudge = prependStartupPromptToNudge(prompt, nudge)
		res.Delivered = true
	case rp != nil && rp.PromptMode == "none":
		res.Nudge = prependStartupPromptToNudge(prompt, nudge)
		res.Delivered = true
	default:
		res.PromptSuffix = shellquote.Quote(prompt)
		res.Delivered = res.PromptSuffix != ""
		if rp != nil && rp.PromptMode == "flag" {
			if rp.PromptFlag != "" {
				res.PromptFlag = rp.PromptFlag
			} else {
				res.Delivered = false
			}
		}
	}
	return res
}
