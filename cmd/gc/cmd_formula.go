package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/spf13/cobra"
)

func newFormulaCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "formula",
		Short: "Manage and inspect formulas",
	}

	cmd.AddCommand(newFormulaListCmd(stdout, stderr))
	cmd.AddCommand(newFormulaShowCmd(stdout, stderr))
	cmd.AddCommand(newFormulaCookCmd(stdout, stderr))
	return cmd
}

func newFormulaListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available formulas",
		Long: `List all formulas available in the city's formula search paths.

Formulas are discovered from city-level and rig-level formula directories
configured via packs and formulas_dir settings.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			paths := allFormulaSearchPaths(stderr)
			if len(paths) == 0 {
				_, _ = fmt.Fprintln(stdout, "No formula search paths configured.")
				return nil
			}

			// Scan search paths for canonical and legacy formula TOML files,
			// deduplicating by name (last path wins, matching formula layer
			// resolution order).
			winners := make(map[string]bool)
			for _, dir := range paths {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					name, ok := formula.TrimTOMLFilename(e.Name())
					if !ok {
						continue
					}
					winners[name] = true
				}
			}

			if len(winners) == 0 {
				_, _ = fmt.Fprintln(stdout, "No formulas found.")
				return nil
			}

			names := make([]string, 0, len(winners))
			for name := range winners {
				names = append(names, name)
			}
			slices.Sort(names)

			for _, name := range names {
				_, _ = fmt.Fprintln(stdout, name)
			}
			return nil
		},
	}
}

func newFormulaShowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <formula-name>",
		Short: "Show a compiled formula recipe",
		Long: `Compile and display a formula recipe.

By default, shows the recipe with {{variable}} placeholders intact.
Use --var to substitute variables and preview the resolved output.

Examples:
  gc formula show mol-feature
  gc formula show mol-feature --var title="Auth system" --var branch=main`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			varFlags, _ := cmd.Flags().GetStringArray("var")

			vars := make(map[string]string, len(varFlags))
			for _, v := range varFlags {
				key, value, ok := strings.Cut(v, "=")
				if ok && key != "" {
					vars[key] = value
				}
			}

			compileVars := vars

			recipe, err := formula.Compile(cmd.Context(), name, cityFormulaSearchPaths(stderr), compileVars)
			if err != nil {
				return err
			}

			// Apply var substitution for display only when --var flags were provided.
			// Without explicit vars, placeholders stay intact per documented behavior.
			var displayVars map[string]string
			if len(vars) > 0 {
				displayVars = formula.ApplyDefaults(
					&formula.Formula{Vars: recipe.Vars},
					vars,
				)
			}

			_, _ = fmt.Fprintf(stdout, "Formula: %s\n", recipe.Name)
			if recipe.Description != "" {
				desc := recipe.Description
				if len(displayVars) > 0 {
					desc = formula.Substitute(desc, displayVars)
				}
				_, _ = fmt.Fprintf(stdout, "Description: %s\n", desc)
			}
			if recipe.Phase != "" {
				_, _ = fmt.Fprintf(stdout, "Phase: %s\n", recipe.Phase)
			}
			if recipe.RootOnly {
				_, _ = fmt.Fprintln(stdout, "Root only: true")
			}
			if len(recipe.Vars) > 0 {
				_, _ = fmt.Fprintln(stdout, "\nVariables:")
				for vname, def := range recipe.Vars {
					var attrs []string
					if def.Required {
						attrs = append(attrs, "required")
					}
					if def.Default != nil {
						attrs = append(attrs, "default="+*def.Default)
					}
					attrStr := ""
					if len(attrs) > 0 {
						attrStr = " (" + strings.Join(attrs, ", ") + ")"
					}
					_, _ = fmt.Fprintf(stdout, "  {{%s}}: %s%s\n", vname, def.Description, attrStr)
				}
			}

			displayCount := len(recipe.Steps)
			for _, s := range recipe.Steps {
				if s.IsRoot {
					displayCount--
				}
			}
			_, _ = fmt.Fprintf(stdout, "\nSteps (%d):\n", displayCount)
			for i, step := range recipe.Steps {
				if step.IsRoot {
					continue
				}
				title := step.Title
				if len(displayVars) > 0 {
					title = formula.Substitute(title, displayVars)
				}

				typeStr := ""
				if step.Type != "" && step.Type != "task" {
					typeStr = fmt.Sprintf(" (%s)", step.Type)
				}

				var blockDeps []string
				for _, dep := range recipe.Deps {
					if dep.StepID == step.ID && dep.Type == "blocks" {
						blockDeps = append(blockDeps, dep.DependsOnID)
					}
				}
				depStr := ""
				if len(blockDeps) > 0 {
					depStr = fmt.Sprintf(" [needs: %s]", strings.Join(blockDeps, ", "))
				}

				connector := "├──"
				if i == len(recipe.Steps)-1 {
					connector = "└──"
				}

				_, _ = fmt.Fprintf(stdout, "  %s %s: %s%s%s\n", connector, step.ID, title, typeStr, depStr)
			}

			return nil
		},
	}

	cmd.Flags().StringArray("var", nil, "variable substitution for preview (key=value)")
	return cmd
}

func newFormulaCookCmd(stdout, stderr io.Writer) *cobra.Command {
	var title string
	var vars []string
	var metadata []string
	var attach string
	cmd := &cobra.Command{
		Use:   "cook <formula-name>",
		Short: "Instantiate a formula into the current bead store",
		Long: `Compile and instantiate a formula as real beads in the current store.

