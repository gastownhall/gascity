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
	// Stderr receives non-fatal diagnostics. Defaults to os.Stderr.
	Stderr io.Writer
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
		return true
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
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	wsDir := filepath.Join(opts.OutputDir, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return fmt.Errorf("creating workspace dir: %w", err)
	}

	// Copy city directory contents into workspace, excluding runtime state.
	if err := copyDirFiltered(opts.CityPath, wsDir, stderr); err != nil {
		return fmt.Errorf("copying city to workspace: %w", err)
	}

	// Copy rig paths into workspace.
	for rigName, rigPath := range opts.RigPaths {
		rigDst := filepath.Join(wsDir, rigName)
		if err := copyDirFiltered(rigPath, rigDst, stderr); err != nil {
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
// File symlinks are dereferenced and copied with the resolved file's mode,
// unless the resolved target is excluded. Directory symlinks are skipped with
// a diagnostic written to stderr. Broken symlinks are skipped; other
// resolution errors are returned.
func copyDirFiltered(src, dst string, stderr io.Writer) error {
	if stderr == nil {
		stderr = os.Stderr
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolving source path %q: %w", src, err)
	}
	src = absSrc

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

		if info.Mode()&os.ModeSymlink != 0 {
			resolvedPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil // broken symlink, skip
				}
				return fmt.Errorf("resolving symlink %q: %w", path, err)
			}
			resolved, err := os.Stat(resolvedPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil // broken symlink, skip
				}
				return fmt.Errorf("stat resolved symlink %q -> %q: %w", path, resolvedPath, err)
			}
			if resolved.IsDir() {
				fmt.Fprintf(stderr, "skipping symlinked directory %s -> %s in build context\n", path, resolvedPath) //nolint:errcheck // best-effort diagnostic
				return nil
			}
			resolvedRel, err := filepath.Rel(src, resolvedPath)
			if err != nil {
				return fmt.Errorf("rel resolved symlink %q -> %q: %w", path, resolvedPath, err)
			}
			if excludedPath(resolvedRel) || excludedResolvedPath(resolvedRel) {
				return nil
			}
			return copyFile(resolvedPath, target, resolved.Mode())
		}

		return copyFile(path, target, info.Mode())
	})
}

func excludedResolvedPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for i, part := range parts {
		if part != citylayout.RuntimeRoot || i == len(parts)-1 {
			continue
		}
		if excludedPath(filepath.Join(parts[i:]...)) {
			return true
		}
	}
	return false
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
