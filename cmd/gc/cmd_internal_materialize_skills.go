package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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
	cmd.AddCommand(newInternalBeadsCredentialCmd(stdout, stderr))
	cmd.AddCommand(newInternalProgramStatusShadowReportCmd(stdout, stderr))
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
	var agentName, workdir, sharedCatalogSnapshot, sharedCatalogSnapshotFile string
	var bestEffort bool
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
				if bestEffort {
					fmt.Fprintf(stderr, "gc internal materialize-skills: city not found: %v; skipping (best-effort)\n", err) //nolint:errcheck // best-effort stderr
					return nil
				}
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				if bestEffort {
					fmt.Fprintf(stderr, "gc internal materialize-skills: city config unavailable: %v; skipping (best-effort)\n", err) //nolint:errcheck // best-effort stderr
					return nil
				}
				fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
			if !ok {
				if bestEffort {
					// Named/wisp session expansions pass a session identity
					// (e.g. "rig/executor-vc-eswi4") that can't be resolved back
					// to a config template. Skills materialization is best-effort
					// session preparation, not a hard prerequisite, so in
					// best-effort mode (the pre_start caller) warn and exit 0
					// rather than failing the whole session start (the session
					// runs without a freshly materialized skill catalog).
					fmt.Fprintf(stderr, "gc internal materialize-skills: unknown agent %q; skipping (best-effort)\n", agentName) //nolint:errcheck // best-effort stderr
					return nil
				}
				fmt.Fprintf(stderr, "gc internal materialize-skills: unknown agent %q\n", agentName) //nolint:errcheck // best-effort stderr
				return errExit
			}
			// Resolve snapshot source: explicit --shared-catalog-snapshot-file
			// → deterministic workdir-local snapshot file (keeps the
			// pre-start command shape stable across upgrades) →
			// --shared-catalog-snapshot (legacy/test path — base64 inline)
			// → env var (legacy upgrade-compat path for sessions that were
			// already launched before the file-indirection rollout).
			explicitSnapshotFile := strings.TrimSpace(sharedCatalogSnapshotFile)
			defaultSnapshotFile := ""
			if explicitSnapshotFile == "" {
				defaultSnapshotFile = skillSnapshotFilePath(workdir, agentName)
			}
			if explicitSnapshotFile != "" {
				data, err := os.ReadFile(explicitSnapshotFile)
				if err != nil {
					fmt.Fprintf(stderr, "gc internal materialize-skills: reading --shared-catalog-snapshot-file %q: %v (falling back to live catalog)\n", explicitSnapshotFile, err) //nolint:errcheck // best-effort stderr
				} else {
					sharedCatalogSnapshot = string(data)
				}
			}
			if strings.TrimSpace(sharedCatalogSnapshot) == "" && defaultSnapshotFile != "" {
				if data, err := os.ReadFile(defaultSnapshotFile); err == nil {
					sharedCatalogSnapshot = string(data)
				}
			}
			if strings.TrimSpace(sharedCatalogSnapshot) == "" {
				sharedCatalogSnapshot = os.Getenv(sharedSkillCatalogSnapshotEnvVar)
			}
			var sharedCatalog *materialize.CityCatalog
			if strings.TrimSpace(sharedCatalogSnapshot) != "" {
				cat, err := decodeSharedCatalogSnapshot(sharedCatalogSnapshot)
				if err != nil {
					fmt.Fprintf(stderr, "gc internal materialize-skills: decoding shared catalog snapshot: %v (falling back to live catalog)\n", err) //nolint:errcheck // best-effort stderr
				} else {
					sharedCatalog = &cat
				}
			}

			if err := materializeSkillsIntoWorkdir(cfg, &agent, cityPath, workdir, sharedCatalog, stdout, stderr); err != nil {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "qualified agent identity (dir/name or name)")
	cmd.Flags().StringVar(&workdir, "workdir", "", "agent working directory (skills materialize into workdir/.<vendor>/skills/)")
	cmd.Flags().StringVar(&sharedCatalogSnapshot, "shared-catalog-snapshot", "", "base64-encoded shared catalog snapshot from the controller")
	cmd.Flags().StringVar(&sharedCatalogSnapshotFile, "shared-catalog-snapshot-file", "", "path to a file containing the base64-encoded shared catalog snapshot (preferred over --shared-catalog-snapshot for large catalogs to avoid argv/env limits)")
	cmd.Flags().BoolVar(&bestEffort, "best-effort", false, "warn and exit 0 instead of failing when city path, city config, or agent identity can't be resolved; used by pre_start so session startup is non-fatal when city state is transiently unavailable (dirty import cache, missing city.toml) or the session identity can't be matched to a config template")
	return cmd
}

func encodeSharedCatalogSnapshot(cat materialize.CityCatalog) (string, error) {
	data, err := json.Marshal(cat)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func decodeSharedCatalogSnapshot(encoded string) (materialize.CityCatalog, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return materialize.CityCatalog{}, err
	}
	var cat materialize.CityCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return materialize.CityCatalog{}, err
	}
	return cat, nil
}

// materializeSkillsIntoWorkdir reconciles the vendor sink under workdir
// for agent's session. See materializeSkillSinkForSession.
func materializeSkillsIntoWorkdir(cfg *config.City, agent *config.Agent, cityPath, workdir string, sharedCatalog *materialize.CityCatalog, stdout, stderr io.Writer) error {
	_, err := materializeSkillSinkForSession(cfg, agent, cityPath, workdir, sharedCatalog, stdout, stderr)
	return err
}

