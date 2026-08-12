package cliauth

// Wire types for the Gas City Service Protocol v0 (auth + identity surface).
// These are the client's view of the protocol. They are deliberately
// vendor-neutral: no account/commercial field (trial, billing, credit, plan,
// quota, …) ever appears here — such policy travels only in the opaque Message
// and Links fields the CLI prints verbatim (spec §5). The field set is pinned by
// TestWireFieldsAreStableAndVendorNeutral and by check-core-boundary.sh (f).

// meResponse is the GET /gc/v0/me response.
type meResponse struct {
	User    meUser            `json:"user"`
	Session sessionInfo       `json:"session"`
	Message string            `json:"message"`
	Links   map[string]string `json:"links"`
	Error   apiError          `json:"error"`
}

// sessionInfo is display-only session metadata (all optional): the CLI shows it
// so a user can see when their session expires and was last used, and correlate
// it with the server-side session list. The client never parses the token itself.
type sessionInfo struct {
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	LastUsed    string `json:"last_used"`
	Fingerprint string `json:"fingerprint"`
}

// meUser identifies the authenticated account by opaque id/handle only — there
// is no org or tenant field.
type meUser struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

// apiError is the error object non-2xx responses may carry.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// deviceCodeResponse is the POST /gc/v0/auth/device/code response (RFC-8628 shape).
type deviceCodeResponse struct {
	DeviceCode              string   `json:"device_code"`
	UserCode                string   `json:"user_code"`
	VerificationURI         string   `json:"verification_uri"`
	VerificationURIComplete string   `json:"verification_uri_complete"`
	ExpiresIn               int      `json:"expires_in"`
	Interval                int      `json:"interval"`
	Error                   apiError `json:"error"`
}

// deviceTokenResponse is the POST /gc/v0/auth/device/token response.
type deviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	Interval    int    `json:"interval"`
}

// browserLoginResult is the payload the loopback callback delivers from the
// browser sign-in page's URL fragment.
type browserLoginResult struct {
	Token   string `json:"token"`
	Service string `json:"service"`
	State   string `json:"state"`
}
