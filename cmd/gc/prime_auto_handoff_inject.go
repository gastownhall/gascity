package main

import (
	"fmt"
	"io"
	"os"
	"strings"

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
		autoHandoff := sessionStartAutoHandoffInjection(stderr)
		injection.text += autoHandoff.text
		if consumeHandoff {
			injection.afterDelivery = autoHandoff.afterDelivery
		}
	}
	return injection
}

// sessionStartAutoHandoffInjection returns only durable auto-handoff mail for
// the current managed session. It intentionally constructs beadmail directly:
// gc handoff persists this continuation class through beadmail regardless of
// any separately configured ordinary-mail provider.
func sessionStartAutoHandoffInjection(stderr io.Writer) primeHookContextInjection {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	mp := beadmail.NewWithStores(msgStore, sessStore)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}
	}
	messages, err := mp.CheckAutoHandoffs(target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}
	}
	if len(messages) == 0 {
		return primeHookContextInjection{}
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
	}
}
