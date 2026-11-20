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
// install did not persist from the shell, because a durable tier supplied
// every secret-class name under the prefix.
type supervisorWithheldEnv struct {
	// Prefix is the provider prefix, for example "ANTHROPIC_".
	Prefix string
	// Dropped are the withheld names the service env does not carry, because
	// secrets.env has no value for them, sorted.
	Dropped []string
	// Replaced are the withheld names that took a secrets.env value other
	// than the shell's, sorted.
	Replaced []string
	// CredentialSources are the durable tiers that supplied the prefix's
	// secret-class names, in source order.
	CredentialSources []supervisorEnvSource
}

// supervisorProviderTiers records, for one provider prefix, which tiers
// supplied its secret-class names and which routing names the shell scan
// supplied.
type supervisorProviderTiers struct {
	// durable are the durable tiers that supplied a secret-class name.
	durable map[supervisorEnvSource]bool
	// shellSecret is set when the calling shell supplied a secret-class name,
	// through the implicit scan or GC_SUPERVISOR_ENV.
	shellSecret bool
	// shellRouting are the routing-class names the implicit shell scan
	// supplied.
	shellRouting []string
}

// note records one service env entry under the prefix. A setting-class name
// records nothing: it neither carries a credential nor routes a call.
func (p *supervisorProviderTiers) note(name string, class processenv.ProviderEnvClass, source supervisorEnvSource) {
	switch class {
	case processenv.ProviderEnvRouting:
		if source == supervisorEnvSourceShell {
			p.shellRouting = append(p.shellRouting, name)
		}
	case processenv.ProviderEnvSecret:
		switch source {
		case supervisorEnvSourceSecretsFile, supervisorEnvSourceLaunchctl:
			p.durable[source] = true
		case supervisorEnvSourceShell, supervisorEnvSourceExplicit:
			p.shellSecret = true
		}
	}
}

// withholdsShellRouting reports whether the shell supplied routing under the
// prefix while a durable tier supplied every secret-class name.
func (p *supervisorProviderTiers) withholdsShellRouting() bool {
	return len(p.durable) > 0 && !p.shellSecret && len(p.shellRouting) > 0
}

// withhold withholds the prefix's shell routing names from env. A name takes
// secrets' non-empty value for it when there is one (the value the
// secrets.env tier would have filled had the shell not set it) and is
// removed from env otherwise. It reports the removed names and the names
// whose value changed.
func (p *supervisorProviderTiers) withhold(prefix string, env map[string]supervisorTrackedEnv, secrets map[string]string) supervisorWithheldEnv {
	group := supervisorWithheldEnv{Prefix: prefix}
	for source := range p.durable {
		group.CredentialSources = append(group.CredentialSources, source)
	}
	slices.Sort(group.CredentialSources)
	sort.Strings(p.shellRouting)
	for _, name := range p.shellRouting {
		val := secrets[name]
		if val == "" {
			delete(env, name)
			group.Dropped = append(group.Dropped, name)
			continue
		}
		if val != env[name].Value {
			group.Replaced = append(group.Replaced, name)
		}
		env[name] = supervisorTrackedEnv{Value: val, Source: supervisorEnvSourceSecretsFile}
	}
	return group
}

// withholdShellRoutingBesideDurableCredentials keeps a provider's routing
// consistent with the tier its credentials come from. For each provider
// prefix whose secret-class names (processenv.ClassifyProviderEnv) all come
// from secrets.env or launchctl, it withholds every routing-class name the
// implicit shell scan supplied (supervisorProviderTiers.withhold) and reports
// the names it dropped or whose value it replaced. Secret- and setting-class
// names and GC_SUPERVISOR_ENV opt-ins are never touched, and a setting-class
// name never decides which tier a prefix's credentials come from.
func withholdShellRoutingBesideDurableCredentials(env map[string]supervisorTrackedEnv, secrets map[string]string) []supervisorWithheldEnv {
	byPrefix := make(map[string]*supervisorProviderTiers)
	for name, entry := range env {
		prefix, class := processenv.ClassifyProviderEnv(name)
		if class == processenv.ProviderEnvNone {
			continue
		}
		p := byPrefix[prefix]
		if p == nil {
			p = &supervisorProviderTiers{durable: make(map[supervisorEnvSource]bool)}
			byPrefix[prefix] = p
		}
		p.note(name, class, entry.Source)
	}
	var withheld []supervisorWithheldEnv
	for prefix, p := range byPrefix {
		if !p.withholdsShellRouting() {
			continue
		}
		if group := p.withhold(prefix, env, secrets); len(group.Dropped)+len(group.Replaced) > 0 {
			withheld = append(withheld, group)
		}
	}
	slices.SortFunc(withheld, func(a, b supervisorWithheldEnv) int {
		return strings.Compare(a.Prefix, b.Prefix)
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

// warnSupervisorWithheldEnv prints, for each provider prefix whose shell
// routing an install withheld, a warning naming the dropped names, the
// durable credential sources, that running agent sessions keep their values
// until a restart, and the two remedies, and a note naming the names that took
// a different secrets.env value instead. It never prints a value.
func warnSupervisorWithheldEnv(stderr io.Writer, withheld []supervisorWithheldEnv) {
	for _, group := range withheld {
		sources := make([]string, 0, len(group.CredentialSources))
		for _, source := range group.CredentialSources {
			sources = append(sources, source.String())
		}
		credentials := fmt.Sprintf("%s credentials come only from %s", group.Prefix, strings.Join(sources, " and "))
		if len(group.Dropped) > 0 {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"gc supervisor install: warning: not persisting this shell's %s: %s, so their routing must come from there too. New agent sessions do not get these names either, and running ones keep their values until gc restart. Add them to %s, or list them in GC_SUPERVISOR_ENV to persist this shell's values.\n",
				strings.Join(group.Dropped, ", "), credentials, supervisorSecretsEnvFilePath())
		}
		if len(group.Replaced) > 0 {
			fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
				"gc supervisor install: note: persisting %s's %s, not this shell's: %s.\n",
				supervisorSecretsEnvFileName, strings.Join(group.Replaced, ", "), credentials)
		}
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
