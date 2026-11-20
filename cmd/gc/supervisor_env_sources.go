package main

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/processenv"
)

// supervisorEnvSource names the tier that supplied a name persisted into the
// supervisor service env. The zero value is gc's own fixed entries.
type supervisorEnvSource int

const (
	// supervisorEnvSourceGC marks an entry gc sets itself.
	supervisorEnvSourceGC supervisorEnvSource = iota
	// supervisorEnvSourceShell marks an entry the implicit scan of the
	// calling shell's environment admitted.
	supervisorEnvSourceShell
	// supervisorEnvSourceExplicit marks a shell value named in
	// GC_SUPERVISOR_ENV.
	supervisorEnvSourceExplicit
	// supervisorEnvSourceSecretsFile marks a value from ${GC_HOME}/secrets.env.
	supervisorEnvSourceSecretsFile
	// supervisorEnvSourceLaunchctl marks a value from `launchctl getenv`.
	supervisorEnvSourceLaunchctl
)

// String returns the label an install report shows for the source.
func (s supervisorEnvSource) String() string {
	switch s {
	case supervisorEnvSourceShell:
		return "shell"
	case supervisorEnvSourceExplicit:
		return "GC_SUPERVISOR_ENV"
	case supervisorEnvSourceSecretsFile:
		return supervisorSecretsEnvFileName
	case supervisorEnvSourceLaunchctl:
		return "launchctl"
	default:
		return "gc"
	}
}

// supervisorTrackedEnv is a service env value with the tier that supplied it.
type supervisorTrackedEnv struct {
	Value  string
	Source supervisorEnvSource
}

// supervisorWithheldEnv is one provider prefix's shell routing names that an
// install did not persist from the shell, because every credential-class
// name under the prefix came from a durable tier.
type supervisorWithheldEnv struct {
	// Prefix is the provider prefix, for example "ANTHROPIC_".
	Prefix string
	// Names are the withheld routing names, sorted.
	Names []string
	// CredentialSources are the durable tiers that supplied the prefix's
	// credential-class names, in source order.
	CredentialSources []supervisorEnvSource
}

// withholdShellRoutingBesideDurableCredentials keeps a provider's routing
// consistent with the tier its credentials come from. For each provider
// prefix whose credential-class names all come from secrets.env or launchctl,
// it withholds every routing-class name the implicit shell scan supplied:
// the name takes secrets' non-empty value for it when there is one (the
// value the secrets.env tier would have filled had the shell not set it), and
// is removed from env otherwise. It reports the withheld names per prefix.
// Credential-class names and GC_SUPERVISOR_ENV opt-ins are never touched.
func withholdShellRoutingBesideDurableCredentials(env map[string]supervisorTrackedEnv, secrets map[string]string) []supervisorWithheldEnv {
	type prefixEnv struct {
		durable      map[supervisorEnvSource]bool
		shellCred    bool
		shellRouting []string
	}
	byPrefix := make(map[string]*prefixEnv)
	for name, entry := range env {
		prefix, ok := processenv.ProviderEnvPrefix(name)
		if !ok {
			continue
		}
		p := byPrefix[prefix]
		if p == nil {
			p = &prefixEnv{durable: make(map[supervisorEnvSource]bool)}
			byPrefix[prefix] = p
		}
		if processenv.IsProviderRoutingEnv(name) {
			if entry.Source == supervisorEnvSourceShell {
				p.shellRouting = append(p.shellRouting, name)
			}
			continue
		}
		switch entry.Source {
		case supervisorEnvSourceSecretsFile, supervisorEnvSourceLaunchctl:
			p.durable[entry.Source] = true
		case supervisorEnvSourceShell, supervisorEnvSourceExplicit:
			p.shellCred = true
		}
	}
	var withheld []supervisorWithheldEnv
	for prefix, p := range byPrefix {
		if len(p.durable) == 0 || p.shellCred || len(p.shellRouting) == 0 {
			continue
		}
		sort.Strings(p.shellRouting)
		for _, name := range p.shellRouting {
			if val := secrets[name]; val != "" {
				env[name] = supervisorTrackedEnv{Value: val, Source: supervisorEnvSourceSecretsFile}
				continue
			}
			delete(env, name)
		}
		sources := make([]supervisorEnvSource, 0, len(p.durable))
		for source := range p.durable {
			sources = append(sources, source)
		}
		slices.Sort(sources)
		withheld = append(withheld, supervisorWithheldEnv{
			Prefix:            prefix,
			Names:             p.shellRouting,
			CredentialSources: sources,
		})
	}
	slices.SortFunc(withheld, func(a, b supervisorWithheldEnv) int {
		switch {
		case a.Prefix < b.Prefix:
			return -1
		case a.Prefix > b.Prefix:
			return 1
		}
		return 0
	})
	return withheld
}

// supervisorEnvSourcesReportHeader opens the sources report a successful
// install prints on stdout.
const supervisorEnvSourcesReportHeader = "Supervisor service env sources (names only):"

// supervisorEnvSourcesReportOrder is the order the sources report lists tiers.
var supervisorEnvSourcesReportOrder = []supervisorEnvSource{
	supervisorEnvSourceShell,
	supervisorEnvSourceExplicit,
	supervisorEnvSourceSecretsFile,
	supervisorEnvSourceLaunchctl,
	supervisorEnvSourceGC,
}

// installSupervisorServiceReporting installs data as goos's platform service
// and, only when the install succeeds, reports where data's env came from
// and warns of the shell routing names it withheld.
func installSupervisorServiceReporting(goos string, data *supervisorServiceData, stdout, stderr io.Writer) int {
	code := installSupervisorService(goos, data, stdout, stderr)
	if code == 0 {
		reportSupervisorServiceEnv(stdout, data)
		warnSupervisorWithheldEnv(stderr, data.WithheldEnv)
	}
	return code
}

// warnSupervisorWithheldEnv prints one warning per provider prefix whose
// shell routing names an install withheld, naming the names, the durable
// credential sources and the two remedies. It never prints a value.
func warnSupervisorWithheldEnv(stderr io.Writer, withheld []supervisorWithheldEnv) {
	for _, group := range withheld {
		sources := make([]string, 0, len(group.CredentialSources))
		for _, source := range group.CredentialSources {
			sources = append(sources, source.String())
		}
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"gc supervisor install: warning: not persisting this shell's %s: %s credentials come only from %s, so their routing must come from there too. Add these names to %s, or list them in GC_SUPERVISOR_ENV to persist this shell's values.\n",
			strings.Join(group.Names, ", "), group.Prefix, strings.Join(sources, " and "), supervisorSecretsEnvFilePath())
	}
}

// reportSupervisorServiceEnv prints, for each tier that supplied any of
// data's ExtraEnv, the sorted names it supplied. It never prints a value.
func reportSupervisorServiceEnv(stdout io.Writer, data *supervisorServiceData) {
	bySource := make(map[supervisorEnvSource][]string)
	for _, v := range data.ExtraEnv {
		bySource[v.Source] = append(bySource[v.Source], v.Name)
	}
	var b strings.Builder
	b.WriteString(supervisorEnvSourcesReportHeader + "\n")
	for _, source := range supervisorEnvSourcesReportOrder {
		names := bySource[source]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "  %s: %s\n", source, strings.Join(names, ", "))
	}
	fmt.Fprint(stdout, b.String()) //nolint:errcheck // best-effort stdout
}
