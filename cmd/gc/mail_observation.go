package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// This file holds the verify-only mail doors: `gc mail check`, `inbox` and
// `peek` read a city's mail without starting or recovering its managed store.
// A stopped, stale or unreachable store answers with the typed
// store-unavailable error instead (exit 69); the recovering twins in
// providers.go and cmd_mail.go stay the path of every mutating mail command.

// mailTargetResolverFunc resolves an identifier onto the mailbox it names.
type mailTargetResolverFunc = func(identifier string, cache *mailIdentitySessionCache) (resolvedMailTarget, error)

// openObservationMailProvider is openCityMailProvider for a verify-only
// caller. An unavailable store returns (nil, state, exitStoreUnavailable) with
// nothing printed, so the caller reports it in its own form (plain text, JSON
// envelope, or an injected notice).
func openObservationMailProvider(stderr io.Writer, cmdName string) (mail.Provider, *storeUnavailableError, int) {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newCommandMailProvider(nil), nil, 0
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, nil, 1
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		if refusal := cityConfigLoadRefusal(cityPath, err); refusal != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, refusal) //nolint:errcheck // best-effort stderr
			return nil, nil, 1
		}
		cfg = nil
	}
	store, err := openObservationStoreAt(context.Background(), cityPath, cityPath, cfg)
	if err != nil {
		if unavailable, ok := asStoreUnavailable(observationReadFailure(err, "city")); ok {
			return nil, unavailable, exitStoreUnavailable
		}
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, nil, 1
	}
	msgStore := resolveMailMessagesStore(observationStorageRoutes(cityPath), store, cfg, cityPath, nil)
	sessStore := observationSessionStore(store, cfg, cityPath)
	return withMailNoticeWithdrawal(newMailProviderWithSessionStore(msgStore, sessStore), sessStore, cfg, stderr), nil, 0
}

// mailUnavailableState is the store-unavailable state behind a failed mail
// read, or nil when the failure is something else: an unreachable server and an
// expired deadline count, a missing message does not.
func mailUnavailableState(err error) *storeUnavailableError {
	unavailable, _ := asStoreUnavailable(observationReadFailure(err, "city"))
	return unavailable
}

// reportObservationUnavailable is the answer of `mail inbox`, `mail peek` and
// `nudge status`
// when their store is unavailable: the typed line on stderr and exit 69, plus
// the store_unavailable error envelope on stdout under --json.
func reportObservationUnavailable(cmdName string, unavailable *storeUnavailableError, jsonOut bool, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s: %v\n", cmdName, unavailable) //nolint:errcheck // best-effort stderr
	if jsonOut {
		_ = writeJSONFailure(stdout, "store_unavailable", unavailable.Error(), exitStoreUnavailable) //nolint:errcheck // best-effort stdout
	}
	return exitStoreUnavailable
}

// reportMailCheckUnavailable is reportObservationUnavailable for `mail check`. An
// injecting check never fails its hook: it exits 0 and tells the session, in a
// system reminder, that its mail was not read and why.
func reportMailCheckUnavailable(unavailable *storeUnavailableError, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "gc mail check: %v\n", unavailable) //nolint:errcheck // best-effort stderr
	if !inject {
		return exitStoreUnavailable
	}
	notice := fmt.Sprintf("[mail check degraded — store unavailable: %s scope=%s; mail was not read, run 'gc mail inbox' once the store is back]", unavailable.State, unavailable.Scope)
	_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", "<system-reminder>\n"+notice+"\n</system-reminder>\n") //nolint:errcheck // best-effort stdout
	return 0
}

// lookupObservedMailTarget is lookupMailTargetFromArgs over a verify-only
// store: it never opens the city's store through the recovering funnel.
func lookupObservedMailTarget(args []string) (resolvedMailTarget, error) {
	if len(args) > 0 {
		return lookupMailTargetWith(openObservationMailTargetResolver, resolveObservedStorelessMailTarget, args[0])
	}
	return lookupDefaultMailTargetWith(openObservationMailTargetResolver, resolveObservedStorelessMailTarget)
}

// openObservationMailTargetResolver is openMailTargetResolver over a
// verify-only store and the observation session routes.
func openObservationMailTargetResolver() (mailTargetResolverFunc, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	store, err := openObservationStoreAt(context.Background(), cityPath, cityPath, cfg)
	if err != nil {
		return nil, mailStoreOpenError{err: err}
	}
	sessStore := observationSessionStore(store, cfg, cityPath)
	return func(identifier string, cache *mailIdentitySessionCache) (resolvedMailTarget, error) {
		return resolveMailTargetsVia(resolveObservedSessionID, cityPath, cfg, sessStore, identifier, cache)
	}, nil
}

// resolveObservedStorelessMailTarget is resolveRawMailTargetForStorelessProvider
// over a verify-only store.
func resolveObservedStorelessMailTarget(identifier string) (resolvedMailTarget, error) {
	return resolveRawMailTargetForStorelessProviderWith(openObservationMailTargetStore, observationSessionStore, identifier)
}

// openObservationMailTargetStore opens the city store for a verify-only mail
// target lookup.
func openObservationMailTargetStore() (beads.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	return openObservationStoreAt(context.Background(), cityPath, cityPath, nil)
}

// resolveRawMailTargetForStorelessProviderWith is
// resolveRawMailTargetForStorelessProvider over the given store opener and
// session routing.
func resolveRawMailTargetForStorelessProviderWith(openTarget func() (beads.Store, error), routeSessions func(beads.Store, *config.City, string) beads.Store, identifier string) (resolvedMailTarget, error) {
	asGiven := resolvedMailTarget{display: identifier, recipients: []string{identifier}}
	store, err := openTarget()
	if err != nil {
		if isNoCityStoreError(err) {
			return asGiven, nil
		}
		return resolvedMailTarget{}, err
	}
	if store == nil {
		return asGiven, nil
	}
	cityPath, cfg := ambientMailTargetConfig()
	target, err := resolveMailTargets(routeSessions(store, cfg, cityPath), identifier)
	if errors.Is(err, session.ErrSessionNotFound) {
		return asGiven, nil
	}
	return target, err
}
