package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
	"github.com/spf13/cobra"
)

// credentialApplyProcedure is the single statement of what changing a
// credential requires. Help text, the report footer and the docs page all read
// from here so they cannot drift apart.
const credentialApplyProcedure = `Changing a credential does not apply itself. A credential change moves no
config fingerprint, so no agent restarts on its own, and the supervisor
resolves session environment from its own environment, fixed when it exec'd.
Applying a new value means: write it where the supervisor reads it, regenerate
the service file so the supervisor re-execs with it, then cycle the agents.
Until the supervisor re-execs, every running session keeps the old credential.`

// newProviderCredentialsCmd builds `gc provider credentials <provider>`:
// report which environment variable holds each of a provider's credentials,
// and what stands between changing it and the fleet using it.
//
// The command is read-only. Writing the credential is deliberately not
// offered: the value has to reach the supervisor's environment, and on that
// path a write can report success while the fleet gets the old key or a blank
// one — the supervisor forwards only names on its own allow-list, a value
// exported in the shell that regenerates the service file wins over the file,
// the effective variable is agent-scoped once an upstream is selected, and
// several deployment shapes never read the file at all. This command surfaces
// each of those instead of walking into them.
func newProviderCredentialsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials <provider>",
		Short: "Show which environment variable holds a provider's credentials",
		Long: `Report which environment variable holds each of a provider's credentials, and
what stands between changing it and the fleet using it.

Which variable that is, is not obvious. A provider declares its credential
env-var names through its upstream_env binding (api_key and auth_token; never
base_url), those names resolve through the provider's inheritance chain, and
each one's value may interpolate a different variable again. This command
performs that resolution and reports, naming the reason, wherever no single
variable holds the credential.

It also reports what would stop a change from taking effect: a variable the
supervisor does not forward into its service environment, a later config layer
that overrides the credential for particular agents, and whether this city's
supervisor reads the machine-local secrets file at all.

` + credentialApplyProcedure,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if code := runProviderCredentials(args[0], stdout, stderr); code != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

