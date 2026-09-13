package gitcred

import (
	"net/url"
	"strings"

	"github.com/gastownhall/gascity/internal/remotesource"
)

// transport classifies a clone URL so the matcher can gate rule pointer types
// against the URL shape.
type transport int

const (
	transportUnsupported transport = iota // file://, local paths — never match
	transportHTTP                         // plaintext http:// — token rules, but NOT the https-only built-in default
	transportHTTPS                        // https:// — token rules and the https-only built-in default
	transportSSH                          // ssh://, scp-form git@host: — ssh_key_file rules
)

// Match selects the credential rule for a host + repo path, assuming secure
// https transport. Precedence is layer-order across layers (an earlier layer
// beats a later one regardless of prefix length) and longest path-prefix within
// a layer. Host comparison is case-insensitive; path prefixes match on
// "/"-segment boundaries with the trailing ".git" stripped from the repo
// segment. Because Match cannot see the request protocol, callers that serve
// live git-credential requests must use MatchRequest so the built-in HTTPS-only
// github.com default is withheld from plaintext http.
func (r *Rules) Match(host, path string) (LoadedRule, bool) {
	return r.matchTransport(host, path, transportHTTPS)
}

// MatchRequest resolves the credential rule for a parsed git-credential request,
// classifying transport from req.Protocol so the built-in HTTPS-only github.com
// default is never served over plaintext http. Only "https" (case-insensitive)
// counts as secure transport; every other protocol value, including "http", is
// treated as insecure.
func (r *Rules) MatchRequest(req Request) (LoadedRule, bool) {
	return r.matchTransport(req.Host, req.Path, transportForProtocol(req.Protocol))
}

// transportForProtocol maps a git-credential "protocol" value to its transport
// class. Only "https" is secure http transport; "http" and any other value are
// treated as insecure so HTTPS-only rules are not served over plaintext.
func transportForProtocol(protocol string) transport {
	if strings.EqualFold(strings.TrimSpace(protocol), "https") {
		return transportHTTPS
	}
	return transportHTTP
}

// MatchSource parses source into a clone URL, extracts host + path, and returns
// the matching rule under the full transport gate: http(s) URLs match only
// helper/token_file/token_env rules; ssh/scp-form URLs match only ssh_key_file
// rules; file:// and local sources never match.
func (r *Rules) MatchSource(source string) (LoadedRule, bool) {
	cloneURL := remotesource.Parse(source).CloneURL
	host, path, tr := hostPathTransport(cloneURL)
	if tr == transportUnsupported {
		return LoadedRule{}, false
	}
	return r.matchTransport(host, path, tr)
}

// matchTransport applies layer-order-outer / longest-prefix-inner selection
// with a transport-compatibility gate. Rules incompatible with tr are skipped
// and matching continues to the next-best candidate.
func (r *Rules) matchTransport(host, path string, tr transport) (LoadedRule, bool) {
	host = stripHostPort(strings.ToLower(strings.TrimSpace(host)))
	if host == "" {
		return LoadedRule{}, false
	}
	candidatePath := normalizeMatchPath(path)
	for _, lyr := range r.layers {
		best := LoadedRule{}
		bestLen := -1
		for _, rule := range lyr.rules {
			if !transportCompatible(rule.Rule, tr) {
				continue
			}
			// The built-in ambient github.com token is an https convenience; never
			// serve it over plaintext http, where the bearer token would travel in
			// cleartext. File/command rules leave httpsOnly false and are unaffected.
			if rule.httpsOnly && tr != transportHTTPS {
				continue
			}
			mHost, mPath := splitMatch(rule.Match)
			if mHost != host {
				continue
			}
			if !pathPrefixMatches(candidatePath, mPath) {
				continue
			}
			if len(mPath) > bestLen {
				best = rule
				bestLen = len(mPath)
			}
		}
		if bestLen >= 0 {
			return best, true
		}
	}
	return LoadedRule{}, false
}

// transportCompatible reports whether rule may serve a URL of transport tr. A
// token-style rule (helper/token_file/token_env) serves both http and https; an
// ssh_key_file rule serves ssh. The plaintext-vs-secure http distinction is
// enforced separately, per-rule, via the httpsOnly gate in matchTransport.
func transportCompatible(rule Rule, tr transport) bool {
	if strings.TrimSpace(rule.SSHKeyFile) != "" {
		return tr == transportSSH
	}
	return tr == transportHTTP || tr == transportHTTPS
}

