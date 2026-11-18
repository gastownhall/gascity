package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/spf13/cobra"
)

type agentPatchCommandOutput struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	configedit.AgentPatchMergeResult
}

func newConfigAgentPatchCmd(stdout, stderr io.Writer) *cobra.Command {
	var change configedit.AgentPatchMerge
	var provider string
	var options []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-patch <agent>",
		Short: "Read or merge provider defaults for an exact singleton agent",
		Long: `Read a raw agent patch and its revision, or merge provider/model defaults.

Writes require --revision from a previous read ("absent" for a missing patch).
Other patch fields survive. --clear-provider and --remove-option expose the
underlying defaults; --delete restores an absent patch, guarded by its revision.
Use the returned before patch to restore prior values with a new revision.

Changes apply at the next start. Existing config drift may restart an idle
session; hand off work first. Named sessions must have an empty work queue.
Targets must have max_active_sessions=1 and at most one named identity.
This command requires a running mutable city API.`,
		Example: `  gc config agent-patch worker --json
  gc config agent-patch worker --revision absent --provider fallback --option model=fast
  gc config agent-patch worker --revision <revision> --clear-provider --remove-option model`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := func(err error) error {
				if asJSON {
					writeJSONError(stdout, stderr, "agent_patch_failed", err.Error(), 1)
				} else {
					fmt.Fprintf(stderr, "gc config agent-patch: %v\n", err)
				} //nolint:errcheck
				return errExit
			}
			if dir, name, qualified := strings.Cut(args[0], "/"); qualified {
				change.Dir, change.Name = dir, name
			} else {
				change.Name = args[0]
			}
			if cmd.Flags().Changed("provider") {
				change.Provider = &provider
			}
			for _, option := range options {
				key, value, ok := strings.Cut(option, "=")
				if !ok || key == "" {
					return fail(fmt.Errorf("option %q must be key=value", option))
				}
				if change.OptionDefaults == nil {
					change.OptionDefaults = make(map[string]string)
				}
				change.OptionDefaults[key] = value
			}
			mutation := change.Provider != nil || change.ClearProvider || len(change.OptionDefaults)+len(change.RemoveOptions) > 0 || change.Delete
			if mutation && change.Revision == "" {
				return fail(fmt.Errorf("--revision is required for a write; read the patch first"))
			}
			cityPath, err := resolveCity()
			if err != nil {
				return fail(err)
			}
			client, reason := supervisorFallthroughMutationAPIClient(cityPath)
			if client == nil {
				return fail(fmt.Errorf("mutable API unavailable (%s)", reason))
			}
			return runAgentPatchCommand(client, args[0], change, mutation, asJSON, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&change.Revision, "revision", "", "expected content revision; absent creates a missing patch")
	cmd.Flags().StringVar(&provider, "provider", "", "provider name for the next start")
	cmd.Flags().StringArrayVar(&options, "option", nil, "provider option key=value (repeatable; model selects the model)")
	cmd.Flags().BoolVar(&change.ClearProvider, "clear-provider", false, "remove only the patch's provider override")
	cmd.Flags().StringArrayVar(&change.RemoveOptions, "remove-option", nil, "remove an option key from this patch (repeatable)")
	cmd.Flags().BoolVar(&change.Delete, "delete", false, "remove the entire patch using its expected revision")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the revision and before/after patches as JSON")
	return cmd
}

func runAgentPatchCommand(client *api.Client, name string, change configedit.AgentPatchMerge, mutation, asJSON bool, stdout, stderr io.Writer) error {
	var result configedit.AgentPatchMergeResult
	var err error
	if mutation {
		result, err = client.MergeAgentPatch(change)
	} else {
		result, err = client.GetAgentPatch(name)
	}
	if err != nil {
		if asJSON {
			writeJSONError(stdout, stderr, "agent_patch_failed", err.Error(), 1)
		} else {
			fmt.Fprintf(stderr, "gc config agent-patch: %v\n", err)
		} //nolint:errcheck
		return errExit
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(agentPatchCommandOutput{SchemaVersion: "1", OK: true, AgentPatchMergeResult: result})
	}
	_, err = fmt.Fprintf(stdout, "Agent patch %s; revision %s\n", name, result.Revision)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
