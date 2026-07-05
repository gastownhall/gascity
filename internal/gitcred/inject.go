package gitcred

import (
	"fmt"
	"os"
	"strings"
)

// osExecutable is the package seam for resolving the running gc binary path.
// Tests stub it to point the deferred git-credential helper at a fixture.
var osExecutable = os.Executable

// Injection is what one network git invocation must add to authenticate. The
// zero value means "run byte-identical to today": no extra argv, no extra env.
type Injection struct {
	// CfgArgs are leading `-c` flags inserted before the git subcommand. For an
	// https match they clear ambient credential helpers and install gc as the
	// helper; for an ssh match they are nil (ssh auth rides Env).
	CfgArgs []string
	// Env is appended to the runner's base environment (HermeticEnv or
	// SanitizedEnv). For https it disables the terminal prompt and passes the
	// non-secret city reference; for ssh it sets GIT_SSH_COMMAND with the key.
	Env []string
	// Matched reports whether a credential rule matched the clone URL. It drives
	// AuthError hint selection.
	Matched bool
	// RuleOrigin is the file (or "$GC_GIT_CREDENTIAL_COMMAND") that supplied the
	// matched rule; "" when unmatched.
	RuleOrigin string
}

// CredentialedNetworkArgs decides, in the parent gc process, whether the
// network git run for cloneURL gets credential injection.
//
//   - gcExe: the gc binary path; "" resolves it via the osExecutable seam.
//   - cityRoot: "" (e.g. gc pack release) skips only the city file layer.
//   - cloneURL: the already-normalized clone URL the seam site has in hand.
//
// It returns a zero Injection and nil error when no credential source exists or
// no rule matches — the byte-identical guarantee. It fails closed (aborting the
// network op rather than degrading to anonymous) on an unparsable rules file,
// bad permissions, bad pointer cardinality, or a matched rule with an
// unresolvable gcExe. Secrets are never read here.
func CredentialedNetworkArgs(gcExe, cityRoot, cloneURL string) (Injection, error) {
	rules, err := Load(cityRoot)
	if err != nil {
		return Injection{}, err
	}

	rule, matched := rules.MatchSource(cloneURL)
	if !matched {
		// No file rule matched. A command-layer fallback still needs gc wired as
		// the helper for http(s) URLs so git consults it; without it, run
		// byte-identical to today.
		if rules.HasCommandLayer() && isHTTPCloneURL(cloneURL) {
			return httpsInjection(gcExe, cityRoot, false, commandLayerOrigin)
		}
		return Injection{}, nil
	}

	if strings.TrimSpace(rule.SSHKeyFile) != "" {
		keyPath := expandUser(rule.SSHKeyFile)
		return Injection{
			Env: []string{
				fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes", shellQuote(keyPath)),
			},
			Matched:    true,
			RuleOrigin: rule.Origin,
		}, nil
	}
	return httpsInjection(gcExe, cityRoot, true, rule.Origin)
}

// httpsInjection builds the credential-helper injection for an http(s) clone.
func httpsInjection(gcExe, cityRoot string, matched bool, origin string) (Injection, error) {
	exe := strings.TrimSpace(gcExe)
	if exe == "" {
		resolved, err := osExecutable()
		if err != nil {
			return Injection{}, fmt.Errorf("resolving gc executable for credential helper: %w", err)
		}
		exe = resolved
	}
	cfg := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!" + shellQuote(exe) + " git-credential",
		"-c", "credential.useHttpPath=true",
	}
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if strings.TrimSpace(cityRoot) != "" {
		env = append(env, EnvCredentialCity+"="+cityRoot)
	}
	return Injection{
		CfgArgs:    cfg,
		Env:        env,
		Matched:    matched,
		RuleOrigin: origin,
	}, nil
}

// isHTTPCloneURL reports whether cloneURL is an http(s) URL the credential
// helper can serve.
func isHTTPCloneURL(cloneURL string) bool {
	_, _, tr := hostPathTransport(cloneURL)
	return tr == transportHTTP
}

// shellQuote single-quotes s for git's "!"-helper, which git runs via sh. A
// single quote inside s is escaped as the standard '\” sequence.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
