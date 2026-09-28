package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/credentialprovider"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/spf13/cobra"
)

const hostedBeadsCredentialSubcommand = "internal beads-credential"

// hostedBeadsCredentialExecutable is a seam for the command projection's
// executable lookup. Production uses os.Executable, while tests can prove the
// absolute-path and shell-quoting contract without invoking a real binary.
var hostedBeadsCredentialExecutable = os.Executable

// hostedBeadsCredentialCommand returns the command bd runs to mint a hosted
// Beads credential. The executable is made absolute before shell quoting so a
// child process cannot resolve a different gc through its inherited PATH, and
// paths containing shell metacharacters remain one argv element.
func hostedBeadsCredentialCommand() (string, error) {
	executable, err := hostedBeadsCredentialExecutable()
	if err != nil {
		return "", fmt.Errorf("resolving the running gc executable for the credential provider: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		return "", errors.New("resolving the running gc executable for the credential provider: empty path")
	}
	if !filepath.IsAbs(executable) {
		executable, err = filepath.Abs(executable)
		if err != nil {
			return "", fmt.Errorf("resolving the running gc executable to an absolute path: %w", err)
		}
	}
	return shellquote.Quote(executable) + " " + hostedBeadsCredentialSubcommand, nil
}

var hostedBeadsCredentialCache = credentialprovider.NewCache()

type hostedBeadsCredentialEnvelope struct {
	Token               string `json:"token"`
	ExpirationTimestamp string `json:"expirationTimestamp"`
}

func newInternalBeadsCredentialCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "beads-credential",
		Short:  "Mint a hosted Beads credential",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			envelope, err := mintHostedBeadsCredential(cmd.Context())
			if err != nil {
				fmt.Fprintf(stderr, "gc internal beads-credential: %v\n", err) //nolint:errcheck
				return errExit
			}
			encoded, err := json.Marshal(envelope)
			if err != nil {
				fmt.Fprintln(stderr, "gc internal beads-credential: encoding credential") //nolint:errcheck
				return errExit
			}
			if _, err := fmt.Fprintln(stdout, string(encoded)); err != nil {
				return err
			}
			return nil
		},
	}
}

func mintHostedBeadsCredential(ctx context.Context) (hostedBeadsCredentialEnvelope, error) {
	argv, err := registryCredentialProviderArgv()
	if err != nil {
		return hostedBeadsCredentialEnvelope{}, err
	}
	provider, err := credentialprovider.New(argv)
	if err != nil {
		return hostedBeadsCredentialEnvelope{}, err
	}
	credential, err := hostedBeadsCredentialCache.Mint(ctx, provider, credentialprovider.Request{
		Audience: "beads",
		// The bd credential-command protocol carries no operation intent, so the
		// bridge cannot request a narrower read-only or write-only credential.
		RequiredScopes: []string{"beads:read", "beads:write"},
	})
	if err != nil {
		return hostedBeadsCredentialEnvelope{}, err
	}
	return hostedBeadsCredentialEnvelope{
		Token:               credential.AccessToken,
		ExpirationTimestamp: credential.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}
