package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/gitcred"
)

// Clone scheme-allowlist sentinels. Each is errors.Is-matchable so a caller
// (the rig-add API) can map it to a 400 invalid_git_url with a caller-safe
// reason. They are the transport-layer half of the G15 hardening: they block the
// primitives an attacker-supplied URL opens (arbitrary-command ext:: transports,
// local-file exfil) before git ever runs.
var (
	// ErrSchemeExt rejects the ext:: transport (and any other "<word>::"
	// transport-helper form). ext::<cmd> runs an arbitrary command as the gc
	// user — the highest-severity RCE primitive a clone URL can open.
	ErrSchemeExt = errors.New("ext:: transport is not permitted")
	// ErrSchemeFile rejects file:// sources, which would read server-local repos
	// into a rig the caller can then pull back — a local-filesystem exfil.
	ErrSchemeFile = errors.New("file:// sources are not permitted")
	// ErrBareLocalPath rejects a bare local path (/abs, ./rel, ~, or a
	// scheme-less shorthand) for the same exfil reason as file://.
	ErrBareLocalPath = errors.New("local filesystem paths are not permitted; use an https URL")
	// ErrSchemeInsecure rejects http:// and git://: plaintext transports whose
	// credentials travel in the clear and whose redirects trivially retarget an
	// internal host.
	ErrSchemeInsecure = errors.New("http:// and git:// are not permitted; use https")
	// ErrSchemeSSHNotEnabled rejects ssh:// and scp-form remotes when the caller
	// did not opt in via CloneOptions.AllowSSH.
	ErrSchemeSSHNotEnabled = errors.New("ssh sources are not enabled for this city")
	// ErrSchemeUnsupported is the fail-closed default for any other scheme
	// (ftp://, gopher://, an unknown "<scheme>://"): not on the allowlist, so it
	// is refused.
	ErrSchemeUnsupported = errors.New("git URL scheme is not permitted; use https")
	// ErrUnparseableURL rejects a URL net/url cannot parse. Failing closed here
	// prevents a string git might reinterpret as a local path or an option from
	// reaching the subprocess.
	ErrUnparseableURL = errors.New("git URL could not be parsed")
	// ErrHostLeadingDash rejects an ssh/scp remote whose host begins with "-".
	// Older git (pre-2.14.1, CVE-2017-1000117) passed such a host to ssh as an
	// option (e.g. -oProxyCommand=...), an argument-injection RCE. Refused even
	// under AllowSSH as defense in depth over modern git's own guard.
	ErrHostLeadingDash = errors.New("ssh host may not begin with '-'")
)

// CloneOptions tunes a hardened clone. The zero value is the safe default:
// https-only, submodules off, redirects refused, no credential injection.
type CloneOptions struct {
	// AllowSSH additionally permits ssh:// and scp-form (git@host:path) URLs.
	// Off by default; auth then rides Cred.Env (GIT_SSH_COMMAND), never the URL.
	AllowSSH bool
	// RecurseSubmodules opts back into submodule fetch. Off by default: a
	// submodule URL is a second untrusted-URL surface the pre-fetch SSRF fence
	// never saw, and .gitmodules can point at ext::/file:// (already fenced by
	// protocol.allow=never, but still a second network fan-out).
	RecurseSubmodules bool
	// Depth, when >0, passes --depth for a shallow clone.
	Depth int
	// Branch, when set, passes --branch to clone a single branch.
	Branch string
	// Cred is optional per-city credential injection (leading -c flags and env).
	// The zero value injects nothing and the clone runs anonymously; a populated
	// value keeps the secret in env/helper so the URL carries no userinfo.
	Cred gitcred.Injection
}

// cloneRunner executes the assembled clone argv with the assembled env. It is a
// package var so tests can capture the exact command and env without spawning
// git; production uses defaultCloneRunner.
var cloneRunner = defaultCloneRunner

