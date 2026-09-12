package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// maxPromptSuffixRawBytes and maxPromptSuffixQuotedBytes bound how large a
// rendered prompt may be before promptDelivery refuses to place it in an argv
// suffix/flag. Linux caps a single exec() argv element well below typical
// rendered-prompt sizes (MAX_ARG_STRLEN); crossing it fails the launch with
// E2BIG. The guard trips when EITHER the raw prompt bytes reach
// maxPromptSuffixRawBytes OR the shellquote.Quote-encoded bytes reach
// maxPromptSuffixQuotedBytes, since quoting (each embedded "'" becomes "'\”")
// can inflate size well past the raw length.
const (
	maxPromptSuffixRawBytes    = 100000
	maxPromptSuffixQuotedBytes = 128000
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
	// OversizedFallback reports whether argv delivery (PromptSuffix/PromptFlag)
	// was skipped in favor of the nudge because the prompt crossed
	// maxPromptSuffixRawBytes or maxPromptSuffixQuotedBytes. Distinct from
	// Delivered=false: an oversized-fallback prompt is still delivered, just
	// through a different mechanism than its configured prompt mode would
	// otherwise select.
	OversizedFallback bool
}

// promptDeliverySupport classifies how a runtime can absorb a prompt that is
// too large for argv once promptDelivery has decided the prompt is oversized.
type promptDeliverySupport int

const (
	// promptDeliverySupportUnsupported is the zero value on purpose: a
	// runtime this package cannot positively confirm support for must fail
	// loud on an oversized prompt, never silently fall through to argv.
	promptDeliverySupportUnsupported promptDeliverySupport = iota
	// promptDeliverySupportNudgeFallback means the runtime has a working
	// post-start Nudge path (runtime.Provider.Nudge) an oversized prompt can
	// be rerouted through instead of argv.
	promptDeliverySupportNudgeFallback
	// promptDeliverySupportArgvSafe means the runtime never carries the
	// prompt through a size-limited OS exec() argv in the first place (e.g.
	// it delivers over its own protocol post-launch), so the size guard does
	// not apply to it at all.
	promptDeliverySupportArgvSafe
)

// promptDeliverySupportFor confirms oversized-prompt support from the
// runtime's own identity rather than assuming any given transport (including
// ACP-style protocols) is argv-bound. Only runtimes this package has
// positively classified, plus a pack-declared runtime whose
// [runtimes.<name>] entry opts in via prompt_delivery (packRuntimes, keyed
// by name — see config.DiscoveredRuntime.PromptDelivery), return a
// non-default value; every other name — including future/custom runtimes —
// hard-fails via the zero value.
func promptDeliverySupportFor(runtimeName string, packRuntimes map[string]config.DiscoveredRuntime) promptDeliverySupport {
	name := strings.TrimSpace(runtimeName)
	// The legacy exec spelling constructs the same native t3bridge provider
	// (runtime_registry.go RegisterPrefix("exec:")), so it is argv-safe too.
	if strings.HasPrefix(name, "exec:") && isLegacyT3BridgeExecScript(strings.TrimPrefix(name, "exec:")) {
		return promptDeliverySupportArgvSafe
	}
	switch name {
	// "" is the default session provider: effectiveSessionProvider returns it
	// when neither the agent nor [session] provider is set, and the registry's
	// SetFallback(tmuxFactory) constructs tmux for it.
	case "", "tmux":
		return promptDeliverySupportNudgeFallback
	// herdr launches via exec argv with no shell-arg slot, and already routes
	// the prime through its post-idle delivery path (herdr/provider.go:246).
	case "herdr":
		return promptDeliverySupportNudgeFallback
	// k8s never reads cfg.PromptSuffix and delivers cfg.Nudge post-start
	// (k8s/provider.go:310,409).
	case "k8s":
		return promptDeliverySupportNudgeFallback
	// hybrid routes per session name to tmux or k8s and delegates Nudge to
	// whichever it routed to (hybrid/hybrid.go:97); both legs are nudge-capable.
	case "hybrid":
		return promptDeliverySupportNudgeFallback
	case "t3bridge":
		return promptDeliverySupportArgvSafe
	default:
		if packRuntimes[name].PromptDelivery == config.PromptDeliveryNudgeFallback {
			return promptDeliverySupportNudgeFallback
		}
		return promptDeliverySupportUnsupported
	}
}

