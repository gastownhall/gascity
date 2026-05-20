package main

import (
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Build metadata — injected via ldflags at build time.
// Falls back to VCS info embedded by the Go toolchain (go install, go build).
var (
	version                  = "dev"
	commit                   = "unknown"
	date                     = "unknown"
	goPseudoVersionSuffixRes = []*regexp.Regexp{
		regexp.MustCompile(`^(.*)\.0\.\d{14}-[0-9a-f]{12,}$`),
		regexp.MustCompile(`^(.*)-0\.\d{14}-[0-9a-f]{12,}$`),
		regexp.MustCompile(`^(.*)-\d{14}-[0-9a-f]{12,}$`),
	}
)

func init() {
	info, ok := debug.ReadBuildInfo()
	version, commit, date = resolveBuildMetadata(version, commit, date, ok, info)
}

func resolveBuildMetadata(
	currentVersion string,
	currentCommit string,
	currentDate string,
	ok bool,
	info *debug.BuildInfo,
) (string, string, string) {
	currentVersion = normalizeVersion(currentVersion)
	if !ok || info == nil {
		return currentVersion, currentCommit, currentDate
	}
	if currentVersion == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		currentVersion = normalizeVersion(info.Main.Version)
	}
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if currentCommit == "unknown" && s.Value != "" {
				currentCommit = s.Value
			}
		case "vcs.time":
			if currentDate == "unknown" && s.Value != "" {
				currentDate = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && currentCommit != "unknown" {
		currentCommit += "-dirty"
	}
	return currentVersion, currentCommit, currentDate
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "(devel)" {
		return "dev"
	}
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	for _, re := range goPseudoVersionSuffixRes {
		if m := re.FindStringSubmatch(v); len(m) == 2 {
			v = m[1]
			break
		}
	}
	if v == "" || v == "0.0.0" {
		return "dev"
	}
	return v
}

func newVersionCmd(stdout, stderr io.Writer) *cobra.Command {
	var longOutput bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print gc version",
		Long: `Print the gc version string.

Use --long to include git commit and build date metadata.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if jsonOut {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc version", versionJSONResult{
					SchemaVersion: "1",
					Version:       version,
					Commit:        commit,
					Date:          date,
					Long:          longOutput,
				})
			}
			if longOutput {
				fmt.Fprintf(stdout, "%s (commit: %s, built: %s)\n", version, commit, date) //nolint:errcheck // best-effort stdout
				return nil
			}
			fmt.Fprintf(stdout, "%s\n", version) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	cmd.Flags().BoolVarP(&longOutput, "long", "l", false, "Include git commit and build date metadata")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

type versionJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Date          string `json:"date"`
	Long          bool   `json:"long"`
}