// runProviderCredentials resolves the provider's credential bindings and
// prints them. It returns a process exit code.
func runProviderCredentials(providerName string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	resolved, err := resolveProviderForCredentials(cfg, providerName)
	if err != nil {
		fmt.Fprintf(stderr, "gc provider credentials: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	bindings := providerCredentialSources(resolved)
	if len(bindings) == 0 {
		fmt.Fprintf(stderr, "gc provider credentials: provider %q declares no upstream_env.api_key or upstream_env.auth_token binding, so which of its env keys holds a credential is not stated anywhere; declare one before changing a credential\n", providerName) //nolint:errcheck // best-effort stderr
		return 1
	}

	secretsPath := supervisorSecretsEnvFilePath()
	fileEntries, fileErr := readSecretsEnvForReport(secretsPath)

	renderCredentialBindings(stdout, providerName, resolved, bindings, secretsPath, fileEntries, fileErr)
	renderCredentialOverrides(stdout, credentialOverrides(cfg, providerName, bindings))
	renderCredentialApplyHint(stdout, secretsPath)
	return 0
}

// resolveProviderForCredentials resolves the provider the way session start
// does.
//
// Session start goes through [config.ResolveProvider] -> lookupProvider, which
// layers a city entry over a same-named built-in even when the entry declares
// no `base` (the Phase A legacy shape). The eager ResolvedProviders cache does
// NOT do that merge — chain.go's walkFromLeaf returns a base-less spec as-is —
// so reading the cache would report no upstream_env binding for
// `[providers.claude]` written without a `base` line, and refuse to resolve a
// credential the fleet demonstrably uses. resolved_cache.go's own validation
// path does the same-name merge; this sides with validation and session start.
func resolveProviderForCredentials(cfg *config.City, providerName string) (*config.ResolvedProvider, error) {
	if _, declared := cfg.Providers[providerName]; !declared {
		if _, isBuiltin := config.BuiltinProviders()[providerName]; !isBuiltin {
			return nil, fmt.Errorf("no provider %q in this city's config", providerName)
		}
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
	if err != nil {
		return nil, fmt.Errorf("resolving provider %q: %w", providerName, err)
	}
	return resolved, nil
}

// readSecretsEnvForReport reads the machine-local secrets file. A missing file
// is the normal case and yields no entries and no error. Any other failure —
// an unreadable root-owned file, a malformed line — is returned, because
// reporting "not set here" for a file that names the variable but cannot be
// parsed sends the operator to change a value that is already there.
func readSecretsEnvForReport(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	entries, err := processenv.ParseEnvFile(string(data))
	if err != nil {
		// The supervisor's own reader gives up on the whole file when it does
		// not parse, dropping every entry rather than the bad line, so this is
		// a fleet-wide condition and not a cosmetic one.
		return nil, fmt.Errorf("%s does not parse as dotenv, so the supervisor drops every entry in it: %w", path, err)
	}
	return entries, nil
}

// renderCredentialBindings prints one line per declared credential role.
func renderCredentialBindings(w io.Writer, providerName string, resolved *config.ResolvedProvider, bindings []credentialBinding, secretsPath string, fileEntries map[string]string, fileErr error) {
	fmt.Fprintf(w, "Provider: %s\n", providerName) //nolint:errcheck // best-effort stdout
	if len(resolved.Chain) > 1 {
		hops := make([]string, 0, len(resolved.Chain))
		for _, h := range resolved.Chain {
			if h.Kind == "builtin" {
				hops = append(hops, config.BasePrefixBuiltin+h.Name)
				continue
			}
			hops = append(hops, "providers."+h.Name)
		}
		fmt.Fprintf(w, "  chain: %s\n", strings.Join(hops, " → ")) //nolint:errcheck // best-effort stdout
	}
	if fileErr != nil {
		fmt.Fprintf(w, "  WARNING: %v\n", fileErr) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintln(w) //nolint:errcheck // best-effort stdout

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tHARNESS VAR\tHELD BY\tNOTES") //nolint:errcheck // best-effort stdout
	for _, b := range bindings {
		if b.Refusal != "" {
			fmt.Fprintf(tw, "%s\t%s\t-\t%s\n", b.Role, b.EnvKey, b.Refusal) //nolint:errcheck // best-effort stdout
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t$%s\t%s\n", b.Role, b.EnvKey, b.SourceVar, //nolint:errcheck // best-effort stdout
			strings.Join(credentialSourceNotes(b, secretsPath, fileEntries, fileErr), "; "))
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
}

// credentialSourceNotes describes where a source variable's value comes from
// and what would stop a change to it from reaching the fleet.
func credentialSourceNotes(b credentialBinding, secretsPath string, fileEntries map[string]string, fileErr error) []string {
	var notes []string
	if b.Inherited {
		notes = append(notes, "not set by provider config; taken from the supervisor environment under its own name")
	}

	// The supervisor's service file only carries names that clear its persist
	// gate. A variable that does not is dropped when the service file is
	// regenerated, and session expansion of "${VAR}" then yields "" — so the
	// fleet comes up with a BLANK credential, not the old one.
	if !shouldPersistSupervisorEnv(b.SourceVar) {
		notes = append(notes, fmt.Sprintf(
			"the supervisor does NOT forward %s into its service environment, so a value placed there is dropped and sessions start with a BLANK credential; opt it in via GC_SUPERVISOR_ENV=%s, or bind the credential to a name the supervisor recognizes",
			b.SourceVar, b.SourceVar))
	}

	_, inFile := fileEntries[b.SourceVar]
	inShell := os.Getenv(b.SourceVar) != ""
	switch {
	case fileErr != nil:
		notes = append(notes, "cannot tell whether "+secretsPath+" sets it — see the warning above")
	case inFile:
		notes = append(notes, "set in "+secretsPath)
	default:
		notes = append(notes, "not set in "+secretsPath)
	}
	if inShell {
		notes = append(notes, "exported in this shell")
	}
	return notes
}

// renderCredentialOverrides reports config layers that override a credential
// key after the provider layer, which makes the effective variable
// agent-scoped rather than provider-scoped.
func renderCredentialOverrides(w io.Writer, overrides []credentialOverride) {
	if len(overrides) == 0 {
		return
	}
	fmt.Fprintf(w, "\nAgent-scoped overrides — for these agents the credential above is NOT the one in use:\n") //nolint:errcheck // best-effort stdout
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ENV KEY\tOVERRIDDEN BY\tWHERE") //nolint:errcheck // best-effort stdout
	for _, o := range overrides {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", o.EnvKey, o.Layer, o.Detail) //nolint:errcheck // best-effort stdout
	}
	tw.Flush()                                                                                                                                                                         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "  Session start merges workspace < provider < agent env and injects the selected\n  upstream's serving env last, so those layers win. Resolve those per agent.\n") //nolint:errcheck // best-effort stdout
}

// renderCredentialApplyHint states what applying a new credential requires,
// naming this city's actual precondition rather than assuming the supervisor
// is gc-managed and reads the secrets file.
func renderCredentialApplyHint(w io.Writer, secretsPath string) {
	fmt.Fprintf(w, "\n%s\n", credentialApplyProcedure) //nolint:errcheck // best-effort stdout

	if delegation, delegated, err := supervisorSystemdDelegation(); err == nil && delegated {
		fmt.Fprintf(w, delegatedSupervisorNote, delegation.Unit, delegation.Scope, secretsPath) //nolint:errcheck // best-effort stdout
		return
	}
	fmt.Fprintf(w, managedSupervisorNote, secretsPath) //nolint:errcheck // best-effort stdout
}

// delegatedSupervisorNote explains that a delegated supervisor never reads the
// machine-local secrets file, so the usual procedure does not apply.
const delegatedSupervisorNote = `
This city delegates its supervisor to systemd unit %q (%s scope), so gc
generates no service file and %s is never read. Put the value where that
unit's environment comes from, then restart the unit yourself.
`

// managedSupervisorNote is the procedure for a supervisor gc owns, including
// the two conditions that make a change silently not take.
const managedSupervisorNote = `
For a gc-managed supervisor, %s is the machine-local source it merges when the
service file is regenerated:

  gc supervisor install    # regenerate the service file with the new value
  gc supervisor stop
  gc supervisor start      # re-exec so the new environment takes
  gc restart               # cycle the agents onto it

The stop is not optional: "gc supervisor start" regenerates the service file
but returns without replacing a supervisor that is already alive, so the
running process keeps its old environment. "gc restart" cycles agents only.

Two caveats this command cannot check for you:

  - A value exported in the shell that regenerates the service file WINS over
    the file, which fills only names that shell left unset. If that shell
    exports the variable, the file entry is ignored and nothing changes.
  - A refused install (a service unit referencing a different gc binary needs
    --force) and a --foreground city both leave the old environment in place
    while still exiting 0.

Confirm the supervisor came up with the new value rather than assuming the
restart carried it.
`
