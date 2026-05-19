package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newDoltConfigCmd(_ io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dolt-config",
		Short:  "Internal Dolt config helpers",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var (
		configFile   string
		host         string
		port         string
		dataDir      string
		logLevel     string
		archiveLevel int
		cityPath     string
		scopeDir     string
		issuePrefix  string
		doltDatabase string
	)

	writeManaged := &cobra.Command{
		Use:    "write-managed",
		Short:  "Write a managed Dolt SQL config file",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := writeManagedDoltConfigFile(configFile, host, port, dataDir, logLevel, archiveLevel); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config write-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	writeManaged.Flags().StringVar(&configFile, "file", "", "path to dolt-config.yaml")
	writeManaged.Flags().StringVar(&host, "host", "", "listener host")
	writeManaged.Flags().StringVar(&port, "port", "", "listener port")
	writeManaged.Flags().StringVar(&dataDir, "data-dir", "", "Dolt data directory")
	writeManaged.Flags().StringVar(&logLevel, "log-level", "warning", "Dolt log level")
	writeManaged.Flags().IntVar(&archiveLevel, "archive-level", 0, "Dolt auto_gc archive_level (0=off, 1=on)")
	_ = writeManaged.MarkFlagRequired("file")
	_ = writeManaged.MarkFlagRequired("host")
	_ = writeManaged.MarkFlagRequired("port")
	_ = writeManaged.MarkFlagRequired("data-dir")
	cmd.AddCommand(writeManaged)

	normalizeScope := &cobra.Command{
		Use:    "normalize-scope",
		Short:  "Normalize canonical bd scope files after backend init",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cityPath == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --city") //nolint:errcheck
				return errExit
			}
			if scopeDir == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --dir") //nolint:errcheck
				return errExit
			}
			if issuePrefix == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --prefix") //nolint:errcheck
				return errExit
			}
			if err := normalizeCanonicalBdScopeFilesForInit(cityPath, scopeDir, issuePrefix, doltDatabase); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config normalize-scope: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := removeScopeLocalDoltServerArtifacts(scopeDir); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config normalize-scope: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	normalizeScope.Flags().StringVar(&cityPath, "city", "", "city root")
	normalizeScope.Flags().StringVar(&scopeDir, "dir", "", "scope root to normalize")
	normalizeScope.Flags().StringVar(&issuePrefix, "prefix", "", "scope issue prefix")
	normalizeScope.Flags().StringVar(&doltDatabase, "dolt-database", "", "pinned Dolt database")
	_ = normalizeScope.MarkFlagRequired("city")
	_ = normalizeScope.MarkFlagRequired("dir")
	_ = normalizeScope.MarkFlagRequired("prefix")
	cmd.AddCommand(normalizeScope)
	return cmd
}

func writeManagedDoltConfigFile(path, host, port, dataDir, logLevel string, archiveLevel int) error {
	if path == "" {
		return fmt.Errorf("missing --file")
	}
	if host == "" {
		return fmt.Errorf("missing --host")
	}
	if port == "" {
		return fmt.Errorf("missing --port")
	}
	if dataDir == "" {
		return fmt.Errorf("missing --data-dir")
	}
	if logLevel == "" {
		logLevel = "warning"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	content := fmt.Sprintf(`# Dolt SQL server configuration — managed by gc-beads-bd
# Do not edit manually; changes are overwritten on each server start.
# To customize, set environment variables:
#   GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER, GC_DOLT_PASSWORD, GC_DOLT_LOGLEVEL

log_level: %s

listener:
  port: %s
  host: %s
  max_connections: 1000
  back_log: 50
  max_connections_timeout_millis: 5000
  read_timeout_millis: 300000
  write_timeout_millis: 300000

data_dir: %q

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: %d
`, logLevel, port, host, dataDir, archiveLevel)
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}
