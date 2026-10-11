package api

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gastownhall/gascity/internal/importsvc"
	"github.com/gastownhall/gascity/internal/ssrf"
)

// validateHTTPPackSource is the HTTP-layer SSRF fence for POST /packs. The
// import service shells `git ls-remote <source>` synchronously and documents
// that HTTP callers must validate the source first (importsvc/source.go's
// defaultHeadCommit). A network caller must not be able to (a) point the server
// at arbitrary local filesystem paths or file:// repos, or (b) drive git
// fetches at loopback, private, link-local, or cloud-metadata destinations. The
// CLI is a trusted local caller and keeps its local-path support; this fence is
// only applied on the HTTP path.
//
// Host validation alone is NOT sufficient: a fenced public host can redirect
// the git fetch to an internal target, and git re-resolves the host at fetch
// time (DNS rebinding). The redirect and transport-abuse classes are closed at
// the git subprocess by git.UntrustedRemoteGitConfigArgs (redirects disabled,
// transports constrained) on both the HEAD probe and the packman clone/tags
// fetch. The DNS-rebinding TOCTOU window remains an accepted residual — git
// re-resolves at fetch time and pinning the resolved IP is out of scope — so
// this fence is one layer of defense in depth, not the sole control.
//
// Blocked sources return an ErrInvalidSource so packImportHTTPError maps them to
// 400, and importantly they never reach the packAddImport seam.
func validateHTTPPackSource(source string) error {
	host, local, file := packSourceHost(source)
	switch {
	case file:
		return fmt.Errorf("%w: file:// sources are not permitted over the API", importsvc.ErrInvalidSource)
	case local:
		return fmt.Errorf("%w: local filesystem sources are not permitted over the API; use a remote git URL", importsvc.ErrInvalidSource)
	case host == "":
		return fmt.Errorf("%w: could not determine a host from the pack source", importsvc.ErrInvalidSource)
	}
	return ensurePublicPackSourceHost(host)
}

// packSourceHost classifies an import source and extracts its network host.
// It reports local=true for local filesystem paths and file=true for file://
// sources; for remote git sources it returns the host and local=file=false.
// The remote-source detection mirrors importsvc.isRemoteImportSource.
func packSourceHost(source string) (host string, local, file bool) {
	switch {
	case strings.HasPrefix(source, "file://"):
		return "", false, true
	case strings.HasPrefix(source, "git@"):
		// scp-like syntax: user@host:path — the host ends at the first ':' or '/'.
		rest := strings.TrimPrefix(source, "git@")
		if strings.HasPrefix(rest, "[") {
			// Bracketed IPv6 literal host, e.g. git@[::1]:repo. Take the address
			// between the brackets; scanning for ':' would otherwise cut at the
			// first ':' inside the literal and yield the bogus host "[".
			if end := strings.IndexByte(rest, ']'); end > 1 {
				return rest[1:end], false, false
			}
			return "", false, false
		}
		if i := strings.IndexAny(rest, ":/"); i >= 0 {
			return rest[:i], false, false
		}
		return rest, false, false
	case strings.HasPrefix(source, "ssh://"),
		strings.HasPrefix(source, "https://"),
		strings.HasPrefix(source, "http://"):
		if u, err := url.Parse(source); err == nil {
			return u.Hostname(), false, false
		}
		return "", false, false
	case strings.HasPrefix(source, "github.com/"):
		return "github.com", false, false
	default:
		// Everything else is a local path (//, ~, absolute, or relative), the
		// same set importsvc resolves against the city directory.
		return "", true, false
	}
}

// ensurePublicPackSourceHost rejects a host that names or resolves to an
// internal destination (loopback, private, link-local, unique-local,
// unspecified, or a cloud metadata IP such as 169.254.169.254). It delegates to
// the shared ssrf fence (also used by the rig-clone path) so the two callers
// cannot drift, and maps the fence's outcome onto importsvc.ErrInvalidSource for
// the 400 mapping. A resolution error is not treated as a block, since the
// subsequent git fetch performs its own resolution and will surface the failure.
func ensurePublicPackSourceHost(host string) error {
	err := ssrf.EnsurePublicHost(host)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ssrf.ErrEmptyHost):
		return fmt.Errorf("%w: could not determine a host from the pack source", importsvc.ErrInvalidSource)
	default:
		// Wrap both sentinels so callers can match ErrInvalidSource (for the 400)
		// and ssrf.ErrBlockedHost (the underlying cause) alike.
		return fmt.Errorf("%w: pack source host is blocked: %w", importsvc.ErrInvalidSource, err)
	}
}