// materializeSkillSinkForSession reconciles the vendor sink under workdir
// over every configured writer of that sink, not agent alone: the sink
// holds one link per name and one ownership manifest, so a pass that
// wanted only agent's skills would prune its co-writers' links. The
// writers are the agents buildSkillSinkIndex lists for the sink (the
// scope-root agents stage 1 materializes there and the agents whose
// sessions' work dir is there) plus agent itself, folded into one
// materialize.SinkUnion exactly as stage 1 folds a scope-root sink, so
// every pass over the sink agrees on what it holds.
//
// sharedCatalog, when non-nil, is the controller's snapshot of agent's
// scope catalog. Co-writers in other scopes load their scope's catalog
// live, once per scope; a co-writer whose catalog cannot be loaded leaves
// the union incomplete. A sink whose union is incomplete or has a clash
// is held: the problems are reported on stderr, no link is added,
// removed or re-pointed, and the pass succeeds so the session still
// starts.
func materializeSkillSinkForSession(cfg *config.City, agent *config.Agent, cityPath, workdir string, sharedCatalog *materialize.CityCatalog, stdout, stderr io.Writer) (materialize.Result, error) {
	if cfg == nil || agent == nil {
		fmt.Fprintln(stderr, "gc internal materialize-skills: missing city config or agent") //nolint:errcheck // best-effort stderr
		return materialize.Result{}, errExit
	}

	provider := effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers)
	vendorSink, sinkOK := materialize.VendorSink(provider)
	if !sinkOK {
		// Providers outside the v0.15.1 four-vendor set (copilot,
		// cursor, pi, omp, or an unknown provider) have no sink.
		// Log once per session spawn per the spec and exit
		// successfully — this is not an error condition.
		fmt.Fprintf(stdout, "gc internal materialize-skills: provider %q has no skill sink in v0.15.1; skipping\n", provider) //nolint:errcheck // best-effort stdout
		return materialize.Result{}, nil
	}

	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		fmt.Fprintf(stderr, "gc internal materialize-skills: resolving workdir %q: %v\n", workdir, err) //nolint:errcheck // best-effort stderr
		return materialize.Result{}, errExit
	}
	sinkDir := filepath.Join(absWorkdir, vendorSink)

	agentCat, err := materialize.LoadAgentCatalog(agent.SkillsDir)
	if err != nil {
		fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
		return materialize.Result{}, errExit
	}

	union := materialize.NewSinkUnion()
	catalogs := stage2SkillScopeCatalogs(cfg, agent, sharedCatalog, union, stderr)
	if sink := buildSkillSinkIndex(cityPath, cfg).lookup(sinkDir); sink != nil {
		for _, w := range sink.Writers {
			if w.Name == agent.QualifiedName() {
				continue
			}
			addSkillSinkWriter(union, w.Agent, w.Name, catalogs.load(agentRigScopeName(w.Agent, cfg.Rigs)))
		}
	}
	union.Add(agent.QualifiedName(), catalogs.load(agentRigScopeName(agent, cfg.Rigs)), agentCat)

	if problems := union.Problems(); len(problems) > 0 {
		reportHeldSkillSink(stderr, "gc internal materialize-skills", sinkDir, problems)
		return materialize.Result{}, nil
	}

	res, err := materialize.Run(materialize.Request{
		SinkDir:          sinkDir,
		Desired:          union.Desired(),
		OwnedRoots:       union.OwnedRoots(),
		LegacyNames:      materialize.LegacyStubNames(),
		LegacyOwnedRoots: materialize.LegacyOwnedRootsFor(cityPath),
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc internal materialize-skills: %v\n", err) //nolint:errcheck // best-effort stderr
		return materialize.Result{}, errExit
	}

	// Log summary to stdout for diagnostic capture. Skipped and
	// Warnings to stderr because they indicate something the
	// operator may want to investigate (user-placed content
	// blocking a sink path, transient I/O failures, etc.).
	if len(res.Materialized) > 0 {
		fmt.Fprintf(stdout, "materialized %d skill(s) into %s: %s\n", //nolint:errcheck // best-effort stdout
			len(res.Materialized),
			sinkDir,
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
	return res, nil
}

// stage2SkillScopeCatalogs loads the shared catalogs of a stage-2 sink's
// writers, once per scope. agent's own scope uses sharedCatalog when the
// controller supplied one; otherwise it loads live, and a load failure
// degrades to no shared entries (its owned roots kept) so stale shared
// links are pruned. Any other scope loads live, and a load failure marks
// union incomplete, since this pass cannot tell that scope's links from
// stale ones.
func stage2SkillScopeCatalogs(cfg *config.City, agent *config.Agent, sharedCatalog *materialize.CityCatalog, union *materialize.SinkUnion, stderr io.Writer) *skillScopeCatalogs {
	ownScope := agentRigScopeName(agent, cfg.Rigs)
	return newSkillScopeCatalogs(func(rigName string) materialize.CityCatalog {
		if rigName == ownScope && sharedCatalog != nil {
			return cloneCityCatalog(*sharedCatalog)
		}
		cat, err := loadSharedSkillCatalog(cfg, rigName)
		if err == nil {
			return cat
		}
		if rigName != ownScope {
			scope := "city scope"
			if rigName != "" {
				scope = fmt.Sprintf("rig %q", rigName)
			}
			union.MarkUnreadable(fmt.Sprintf("load shared skill catalog for %s: %v", scope, err))
			return cat
		}
		fmt.Fprintf(stderr, "gc internal materialize-skills: shared skill catalog unavailable for %q: %v\n", agent.QualifiedName(), err) //nolint:errcheck // best-effort stderr
		cat.Entries = nil
		cat.Shadowed = nil
		return cat
	})
}
