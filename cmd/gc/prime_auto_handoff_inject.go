package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

type primeHookContextInjection struct {
	text          string
	afterDelivery func()
}

// primeHookContextSuffix builds the single provider-hook context owned by gc
// prime. A managed SessionStart receives durable auto-handoff mail here because
// a recycled successor can otherwise idle before any UserPromptSubmit hook.
//
// consumeHandoff gates only the destructive archive: preview callers (--json)
// still render the exact text the hook would emit, but must not consume the
// durable mail out from under the real SessionStart invocation.
//
// sessionStartStore, when non-nil, is the city store the caller already opened
// (and proved the live managed session against) for this SessionStart; the
// mail context reuses it instead of resolving the city and opening it again.
func primeHookContextSuffix(cityPath string, sessionStartStore beads.Store, hookMode bool, hookContext primeHookContext, stderr io.Writer, consumeHandoff bool) primeHookContextInjection {
	if !hookMode {
		return primeHookContextInjection{}
	}
	injection := primeHookContextInjection{text: wispStepInjectionContent(cityPath)}
	if primeHookSessionStart(hookContext) {
		var (
			autoHandoff          primeHookContextInjection
			autoHandoffIDs       map[string]bool
			ordinaryMailProvider mail.Provider
		)
		if sessionStartStore != nil {
			autoHandoff, autoHandoffIDs, ordinaryMailProvider = sessionStartAutoHandoffInjectionWithStore(sessionStartStore, cityPath, stderr)
		} else {
			autoHandoff, autoHandoffIDs, ordinaryMailProvider = sessionStartAutoHandoffInjection(stderr)
		}
		injection.text += autoHandoff.text
		if consumeHandoff {
			injection.afterDelivery = autoHandoff.afterDelivery
		}
		// dip-bj7pgj: an autonomous/promptless restart runs this SessionStart
		// hook but never the UserPromptSubmit mail hook, so also surface ordinary
		// unread mail here so such a wake is not blind to it (including a
		// priority:1 message that was not sent as an auto-handoff). This block is
		// READ-ONLY — it never archives, so it can never consume/hide a message —
		// and it excludes the auto-handoff messages already rendered above so a
		// beadmail-backed ordinary provider does not double-render them.
		injection.text += primeUnreadMailInjectionWithProvider(autoHandoffIDs, ordinaryMailProvider)
	}
	return injection
}

// primeInjectMailContent returns the unread-mail <system-reminder> block that
// `gc mail check --inject` produces for the current agent, or "" when there is
// no unread mail or the read fails. It is defense-in-depth for the promptless-
// wake gap (gastownhall/gascity dip-bj7pgj): an autonomous/promptless restart
// runs the SessionStart prime hook but NOT the UserPromptSubmit mail hook, so
// without it such a wake starts blind to unread mail — including priority:1
// restart handoffs. It is the standalone (no-exclusion) form of the ordinary-
// mail injection folded into the SessionStart hook context; see
// primeUnreadMailInjection.
func primeInjectMailContent() string {
	return primeUnreadMailInjection(nil)
}

// primeUnreadMailInjection renders the current agent's ordinary unread mail as a
// priority-sorted <system-reminder> block (the same shape the check path emits),
// excluding any message IDs in skip — the auto-handoff messages already rendered
// by sessionStartAutoHandoffInjection, so a beadmail-backed ordinary provider
// does not double-render them. It is READ-ONLY: unlike the check path it never
// archives/mutates mail (so the SessionStart preview cannot consume/hide a
// message), and any error degrades silently to "" so a prime is never blocked.
func primeUnreadMailInjection(skip map[string]bool) string {
	return primeUnreadMailInjectionWithProvider(skip, nil)
}

