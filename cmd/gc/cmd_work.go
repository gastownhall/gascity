package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workselect"
)

type workCommandOptions struct {
	Agent string
	JSON  bool
}

func newWorkCmd(stdout, stderr io.Writer) *cobra.Command {
	base := &workCommandOptions{}
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Inspect typed work selectors",
	}
	cmd.PersistentFlags().StringVar(&base.Agent, "agent", "", "agent identity (defaults to $GC_TEMPLATE, $GC_ALIAS, or $GC_AGENT)")

	countOpts := &workCommandOptions{}
	countCmd := &cobra.Command{
		Use:   "count",
		Short: "Count work matching an agent's typed selector",
		RunE: func(_ *cobra.Command, _ []string) error {
			countOpts.Agent = base.Agent
			if code := cmdWorkCount(*countOpts, stdout, stderr); code != 0 {
				return errExit
			}
			return nil
		},
	}
	countCmd.Flags().BoolVar(&countOpts.JSON, "json", false, "print JSON")

	nextOpts := &workCommandOptions{}
	nextCmd := &cobra.Command{
		Use:   "next",
		Short: "Print the next work item matching an agent's typed selector",
		RunE: func(_ *cobra.Command, _ []string) error {
			nextOpts.Agent = base.Agent
			if code := cmdWorkNext(*nextOpts, stdout, stderr); code != 0 {
				return errExit
			}
			return nil
		},
	}

	cmd.AddCommand(countCmd, nextCmd)
	return cmd
}

func cmdWorkCount(opts workCommandOptions, stdout, stderr io.Writer) int {
	ctx, err := resolveWorkCommandContext(opts, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc work count: %v\n", err) //nolint:errcheck
		return 1
	}
	n, err := workSelectorCountForController(ctx.store, ctx.selector)
	if err != nil {
		fmt.Fprintf(stderr, "gc work count: %v\n", err) //nolint:errcheck
		return 1
	}
	if opts.JSON {
		_ = json.NewEncoder(stdout).Encode(map[string]int{"count": n})
	} else {
		fmt.Fprintln(stdout, n) //nolint:errcheck
	}
	return 0
}

func cmdWorkNext(opts workCommandOptions, stdout, stderr io.Writer) int {
	ctx, err := resolveWorkCommandContext(opts, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc work next: %v\n", err) //nolint:errcheck
		return 1
	}
	next, ok, err := workselect.Next(ctx.store, ctx.selector)
	if err != nil {
		fmt.Fprintf(stderr, "gc work next: %v\n", err) //nolint:errcheck
		return 1
	}
	if !ok {
		fmt.Fprintln(stderr, "gc work next: no matching work") //nolint:errcheck
		return 1
	}
	writeBeadJSON(next, stdout)
	return 0
}

type workCommandContext struct {
	cityPath string
	cfg      *config.City
	agent    config.Agent
	selector config.WorkSelector
	store    beads.Store
}

func resolveWorkCommandContext(opts workCommandOptions, stderr io.Writer) (workCommandContext, error) {
	agentName := strings.TrimSpace(opts.Agent)
	if agentName == "" {
		agentName = firstNonEmptyWork(os.Getenv("GC_TEMPLATE"), os.Getenv("GC_ALIAS"), os.Getenv("GC_AGENT"))
	}
	if agentName == "" {
		return workCommandContext{}, fmt.Errorf("agent not specified (pass --agent or set $GC_AGENT)")
	}
	cityPath, err := resolveCity()
	if err != nil {
		return workCommandContext{}, err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return workCommandContext{}, err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	agentCfg, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		return workCommandContext{}, fmt.Errorf("agent %q not found in config", agentName)
	}
	selector := agentCfg.WorkSelector
	if selector.IsZero() {
		return workCommandContext{}, fmt.Errorf("agent %q has no work_selector", agentCfg.QualifiedName())
	}
	cityName := loadedCityName(cfg, cityPath)
	selector = expandWorkSelectorTemplates(cityPath, cityName, &agentCfg, cfg.Rigs, "work_selector", selector, stderr)
	storeRoot := agentStoreRoot(cityPath, cfg, &agentCfg)
	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return workCommandContext{}, err
	}
	return workCommandContext{
		cityPath: cityPath,
		cfg:      cfg,
		agent:    agentCfg,
		selector: selector,
		store:    store,
	}, nil
}

func workSelectorCountForController(store beads.Store, selector config.WorkSelector) (int, error) {
	compiled, err := workselect.Compile(selector, 0)
	if err != nil {
		return 0, err
	}
	items, err := listForControllerDemand(store, compiled.Query)
	if err != nil {
		if !beads.IsPartialResult(err) || len(items) == 0 {
			return 0, err
		}
	}
	filtered, filterErr := workselect.ApplyStorePostFilters(store, items, compiled)
	if filterErr != nil {
		return 0, filterErr
	}
	return len(filtered), err
}

func firstNonEmptyWork(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func agentStoreRoot(cityPath string, cfg *config.City, agentCfg *config.Agent) string {
	rigName := configuredRigName(cityPath, agentCfg, cfg.Rigs)
	if rigName == "" {
		return cityPath
	}
	return rigRootForName(rigName, cfg.Rigs)
}

func expandWorkSelectorTemplates(
	cityPath, cityName string,
	agentCfg *config.Agent,
	rigs []config.Rig,
	field string,
	selector config.WorkSelector,
	stderr io.Writer,
) config.WorkSelector {
	expand := func(name, value string) string {
		if strings.TrimSpace(value) == "" {
			return value
		}
		return expandAgentCommandTemplate(cityPath, cityName, agentCfg, rigs, field+"."+name, value, stderr)
	}
	selector.Status = expand("status", selector.Status)
	selector.Type = expand("type", selector.Type)
	selector.ExcludeType = expand("exclude_type", selector.ExcludeType)
	selector.Label = expand("label", selector.Label)
	selector.Assignee = expand("assignee", selector.Assignee)
	selector.Parent = expand("parent", selector.Parent)
	selector.Tier = expand("tier", selector.Tier)
	selector.Sort = expand("sort", selector.Sort)
	if len(selector.Metadata) > 0 {
		metadata := make(map[string]string, len(selector.Metadata))
		for k, v := range selector.Metadata {
			metadata[k] = expand("metadata."+k, v)
		}
		selector.Metadata = metadata
	}
	return selector
}
