// Command gen-command-census validates the committed Cobra census and
// deterministically regenerates its typed runtime table, product-metrics
// decode catalog, and public example schema enum.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gastownhall/gascity/internal/commandcensus"
)

type generatorOptions struct {
	Root  string
	Check bool
}

func main() {
	check := flag.Bool("check", false, "fail if committed generated artifacts are stale")
	root := flag.String("root", "", "repository root (defaults to the generator source root)")
	flag.Parse()
	resolvedRoot := *root
	if resolvedRoot == "" {
		_, source, _, ok := runtime.Caller(0)
		if !ok {
			fmt.Fprintln(os.Stderr, "gen-command-census: cannot locate source root")
			os.Exit(1)
		}
		resolvedRoot = filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	}
	if err := runGenerator(generatorOptions{Root: resolvedRoot, Check: *check}); err != nil {
		fmt.Fprintln(os.Stderr, "gen-command-census:", err)
		os.Exit(1)
	}
}

func runGenerator(options generatorOptions) error {
	paths := struct {
		manifest string
		runtime  string
		catalog  string
		schema   string
	}{
		manifest: filepath.Join(options.Root, "cmd/gc/productmetrics_command_census.json"),
		runtime:  filepath.Join(options.Root, "cmd/gc/metrics_census_gen.go"),
		catalog:  filepath.Join(options.Root, "internal/productmetrics/command_ids_gen.go"),
		schema:   filepath.Join(options.Root, "schemas/metrics/example/result.schema.json"),
	}
	manifestData, err := os.ReadFile(paths.manifest)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := commandcensus.DecodeManifest(manifestData)
	if err != nil {
		return err
	}
	if err := commandcensus.ValidateManifest(manifest); err != nil {
		return err
	}
	schemaData, err := os.ReadFile(paths.schema)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	existingCatalog, err := os.ReadFile(paths.catalog)
	if err != nil {
		return fmt.Errorf("read generated catalog: %w", err)
	}
	previous, err := commandcensus.ParseGeneratedAllocationLedger(existingCatalog)
	if err != nil {
		return err
	}
	if err := commandcensus.ValidateEvolution(previous, manifest); err != nil {
		return err
	}
	artifacts, err := commandcensus.GenerateArtifacts(manifest, schemaData)
	if err != nil {
		return err
	}

	outputs := []struct {
		path string
		data []byte
	}{
		{path: paths.runtime, data: []byte(artifacts.RuntimeGo)},
		{path: paths.catalog, data: []byte(artifacts.CatalogGo)},
		{path: paths.schema, data: []byte(artifacts.SchemaJSON)},
	}
	if options.Check {
		for _, output := range outputs {
			committed, err := os.ReadFile(output.path)
			if err != nil {
				return fmt.Errorf("read %s: %w", output.path, err)
			}
			if !bytes.Equal(committed, output.data) {
				return fmt.Errorf("generated artifact %s is stale; run go run ./cmd/gen-command-census", output.path)
			}
		}
		return nil
	}
	for _, output := range outputs {
		if err := atomicWriteGeneratedFile(output.path, output.data); err != nil {
			return fmt.Errorf("write %s: %w", output.path, err)
		}
	}
	return nil
}

func atomicWriteGeneratedFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".gen-command-census-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
