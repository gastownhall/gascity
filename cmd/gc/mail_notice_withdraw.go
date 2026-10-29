package main

// Mail notice withdrawal (design slice 7a, Rule 3 of
// conversation_lane_produce.go): with message listed in [ops] authoritative,
// a mail notice is a lane op whose causation is its message. Once the
// message is read, marked read, archived or deleted, its open notices say
// nothing the recipient does not know, so the mail provider cancels them,
// best effort, after the mutation succeeds. An archive of a message already
// archived, or already gone, cancels them again, so a cancel that failed is
// retried as a repeated read retries it. A notice already started keeps the
// cancel as a request (opjournal.Store.Cancel); the lane decides.
//
// The scope is stated, not universal: the provider gc mail opens
// (openCityMailProvider), the API's (newCityMailProvider), and the
// SessionStart handoff archive (sessionStartMailInjection). A message
// consumed any other way (an exec mail provider, a direct bd edit, another
// binary, a read that lands before the notice is proposed) can leave its
// notice open, and the agent then finds no new mail. Parity with #5321's
// delivery-time check is partial until the lane has a delivery-time check
// of its own.

import (
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// mailNoticeWithdrawReason is the reason a withdrawn mail notice's cancel
// records.
const mailNoticeWithdrawReason = "the mail it announces was read or archived"

// withMailNoticeWithdrawal makes a read, mark-read, archive or delete of a
// message through p cancel the open lane notices it caused, when cfg's [ops]
// authoritative lists message; otherwise it returns p unchanged. sessions is
// the sessions-class store the notices live in. Failures are logged to warn
// and never fail the mail operation.
//
// The city's bead mail provider is returned as itself with the withdrawal
// registered as its consumed hook (beadmail.Provider.OnConsumed), so every
// capability its readers assert (beadmail.SessionInboxReader, the handoff
// archives, mail.DedupSender, mail.MultiRecipientInboxer) stays visible and
// its handoff archives withdraw too. Any other provider is wrapped, keeping
// its optional mail.DedupSender and mail.MultiRecipientInboxer.
func withMailNoticeWithdrawal(p mail.Provider, sessions beads.Store, cfg *config.City, warn io.Writer) mail.Provider {
	if p == nil || isNilBeadStore(sessions) || !cliLaneExecutesMessage(cfg) {
		return p
	}
	if bp, ok := p.(*beadmail.Provider); ok {
		return withBeadMailNoticeWithdrawal(bp, sessions, cfg, warn)
	}
	w := &mailNoticeWithdrawer{Provider: p, sessions: sessions, warn: warn}
	dedup, isDedup := p.(mail.DedupSender)
	inboxer, isInboxer := p.(mail.MultiRecipientInboxer)
	switch {
	case isDedup && isInboxer:
		return struct {
			*mailNoticeWithdrawer
			mail.DedupSender
			mail.MultiRecipientInboxer
		}{w, dedup, inboxer}
	case isDedup:
		return struct {
			*mailNoticeWithdrawer
			mail.DedupSender
		}{w, dedup}
	case isInboxer:
		return struct {
			*mailNoticeWithdrawer
			mail.MultiRecipientInboxer
		}{w, inboxer}
	}
	return w
}

// withBeadMailNoticeWithdrawal is withMailNoticeWithdrawal for a caller that
// keeps calling the bead mail provider's own methods, such as the SessionStart
// handoff archive: it registers the withdrawal as bp's consumed hook when cfg's
// [ops] authoritative lists message, and returns bp.
func withBeadMailNoticeWithdrawal(bp *beadmail.Provider, sessions beads.Store, cfg *config.City, warn io.Writer) *beadmail.Provider {
	if bp == nil || isNilBeadStore(sessions) || !cliLaneExecutesMessage(cfg) {
		return bp
	}
	w := &mailNoticeWithdrawer{Provider: bp, sessions: sessions, warn: warn}
	bp.OnConsumed(w.withdraw)
	return bp
}

// mailNoticeWithdrawer is a mail.Provider that withdraws a message's lane
// notices after the message is read or archived. It wraps providers other
// than beadmail's, which call withdraw from their consumed hook instead.
type mailNoticeWithdrawer struct {
	mail.Provider
	sessions beads.Store
	warn     io.Writer
}

// Read implements mail.Provider.
func (w *mailNoticeWithdrawer) Read(id string) (mail.Message, error) {
	m, err := w.Provider.Read(id)
	if err == nil {
		w.withdraw(id)
	}
	return m, err
}

// MarkRead implements mail.Provider.
func (w *mailNoticeWithdrawer) MarkRead(id string) error {
	err := w.Provider.MarkRead(id)
	if err == nil {
		w.withdraw(id)
	}
	return err
}

// Archive implements mail.Provider.
func (w *mailNoticeWithdrawer) Archive(id string) error {
	err := w.Provider.Archive(id)
	if archivedAfter(err) {
		w.withdraw(id)
	}
	return err
}

// Delete implements mail.Provider.
func (w *mailNoticeWithdrawer) Delete(id string) error {
	err := w.Provider.Delete(id)
	if archivedAfter(err) {
		w.withdraw(id)
	}
	return err
}

// archivedAfter reports whether an archive or delete that answered err left
// its message archived: it succeeded, or the message already was
// (mail.ErrAlreadyArchived), so a withdrawal that failed after the first
// archive is retried, as a repeated read retries it.
func archivedAfter(err error) bool {
	return err == nil || errors.Is(err, mail.ErrAlreadyArchived)
}

// ArchiveMany implements mail.Provider.
func (w *mailNoticeWithdrawer) ArchiveMany(ids []string) ([]mail.ArchiveResult, error) {
	results, err := w.Provider.ArchiveMany(ids)
	w.withdrawSucceeded(results)
	return results, err
}

// DeleteMany implements mail.Provider.
func (w *mailNoticeWithdrawer) DeleteMany(ids []string) ([]mail.ArchiveResult, error) {
	results, err := w.Provider.DeleteMany(ids)
	w.withdrawSucceeded(results)
	return results, err
}

// withdrawSucceeded withdraws the notices of every message results reports
// archived.
func (w *mailNoticeWithdrawer) withdrawSucceeded(results []mail.ArchiveResult) {
	for _, r := range results {
		if archivedAfter(r.Err) && r.ID != "" {
			w.withdraw(r.ID)
		}
	}
}

// withdraw cancels the open lane notices message id caused, best effort.
func (w *mailNoticeWithdrawer) withdraw(id string) {
	if id == "" {
		return
	}
	st, err := opjournal.NewStore(beads.SessionStore{Store: w.sessions}, clock.Real{})
	if err != nil {
		w.warnf("opening the op journal to withdraw the notices of %s: %v", id, err)
		return
	}
	ops, err := st.OpenByCausation(id)
	if err != nil {
		w.warnf("listing the notices of %s: %v", id, err)
		return
	}
	for _, op := range ops {
		if op.Producer.Principal != mailNoticePrincipal {
			continue
		}
		if _, _, err := st.Cancel(op.ID, mailNoticePrincipal, mailNoticeWithdrawReason); err != nil {
			w.warnf("withdrawing notice %s of %s: %v", op.ID, id, err)
		}
	}
}

func (w *mailNoticeWithdrawer) warnf(format string, args ...any) {
	if w.warn != nil {
		fmt.Fprintf(w.warn, "gc mail: warning: "+format+"\n", args...) //nolint:errcheck // best-effort warning
		return
	}
	log.Printf("gc mail: "+format, args...)
}