// splitMatch splits a normalized rule match ("host" or "host/path-prefix") into
// its lowercased host and path-prefix. The trailing "/*" or "/" is already
// stripped at authoring time, but strip defensively here too.
func splitMatch(match string) (host, path string) {
	match = strings.TrimSpace(match)
	match = strings.TrimSuffix(match, "/*")
	match = strings.TrimSuffix(match, "/")
	if i := strings.IndexByte(match, '/'); i >= 0 {
		return stripHostPort(strings.ToLower(match[:i])), normalizeMatchPath(match[i+1:])
	}
	return stripHostPort(strings.ToLower(match)), ""
}

// stripHostPort removes a trailing ":port" from a host so a credential rule is
// matched host-scoped, not port-scoped. Git supplies the request host WITH the
// port under credential.useHttpPath=true, while the parent's URL parse strips
// it via url.Hostname; normalizing both sides here keeps a bare-host rule
// matching both. An IPv6 literal ("[::1]" or "[::1]:8443") keeps its bracketed
// address; only the port suffix after the closing bracket is dropped.
func stripHostPort(host string) string {
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: strip only a ":port" that follows the closing bracket.
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[:end+1]
		}
		return host
	}
	// A bare IPv6 address (multiple colons, no brackets) has no port; leave it.
	if strings.Count(host, ":") > 1 {
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// normalizeMatchPath trims leading/trailing slashes and strips a trailing
// ".git" on the final segment so "github.com/org/repo.git" and
// "github.com/org/repo" match the same rule.
func normalizeMatchPath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	// Lower-case the path so host/path matching is case-insensitive on both
	// sides (the candidate URL path and the rule match are normalized the same
	// way). GitHub org/repo names are case-insensitive; treating the whole path
	// this way keeps a "GitHub.com/Org" rule matching a "github.com/org" URL.
	path = strings.ToLower(path)
	segments := strings.Split(path, "/")
	last := len(segments) - 1
	segments[last] = strings.TrimSuffix(segments[last], ".git")
	return strings.Join(segments, "/")
}

// pathPrefixMatches reports whether prefix matches candidate on "/"-segment
// boundaries. An empty prefix (host-only rule) matches any path.
func pathPrefixMatches(candidate, prefix string) bool {
	if prefix == "" {
		return true
	}
	if candidate == prefix {
		return true
	}
	return strings.HasPrefix(candidate, prefix+"/")
}

// hostPathTransport extracts the host, repo path, and transport class from a
// clone URL. It handles http(s)/ssh URL forms and the scp-form
// "git@host:org/repo" shape remotesource leaves intact.
func hostPathTransport(cloneURL string) (host, path string, tr transport) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return "", "", transportUnsupported
	}
	if strings.HasPrefix(cloneURL, "file://") || !strings.Contains(cloneURL, "://") && !isSCPForm(cloneURL) {
		return "", "", transportUnsupported
	}
	if scpHost, scpPath, ok := parseSCPForm(cloneURL); ok {
		return scpHost, scpPath, transportSSH
	}
	u, err := url.Parse(cloneURL)
	if err != nil {
		return "", "", transportUnsupported
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Hostname(), strings.Trim(u.Path, "/"), transportHTTPS
	case "http":
		return u.Hostname(), strings.Trim(u.Path, "/"), transportHTTP
	case "ssh":
		return u.Hostname(), strings.Trim(u.Path, "/"), transportSSH
	default:
		return "", "", transportUnsupported
	}
}

// isSCPForm reports whether s looks like an scp-form remote (user@host:path)
// rather than a bare local path.
func isSCPForm(s string) bool {
	_, _, ok := parseSCPForm(s)
	return ok
}

// parseSCPForm parses the scp-form "user@host:org/repo" remote shape. It
// requires an "@", a ":" after the host, and rejects absolute local paths and
// URL-scheme strings.
func parseSCPForm(s string) (host, path string, ok bool) {
	if strings.Contains(s, "://") {
		return "", "", false
	}
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return "", "", false
	}
	rest := s[at+1:]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return "", "", false
	}
	host = rest[:colon]
	path = strings.Trim(rest[colon+1:], "/")
	if host == "" {
		return "", "", false
	}
	return host, path, true
}
