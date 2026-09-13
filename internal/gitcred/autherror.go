// Package gitcred resolves git credentials for private pack imports and injects
// them into the network git seams via git's native credential-helper protocol.
package gitcred

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// AuthError is a network git failure classified as an authentication or
// authorization problem. Its Error() is a terse single line; the multi-line
// remediation hint lives in the CLI, so API error bodies stay single-line while
// errors.As still finds the type through the wrapping chains.
type AuthError struct {
	// Host is the remote host, e.g. "github.com".
	Host string
	// OrgPrefix is "host/first-path-segment", e.g. "github.com/gascity"; it
	// drives the CLI hint's suggested credential match.
	OrgPrefix string
	// Repo is the redacted clone URL.
	Repo string
	// Matched reports whether a credential rule matched (rejected credential vs
	// missing credential).
	Matched bool
	// RuleOrigin is the file (or command-layer marker) that supplied the matched
	// rule; "" when unmatched.
	RuleOrigin string
	// Output is the trimmed git stderr line that triggered classification.
	Output string
	// Err is the underlying error (the *exec.ExitError).
	Err error
}

// Error returns a terse single-line description. The multi-line remediation
// hint is printed by the CLI, not here.
func (e *AuthError) Error() string {
	if e.Matched {
		return fmt.Sprintf("authentication failed for %s (credential rule from %s was rejected): %s",
			e.Host, e.RuleOrigin, e.Output)
	}
	return fmt.Sprintf("authentication required for %s (no credential rule matches %s): %s",
		e.Host, e.Repo, e.Output)
}

// Unwrap exposes the underlying error for errors.Is/As chains.
func (e *AuthError) Unwrap() error { return e.Err }

// authTriggerSubstrings are the git stderr fragments (all on exit 128) that
// mark an authentication or authorization failure.
var authTriggerSubstrings = []string{
	"could not read Username",
	"could not read Password",
	"terminal prompts disabled",
	"Authentication failed for",
	"Invalid username or",
	"The requested URL returned error: 401",
	"The requested URL returned error: 403",
	"Permission denied (publickey",
	"Host key verification failed",
}

// ClassifyAuthError returns an *AuthError when (out, err) is an authentication
// failure, or nil otherwise so the caller falls back to its existing wrap. The
// gate is an *exec.ExitError with ExitCode() == 128. "Repository not found" is
// classified only when a rule matched (a private-repo 404 that a valid token
// would resolve); unmatched it is indistinguishable from a typo and stays
// untyped.
func ClassifyAuthError(cloneURL string, inj Injection, out string, err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		return nil
	}
	triggered := false
	for _, sub := range authTriggerSubstrings {
		if strings.Contains(out, sub) {
			triggered = true
			break
		}
	}
	if !triggered && inj.Matched && strings.Contains(out, "Repository not found") {
		triggered = true
	}
	if !triggered {
		return nil
	}
	host, path, _ := hostPathTransport(cloneURL)
	return &AuthError{
		Host:       host,
		OrgPrefix:  orgPrefix(host, path),
		Repo:       RedactUserinfo(cloneURL),
		Matched:    inj.Matched,
		RuleOrigin: inj.RuleOrigin,
		Output:     triggerLine(out),
		Err:        err,
	}
}

// orgPrefix returns "host/first-path-segment", or just the host when the path
// has no segments.
func orgPrefix(host, path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return host
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return host + "/" + path[:i]
	}
	return host + "/" + path
}

// triggerLine returns the first non-empty stderr line matching a trigger, or
// the last non-empty line as a fallback, for a terse single-line message.
func triggerLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		for _, sub := range authTriggerSubstrings {
			if strings.Contains(line, sub) {
				return line
			}
		}
		if strings.Contains(line, "Repository not found") {
			return line
		}
	}
	return last
}
