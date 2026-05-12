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
	project            string
	projectName        string
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
	ProjectPath         string
	ProjectName         string
	CityRoot            string
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

Auto-detection:
  --provider     defaults to workspace.provider in city.toml
  --project      defaults to current working directory
  --project-name defaults to basename(--project)

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
	cmd.Flags().StringVar(&opts.project, "project", "", "project root path (default: cwd)")
	cmd.Flags().StringVar(&opts.projectName, "project-name", "", "project display name (default: basename of --project)")
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

	projectPath := strings.TrimSpace(opts.project)
	if projectPath == "" {
		projectPath, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
	}
	projectPath, err = filepath.Abs(projectPath)
	if err != nil {
		return fmt.Errorf("abs project path: %w", err)
	}
	projectName := strings.TrimSpace(opts.projectName)
	if projectName == "" {
		projectName = filepath.Base(projectPath)
	}

	metaSource := metaAgentAuthorPrompt
	if opts.metaPromptOverride != "" {
		metaSource, err = os.ReadFile(opts.metaPromptOverride)
		if err != nil {
			return fmt.Errorf("read meta-prompt override: %w", err)
		}
	}
	displayName := providerDisplayNameFor(resolved.Name, cfg.Providers)
	rendered, err := renderMetaPrompt(string(metaSource), metaPromptCtx{
		Role:                strings.TrimSpace(opts.role),
		ProviderKey:         resolved.Name,
		ProviderDisplayName: displayName,
		ProjectPath:         projectPath,
		ProjectName:         projectName,
		CityRoot:            cityPath,
	})
	if err != nil {
		return fmt.Errorf("render meta-prompt: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, synthTimeout)
	defer cancel()
	out, err := runner(callCtx, resolved, rendered, projectPath)
	if err != nil {
		return fmt.Errorf("synth via %s: %w", resolved.Command, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return errors.New("provider returned empty output")
	}

	if opts.write {
		dst, err := writePromptOutput(cityPath, opts.role, opts.force, projectPath, projectName, resolved.Name, displayName, out)
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
// than clobbering. Prepends a comment header recording the synth inputs.
func writePromptOutput(cityPath, role string, force bool, projectPath, projectName, providerKey, providerDisplayName, body string) (string, error) {
	dst := filepath.Join(cityPath, "agents", role, "prompt.template.md")
	if !force {
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("destination %s exists; pass --force to overwrite", dst)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	header := fmt.Sprintf(`<!--
Generated by `+"`"+`gc prompt synth`+"`"+` on %s.
  role:     %s
  provider: %s (%s)
  project:  %s (%s)
LLM-generated content. Review carefully before relying on it.
-->

`, time.Now().UTC().Format("2006-01-02"), role, providerKey, providerDisplayName, projectName, projectPath)
	if err := os.WriteFile(dst, []byte(header+body+"\n"), 0o644); err != nil {
		return "", err
	}
	return dst, nil
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
