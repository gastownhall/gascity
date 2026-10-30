package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
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
// sessionStart, when non-nil, is the city store and the live managed session
// the caller already proved for this SessionStart; the mail context reuses
// both instead of resolving the city, opening its store, and reading the
// session again.
//
// step, when non-nil, is the step reminder lookup the caller started for
// cityPath; nil reads the reminder here.
func primeHookContextSuffix(cityPath string, sessionStart *primeSessionStart, step *wispStepLookup, hookMode bool, hookContext primeHookContext, stderr io.Writer, consumeHandoff bool) primeHookContextInjection {
	if !hookMode {
		return primeHookContextInjection{}
	}
	// The step reminder's answer is taken last: its lookup has been in flight
	// since the hook started, and the SessionStart mail read below does not
	// depend on it, so reading the mail first overlaps the two instead of
	// starting the read only once the answer is in. The reminder still leads
	// the text.
	var mail primeHookContextInjection
	if primeHookSessionStart(hookContext) {
		var startMail sessionStartMail
		if sessionStart != nil {
			startMail = sessionStartMailInjection(sessionStart.store, sessionStart.liveInfo(), cityPath, stderr)
		} else {
			startMail = sessionStartMailFromCity(stderr)
		}
		mail.text += startMail.autoHandoff.text
		if consumeHandoff {
			mail.afterDelivery = startMail.autoHandoff.afterDelivery
		}
		// dip-bj7pgj: an autonomous/promptless restart runs this SessionStart
		// hook but never the UserPromptSubmit mail hook, so also surface ordinary
		// unread mail here so such a wake is not blind to it (including a
		// priority:1 message that was not sent as an auto-handoff). This block is
		// READ-ONLY — it never archives, so it can never consume/hide a message —
		// and it excludes the auto-handoff messages already rendered above so a
		// beadmail-backed ordinary provider does not double-render them.
		if !startMail.ownSessionClosed {
			mail.text += primeUnreadMailInjectionFromInbox(startMail.autoHandoffIDs, startMail.ordinaryProvider, startMail.inbox)
		}
	}
	return primeHookContextInjection{text: step.contentFor(cityPath) + mail.text, afterDelivery: mail.afterDelivery}
}

