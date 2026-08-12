package gitcred

import (
	"net/url"
	"strings"
)

// RedactUserinfo replaces any userinfo (user or user:password) embedded in an
// http(s)/ssh URL with "***" so a credential-bearing source never reaches a
// log line, error string, or config file. URLs without userinfo are returned
// unchanged, as are strings that do not parse as a URL (scp-form remotes such
// as git@host:org/repo carry their identity in the path and are left intact).
//
// A URL whose userinfo carries a malformed token (an invalid %-escape, a raw
// space, "^", "|", ...) makes url.Parse fail; in that case a string fallback
// still masks the userinfo so the raw secret never survives.
func RedactUserinfo(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return redactUserinfoString(rawURL)
	}
	if u.User == nil {
		return rawURL
	}
	// Rebuild the URL without url.User so the "***" placeholder is not
	// percent-encoded (url.URL.String would render "*" as "%2A").
	u.User = nil
	rebuilt := u.String()
	if i := strings.Index(rebuilt, "://"); i >= 0 {
		return rebuilt[:i+3] + "***@" + rebuilt[i+3:]
	}
	return "***@" + rebuilt
}

// redactUserinfoString masks the userinfo of a URL that url.Parse rejected. It
// replaces everything between "://" and the last "@" that precedes the first
// "/" of the path (the authority-section "@") with "***". A string without a
// scheme separator or an authority "@" is returned unchanged.
func redactUserinfoString(rawURL string) string {
	sep := strings.Index(rawURL, "://")
	if sep < 0 {
		return rawURL
	}
	authStart := sep + 3
	// The userinfo "@" lives in the authority, before the first "/" of the path.
	rest := rawURL[authStart:]
	pathStart := strings.IndexByte(rest, '/')
	authority := rest
	if pathStart >= 0 {
		authority = rest[:pathStart]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return rawURL
	}
	return rawURL[:authStart] + "***" + rawURL[authStart+at:]
}
