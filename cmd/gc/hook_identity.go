package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// Exact hook identity (engdocs/design/worker-hook-request-budget.md, "Exact
// hook identity").
//
// A session-side hook knows its own session bead id: template resolution
// projects it as GC_SESSION_ID. One Get of that id is the whole identity — the
// session's mailbox routes and the addresses it sends from are fields of the
// bead. Hooks used to start from GC_ALIAS instead, probe it as a bead id (a
// guaranteed miss) and then search the ledger for the bead the id would have
// named. When GC_SESSION_ID is unset or names no session bead, callers fall
// back to that legacy resolution, unchanged.

// hookIdentity is the calling session, resolved from GC_SESSION_ID with one Get.
type hookIdentity struct {
	info session.Info
}

// resolveHookIdentity resolves GC_SESSION_ID against the session store. It
// reports false, with no error, when the variable is unset or names no session
// bead, so the caller runs its legacy resolution; a store error is returned.
func resolveHookIdentity(sessStore beads.Store) (hookIdentity, bool, error) {
	id := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if _, reserved := reservedMailSenderIdentity(id); reserved {
		return hookIdentity{}, false, nil
	}
	info, ok, err := resolveExactSessionInfo(sessStore, id)
	if err != nil || !ok {
		return hookIdentity{}, false, err
	}
	return hookIdentity{info: info}, true, nil
}

// mailboxAddress is the address the session sends mail from: its alias, else
// its bead id, else its session name.
func (h hookIdentity) mailboxAddress() string {
	return session.MailboxAddressFromInfo(h.info)
}

// pinSender hands the resolved session to a mail provider that can take it,
// so sending from the session's own address does not resolve it again.
func (h hookIdentity) pinSender(mp mail.Provider) {
	if pinner, ok := mp.(mailSenderPinner); ok {
		pinner.PinSender(h.info)
	}
}

// mailSenderPinner is the optional mail-provider capability of taking the
// sending session pre-resolved (beadmail.Provider.PinSender).
type mailSenderPinner interface {
	PinSender(session.Info)
}

// resolveExactSessionInfo resolves identifier as a session bead id with one
// Get — the first step every session resolution takes. It reports false, with
// no error, for an identifier that is blank, a template target, or not the id
// of a session bead.
func resolveExactSessionInfo(sessStore beads.Store, identifier string) (session.Info, bool, error) {
	if sessStore == nil || strings.TrimSpace(identifier) == "" {
		return session.Info{}, false, nil
	}
	if isTemplateTarget(identifier) {
		return session.Info{}, false, nil
	}
	info, _, err := session.ResolveSessionRecordByExactID(sessStore, identifier)
	switch {
	case err == nil:
		return info, true, nil
	case errors.Is(err, session.ErrSessionNotFound):
		return session.Info{}, false, nil
	default:
		return session.Info{}, false, err
	}
}

// mailTargetFromSessionInfo is the mailbox of a resolved session: its
// addresses for display and per-recipient providers, and its full route set
// for an inbox read that takes the routes as given.
func mailTargetFromSessionInfo(info session.Info, identifier string) (resolvedMailTarget, error) {
	addresses := session.MailboxAddressesFromInfo(info)
	if len(addresses) == 0 {
		return resolvedMailTarget{}, errNoMailboxIdentity(identifier)
	}
	return resolvedMailTarget{
		display:    addresses[0],
		recipients: addresses,
		routes:     session.RecipientRoutesFromInfo(info),
	}, nil
}

// errNoMailboxIdentity is the error for a resolved session that publishes no
// mailbox address.
func errNoMailboxIdentity(identifier string) error {
	return fmt.Errorf("session %q has no mailbox identity", identifier)
}
