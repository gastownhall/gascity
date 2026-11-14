// Command gen-schema-baseline seeds and extends the committed schema
// contract baseline from the built-in schemas. It only appends: a breaking
// change stays visible to the CI gate until the author bumps schema_version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/schemacontract"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gen-schema-baseline:", err)
		os.Exit(1)
	}
}

// run resolves the repository root from -root or BUILD_WORKSPACE_DIRECTORY,
// merges the live schemas into the baseline file, and writes it atomically
// only when the merge succeeded for every schema.
func run(args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet("gen-schema-baseline", flag.ContinueOnError)
	rootFlag := flags.String("root", "", "repository root (defaults to BUILD_WORKSPACE_DIRECTORY)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	root := *rootFlag
	if root == "" {
		root = getenv("BUILD_WORKSPACE_DIRECTORY")
	}
	if root == "" {
		return errors.New("no repository root: pass -root or run with `bazel run //cmd/gen-schema-baseline`")
	}
	live, err := schemacontract.ExtractAll(os.DirFS(filepath.Join(root, "schemas")))
	if err != nil {
		return fmt.Errorf("extracting schemas: %w", err)
	}
	path := filepath.Join(root, schemacontract.BaselinePath)
	baseline := schemacontract.Baseline{Format: 1}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if baseline, err = schemacontract.ParseBaseline(existing); err != nil {
			return fmt.Errorf("reading %s: %w", schemacontract.BaselinePath, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("reading %s: %w", schemacontract.BaselinePath, err)
	}
	merged, summary, err := schemacontract.Merge(baseline, live)
	if err != nil {
		return err
	}
	data, err := schemacontract.Marshal(merged)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "gen-schema-baseline: seeded %d, entries added %d, records appended %d\n",
		summary.Seeded, summary.EntriesAdded, summary.RecordsAppended)
	return err
}

// writeAtomic replaces path with data through a temp file and a rename.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".baseline-*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // best-effort cleanup after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the one to report
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
