package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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
	cmd.AddCommand(newFormulaCatalogCmd(stdout, stderr))
	cmd.AddCommand(newFormulaCookCmd(stdout, stderr))
	return cmd
}

func newFormulaListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available formulas",
		Long: `List all formulas available in the city's formula search paths.

Formulas are discovered from city-level and rig-level formula directories
configured via packs and formulas_dir settings.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, paths, rows := listFormulaRows(stderr)
			if jsonOutput {
				return writeCLIJSONLine(stdout, formulaListJSON{
					SchemaVersion: "1",
					OK:            true,
					CityPath:      cityPath,
					SearchPaths:   paths,
					Formulas:      rows,
					Summary:       formulaListSummaryJSON{Count: len(rows)},
				})
			}
			if len(paths) == 0 {
				_, _ = fmt.Fprintln(stdout, "No formula search paths configured.")
				return nil
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(stdout, "No formulas found.")
				return nil
			}

			for _, row := range rows {
				_, _ = fmt.Fprintln(stdout, row.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

func newFormulaShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <formula-name>",
		Short: "Show a compiled formula recipe",
		Long: `Compile and display a formula recipe.

By default, shows the recipe with {{variable}} placeholders intact.
Use --var to substitute variables and preview the resolved output.

When --rig is set (or cwd is inside a rig), rig-scoped formula_vars from
city.toml are shown as "(rig default=...)" alongside each applicable var.