// Clone performs a hardened `git clone <url> <dst>` into a dst the caller owns.
// url is validated against the scheme allowlist before git runs; a rejected
// scheme returns one of the Err* sentinels and NO subprocess is spawned. The
// caller MUST have already run the SSRF host fence (internal/ssrf.EnsurePublicHost)
// — Clone re-asserts the scheme guard fail-closed but does not itself resolve
// DNS. All hardening rides argv (-c overrides) and an env built on HermeticEnv;
// Clone never runs a shell.
//
// ctx bounds the clone: a WAN fetch can exceed ordinary session timeouts, so the
// caller threads its own watchdog-anchored deadline and exec cancellation
// follows it. Embedded userinfo (https://user:token@host) is tolerated but never
// persisted: every error string is rendered through gitcred.RedactUserinfo and
// any embedded password is scrubbed from git's own output.
func Clone(ctx context.Context, url, dst string, opts CloneOptions) error {
	redacted := gitcred.RedactUserinfo(url)
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("cloning %s: destination path is empty", redacted)
	}
	if err := classifyCloneScheme(url, opts.AllowSSH); err != nil {
		return fmt.Errorf("cloning %s: %w", redacted, err)
	}
	args := assembleCloneArgs(url, dst, opts)
	env := cloneEnv(opts)
	if err := cloneRunner(ctx, args, env); err != nil {
		return fmt.Errorf("cloning %s: %w", redacted, scrubCloneError(err, url))
	}
	return nil
}

// classifyCloneScheme applies the G15 scheme allowlist. It returns nil for an
// allowed URL (https, or ssh/scp when allowSSH) and one of the Err* sentinels
// otherwise. It fails closed: any form it does not positively recognize as safe
// is rejected.
func classifyCloneScheme(raw string, allowSSH bool) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ErrBareLocalPath
	}
	// 1. "<transport>::<addr>" smart-transport helper (ext::, fd::, foo::).
	//    Checked before url.Parse because these are not parseable URL schemes.
	if isTransportHelperForm(trimmed) {
		return ErrSchemeExt
	}
	// 2. file: scheme (case-insensitive), ANY slash count — file://, file:///,
	//    and the single-slash file:/path form all denote local-file access.
	//    Checked before the scp-form step so "file:/etc/x" is not misread as an
	//    ssh remote to a host literally named "file".
	if len(trimmed) >= 5 && strings.EqualFold(trimmed[:5], "file:") {
		return ErrSchemeFile
	}
	// 3. scp-form (user@host:path) — a valid ssh remote with no "://".
	if isSCPForm(trimmed) {
		if scpHostLeadingDash(trimmed) {
			return ErrHostLeadingDash
		}
		if allowSSH {
			return nil
		}
		return ErrSchemeSSHNotEnabled
	}
	// 4. everything else must parse as a URL and carry an allowed scheme.
	u, err := url.Parse(trimmed)
	if err != nil {
		return ErrUnparseableURL
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "ssh":
		if strings.HasPrefix(u.Hostname(), "-") {
			return ErrHostLeadingDash
		}
		if allowSSH {
			return nil
		}
		return ErrSchemeSSHNotEnabled
	case "http", "git":
		return ErrSchemeInsecure
	case "":
		// A scheme-less string that reached here is a bare local path or a
		// scheme-less shorthand (github.com/o/r) that would resolve locally.
		return ErrBareLocalPath
	default:
		return ErrSchemeUnsupported
	}
}

// isTransportHelperForm reports whether s begins with a "<scheme>::" smart
// transport helper. It is a stricter, anchored check than a bare
// strings.Contains("::") so a bracketed IPv6 literal (git@[::1]:repo,
// https://[::1]/r) whose "::" lives inside the address is NOT misread as a
// helper: the run of characters before the first "::" must be a valid,
// standalone URL-scheme token (alpha, then alnum/+/-/.).
func isTransportHelperForm(s string) bool {
	i := strings.Index(s, "::")
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if j == 0 {
			if !alpha {
				return false
			}
			continue
		}
		alnum := alpha || (c >= '0' && c <= '9')
		if !alnum && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// isSCPForm reports whether s is git's scp-like remote syntax
// (user@host:path, host:path). git recognizes it only when there is no "://"
// and the first ":" is not preceded by a "/", so "./rel:path" stays a local
// path. The "<scheme>::" helper family is already handled before this call.
func isSCPForm(s string) bool {
	if strings.Contains(s, "://") {
		return false
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash < 0 || colon < slash
}

// scpHostLeadingDash reports whether the ssh authority of an scp-form remote
// ([user@]host:path) begins with "-" — the CVE-2017-1000117 option-smuggling
// vector. It flags a dash on either the full pre-":" authority (e.g.
// "-oProxyCommand=x@host") or the post-"@" host (e.g. "user@-host"), since git
// may hand either to ssh as an argument.
func scpHostLeadingDash(s string) bool {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return false
	}
	authority := s[:colon]
	if strings.HasPrefix(authority, "-") {
		return true
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		return strings.HasPrefix(authority[at+1:], "-")
	}
	return false
}

// assembleCloneArgs builds the full argv: the hardening -c overrides, the
// credential injection's -c flags, then the clone subcommand with a "--"
// terminator so a URL beginning with "-" can never be parsed as an option.
func assembleCloneArgs(url, dst string, opts CloneOptions) []string {
	args := rigCloneHardeningArgs(opts)
	args = append(args, opts.Cred.CfgArgs...)
	args = append(args, "clone")
	if !opts.RecurseSubmodules {
		args = append(args, "--no-recurse-submodules")
	}
	if opts.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(opts.Depth))
	}
	if b := strings.TrimSpace(opts.Branch); b != "" {
		args = append(args, "--branch", b)
	}
	args = append(args, "--", url, dst)
	return args
}

