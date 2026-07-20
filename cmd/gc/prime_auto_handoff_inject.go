package main

import (
	"fmt"
	"io"
	"os"
	"strings"

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
func primeHookContextSuffix(cityPath string, hookMode bool, hookContext primeHookContext, stderr io.Writer, consumeHandoff bool) primeHookContextInjection {
	if !hookMode {
		return primeHookContextInjection{}
	}
	injection := primeHookContextInjection{text: wispStepInjectionContent(cityPath)}
	if primeHookSessionStart(hookContext) {
		autoHandoff, autoHandoffIDs := sessionStartAutoHandoffInjection(stderr)
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
		injection.text += primeUnreadMailInjection(autoHandoffIDs)
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
	messages := primeUnreadMailMessages()
	if len(skip) > 0 {
		kept := make([]mail.Message, 0, len(messages))
		for _, m := range messages {
			if !skip[m.ID] {
				kept = append(kept, m)
			}
		}
		messages = kept
	}
	if len(messages) == 0 {
		return ""
	}
	return formatInjectOutput(messages)
}

// primeUnreadMailMessages returns the current agent's unread ordinary mail via
// the configured city mail provider, resolving the self-recipient exactly as the
// check path does (GC_SESSION_ID/GC_ALIAS/GC_AGENT via
// defaultMailIdentityCandidates). It is read-only and returns nil on any error.
func primeUnreadMailMessages() []mail.Message {
	mp, _ := openCityMailProvider(io.Discard, "gc prime")
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
// the current managed session, along with the set of auto-handoff message IDs it
// rendered (so the ordinary-unread-mail block can dedup against them). It
// intentionally constructs beadmail directly: gc handoff persists this
// continuation class through beadmail regardless of any separately configured
// ordinary-mail provider.
func sessionStartAutoHandoffInjection(stderr io.Writer) (primeHookContextInjection, map[string]bool) {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}, nil
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	mp := beadmail.NewWithStores(msgStore, sessStore)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil
	}
	messages, err := mp.CheckAutoHandoffs(target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil
	}
	if len(messages) == 0 {
		return primeHookContextInjection{}, nil
	}
	ids := make(map[string]bool, len(messages))
	for _, m := range messages {
		ids[m.ID] = true
	}
	injectedMessages := sortMailByPriority(messages)
	if len(injectedMessages) > mailInjectMaxMessages {
		injectedMessages = injectedMessages[:mailInjectMaxMessages]
	}
	return primeHookContextInjection{
		text: formatInjectOutput(messages),
		afterDelivery: func() {
			archiveInjectedAutoHandoffMessages(mp, injectedMessages, stderr)
		},
	}, ids
}