Examples:
  gc formula show mol-feature
  gc formula show mol-feature --var title="Auth system" --var branch=main
  gc formula show mol-polecat-work --rig mo`,
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

			cityPath, err := resolveCity()
			if err != nil {
				return formulaCommandError(stderr, "gc formula show", jsonOutput, err)
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc formula show", jsonOutput, err)
			}
			scope, err := resolveFormulaScope(cfg, cityPath)
			if err != nil {
				return formulaCommandError(stderr, "gc formula show", jsonOutput, err)
			}
			searchPaths := scope.searchPaths
			rigVars := rigFormulaVarsForScope(cfg, cityPath)
			recipe, err := formula.CompileWithoutRuntimeVarValidation(cmd.Context(), name, searchPaths, compileVars)
			if err != nil {
				return formulaCommandError(stderr, "gc formula show", jsonOutput, err)
			}
			if len(vars) > 0 {
				if err := formula.ValidateProvidedVarDefs(recipe.Vars, vars); err != nil {
					return formulaCommandError(stderr, "gc formula show", jsonOutput, err)
				}
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

			if jsonOutput {
				return writeCLIJSONLine(stdout, formulaShowJSONFromRecipe(recipe, cityPath, scope, rigVars, vars, displayVars))
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
				names := make([]string, 0, len(recipe.Vars))
				for name := range recipe.Vars {
					names = append(names, name)
				}
				slices.Sort(names)

				requiredNames := make([]string, 0, len(names))
				optionalNames := make([]string, 0, len(names))
				for _, name := range names {
					def := recipe.Vars[name]
					if def != nil && def.Required {
						requiredNames = append(requiredNames, name)
						continue
					}
					optionalNames = append(optionalNames, name)
				}

				if len(requiredNames) > 0 {
					_, _ = fmt.Fprintln(stdout, "\nRequired vars:")
					for _, name := range requiredNames {
						def := recipe.Vars[name]
						var attrs []string
						if v, ok := rigVars[name]; ok {
							attrs = append(attrs, "rig default="+strconv.Quote(v))
						}
						attrStr := ""
						if len(attrs) > 0 {
							attrStr = " (" + strings.Join(attrs, ", ") + ")"
						}
						_, _ = fmt.Fprintf(stdout, "  {{%s}}: %s%s\n", name, def.Description, attrStr)
					}
				}
				if len(optionalNames) > 0 {
					header := "\nVariables:"
					if len(requiredNames) > 0 {
						header = "\nOptional vars:"
					}
					_, _ = fmt.Fprintln(stdout, header)
					for _, name := range optionalNames {
						def := recipe.Vars[name]
						var attrs []string
						if v, ok := rigVars[name]; ok {
							attrs = append(attrs, "rig default="+strconv.Quote(v))
						} else if def != nil && def.Default != nil {
							attrs = append(attrs, "default="+*def.Default)
						}
						attrStr := ""
						if len(attrs) > 0 {
							attrStr = " (" + strings.Join(attrs, ", ") + ")"
						}
						_, _ = fmt.Fprintf(stdout, "  {{%s}}: %s%s\n", name, def.Description, attrStr)
					}
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

func newFormulaCatalogCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:    "catalog",
		Short:  "List formulas opted into agent workflow discovery",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return formulaCommandError(stderr, "gc formula catalog", jsonOutput, err)
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc formula catalog", jsonOutput, err)
			}
			scope, err := resolveFormulaScope(cfg, cityPath)
			if err != nil {
				return formulaCommandError(stderr, "gc formula catalog", jsonOutput, err)
			}
			entries, warnings := formulaCatalogEntries(scope.searchPaths)
			if jsonOutput {
				return writeCLIJSONLine(stdout, formulaCatalogJSONFromEntries(entries, warnings))
			}
			for _, warning := range warnings {
				_, _ = fmt.Fprintln(stderr, warning.Message)
			}
			for _, entry := range entries {
				_, _ = fmt.Fprintf(stdout, "%s\t%s\n", entry.Name, entry.Description)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

type formulaListJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	OK            bool                   `json:"ok"`
	CityPath      string                 `json:"city_path,omitempty"`
	SearchPaths   []string               `json:"search_paths"`
	Formulas      []formulaListRowJSON   `json:"formulas"`
	Summary       formulaListSummaryJSON `json:"summary"`
	Warnings      []jsonContractWarning  `json:"warnings,omitempty"`
}

type formulaListRowJSON struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
}

type formulaListSummaryJSON struct {
	Count int `json:"count"`
}

type formulaCatalogJSON struct {
	SchemaVersion string                    `json:"schema_version"`
	OK            bool                      `json:"ok"`
	Formulas      []formulaCatalogEntryJSON `json:"formulas"`
	Summary       formulaListSummaryJSON    `json:"summary"`
	Warnings      []jsonContractWarning     `json:"warnings,omitempty"`
}

type formulaCatalogEntryJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type formulaShowJSON struct {
	SchemaVersion string                `json:"schema_version"`
	OK            bool                  `json:"ok"`
	CityPath      string                `json:"city_path,omitempty"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	Phase         string                `json:"phase,omitempty"`
	Pour          bool                  `json:"pour,omitempty"`
	RootOnly      bool                  `json:"root_only,omitempty"`
	SearchPaths   []string              `json:"search_paths"`
	Vars          []formulaVarJSON      `json:"vars,omitempty"`
	Steps         []formulaStepJSON     `json:"steps"`
	Deps          []formulaDepJSON      `json:"deps,omitempty"`
	ProvidedVars  map[string]string     `json:"provided_vars,omitempty"`
	Warnings      []jsonContractWarning `json:"warnings,omitempty"`
}

