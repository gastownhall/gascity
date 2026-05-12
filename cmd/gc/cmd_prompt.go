package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// synthTimeout caps the LLM subprocess for `gc prompt synth`. Generation
// can be slow (large outputs, slow models) but should never block forever.
const synthTimeout = 5 * time.Minute

// promptSynthRunner runs the configured provider as a one-shot subprocess
// with the rendered meta-prompt and returns its stdout. Defined as a
// function type so tests can inject a fake.
type promptSynthRunner func(ctx context.Context, provider *config.ResolvedProvider, prompt, workDir string) (string, error)

// promptSynthOpts holds the parsed flags for `gc prompt synth`.
type promptSynthOpts struct {
	role               string
	provider           string
	rig                string
	writerAgent        string
	write              bool
	force              bool
	city               string
	metaPromptOverride string
}

// metaPromptCtx is the data passed to the meta-prompt template at render
// time. The meta-prompt uses [[ ]] delimiters so its body can mention
// literal {{ }} that the LLM should reproduce in its output.
type metaPromptCtx struct {
	Role                string
	ProviderKey         string
	ProviderDisplayName string

	// ContextType discriminates the agent's runtime scope: "rig" when
	// the agent is attached to a registered project repository,
	// "city" when the agent is HQ-only (mayor, deacon, etc.).
	ContextType string

	// City* fields are populated regardless of ContextType — every
	// agent runs inside a city.
	CityName string
	CityPath string

	// Rig* fields are populated only when ContextType == "rig".
	RigName          string
	RigPath          string
	RigDefaultBranch string

	// Baseline carries the existing prompt content (if any) for the
	// LLM to refine rather than design from scratch. BaselineSource
	// records where it came from so the meta-prompt can be honest about
	// whether this is "the current template" or "a structural reference
	// from another role".
	Baseline       string
	BaselineSource string
	HasOwnBaseline bool
}

