package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/spf13/cobra"
)

// newInternalCmd builds the hidden `gc internal` subcommand tree. These
// commands are invoked by the supervisor, session PreStart hooks, and
// other SDK infrastructure — not by humans. The parent command is
// hidden from --help to reduce accidental direct use.
func newInternalCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "internal",
		Short:  "Internal gc subcommands (not for direct human use)",
		Hidden: true,
	}
	cmd.AddCommand(newInternalMaterializeSkillsCmd(stdout, stderr))
	cmd.AddCommand(newInternalProjectMCPCmd(stdout, stderr))
	return cmd
}

// newInternalMaterializeSkillsCmd materializes skills for one agent
// into one working directory. Invoked from a session PreStart when the
// runtime is stage-2-eligible (subprocess, tmux) and the session's
// WorkDir differs from the agent's scope root. See
// engdocs/proposals/skill-materialization.md for the two-stage design.
//
// This is a thin wrapper over internal/materialize.Run:
// resolve city config → find named agent → look up its vendor sink →
// build desired set → materialize. Never invoked by humans directly.
func newInternalMaterializeSkillsCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentName, workdir string
	cmd := &cobra.Command{
		Use:    "materialize-skills",
		Short:  "Materialize skills for one agent into one workdir",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(agentName) == "" {
				fmt.Fprintln(stderr, "gc internal materialize-skills: --agent is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if strings.TrimSpace(workdir) == "" {
				fmt.Fprintln(stderr, "gc internal materialize-skills: --workdir is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
			if !ok {
				fmt.Fprintf(stderr, "gc internal materialize-skills: unknown agent %q\n", agentName) //nolint:errcheck // best-effort stderr
				return errExit
			}

			provider := effectiveAgentProvider(&agent, cfg.Workspace.Provider)
			vendorSink, sinkOK := materialize.VendorSink(provider)
			if !sinkOK {
				// Providers outside the v0.15.1 four-vendor set (copilot,
				// cursor, pi, omp, or an unknown provider) have no sink.
				// Log once per session spawn per the spec and exit
				// successfully — this is not an error condition.
				fmt.Fprintf(stdout, "gc internal materialize-skills: provider %q has no skill sink in v0.15.1; skipping\n", provider) //nolint:errcheck // best-effort stdout
				return nil
			}

			rigName := agentRigScopeName(&agent, cfg.Rigs)
			cityCat, err := loadSharedSkillCatalog(cfg, rigName)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: shared skill catalog unavailable for %q: %v\n", agent.QualifiedName(), err) //nolint:errcheck // best-effort stderr
				cityCat.Entries = nil
				cityCat.Shadowed = nil
			}
			agentCat, err := materialize.LoadAgentCatalog(agent.SkillsDir)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			desired := materialize.EffectiveSet(cityCat, agentCat)

			owned := append([]string{}, cityCat.OwnedRoots...)
			if agentCat.OwnedRoot != "" {
				owned = append(owned, agentCat.OwnedRoot)
			}

			absWorkdir, err := filepath.Abs(workdir)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: resolving workdir %q: %v\n", workdir, err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			res, err := materialize.Run(materialize.Request{
				SinkDir:     filepath.Join(absWorkdir, vendorSink),
				Desired:     desired,
				OwnedRoots:  owned,
				LegacyNames: materialize.LegacyStubNames(),
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			// Log summary to stdout for diagnostic capture. Skipped and
			// Warnings to stderr because they indicate something the
			// operator may want to investigate (user-placed content
			// blocking a sink path, transient I/O failures, etc.).
			if len(res.Materialized) > 0 {
				fmt.Fprintf(stdout, "materialized %d skill(s) into %s: %s\n", //nolint:errcheck // best-effort stdout
					len(res.Materialized),
					filepath.Join(absWorkdir, vendorSink),
					strings.Join(res.Materialized, ", "),
				)
			}
			if len(res.LegacyMigrated) > 0 {
				fmt.Fprintf(stdout, "legacy stubs migrated: %s\n", strings.Join(res.LegacyMigrated, ", ")) //nolint:errcheck // best-effort stdout
			}
			for _, s := range res.Skipped {
				fmt.Fprintf(stderr, "warning: skipped skill %q at %s — %s\n", s.Name, s.Path, s.Reason) //nolint:errcheck // best-effort stderr
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(stderr, "warning: %s\n", w) //nolint:errcheck // best-effort stderr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "qualified agent identity (dir/name or name)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "agent working directory (skills materialize into workdir/.<vendor>/skills/)")
	return cmd
}
