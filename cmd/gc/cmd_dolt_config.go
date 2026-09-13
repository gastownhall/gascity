package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/config"
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
		autoGC       bool
		maxConns     int
		readTimeout  int
		writeTimeout int
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
			doltConfig := config.DoltConfig{
				ArchiveLevel:       &archiveLevel,
				AutoGCEnabled:      &autoGC,
				MaxConnections:     maxConns,
				ReadTimeoutMillis:  readTimeout,
				WriteTimeoutMillis: writeTimeout,
			}
			if err := writeManagedDoltConfigFile(configFile, host, port, dataDir, logLevel, doltConfig); err != nil {
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
	writeManaged.Flags().BoolVar(&autoGC, "auto-gc-enabled", true, "enable Dolt incremental auto-GC")
	writeManaged.Flags().IntVar(&maxConns, "max-connections", 0, "Dolt listener max_connections (0=managed default)")
	writeManaged.Flags().IntVar(&readTimeout, "read-timeout-millis", 0, "Dolt listener read_timeout_millis (0=managed default)")
	writeManaged.Flags().IntVar(&writeTimeout, "write-timeout-millis", 0, "Dolt listener write_timeout_millis (0=managed default)")
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
			if err := syncManagedDoltPortMirrors(cityPath); err != nil {
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

	var reindexCheck bool
	reindex := &cobra.Command{
		Use:    "doltlite-reindex",
		Short:  "Rebuild a DoltLite store's SQLite secondary indexes after flatten/gc",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// --check reports whether this build can reindex in process, without
			// touching the store. The maintenance path probes this before
			// running the stale-index-producing flatten/gc so it never creates
			// index corruption a non-native build cannot heal (ga-7hei).
			if reindexCheck {
				if !doltliteReindexSupported() {
					fmt.Fprintln(stderr, "gc dolt-config doltlite-reindex: in-process reindex unavailable in this build (needs -tags gascity_native_beads)") //nolint:errcheck
					return errExit
				}
				return nil
			}
			if scopeDir == "" {
				fmt.Fprintln(stderr, "gc dolt-config doltlite-reindex: missing --dir") //nolint:errcheck
				return errExit
			}
			if err := runDoltliteReindex(scopeDir); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config doltlite-reindex: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	reindex.Flags().StringVar(&scopeDir, "dir", "", "DoltLite store root to reindex")
	reindex.Flags().BoolVar(&reindexCheck, "check", false, "report whether this build can reindex in process, then exit without reindexing")
	_ = reindex.MarkFlagRequired("dir")
	cmd.AddCommand(reindex)
	return cmd
}

func writeManagedDoltConfigFile(path, host, port, dataDir, logLevel string, doltConfig config.DoltConfig) error {
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
	archiveLevel := doltConfig.EffectiveArchiveLevel()
	autoGCEnabled := doltConfig.EffectiveAutoGCEnabled()
	autoGCSysVar := doltConfig.AutoGCSysVar()
	maxConnections := doltConfig.EffectiveMaxConnections()
	readTimeoutMillis := doltConfig.EffectiveReadTimeoutMillis()
	writeTimeoutMillis := doltConfig.EffectiveWriteTimeoutMillis()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	waitTimeout := managedDoltWaitTimeoutForConfig(doltConfig, os.Getenv)
	waitTimeoutLine := ""
	if waitTimeout > 0 {
		waitTimeoutLine = fmt.Sprintf("  wait_timeout: %q\n", strconv.Itoa(waitTimeout))
	}
	content := fmt.Sprintf(`# Dolt SQL server configuration — managed by gc-beads-bd
# Do not edit manually; changes are overwritten on each server start.
# To customize, set environment variables:
#   GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER, GC_DOLT_PASSWORD, GC_DOLT_LOGLEVEL

log_level: %s

listener:
  port: %s
  host: %s
  # read_timeout is dolt's ONLY idle-connection reaper on this dolt version --
  # wait_timeout below is accepted but reaps nothing (measured for #5383).
  # Nominally it bounds the inter-row produce gap, but queries that produce no
  # rows until they finish (aggregates, large UPDATEs) are cut at this bound
  # as a wall-clock cap on the whole result phase. Only a config change plus
  # restart takes effect -- SET SESSION/GLOBAL cannot override it at runtime.
  # Raised to leave headroom for legitimate long-running maintenance queries
  # (#5383) while staying under half of write_timeout_millis so #3101's outer
  # deadline still catches a genuine pile-up first (#3626). city.toml [dolt]
  # overrides can raise it further for cities with slower live operations.
  max_connections: %d
  back_log: 50
  max_connections_timeout_millis: 5000
  read_timeout_millis: %d
  write_timeout_millis: %d

data_dir: %q

# Incremental auto-GC bounds the noms journal so it never reaches GB scale,
# shrinking both the unclean-stop corruption window and the recovery blast
# radius (#3176). Historically OFF to work around dolt#10944 (load-avg gating
# that never fired); fixed upstream in dolt 2.0.3 and the managed floor is
# 2.1.0+. Scheduled compaction (gc dolt compact) still handles history
# flattening — see #1918, #1200 for that lineage. Override via city.toml
# [dolt] auto_gc_enabled or GC_DOLT_AUTO_GC_ENABLED.
behavior:
  auto_gc_behavior:
    enable: %t
    archive_level: %d

# Managed Gas City workloads generate short-lived probe and metadata queries.
# Dolt's persistent stats worker can make those tiny databases grow large
# stats stores and burn CPU, especially on macOS endpoint-managed machines.
# Keep stats disabled for managed servers; use explicit gc dolt maintenance
# commands for storage cleanup instead of background workers.
system_variables:
  dolt_auto_gc_enabled: %q
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
%s`, logLevel, port, host, maxConnections, readTimeoutMillis, writeTimeoutMillis, dataDir, autoGCEnabled, archiveLevel, autoGCSysVar, waitTimeoutLine)
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

// managedDoltWaitTimeoutForConfig resolves the managed wait_timeout with
// city.toml winning over the ambient GC_DOLT_WAIT_TIMEOUT, matching how every
// other [dolt] field resolves. The env path is kept as the fallback because it
// carries an escape hatch city.toml cannot express: a negative value suppresses
// the wait_timeout line entirely, whereas an omitted or zero config field means
// "use the managed default".
func managedDoltWaitTimeoutForConfig(doltConfig config.DoltConfig, getenv func(string) string) int {
	if doltConfig.WaitTimeoutSeconds > 0 {
		return doltConfig.WaitTimeoutSeconds
	}
	return managedDoltWaitTimeoutFromEnv(getenv)
}

// managedDoltWaitTimeoutFromEnv takes its lookup as a parameter so the
// resolution order can be tested without mutating the process environment,
// which the cmd/gc environment debt ratchet forbids growing (see TESTING.md).
func managedDoltWaitTimeoutFromEnv(getenv func(string) string) int {
	if getenv == nil {
		return config.DefaultDoltWaitTimeoutSeconds
	}
	raw := getenv("GC_DOLT_WAIT_TIMEOUT")
	if raw == "" {
		return config.DefaultDoltWaitTimeoutSeconds
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return config.DefaultDoltWaitTimeoutSeconds
	}
	if n < 0 {
		return 0
	}
	return n
}