This is a low-level workflow construction tool. It creates the formula root
and all compiled step beads without routing any work.

With --attach=<bead-id>, the sub-DAG is created as children of the given
bead. The bead gains a blocking dependency on the sub-DAG root, so it won't
close until the sub-DAG completes. This is the core primitive for late-bound
DAG expansion — any agent, script, or workflow step can call it to expand a
bead into a sub-workflow at runtime.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				return err
			}
			store, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				return err
			}

			cookVars := parseFormulaVars(vars)

			if attach != "" {
				recipe, err := formula.Compile(cmd.Context(), args[0], cfg.FormulaLayers.City, cookVars)
				if err != nil {
					return fmt.Errorf("compile: %w", err)
				}

				result, err := molecule.Attach(cmd.Context(), store, recipe, attach, molecule.AttachOptions{
					Title: title,
					Vars:  cookVars,
				})
				if err != nil {
					return err
				}

				_, _ = fmt.Fprintf(stdout, "Attached: %s -> %s (root: %s)\n", attach, result.RootID, result.WorkflowRootID)
				_, _ = fmt.Fprintf(stdout, "Root: %s\n", result.RootID)
				_, _ = fmt.Fprintf(stdout, "Created: %d\n", result.Created)

				// Poke control dispatcher to pick up new beads
				_ = pokeControlDispatch(cityPath)
				return nil
			}

			result, err := molecule.Cook(cmd.Context(), store, args[0], cfg.FormulaLayers.City, molecule.Options{
				Title: title,
				Vars:  cookVars,
			})
			if err != nil {
				return err
			}

			rootMeta, err := parseMetadataArgs(metadata)
			if err != nil {
				return err
			}
			if len(rootMeta) > 0 {
				if err := store.SetMetadataBatch(result.RootID, rootMeta); err != nil {
					return fmt.Errorf("setting root metadata on %s: %w", result.RootID, err)
				}
			}

			_, _ = fmt.Fprintf(stdout, "Root: %s\n", result.RootID)
			_, _ = fmt.Fprintf(stdout, "Created: %d\n", result.Created)
			keys := make([]string, 0, len(result.IDMapping))
			for stepID := range result.IDMapping {
				keys = append(keys, stepID)
			}
			slices.Sort(keys)
			for _, stepID := range keys {
				_, _ = fmt.Fprintf(stdout, "%s -> %s\n", stepID, result.IDMapping[stepID])
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&title, "title", "t", "", "override root bead title")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "variable substitution for formula (key=value, repeatable)")
	cmd.Flags().StringArrayVar(&metadata, "meta", nil, "set root bead metadata after cook (key=value, repeatable)")
	cmd.Flags().StringVar(&attach, "attach", "", "attach sub-DAG to existing bead (bead gains blocking dep on sub-DAG root)")
	return cmd
}

func parseFormulaVars(varFlags []string) map[string]string {
	if len(varFlags) == 0 {
		return nil
	}
	vars := make(map[string]string, len(varFlags))
	for _, v := range varFlags {
		key, value, ok := strings.Cut(v, "=")
		if ok && key != "" {
			vars[key] = value
		}
	}
	if len(vars) == 0 {
		return nil
	}
	return vars
}

func parseMetadataArgs(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid metadata %q (want key=value)", item)
		}
		out[key] = value
	}
	return out, nil
}

// cityFormulaSearchPaths returns the city-level formula search paths.
// Best-effort: returns nil if no city is loaded.
func cityFormulaSearchPaths(warningWriter ...io.Writer) []string {
	cityPath, err := resolveCity()
	if err != nil {
		return nil
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return nil
	}
	return cfg.FormulaLayers.City
}

// allFormulaSearchPaths returns the deduplicated union of formula search
// paths across city and all rigs. Used by gc formula list to discover
// every available formula regardless of scope.
func allFormulaSearchPaths(warningWriter ...io.Writer) []string {
	cityPath, err := resolveCity()
	if err != nil {
		return nil
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var all []string
	add := func(paths []string) {
		for _, p := range paths {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				all = append(all, p)
			}
		}
	}
	add(cfg.FormulaLayers.City)
	for _, layers := range cfg.FormulaLayers.Rigs {
		add(layers)
	}
	return all
}
