package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/spf13/cobra"
)

// newGitCredentialCmd builds the hidden `gc git-credential <operation>` helper
// git invokes when credential injection wires gc as the credential helper. The
// operation is a positional arg (not a subcommand) because git calls
// "<helper> get|store|erase" and the protocol requires unknown operations be
// ignored.
func newGitCredentialCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:          "git-credential <operation>",
		Short:        "Git credential helper (invoked by git, not directly)",
		Hidden:       true,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runGitCredential(args[0], cmd.InOrStdin(), stdout, stderr); err != nil {
				return errExit
			}
			return nil
		},
	}
}

// runGitCredential implements the git-credential protocol for the "get"
// operation and drains-and-ignores every other operation. It resolves the
// matching credential from the gitcred rules (re-loaded here using
// GC_CREDENTIAL_CITY for the city layer) and writes it to stdout. It never logs
// the request or the credential, and its errors never contain a secret.
func runGitCredential(op string, stdin io.Reader, stdout, stderr io.Writer) error {
	// Always drain stdin first: git writes the request unconditionally, and an
	// unread pipe risks SIGPIPE on git's side.
	req, err := gitcred.ReadRequest(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "gc git-credential: reading request: %v\n", err) //nolint:errcheck
		return err
	}
	if op != "get" {
		// store/erase/future ops: protocol no-op after the drain.
		return nil
	}

	cityRoot := strings.TrimSpace(os.Getenv(gitcred.EnvCredentialCity))
	rules, err := gitcred.Load(cityRoot)
	if err != nil {
		// The injection side already matched, so a broken rules file must fail
		// the clone loudly rather than silently degrade to anonymous.
		fmt.Fprintf(stderr, "gc git-credential: %v\n", err) //nolint:errcheck
		return err
	}

	rule, matched := rules.Match(req.Host, req.Path)
	if !matched {
		if !rules.HasCommandLayer() {
			// Protocol decline: zero output, exit 0. git then fails under
			// GIT_TERMINAL_PROMPT=0, which is the intended outcome.
			return nil
		}
		cred, ok, cmdErr := gitcred.RunCredentialCommand(os.Getenv(gitcred.EnvCredentialCommand), req)
		if cmdErr != nil {
			fmt.Fprintf(stderr, "gc git-credential: %v\n", cmdErr) //nolint:errcheck
			return cmdErr
		}
		if !ok {
			return nil // command declined
		}
		return gitcred.WriteCredential(stdout, cred)
	}

	if strings.TrimSpace(rule.SSHKeyFile) != "" {
		// ssh rules are served via GIT_SSH_COMMAND on the injection side, not the
		// credential helper — decline silently here.
		return nil
	}

	cred, err := gitcred.Resolve(rule.Rule)
	if err != nil {
		fmt.Fprintf(stderr, "gc git-credential: resolving credential for %s: %v\n", rule.Match, err) //nolint:errcheck
		return err
	}
	return gitcred.WriteCredential(stdout, cred)
}
