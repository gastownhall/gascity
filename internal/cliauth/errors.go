package cliauth

import "fmt"

// AuthErrorKind classifies a rejected protocol request so the CLI can advise
// correctly: a bad/expired session (re-login), an authenticated-but-forbidden
// action (do NOT re-login), or a retryable server failure. This split exists so
// a caller lacking a scope for some action never loops on "run gc login".
type AuthErrorKind int

const (
	// KindUnauthenticated is a 401 (or an invalid_token body): the session is
	// missing/expired/invalid — re-login is the remedy.
	KindUnauthenticated AuthErrorKind = iota
	// KindForbidden is a 403: authenticated but not permitted for this action —
	// re-login will not help; surface the server message.
	KindForbidden
	// KindServerError is a 5xx: a server-side failure, retryable.
	KindServerError
	// KindOther is any other non-2xx.
	KindOther
)

// AuthError is a classified non-2xx protocol response. The server-authored
// Message (when present) is printed verbatim.
type AuthError struct {
	Kind    AuthErrorKind
	Status  int
	Code    string
	Message string
	Action  string // what the client was doing, e.g. "checking login"
}

func (e *AuthError) Error() string {
	switch {
	case e.Message != "":
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Action, e.Message, e.Status)
	case e.Code != "":
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Action, e.Code, e.Status)
	default:
		return fmt.Sprintf("%s: HTTP %d", e.Action, e.Status)
	}
}

// Unauthenticated reports whether re-login is the right remedy (a 401 or an
// invalid_token body).
func (e *AuthError) Unauthenticated() bool { return e.Kind == KindUnauthenticated }

// newAuthError classifies an HTTP status plus an optional error body. A 401, or
// any status carrying the well-known invalid_token code, is unauthenticated; a
// 403 is forbidden; 5xx is a server error.
func newAuthError(action string, status int, code, message string) *AuthError {
	kind := KindOther
	switch {
	case status == 401 || code == "invalid_token":
		kind = KindUnauthenticated
	case status == 403:
		kind = KindForbidden
	case status >= 500:
		kind = KindServerError
	}
	return &AuthError{Kind: kind, Status: status, Code: code, Message: message, Action: action}
}