// errOversizedPromptUnsupportedRuntime is the sentinel wrapped by the error
// promptDelivery returns when a prompt is oversized and its effective runtime
// has no confirmed post-start delivery path. Callers match it with errors.Is.
var errOversizedPromptUnsupportedRuntime = errors.New("startup prompt exceeds argv-safety threshold for a runtime with no fallback delivery")

// promptDelivery decides how a rendered startup prompt is delivered for a
// session launch. It is the single pure statement of the priming policy that
// was previously duplicated across the launch, ACP, and prompt-mode branches.
//
// Delivery mechanism, in precedence order:
//   - ACP or a "none" prompt-mode provider: prepend the prompt to the nudge.
//     Neither is argv-bound, so the size guard below never applies to them.
//   - flag prompt-mode with a configured flag: pass the prompt via argv suffix
//     plus the provider's prompt flag.
//   - default: pass the prompt as a quoted argv suffix.
//
// Before either argv branch runs, an oversized prompt (see
// maxPromptSuffixRawBytes / maxPromptSuffixQuotedBytes) is routed by
// runtimeName instead: an argv-safe runtime proceeds normally, a
// nudge-fallback runtime reroutes through the nudge (OversizedFallback=true),
// and any other runtime hard-fails with errOversizedPromptUnsupportedRuntime
// before the caller can construct a runtime.Config or call Provider.Start —
// the error names the effective runtime and byte counts/limits, never prompt
// content.
//
// An empty prompt delivers nothing. Judgment about prompt *content* stays in
// templates; this function only routes an already-rendered prompt and reports
// delivered/not-delivered.
//
// packRuntimes is the city's pack-declared runtime registry
// (config.City.Runtimes), consulted only for the oversized-prompt guard's
// promptDeliverySupportFor classification; nil is fine when runtimeName is a
// builtin runtime. promptDelivery remains pure/I-O-free: no provider
// construction, handshake, or subprocess spawn happens here.
func promptDelivery(prompt string, isACP bool, rp *config.ResolvedProvider, nudge string, runtimeName string, packRuntimes map[string]config.DiscoveredRuntime) (promptDeliveryResult, error) {
	res := promptDeliveryResult{Nudge: nudge}
	if prompt == "" {
		return res, nil
	}
	switch {
	case isACP:
		res.Nudge = prependStartupPromptToNudge(prompt, nudge)
		res.Delivered = true
		return res, nil
	case rp != nil && rp.PromptMode == "none":
		res.Nudge = prependStartupPromptToNudge(prompt, nudge)
		res.Delivered = true
		return res, nil
	}

	suffix := shellquote.Quote(prompt)
	if len(prompt) >= maxPromptSuffixRawBytes || len(suffix) >= maxPromptSuffixQuotedBytes {
		switch promptDeliverySupportFor(runtimeName, packRuntimes) {
		case promptDeliverySupportArgvSafe:
			// Falls through to normal argv delivery below: this runtime
			// never carries the prompt through a size-limited argv, so the
			// guard does not apply to it.
		case promptDeliverySupportNudgeFallback:
			res.Nudge = prependStartupPromptToNudge(prompt, nudge)
			res.Delivered = true
			res.OversizedFallback = true
			return res, nil
		default:
			return promptDeliveryResult{}, fmt.Errorf(
				"%w: runtime %q, prompt %d raw bytes / %d argv-encoded bytes (limits: %d raw / %d argv-encoded)",
				errOversizedPromptUnsupportedRuntime, runtimeName, len(prompt), len(suffix),
				maxPromptSuffixRawBytes, maxPromptSuffixQuotedBytes)
		}
	}

	res.PromptSuffix = suffix
	res.Delivered = res.PromptSuffix != ""
	if rp != nil && rp.PromptMode == "flag" {
		if rp.PromptFlag != "" {
			res.PromptFlag = rp.PromptFlag
		} else {
			res.Delivered = false
		}
	}
	return res, nil
}