// sessionStartMail is the mail a SessionStart hook surfaces: the auto-handoff
// injection, the ids it rendered, the ordinary-mail provider built over the
// same stores, and — when the session's own mailbox was read in one listing —
// that inbox, which the ordinary-mail block reuses instead of reading again.
type sessionStartMail struct {
	autoHandoff      primeHookContextInjection
	autoHandoffIDs   map[string]bool
	ordinaryProvider mail.Provider
	inbox            *beadmail.Inbox
	// ownSessionClosed is set when the hook's own session bead is closed:
	// neither block surfaces mail (refuseClosedHookSession).
	ownSessionClosed bool
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
func primeUnreadMailInjectionWithProvider(skip map[string]bool, mp mail.Provider) string {
	return primeUnreadMailInjectionFromInbox(skip, mp, nil)
}

// primeUnreadMailInjectionFromInbox is primeUnreadMailInjectionWithProvider
// with the session's own inbox already read (nil when it was not); see
// primeUnreadMailMessagesFromInbox.
func primeUnreadMailInjectionFromInbox(skip map[string]bool, mp mail.Provider, inbox *beadmail.Inbox) string {
	messages := primeUnreadMailMessagesFromInbox(mp, inbox)
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
// the configured city mail provider, using the same identity candidates as the
// check path (GC_SESSION_ID/GC_ALIAS/GC_AGENT via defaultMailIdentityCandidates)
// but resolved by the provider's own recipient routing rather than by
// resolveMailTargetsWithConfig — so this reads the union of those candidates,
// not the first-resolving target. It is read-only and returns nil on any error.
// A nil mp opens the configured city mail provider.
func primeUnreadMailMessages(mp mail.Provider) []mail.Message {
	return primeUnreadMailMessagesFromInbox(mp, nil)
}

// primeUnreadMailMessagesFromInbox is primeUnreadMailMessages reusing the
// session's own inbox when the caller read it through the same bead-backed
// provider. The inbox is the session's whole mailbox
// (beadmail.Provider.InboxForSession), so each identity candidate that is one
// of its routes is answered by it, and any other candidate is checked as
// before. Without an inbox, or with a provider that does not read session
// inboxes, every candidate is checked.
func primeUnreadMailMessagesFromInbox(mp mail.Provider, inbox *beadmail.Inbox) []mail.Message {
	if mp == nil {
		mp, _ = openCityMailProvider(io.Discard, "gc prime")
	}
	if mp == nil {
		return nil
	}
	candidates := defaultMailIdentityCandidates()
	var fromInbox []mail.Message
	if _, reads := mp.(beadmail.SessionInboxReader); reads && inbox != nil {
		routes := make(map[string]bool)
		for _, route := range inbox.Routes() {
			routes[route] = true
		}
		var rest []string
		for _, candidate := range candidates {
			if !routes[candidate] {
				rest = append(rest, candidate)
			}
		}
		if len(rest) < len(candidates) {
			fromInbox = inbox.Unread()
			candidates = rest
		}
	}
	checked, err := collectMailMessages(mp.Check, candidates)
	if err != nil {
		return nil
	}
	return mergeMailByArrival(fromInbox, checked)
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
	startMail := sessionStartMailFromCity(stderr)
	return startMail.autoHandoff, startMail.autoHandoffIDs, startMail.ordinaryProvider
}

// sessionStartMailFromCity is sessionStartMailInjection over a freshly opened
// city store, for a caller that holds no store or proven session. The whole
// sessionStartMail is returned, so a closed own session (ownSessionClosed)
// still silences the ordinary-mail block.
func sessionStartMailFromCity(stderr io.Writer) sessionStartMail {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return sessionStartMail{}
	}
	return sessionStartMailInjection(store, nil, cityPath, stderr)
}

// sessionStartAutoHandoffInjectionWithStore is sessionStartAutoHandoffInjection
// over a caller-owned city store opened at cityPath. Class-store routing runs
// exactly as it does for a freshly opened store; only the redundant city
// resolution and store open are skipped.
func sessionStartAutoHandoffInjectionWithStore(store beads.Store, cityPath string, stderr io.Writer) (primeHookContextInjection, map[string]bool, mail.Provider) {
	startMail := sessionStartMailInjection(store, nil, cityPath, stderr)
	return startMail.autoHandoff, startMail.autoHandoffIDs, startMail.ordinaryProvider
}

// sessionStartMailInjection is sessionStartAutoHandoffInjectionWithStore for a
// caller that may already hold the hook's live session (live, nil when it does
// not). The session's mailbox comes off that session rather than a fresh read
// of GC_SESSION_ID, and is read once: its inbox answers the auto-handoff block
// here and is handed back for the ordinary-mail block.
func sessionStartMailInjection(store beads.Store, live *session.Info, cityPath string, stderr io.Writer) sessionStartMail {
	if store == nil {
		return sessionStartMail{}
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
	startMail := sessionStartMail{ordinaryProvider: ordinaryMailProvider}
	// The handoff archive below consumes what it injected, so with message
	// listed it withdraws the handoffs' lane notices, as gc mail check
	// --inject's archive does through openCityMailProvider.
	mp := withBeadMailNoticeWithdrawal(beadmail.NewWithStores(msgStore, sessStore), sessStore, cfg, stderr)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	var (
		target resolvedMailTarget
		err    error
	)
	if live != nil && live.ID == sessionID {
		target, err = mailTargetFromSessionInfo(*live, sessionID)
	} else {
		target, err = resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return startMail
	}
	if err := refuseClosedHookSession(target); err != nil {
		fmt.Fprintf(stderr, "gc prime: %v; surfacing no mail\n", err) //nolint:errcheck // best-effort hook diagnostics
		return sessionStartMail{ownSessionClosed: true}
	}
	var messages []mail.Message
	if target.session != nil {
		inbox, inboxErr := mp.InboxForSession(*target.session)
		if inboxErr != nil {
			fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", inboxErr) //nolint:errcheck // best-effort hook diagnostics
			return startMail
		}
		startMail.inbox = inbox
		messages = inbox.UnreadAutoHandoffs()
	} else {
		messages, err = mp.CheckAutoHandoffs(target.recipients)
		if err != nil {
			fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
			return startMail
		}
	}
	if len(messages) == 0 {
		return startMail
	}
	ids := make(map[string]bool, len(messages))
	for _, m := range messages {
		ids[m.ID] = true
	}
	inbox := startMail.inbox
	startMail.autoHandoffIDs = ids
	startMail.autoHandoff = primeHookContextInjection{
		text: formatInjectOutput(messages),
		afterDelivery: func() {
			// Unless the managing gc hook run would discard the delivered
			// output before the archive finished (hook_deadline.go).
			archiveInjectedAutoHandoffMessagesFrom("gc prime", mp, inbox, selectMailInjectWindow(messages), stderr)
		},
	}
	return startMail
}
