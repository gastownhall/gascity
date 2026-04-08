package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/buildimage"
	"github.com/spf13/cobra"
)

func newBuildImageCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		tag         string
		baseImage   string
		rigPaths    []string
		push        bool
		contextOnly bool
	)

	cmd := &cobra.Command{
		Use:   "build-image [city-path]",
		Short: "Build a prebaked agent container image",
		Long: `Assemble a Docker build context from city config, prompts, formulas,
and rig content, then build a container image with everything pre-staged.

Pods using the prebaked image skip init containers and file staging,
reducing startup from 30-60s to seconds. Configure with prebaked = true
in [session.k8s].

Secrets (Claude credentials) are never baked — they stay as K8s Secret
volume mounts at runtime.`,
		Example: `  # Build context only (no docker build)
  gc build-image ~/bright-lights --context-only

  # Build and tag image
  gc build-image ~/bright-lights --tag my-city:latest

  # Build with rig content baked in
  gc build-image ~/bright-lights --tag my-city:latest --rig-path demo:/path/to/demo

  # Build and push to registry
  gc build-image ~/bright-lights --tag registry.io/my-city:latest --push`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			code := doBuildImage(args, tag, baseImage, rigPaths, push, contextOnly, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&tag, "tag", "", "image tag (required unless --context-only)")
	cmd.Flags().StringVar(&baseImage, "base-image", "gc-agent:latest", "base Docker image")
	cmd.Flags().StringSliceVar(&rigPaths, "rig-path", nil, "rig name:path pairs (repeatable)")
	cmd.Flags().BoolVar(&push, "push", false, "push image after building")
	cmd.Flags().BoolVar(&contextOnly, "context-only", false, "write build context without running docker build")

	return cmd
}

func doBuildImage(args []string, tag, baseImage string, rigPaths []string, push, contextOnly bool, stdout, stderr io.Writer) int {
	if !contextOnly && tag == "" {
		fmt.Fprintln(stderr, "gc build-image: --tag is required (or use --context-only)") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Resolve city path.
	var cityPath string
	if len(args) > 0 {
		cityPath = args[0]
	} else {
		var err error
		cityPath, err = resolveCommandCity(nil)
		if err != nil {
			fmt.Fprintf(stderr, "gc build-image: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Parse rig-path flags (name:path format).
	rigs := make(map[string]string)
	for _, rp := range rigPaths {
		name, path, ok := strings.Cut(rp, ":")
		if !ok || name == "" || path == "" {
			fmt.Fprintf(stderr, "gc build-image: invalid --rig-path %q (expected name:path)\n", rp) //nolint:errcheck // best-effort stderr
			return 1
		}
		rigs[name] = path
	}

	// Create temp output dir (or use a named one for context-only).
	outputDir, err := os.MkdirTemp("", "gc-build-image-*")
	if err != nil {
		fmt.Fprintf(stderr, "gc build-image: creating temp dir: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !contextOnly {
		defer func() { _ = os.RemoveAll(outputDir) }()
	}

	// Assemble build context.
	opts := buildimage.Options{
		CityPath:  cityPath,
		OutputDir: outputDir,
		BaseImage: baseImage,
		Tag:       tag,
		RigPaths:  rigs,
	}
	if err := buildimage.AssembleContext(opts); err != nil {
		fmt.Fprintf(stderr, "gc build-image: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if contextOnly {
		fmt.Fprintf(stdout, "Build context written to: %s\n", outputDir) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Build image.
	fmt.Fprintf(stdout, "Building image %s...\n", tag) //nolint:errcheck // best-effort stdout
	ctx := context.Background()
	if err := buildimage.Build(ctx, outputDir, tag, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "gc build-image: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Image built: %s\n", tag) //nolint:errcheck // best-effort stdout

	// Push if requested.
	if push {
		fmt.Fprintf(stdout, "Pushing %s...\n", tag) //nolint:errcheck // best-effort stdout
		if err := buildimage.Push(ctx, tag, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "gc build-image: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Pushed: %s\n", tag) //nolint:errcheck // best-effort stdout
	}

	return 0
}