func newPromptCmd(stdout, stderr io.Writer) *cobra.Command {
	var cmd *cobra.Command
	cmd = &cobra.Command{
		Use:   "prompt",
		Short: "Author and inspect agent prompt templates",
		Long: `Subcommands for authoring agent prompt templates.

Currently the only subcommand is 'synth', which invokes the configured
provider in one-shot mode to generate a prompt template for a given role.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc prompt: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.AddCommand(newPromptSynthCmd(stdout, stderr, defaultPromptSynthRunner))
	return cmd
}

func newPromptSynthCmd(stdout, stderr io.Writer, runner promptSynthRunner) *cobra.Command {
	opts := promptSynthOpts{}
	cmd := &cobra.Command{
		Use:   "synth",
		Short: "Generate an agent prompt template by invoking the LLM",
		Long: `Renders a meta-prompt with the given parameters, invokes the configured
provider in one-shot mode, and emits the generated prompt template.

The default behaviour prints the generated prompt to stdout. Pass --write
to save it directly to <city>/agents/<role>/prompt.template.md (use --force
to overwrite an existing file).

Context type is determined by --rig:

  (no --rig)     City context. The agent is HQ-only and operates at
                 the city level (e.g. mayor, deacon). The meta-prompt
                 emphasizes coordination, dispatch, monitoring.
  --rig <name>   Rig context. The agent is attached to the named rig
                 (looked up in city.toml). The meta-prompt includes
                 the rig path, default branch, and project-aware
                 guidance (git operations, branch management, etc.).

Auto-detection:
  --provider     defaults to workspace.provider in city.toml

Baseline:
  The synth pulls in an existing prompt template as a refinement
  baseline so the LLM iterates on a known-good shape rather than
  designing from scratch. Resolution priority:
    1. <city>/agents/<role>/prompt.template.md     (user customization)
    2. <city>/.gc/system/packs/*/agents/<role>/    (pack default)
    3. embedded prompts/<role>.md                  (built-in fallback)
    4. embedded prompts/mayor.md                   (structural reference,
                                                     used only when no
                                                     role-specific source
                                                     exists)

Two execution modes are planned:

  --writer-agent ""        Direct mode (default). Spawns a one-shot
                           subprocess of the configured provider; no
                           Gas City agent is involved. Useful for
                           bootstrap and offline-friendly invocations.

  --writer-agent <name>    Slingued mode (NOT YET IMPLEMENTED, returns
                           an error). Will sling the synth as work to
                           the named agent via mol-prompt-synth, with
                           the result written by the agent's session.

The output is LLM-generated. Review it carefully before relying on it.
When --write is used, a comment header records the inputs and generation
date for traceability.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runPromptSynth(cmd.Context(), opts, runner, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc prompt synth: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.role, "role", "", "agent role to design (required, e.g. mayor, polecat, witness)")
	cmd.Flags().StringVar(&opts.provider, "provider", "", "target AI provider key (default: city.toml workspace.provider)")
	cmd.Flags().StringVar(&opts.rig, "rig", "", "rig name from city.toml (default: empty = city/HQ context, no rig)")
	cmd.Flags().StringVar(&opts.writerAgent, "writer-agent", "", "Gas City agent to delegate the synth to (default: empty = direct mode, no agent). NOTE: slingued mode not yet implemented")
	cmd.Flags().BoolVar(&opts.write, "write", false, "write to <city>/agents/<role>/prompt.template.md instead of stdout")
	cmd.Flags().BoolVar(&opts.force, "force", false, "with --write, overwrite the destination if it exists")
	cmd.Flags().StringVar(&opts.city, "city", "", "city path (default: auto-resolve)")
	cmd.Flags().StringVar(&opts.metaPromptOverride, "meta-prompt", "", "override the embedded meta-prompt with a file path")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func runPromptSynth(ctx context.Context, opts promptSynthOpts, runner promptSynthRunner, stdout, stderr io.Writer) error {
	if strings.TrimSpace(opts.writerAgent) != "" {
		// Slingued mode lives in step 3 (mol-prompt-synth formula +
		// auto-trigger from `gc agent add --synth`). The flag is wired
		// up here so the CLI surface stays stable across releases —
		// scripts and users can target the final form without having
		// to update later. Until step 3 lands, fail loudly rather than
		// silently fall back to direct mode (would mask user intent).
		return fmt.Errorf("--writer-agent=%q: slingued mode not yet implemented (planned for the next PR); use --writer-agent='' for direct mode", opts.writerAgent)
	}
	cityPath, err := resolveCityForSynth(opts.city)
	if err != nil {
		return fmt.Errorf("resolve city: %w", err)
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return fmt.Errorf("load city config: %w", err)
	}

	providerKey := strings.TrimSpace(opts.provider)
	if providerKey == "" {
		providerKey = strings.TrimSpace(cfg.Workspace.Provider)
	}
	if providerKey == "" {
		return errors.New("no provider specified and city.toml has no workspace.provider; pass --provider")
	}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: providerKey}, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return fmt.Errorf("resolve provider %q: %w", providerKey, err)
	}
	if len(resolved.PrintArgs) == 0 {
		return fmt.Errorf("provider %q does not support one-shot mode (no print_args configured)", resolved.Name)
	}

	role := strings.TrimSpace(opts.role)
	mctx := metaPromptCtx{
		Role:                role,
		ProviderKey:         resolved.Name,
		ProviderDisplayName: providerDisplayNameFor(resolved.Name, cfg.Providers),
		CityName:            strings.TrimSpace(cfg.Workspace.Name),
		CityPath:            cityPath,
	}
	if mctx.CityName == "" {
		mctx.CityName = filepath.Base(cityPath)
	}

	if rigName := strings.TrimSpace(opts.rig); rigName != "" {
		rig := findRigByName(rigName, cfg.Rigs)
		if rig == nil {
			return fmt.Errorf("rig %q not found in city.toml; known: %s", rigName, knownRigNames(cfg.Rigs))
		}
		mctx.ContextType = "rig"
		mctx.RigName = rig.Name
		mctx.RigPath = rig.Path
		mctx.RigDefaultBranch = rig.EffectiveDefaultBranch()
	} else {
		mctx.ContextType = "city"
	}

	mctx.Baseline, mctx.BaselineSource, mctx.HasOwnBaseline = loadBaselinePrompt(cityPath, role)

	metaSource := metaAgentAuthorPrompt
	if opts.metaPromptOverride != "" {
		metaSource, err = os.ReadFile(opts.metaPromptOverride)
		if err != nil {
			return fmt.Errorf("read meta-prompt override: %w", err)
		}
	}
	rendered, err := renderMetaPrompt(string(metaSource), mctx)
	if err != nil {
		return fmt.Errorf("render meta-prompt: %w", err)
	}

	// Working directory for the subprocess: rig path when targeting a
	// rig (so the LLM has CWD context for any tools it might invoke),
	// otherwise the city path.
	workDir := mctx.CityPath
	if mctx.ContextType == "rig" && mctx.RigPath != "" {
		workDir = mctx.RigPath
	}
	callCtx, cancel := context.WithTimeout(ctx, synthTimeout)
	defer cancel()
	out, err := runner(callCtx, resolved, rendered, workDir)
	if err != nil {
		return fmt.Errorf("synth via %s: %w", resolved.Command, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return errors.New("provider returned empty output")
	}

	if opts.write {
		dst, err := writePromptOutput(cityPath, role, opts.force, mctx, out)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "gc prompt synth: wrote %s — review before use\n", dst) //nolint:errcheck // best-effort stderr
		return nil
	}
	if _, err := fmt.Fprintln(stdout, out); err != nil {
		return err
	}
	return nil
}

func resolveCityForSynth(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	return resolveCity()
}

// renderMetaPrompt parses source as a Go text/template with [[ ]]
// delimiters and executes it against ctx. The non-default delimiters let
// the meta-prompt body reference literal {{ }} (Gas City template syntax)
// without escaping.
func renderMetaPrompt(source string, ctx metaPromptCtx) (string, error) {
	t, err := template.New("meta").Delims("[[", "]]").Parse(source)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// writePromptOutput writes body to <cityPath>/agents/<role>/prompt.template.md.
// When force is false and the destination exists, returns an error rather
// than clobbering. Prepends a comment header recording the synth inputs
// (role, provider, context type, baseline source) for traceability.
func writePromptOutput(cityPath, role string, force bool, mctx metaPromptCtx, body string) (string, error) {
	dst := filepath.Join(cityPath, "agents", role, "prompt.template.md")
	if !force {
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("destination %s exists; pass --force to overwrite", dst)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	contextLine := fmt.Sprintf("city %q (%s)", mctx.CityName, mctx.CityPath)
	if mctx.ContextType == "rig" {
		contextLine = fmt.Sprintf("rig %q at %s (city %q)", mctx.RigName, mctx.RigPath, mctx.CityName)
	}
	baselineLine := "none"
	if mctx.BaselineSource != "" {
		baselineLine = mctx.BaselineSource
	}
	header := fmt.Sprintf(`<!--
Generated by `+"`"+`gc prompt synth`+"`"+` on %s.
  role:     %s
  provider: %s (%s)
  context:  %s
  baseline: %s
LLM-generated content. Review carefully before relying on it.
-->

`, time.Now().UTC().Format("2006-01-02"), role, mctx.ProviderKey, mctx.ProviderDisplayName, contextLine, baselineLine)
	if err := os.WriteFile(dst, []byte(header+body+"\n"), 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// findRigByName returns the matching rig (by Name) from the configured
// list, or nil when none matches.
func findRigByName(name string, rigs []config.Rig) *config.Rig {
	for i := range rigs {
		if rigs[i].Name == name {
			return &rigs[i]
		}
	}
	return nil
}

// knownRigNames returns a comma-separated list of configured rig names
// for use in error messages.
func knownRigNames(rigs []config.Rig) string {
	names := make([]string, 0, len(rigs))
	for i := range rigs {
		names = append(names, rigs[i].Name)
	}
	if len(names) == 0 {
		return "(none configured)"
	}
	return strings.Join(names, ", ")
}

// loadBaselinePrompt finds an existing prompt template to feed the LLM
// as a refinement baseline. Returns the content, a human-readable
// source descriptor (for the meta-prompt and the file header), and a
// flag indicating whether the baseline is role-specific (vs a
// structural reference borrowed from another role).
//
// Resolution priority:
//  1. <cityPath>/agents/<role>/prompt.template.md (user customization)
//  2. <cityPath>/.gc/system/packs/<any>/agents/<role>/prompt.template.md (pack default)
//  3. embedded prompts/<role>.md (built-in fallback, only mayor today)
//  4. embedded prompts/mayor.md (structural reference, last resort)
func loadBaselinePrompt(cityPath, role string) (content, source string, ownToRole bool) {
	if role == "" {
		return "", "", false
	}

	// 1. User customization in the city.
	userFile := filepath.Join(cityPath, "agents", role, "prompt.template.md")
	if data, err := os.ReadFile(userFile); err == nil {
		return string(data), fmt.Sprintf("city customization at agents/%s/prompt.template.md", role), true
	}

	// 2. Pack defaults — scan all materialized packs.
	packGlob := filepath.Join(cityPath, ".gc", "system", "packs", "*", "agents", role, "prompt.template.md")
	if matches, err := filepath.Glob(packGlob); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			if data, err := os.ReadFile(m); err == nil {
				rel, _ := filepath.Rel(cityPath, m)
				return string(data), "pack default at " + rel, true
			}
		}
	}

	// 3. Embedded role-specific default.
	if data, err := defaultPrompts.ReadFile("prompts/" + role + ".md"); err == nil {
		return string(data), "embedded prompts/" + role + ".md", true
	}

	// 4. Embedded mayor as structural reference (only if role != "mayor"
	// — otherwise we'd have returned at step 3).
	if role != "mayor" {
		if data, err := defaultPrompts.ReadFile("prompts/mayor.md"); err == nil {
			return string(data), "embedded prompts/mayor.md (structural reference; no default exists for " + role + ")", false
		}
	}

	return "", "", false
}

// defaultPromptSynthRunner runs the configured provider one-shot via
// exec.CommandContext. Mirrors the pattern in
// internal/api/title_generate.go but uses the full synthTimeout and
// surfaces stderr in the error so failures are diagnosable.
func defaultPromptSynthRunner(ctx context.Context, provider *config.ResolvedProvider, prompt, workDir string) (string, error) {
	if provider == nil {
		return "", errors.New("nil provider")
	}
	args := append([]string(nil), provider.Args...)
	args = append(args, provider.PrintArgs...)
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, provider.Command, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return "", fmt.Errorf("%w (stderr: %s)", err, stderrText)
		}
		return "", err
	}
	return stdout.String(), nil
}