// primeUnreadMailInjectionWithProvider is primeUnreadMailInjection over a
// caller-supplied ordinary-mail provider. SessionStart passes the provider it
// already built over its open stores; nil opens the configured city provider.
//
// Because this block never archives, every message it renders is a copy with
// ArchivedOnDelivery cleared: an auto-handoff that reaches it (the auto-handoff
// read failed, so skip is nil, or the message is addressed to an identity
// candidate outside the auto-handoff read's recipients) must not be labeled
// "archived on delivery", and the block keeps its 'gc mail inbox' promise.
func primeUnreadMailInjectionWithProvider(skip map[string]bool, mp mail.Provider) string {
	messages := primeUnreadMailMessages(mp)
	kept := make([]mail.Message, 0, len(messages))
	for _, m := range messages {
		if skip[m.ID] {
			continue
		}
		m.ArchivedOnDelivery = false
		kept = append(kept, m)
	}
	if len(kept) == 0 {
		return ""
	}
	return formatInjectOutput(kept)
}

// primeUnreadMailMessages returns the current agent's unread ordinary mail via
// the configured city mail provider, using the same identity candidates as the
// check path (GC_SESSION_ID/GC_ALIAS/GC_AGENT via defaultMailIdentityCandidates)
// but resolved by the provider's own recipient routing rather than by
// resolveMailTargetsWithConfig — so this reads the union of those candidates,
// not the first-resolving target. It is read-only and returns nil on any error.
// A nil mp opens the configured city mail provider.
func primeUnreadMailMessages(mp mail.Provider) []mail.Message {
	if mp == nil {
		mp, _ = openCityMailProvider(io.Discard, "gc prime")
	}
	if mp == nil {
		return nil
	}
	messages, err := collectMailMessages(mp.Check, defaultMailIdentityCandidates())
	if err != nil {
		return nil
	}
	return messages
}

// sessionStartAutoHandoffInjection returns only durable auto-handoff mail for
// the current managed session, the set of auto-handoff message IDs it rendered
// (so the ordinary-unread-mail block can dedup against them), and the
// configured ordinary-mail provider built over the same stores (nil when it
// could not be built, which sends the caller back to the standalone opener). It
// intentionally constructs beadmail directly for the auto-handoff read: gc
// handoff persists this continuation class through beadmail regardless of any
// separately configured ordinary-mail provider.
func sessionStartAutoHandoffInjection(stderr io.Writer) (primeHookContextInjection, map[string]bool, mail.Provider) {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}, nil, nil
	}
	return sessionStartAutoHandoffInjectionWithStore(store, cityPath, stderr)
}

// sessionStartAutoHandoffInjectionWithStore is sessionStartAutoHandoffInjection
// over a caller-owned city store opened at cityPath. Class-store routing runs
// exactly as it does for a freshly opened store; only the redundant city
// resolution and store open are skipped.
func sessionStartAutoHandoffInjectionWithStore(store beads.Store, cityPath string, stderr io.Writer) (primeHookContextInjection, map[string]bool, mail.Provider) {
	if store == nil {
		return primeHookContextInjection{}, nil, nil
	}
	cfg, cfgErr := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	// The ordinary provider is exactly what openCityMailProvider would build:
	// the same name precedence as mailProviderName (GC_MAIL, then [mail]
	// provider), the same class-routed stores, and the cached beadmail. A config
	// that failed to load leaves it nil so the standalone opener decides.
	var ordinaryMailProvider mail.Provider
	if cfgErr == nil && cfg != nil {
		providerName := cfg.Mail.Provider
		if override := os.Getenv("GC_MAIL"); override != "" {
			providerName = override
		}
		ordinaryMailProvider = newMailProviderNamedWithSessionStore(providerName, msgStore, sessStore, true)
	}
	mp := beadmail.NewWithStores(msgStore, sessStore)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	messages, err := mp.CheckAutoHandoffs(target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	if len(messages) == 0 {
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	ids := make(map[string]bool, len(messages))
	for _, m := range messages {
		ids[m.ID] = true
	}
	return primeHookContextInjection{
		text: formatInjectOutput(messages),
		afterDelivery: func() {
			archiveInjectedAutoHandoffMessages(mp, selectMailInjectWindow(messages).allShown(), stderr)
		},
	}, ids, ordinaryMailProvider
}
