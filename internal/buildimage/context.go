package buildimage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// Options configures the build context assembly.
type Options struct {
	// CityPath is the resolved city directory on disk.
	CityPath string
	// OutputDir is where to write the build context (Dockerfile + workspace/).
	OutputDir string
	// BaseImage is the Docker base image. Default: "gc-agent:latest".
	BaseImage string
	// Tag is the image tag for docker build.
	Tag string
	// RigPaths maps rig name → local repo path for baking rig content.
	RigPaths map[string]string
}

// Manifest records what was baked into the image for debugging.
type Manifest struct {
	Version   int       `json:"version"`
	CityName  string    `json:"city_name"`
	Built     time.Time `json:"built"`
	BaseImage string    `json:"base_image"`
}

// excludedPaths returns true for paths that should never be baked.
func excludedPath(rel string) bool {
	if rel == citylayout.RuntimeRoot {
		return false
	}
	if strings.HasPrefix(rel, citylayout.RuntimeRoot+"/") {
		switch {
		case rel == citylayout.LegacyClaudeHookFile:
			return false
		case rel == citylayout.LegacyPromptsRoot,
			strings.HasPrefix(rel, citylayout.LegacyPromptsRoot+"/"):
			return false
		case rel == citylayout.LegacyFormulasRoot,
			strings.HasPrefix(rel, citylayout.LegacyFormulasRoot+"/"):
			return false
		case rel == citylayout.LegacyScriptsRoot,
			strings.HasPrefix(rel, citylayout.LegacyScriptsRoot+"/"):
			return false
		default:
			return true
		}
	}
	// Runtime state files.
	if rel == ".gc/controller.lock" || rel == ".gc/controller.sock" ||
		rel == ".gc/events.jsonl" {
		return true
	}
	// Agent registry (runtime state).
	if strings.HasPrefix(rel, ".gc/agents/") {
		return true
	}
	// Secrets: match exact base names and specific extensions, not substrings.
	base := filepath.Base(rel)
	ext := filepath.Ext(base)
	if base == ".env" || base == "credentials.json" || base == "credentials.yaml" ||
		base == "credentials.yml" || ext == ".secret" || ext == ".pem" || ext == ".key" {
		return true
	}
	return false
}

// AssembleContext builds the Docker build context directory.
// It creates outputDir/workspace/ with city content and outputDir/Dockerfile.
func AssembleContext(opts Options) error {
	if opts.CityPath == "" {
		return fmt.Errorf("city path is required")
	}
	if opts.OutputDir == "" {
		return fmt.Errorf("output dir is required")
	}
	if opts.BaseImage == "" {
		opts.BaseImage = "gc-agent:latest"
	}

	wsDir := filepath.Join(opts.OutputDir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("creating workspace dir: %w", err)
	}

	// Copy city directory contents into workspace, excluding runtime state.
	if err := copyDirFiltered(opts.CityPath, wsDir); err != nil {
		return fmt.Errorf("copying city to workspace: %w", err)
	}

	// Copy rig paths into workspace.
	for rigName, rigPath := range opts.RigPaths {
		rigDst := filepath.Join(wsDir, rigName)
		if err := copyDirFiltered(rigPath, rigDst); err != nil {
			return fmt.Errorf("copying rig %q: %w", rigName, err)
		}
	}

	// Write prebaked manifest.
	cityName := filepath.Base(opts.CityPath)
	manifest := Manifest{
		Version:   1,
		CityName:  cityName,
		Built:     time.Now().UTC(),
		BaseImage: opts.BaseImage,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, ".gc-prebaked"), manifestData, 0o644); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}

	// Generate Dockerfile.
	dockerfile := GenerateDockerfile(opts.BaseImage)
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "Dockerfile"), dockerfile, 0o644); err != nil {
		return fmt.Errorf("writing Dockerfile: %w", err)
	}

	return nil
}

// copyDirFiltered copies src directory to dst, skipping excluded paths.
func copyDirFiltered(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		fullRel, err := filepath.Rel(filepath.Dir(src), path)
		if err != nil {
			return err
		}
		if excludedPath(rel) || excludedPath(fullRel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		return copyFile(path, target, info.Mode())
	})
}

// copyFile copies a single file.
func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
