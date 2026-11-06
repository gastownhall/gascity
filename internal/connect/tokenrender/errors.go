package tokenrender

import (
	"errors"
	"strings"
)

// Cause is a stable renderer failure cause (DES-CN §3.8). Causes are logged
// and shown by gc doctor; they are never a wire code.
type Cause string

// Renderer failure causes.
const (
	// CauseOperatorUnreadable means an output's operator source is configured
	// but cannot be read or parsed; that output is left as it is.
	CauseOperatorUnreadable Cause = "render_operator_unreadable"
	// CauseRowsUnavailable means the session rows could not be listed; no
	// output is written beyond its seed.
	CauseRowsUnavailable Cause = "render_rows_unavailable"
	// CauseForeignSentinel means an output holds gc lines but not this key's
	// and city's sentinel: another key or city rendered it.
	CauseForeignSentinel Cause = "render_foreign_sentinel"
	// CauseNewerFormat means an output holds a gc line of a newer format: a
	// newer renderer owns it.
	CauseNewerFormat Cause = "render_newer_format"
	// CauseTooLarge means the render exceeds bd serve's 1 MiB cap.
	CauseTooLarge Cause = "render_too_large"
	// CauseWriteFailed means an output could not be read or replaced.
	CauseWriteFailed Cause = "render_write_failed"
	// CauseRowIneligible means an Ensure's row was not rendered: the row is
	// not open, not eligible, or at another generation.
	CauseRowIneligible Cause = "render_row_ineligible"
	// CauseSeedWouldDrop means an output holds no gc line but holds content
	// the seed would drop: more than bd serve's 1 MiB cap (a mis-pointed
	// path), or token lines its operator source does not hold. The file is
	// left as it is; move those lines into the operator source first.
	CauseSeedWouldDrop Cause = "render_seed_would_drop"
)

// Causes lists every renderer cause in §3.8's order.
func Causes() []Cause {
	return []Cause{
		CauseOperatorUnreadable, CauseRowsUnavailable, CauseForeignSentinel,
		CauseNewerFormat, CauseTooLarge, CauseWriteFailed, CauseRowIneligible,
		CauseSeedWouldDrop,
	}
}

// Error is a typed renderer failure. Ledger names the output's ledger when
// the failure is one output's. Detail and Err never carry a key, a bearer,
// the sentinel or an operator token.
type Error struct {
	Cause  Cause
	Ledger string
	Detail string
	Err    error
}

// Sentinel errors for errors.Is: an *Error matches the sentinel of its cause.
var (
	ErrOperatorUnreadable = &Error{Cause: CauseOperatorUnreadable}
	ErrRowsUnavailable    = &Error{Cause: CauseRowsUnavailable}
	ErrForeignSentinel    = &Error{Cause: CauseForeignSentinel}
	ErrNewerFormat        = &Error{Cause: CauseNewerFormat}
	ErrTooLarge           = &Error{Cause: CauseTooLarge}
	ErrWriteFailed        = &Error{Cause: CauseWriteFailed}
	ErrRowIneligible      = &Error{Cause: CauseRowIneligible}
	ErrSeedWouldDrop      = &Error{Cause: CauseSeedWouldDrop}
)

// Error renders "<cause>: ledger <name>: <detail>: <err>", omitting empty parts.
func (e *Error) Error() string {
	parts := []string{string(e.Cause)}
	if e.Ledger != "" {
		parts = append(parts, "ledger "+e.Ledger)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel of e's cause.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Cause == e.Cause && t.Ledger == "" && t.Detail == "" && t.Err == nil
}

// CauseOf returns the cause of the first *Error in err's chain.
func CauseOf(err error) (Cause, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Cause, true
	}
	return "", false
}