type formulaVarJSON struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Default     *string  `json:"default,omitempty"`
	RigDefault  *string  `json:"rig_default,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Type        string   `json:"type,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type formulaStepJSON struct {
	ID          string            `json:"id"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Notes       string            `json:"notes,omitempty"`
	Type        string            `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Assignee    string            `json:"assignee,omitempty"`
	IsRoot      bool              `json:"is_root,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type formulaDepJSON struct {
	StepID      string `json:"step_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type,omitempty"`
	Metadata    string `json:"metadata,omitempty"`
}

type jsonContractWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func listFormulaRows(warningWriter ...io.Writer) (string, []string, []formulaListRowJSON) {
	cityPath, err := resolveCity()
	if err != nil {
		return "", nil, nil
	}
	cfg, err := loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return cityPath, nil, nil
	}
	paths := formulaSearchPathsForList(cfg)

	rows := formulaRowsForSearchPaths(paths)
	return cityPath, paths, rows
}

func formulaRowsForSearchPaths(paths []string) []formulaListRowJSON {
	return formulaRowsForSearchPathsWithSource(formula.FSSource{}, paths)
}

func formulaRowsForSearchPathsWithSource(src formula.Source, paths []string) []formulaListRowJSON {
	winners := formula.ResolveAllWithSource(src, paths)

	names := make([]string, 0, len(winners))
	for name := range winners {
		names = append(names, name)
	}
	slices.Sort(names)

	rows := make([]formulaListRowJSON, 0, len(names))
	for _, name := range names {
		rows = append(rows, formulaListRowJSON{Name: name, Source: winners[name]})
	}
	return rows
}

func formulaCatalogEntries(searchPaths []string) ([]formulaCatalogEntryJSON, []jsonContractWarning) {
	parser := formula.NewParser(searchPaths...).SetSource(formula.SourceFromEnv())
	rows := formulaRowsForSearchPathsWithSource(parser.Source(), searchPaths)

	entries := make([]formulaCatalogEntryJSON, 0, len(rows))
	warnings := make([]jsonContractWarning, 0)
	for _, row := range rows {
		parsed, err := parser.ParseFile(row.Source)
		if err != nil {
			warnings = append(warnings, formulaCatalogWarning("formula_catalog_parse_failed", row.Name, err))
			continue
		}
		if parsed.Catalog == nil {
			continue
		}

		name := strings.TrimSpace(parsed.Catalog.Name)
		if name == "" {
			warnings = append(warnings, formulaCatalogWarning("formula_catalog_invalid_metadata", row.Name, fmt.Errorf("catalog.name is required")))
			continue
		}
		if name != row.Name {
			warnings = append(warnings, formulaCatalogWarning("formula_catalog_invalid_metadata", row.Name, fmt.Errorf("catalog.name %q must match formula name %q", name, row.Name)))
			continue
		}
		description := strings.TrimSpace(parsed.Catalog.Description)
		if description == "" {
			warnings = append(warnings, formulaCatalogWarning("formula_catalog_invalid_metadata", row.Name, fmt.Errorf("catalog.description is required")))
			continue
		}
		entries = append(entries, formulaCatalogEntryJSON{
			Name:        name,
			Description: description,
		})
	}

	slices.SortFunc(entries, func(a, b formulaCatalogEntryJSON) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries, warnings
}

func formulaCatalogWarning(code, name string, err error) jsonContractWarning {
	return jsonContractWarning{
		Code:    code,
		Message: fmt.Sprintf("skipping formula %q: %v", name, err),
	}
}

func formulaCatalogJSONFromEntries(entries []formulaCatalogEntryJSON, warnings []jsonContractWarning) formulaCatalogJSON {
	return formulaCatalogJSON{
		SchemaVersion: "1",
		OK:            true,
		Formulas:      entries,
		Summary:       formulaListSummaryJSON{Count: len(entries)},
		Warnings:      warnings,
	}
}

func formulaSearchPathsForList(cfg *config.City) []string {
	if cfg == nil {
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

func formulaCommandError(stderr io.Writer, command string, jsonOutput bool, err error) error {
	if err == nil || jsonOutput {
		return err
	}
	fmt.Fprintf(stderr, "%s: %v\n", command, err) //nolint:errcheck // best-effort stderr
	return errExit
}

func formulaShowJSONFromRecipe(recipe *formula.Recipe, cityPath string, scope formulaScope, rigVars, providedVars, displayVars map[string]string) formulaShowJSON {
	out := formulaShowJSON{
		SchemaVersion: "1",
		OK:            true,
		CityPath:      cityPath,
		Name:          recipe.Name,
		Description:   recipe.Description,
		Phase:         recipe.Phase,
		Pour:          recipe.Pour,
		RootOnly:      recipe.RootOnly,
		SearchPaths:   scope.searchPaths,
		ProvidedVars:  providedVars,
	}
	if len(displayVars) > 0 {
		out.Description = formula.Substitute(out.Description, displayVars)
	}

	names := recipe.VariableNames()
	out.Vars = make([]formulaVarJSON, 0, len(names))
	for _, name := range names {
		def := recipe.Vars[name]
		if def == nil {
			out.Vars = append(out.Vars, formulaVarJSON{Name: name})
			continue
		}
		row := formulaVarJSON{
			Name:        name,
			Description: def.Description,
			Default:     def.Default,
			Required:    def.Required,
			Type:        def.Type,
			Pattern:     def.Pattern,
			Enum:        def.Enum,
		}
		if v, ok := rigVars[name]; ok {
			rigDefault := v
			row.RigDefault = &rigDefault
		}
		out.Vars = append(out.Vars, row)
	}

	out.Steps = make([]formulaStepJSON, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		row := formulaStepJSON{
			ID:          step.ID,
			Title:       step.Title,
			Description: step.Description,
			Notes:       step.Notes,
			Type:        step.Type,
			Priority:    step.Priority,
			Labels:      step.Labels,
			Assignee:    step.Assignee,
			IsRoot:      step.IsRoot,
			Metadata:    step.Metadata,
		}
		if len(displayVars) > 0 {
			row.Title = formula.Substitute(row.Title, displayVars)
			row.Description = formula.Substitute(row.Description, displayVars)
			row.Notes = formula.Substitute(row.Notes, displayVars)
		}
		out.Steps = append(out.Steps, row)
	}

	out.Deps = make([]formulaDepJSON, 0, len(recipe.Deps))
	for _, dep := range recipe.Deps {
		out.Deps = append(out.Deps, formulaDepJSON{
			StepID:      dep.StepID,
			DependsOnID: dep.DependsOnID,
			Type:        dep.Type,
			Metadata:    dep.Metadata,
		})
	}
	return out
}

func newFormulaCookCmd(stdout, stderr io.Writer) *cobra.Command {
	var title string
	var vars []string
	var metadata []string
	var attach string
	var jsonOutput bool
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
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}
			scope, err := resolveFormulaScope(cfg, cityPath)
			if err != nil {
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}
			store, err := openStoreAtForCity(scope.storeRoot, cityPath)
			if err != nil {
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}

			cookVars := parseFormulaVars(vars)

			if attach != "" {
				recipe, err := formula.CompileWithoutRuntimeVarValidation(cmd.Context(), args[0], scope.searchPaths, cookVars)
				if err != nil {
					return formulaCommandError(stderr, "gc formula cook: compile", jsonOutput, err)
				}

				result, err := molecule.Attach(cmd.Context(), store, recipe, attach, molecule.AttachOptions{
					Title: title,
					Vars:  cookVars,
				})
				if err != nil {
					return formulaCommandError(stderr, "gc formula cook: attach", jsonOutput, err)
				}

				if jsonOutput {
					if err := writeCLIJSONLineOrErr(stdout, stderr, "gc formula cook", formulaCookJSONResult{
						SchemaVersion:  "1",
						OK:             true,
						Formula:        args[0],
						Mode:           "attach",
						AttachBeadID:   attach,
						RootID:         result.RootID,
						WorkflowRootID: result.WorkflowRootID,
						Created:        result.Created,
					}); err != nil {
						return err
					}
					_ = pokeControlDispatch(cityPath)
					return nil
				}
				_, _ = fmt.Fprintf(stdout, "Attached: %s -> %s (root: %s)\n", attach, result.RootID, result.WorkflowRootID)
				_, _ = fmt.Fprintf(stdout, "Root: %s\n", result.RootID)
				_, _ = fmt.Fprintf(stdout, "Created: %d\n", result.Created)

				// Poke control dispatcher to pick up new beads
				_ = pokeControlDispatch(cityPath)
				return nil
			}

			result, err := molecule.Cook(cmd.Context(), store, args[0], scope.searchPaths, molecule.Options{
				Title: title,
				Vars:  cookVars,
			})
			if err != nil {
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}

			rootMeta, err := parseMetadataArgs(metadata)
			if err != nil {
				return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
			}
			if len(rootMeta) > 0 {
				if err := store.SetMetadataBatch(result.RootID, rootMeta); err != nil {
					err := fmt.Errorf("setting root metadata on %s: %w", result.RootID, err)
					return formulaCommandError(stderr, "gc formula cook", jsonOutput, err)
				}
			}

			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc formula cook", formulaCookJSONResult{
					SchemaVersion: "1",
					OK:            true,
					Formula:       args[0],
					Mode:          "cook",
					RootID:        result.RootID,
					Created:       result.Created,
					IDMapping:     result.IDMapping,
				})
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSONL summary")
	return cmd
}

type formulaCookJSONResult struct {
	SchemaVersion  string            `json:"schema_version"`
	OK             bool              `json:"ok"`
	Formula        string            `json:"formula"`
	Mode           string            `json:"mode"`
	AttachBeadID   string            `json:"attach_bead_id,omitempty"`
	RootID         string            `json:"root_id"`
	WorkflowRootID string            `json:"workflow_root_id,omitempty"`
	Created        int               `json:"created"`
	IDMapping      map[string]string `json:"id_mapping,omitempty"`
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

// formulaScope is the resolved rig/city context for a formula invocation.
// searchPaths falls back to city-level layers when the rig has no
// rig-specific entry (see FormulaLayers.SearchPaths).
type formulaScope struct {
	storeRoot   string
	searchPaths []string
}

// resolveFormulaScope determines the rig (if any) under which a formula
// invocation should run. Priority: --rig flag > enclosing rig from cwd >
// city.
func resolveFormulaScope(cfg *config.City, cityPath string) (formulaScope, error) {
	if name := strings.TrimSpace(rigFlag); name != "" {
		rig, ok := rigByName(cfg, name)
		if !ok {
			return formulaScope{}, fmt.Errorf("rig %q not found", name)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return formulaScope{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it", rig.Name, rig.Name)
		}
		return rigFormulaScope(cfg, cityPath, rig), nil
	}

	if cwd, err := os.Getwd(); err == nil {
		// resolveRigForDir already filters unbound rigs (see
		// rig_scope_resolution.go), so a true return guarantees rig.Path is
		// non-empty.
		if rig, ok, rerr := resolveRigForDir(cfg, cityPath, cwd); rerr != nil {
			return formulaScope{}, rerr
		} else if ok {
			return rigFormulaScope(cfg, cityPath, rig), nil
		}
	}

	return formulaScope{
		storeRoot:   cityPath,
		searchPaths: cfg.FormulaLayers.City,
	}, nil
}

func rigFormulaScope(cfg *config.City, cityPath string, rig config.Rig) formulaScope {
	return formulaScope{
		storeRoot:   resolveStoreScopeRoot(cityPath, rig.Path),
		searchPaths: cfg.FormulaLayers.SearchPaths(rig.Name),
	}
}

// rigFormulaVarsForScope returns rig-scoped formula var defaults for the
// active scope (honoring --rig and cwd). Returns an empty map when no rig
// context is active so callers can treat the result as read-only
// annotations without nil checks.
func rigFormulaVarsForScope(cfg *config.City, cityPath string) map[string]string {
	if cfg == nil {
		return map[string]string{}
	}
	if name := strings.TrimSpace(rigFlag); name != "" {
		if rig, ok := rigByName(cfg, name); ok {
			return cloneStringMap(rig.FormulaVars)
		}
		return map[string]string{}
	}
	if cwd, err := os.Getwd(); err == nil {
		if rig, ok, rerr := resolveRigForDir(cfg, cityPath, cwd); rerr == nil && ok {
			return cloneStringMap(rig.FormulaVars)
		}
	}
	return map[string]string{}
}
