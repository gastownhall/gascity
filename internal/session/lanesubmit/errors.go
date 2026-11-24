package lanesubmit

import "github.com/gastownhall/gascity/internal/opjournal"

// KeyError is a client key the journal refuses, or one no op id derives
// from for the session. Nothing was written.
type KeyError struct{ Err error }

// Error returns the refusal's cause.
func (e *KeyError) Error() string { return e.Err.Error() }

// Unwrap returns the journal's refusal.
func (e *KeyError) Unwrap() error { return e.Err }

// MintError is a key that could not be minted. Nothing was written.
type MintError struct{ Err error }

// Error returns the refusal's cause.
func (e *MintError) Error() string { return e.Err.Error() }

// Unwrap returns the minting error.
func (e *MintError) Unwrap() error { return e.Err }

// JournalError is the journal's refusal of the request, its preview or its
// proposal: a key that names a different request (*opjournal.KeyReuseError),
// a payload it refuses, or a write that failed.
type JournalError struct{ Err error }

// Error returns the refusal's cause.
func (e *JournalError) Error() string { return e.Err.Error() }

// Unwrap returns the journal's refusal.
func (e *JournalError) Unwrap() error { return e.Err }

// ReadError is a read of the op the key names, ID, that failed. Nothing was
// written.
type ReadError struct {
	ID  string
	Err error
}

// Error returns the refusal's cause.
func (e *ReadError) Error() string { return e.Err.Error() }

// Unwrap returns the read's error.
func (e *ReadError) Unwrap() error { return e.Err }

// KindError is a key that names Op, an op of another kind. Nothing was
// written.
type KindError struct{ Op opjournal.Op }

// Error names the op the key names and its kind.
func (e *KindError) Error() string {
	return "the key names a " + string(e.Op.Kind) + " op, " + e.Op.ID
}

// ResumeError is the session's refusal, Err, of the request's resume: before
// anything was written (Op nil), or once Op was written or found. An owned
// op was canceled first: Cancel and CancelErr are what the cancel answered.
type ResumeError struct {
	Session   string
	Err       error
	Op        *opjournal.Op
	Owned     bool
	Cancel    opjournal.CancelResult
	CancelErr error
}

// Error returns the refusal's cause.
func (e *ResumeError) Error() string { return e.Err.Error() }

// Unwrap returns the session's refusal.
func (e *ResumeError) Unwrap() error { return e.Err }

// Canceled reports whether the cancel closed the owned op, so it delivers
// nothing.
func (e *ResumeError) Canceled() bool {
	return e.Owned && e.CancelErr == nil && e.Cancel == opjournal.CancelClosed
}