// rigCloneHardeningArgs returns the leading `git -c` overrides that harden the
// rig clone. It is the stricter sibling of UntrustedRemoteGitConfigArgs: file
// and ext transports are DENIED (the pack path allows file:// for CLI-local
// packs; the rig path must not), and redirects are refused so a fenced public
// host cannot bounce the fetch to an internal target after the SSRF check.
func rigCloneHardeningArgs(opts CloneOptions) []string {
	args := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if opts.AllowSSH {
		args = append(args, "-c", "protocol.ssh.allow=always")
	}
	args = append(args,
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=never",
		"-c", "http.followRedirects=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
	)
	if !opts.RecurseSubmodules {
		args = append(args, "-c", "submodule.recurse=false")
	}
	return args
}

// cloneEnv builds the clone process environment on HermeticEnv (which strips
// repo-discovery vars and pins GIT_CONFIG_NOSYSTEM=1 / GIT_CONFIG_GLOBAL=/dev/null
// so no system or user git config rewrites the URL or leaks a credential), then
// adds the prompt/askpass/LFS knobs and finally the credential injection's env.
func cloneEnv(opts CloneOptions) []string {
	env := append(HermeticEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
	env = append(env, opts.Cred.Env...)
	return env
}

// defaultCloneRunner runs `git <args>` with env and returns a combined-output
// error. It never runs a shell (argv-only, mirroring runCtx).
func defaultCloneRunner(ctx context.Context, args, env []string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// scrubCloneError removes any credential embedded in rawURL from a clone error's
// message. git echoes the remote URL in transport failures, so even though the
// error is already prefixed with a redacted URL, the subprocess text itself
// could still carry the raw user:password. This masks the password substring so
// no secret survives into a log, event, or returned error.
func scrubCloneError(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	scrubbed := msg
	if u, parseErr := url.Parse(strings.TrimSpace(rawURL)); parseErr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			scrubbed = strings.ReplaceAll(scrubbed, pw, "***")
		}
		// Mask the whole userinfo token too, in case git echoed "user:pass@host".
		if userinfo := u.User.String(); userinfo != "" {
			scrubbed = strings.ReplaceAll(scrubbed, userinfo, "***")
		}
	} else if userinfo := rawURLUserinfo(strings.TrimSpace(rawURL)); userinfo != "" {
		// url.Parse rejected the credential URL (e.g. an invalid %-escape or a raw
		// space in the userinfo) but git may still have echoed the raw token. Mask
		// the substring extracted directly from the URL so no secret survives — the
		// fail-open path this replaces leaked it into the returned error.
		scrubbed = strings.ReplaceAll(scrubbed, userinfo, "***")
	}
	if scrubbed == msg {
		return err
	}
	return errors.New(scrubbed)
}

// rawURLUserinfo extracts the authority userinfo ("user:pass") from a URL string
// even when url.Parse rejects it, so a malformed credential URL can still be
// scrubbed from an error message. It returns "" when there is no scheme
// separator or no authority "@".
func rawURLUserinfo(rawURL string) string {
	sep := strings.Index(rawURL, "://")
	if sep < 0 {
		return ""
	}
	authority := rawURL[sep+3:]
	if slash := strings.IndexByte(authority, '/'); slash >= 0 {
		authority = authority[:slash]
	}
	at := strings.LastIndexByte(authority, '@')
	if at <= 0 {
		return ""
	}
	return authority[:at]
}
