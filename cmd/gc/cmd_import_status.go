package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/remotesource"
	"github.com/spf13/cobra"
)

// importStatusSchemaVersion identifies the JSON document emitted by
// "gc import status --json".
const importStatusSchemaVersion = "1"

// ImportStatusJSON is the JSON output format for "gc import status --json".
// It is the machine-readable drift surface for pack imports: every
// declared import binding across scopes (root pack, default-rig, and
// rig-scoped), the packs.lock pin closure, and the lockfile content hash.
type ImportStatusJSON struct {
	SchemaVersion string `json:"schema_version"`
	// OK is the top-level success discriminator every gc --json result
	// document must carry (see schemas/import/status/result.schema.json).
	OK bool `json:"ok"`
	// Root is the absolute city or pack root the status was computed from.
	Root string `json:"root"`
	// PacksLockPath is the absolute path of the packs.lock file (which
	// may not exist; see PacksLockSHA256).
	PacksLockPath string `json:"packs_lock_path"`
	// PacksLockSHA256 is the hex-encoded SHA-256 digest of the raw
	// packs.lock contents. Omitted when the file does not exist.
	PacksLockSHA256 string `json:"packs_lock_sha256,omitempty"`
	// Imports lists every declared import binding, sorted by name.
	Imports []ImportStatusEntry `json:"imports"`
	// LockedPacks mirrors the full packs.lock closure (direct and
	// transitive pins), sorted by source.
	LockedPacks []ImportStatusLockedPack `json:"locked_packs"`
	// Upstream summarizes the freshness walk. Present only under
	// --check-upstream, so the default document is unchanged.
	Upstream *ImportStatusUpstreamSummary `json:"upstream,omitempty"`

	// upstreamErrors holds the resolution failures as typed errors, which the
	// document's own string fields cannot carry. It is unexported, so it never
	// reaches the wire; it exists so a *gitcred.AuthError still reaches
	// printCredentialHint and the operator is told to register a credential
	// rather than reading a raw git rejection.
	upstreamErrors []error
}

// ImportStatusUpstreamSummary is the aggregate freshness verdict.
//
// It follows the "gc lint --json" precedent: "ok" keeps meaning "the command
// ran" and a separate "passed" carries the verdict, so a consumer never has to
// infer the exit code from the entry list.
type ImportStatusUpstreamSummary struct {
	// Passed mirrors the process exit code: false when any import is behind,
	// or when any is unreachable and --fail-on-unreachable was passed.
	Passed        bool `json:"passed"`
	Checked       int  `json:"checked"`
	Current       int  `json:"current"`
	Behind        int  `json:"behind"`
	Unreachable   int  `json:"unreachable"`
	NotApplicable int  `json:"not_applicable"`
}

// ImportStatusUpstream is one import's freshness answer.
type ImportStatusUpstream struct {
	// Verdict is one of "current", "behind", "unreachable", or
	// "not_applicable". The schema pins it as an enum.
	Verdict string `json:"verdict"`
	// ResolvedRef names what upstream was resolved through: a symbolic ref
	// such as "refs/heads/main", or the selected tag for a semver constraint.
	ResolvedRef string `json:"resolved_ref,omitempty"`
	// ResolvedCommit is the commit upstream resolved to.
	ResolvedCommit string `json:"resolved_commit,omitempty"`
	// Error explains an unreachable verdict, or why an import has no upstream
	// to compare against.
	Error string `json:"error,omitempty"`
}

// ImportStatusEntry is one declared import binding in the status output.
type ImportStatusEntry struct {
	// Name is the scoped binding key: "pack:<name>" for root-pack
	// imports, "default-rig:<name>" for default rig imports, and
	// "rig:<rig>:<name>" for rig-scoped imports.
	Name string `json:"name"`
	// Source is the declared source exactly as authored in TOML.
	Source string `json:"source"`
	// Constraint is the declared version constraint, when present.
	Constraint string `json:"constraint,omitempty"`
	// Kind is "remote" for git-backed sources and "path" for local
	// directory sources.
	Kind string `json:"kind"`
	// Path is the resolved absolute path for kind "path" entries.
	Path string `json:"path,omitempty"`
	// Pin is the packs.lock resolution for kind "remote" entries.
	// Omitted when the source has no lock entry (unlocked).
	Pin *ImportStatusPin `json:"pin,omitempty"`
	// Upstream is this import's freshness answer. Present only under
	// --check-upstream.
	Upstream *ImportStatusUpstream `json:"upstream,omitempty"`
}

// ImportStatusPin is the packs.lock resolution pinned for a remote import.
type ImportStatusPin struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Fetched string `json:"fetched,omitempty"`
}

// ImportStatusLockedPack is one packs.lock entry in the status output.
type ImportStatusLockedPack struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Fetched string `json:"fetched,omitempty"`
}

func newImportStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts importStatusOptions
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report declared imports and packs.lock pins",
		Long: `Report declared imports and packs.lock pins.

Covers every import scope (root pack [imports.*], [defaults.rig.imports.*],
and rig-scoped [rigs.imports.*]) plus the full packs.lock closure and the
lockfile content hash. With --json the output is a stable machine-readable
document for drift checkers.

Without --check-upstream the command is entirely offline and reports only what
is already on disk: a pin can be years stale and still look healthy. With
--check-upstream each declared remote import's source is resolved over the
network and compared against its packs.lock pin, and the command exits 1 if any
pin is behind.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// A flag that is silently ignored reads as a passing gate, which is
			// the failure mode this whole command exists to remove.
			if opts.FailOnUnreachable && !opts.CheckUpstream {
				fmt.Fprintln(stderr, "gc import status: --fail-on-unreachable requires --check-upstream") //nolint:errcheck
				return errExit
			}
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import status: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportStatusWithOptions(cityPath, opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit JSON result")
	cmd.Flags().BoolVar(&opts.CheckUpstream, "check-upstream", false,
		"resolve each remote import's source and compare it against its packs.lock pin (network)")
	cmd.Flags().BoolVar(&opts.FailOnUnreachable, "fail-on-unreachable", false,
		"with --check-upstream, also exit 1 when an import's upstream cannot be resolved")
	return cmd
}

// importStatusOptions carries the "gc import status" flag set. The zero value
// is today's offline behavior.
type importStatusOptions struct {
	JSON              bool
	CheckUpstream     bool
	FailOnUnreachable bool
}

// doImportStatus is the pure logic for "gc import status". It reads the
// declared import set across all scopes plus packs.lock and renders
// either the human-readable summary or the typed JSON document.
func doImportStatus(cityPath string, jsonOut bool, stdout, stderr io.Writer) int {
	return doImportStatusWithOptions(cityPath, importStatusOptions{JSON: jsonOut}, stdout, stderr)
}

// doImportStatusWithOptions is doImportStatus with the freshness flags. With
// opts.CheckUpstream false it is the offline command exactly as it has always
// behaved: no network call, no new field emitted, exit 0.
func doImportStatusWithOptions(cityPath string, opts importStatusOptions, stdout, stderr io.Writer) int {
	status, err := buildImportStatus(cityPath, opts)
	if err != nil {
		fmt.Fprintf(stderr, "gc import status: %v\n", err) //nolint:errcheck
		return 1
	}
	if opts.JSON {
		data, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc import status: encoding JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stdout, string(data)) //nolint:errcheck
	} else {
		writeImportStatusText(stdout, status)
	}
	if status.Upstream == nil {
		return 0
	}
	writeImportStatusUpstreamStderr(stderr, status)
	if status.Upstream.Passed {
		return 0
	}
	return 1
}

// buildImportStatus assembles the import status document for cityPath.
func buildImportStatus(cityPath string, opts importStatusOptions) (*ImportStatusJSON, error) {
	fs := fsys.OSFS{}
	allImports, err := collectAllImportsFS(cityPath)
	if err != nil {
		return nil, err
	}

	lockPath := filepath.Join(cityPath, packman.LockfileName)
	status := &ImportStatusJSON{
		SchemaVersion: importStatusSchemaVersion,
		OK:            true,
		Root:          cityPath,
		PacksLockPath: lockPath,
	}
	// Read packs.lock once and derive both the hash and the pins from
	// the same bytes: a concurrent atomic lockfile rewrite between two
	// reads could otherwise emit a document whose packs_lock_sha256
	// does not match its own pin set.
	lockData, err := fs.ReadFile(lockPath)
	switch {
	case err == nil:
		sum := sha256.Sum256(lockData)
		status.PacksLockSHA256 = hex.EncodeToString(sum[:])
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading %s: %w", packman.LockfileName, err)
	}
	lock, err := packman.ParseLockfile(lockData)
	if err != nil {
		return nil, err
	}
	status.Imports = make([]ImportStatusEntry, 0, len(allImports))
	status.LockedPacks = make([]ImportStatusLockedPack, 0, len(lock.Packs))

	names := make([]string, 0, len(allImports))
	for name := range allImports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		imp := allImports[name]
		entry := ImportStatusEntry{
			Name:       name,
			Source:     imp.Source,
			Constraint: imp.Version,
		}
		if isRemoteImportSource(imp.Source) {
			entry.Kind = "remote"
			if pack, ok := lock.Packs[imp.Source]; ok {
				entry.Pin = &ImportStatusPin{
					Version: pack.Version,
					Commit:  pack.Commit,
					Fetched: formatImportStatusTime(pack.Fetched),
				}
			}
		} else {
			entry.Kind = "path"
			if abs, pathErr := resolveImportAddPath(cityPath, imp.Source); pathErr == nil {
				entry.Path = abs
			}
		}
		status.Imports = append(status.Imports, entry)
	}

	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := lock.Packs[source]
		status.LockedPacks = append(status.LockedPacks, ImportStatusLockedPack{
			Source:  source,
			Version: pack.Version,
			Commit:  pack.Commit,
			Fetched: formatImportStatusTime(pack.Fetched),
		})
	}
	if opts.CheckUpstream {
		if err := addImportStatusUpstream(status, cityPath, allImports, lock, opts); err != nil {
			return nil, err
		}
	}
	return status, nil
}

// addImportStatusUpstream runs the freshness walk and folds it into the
// document: one verdict per entry plus the aggregate summary.
func addImportStatusUpstream(status *ImportStatusJSON, cityPath string, allImports map[string]config.Import, lock *packman.Lockfile, opts importStatusOptions) error {
	report, err := checkUpstreamImports(cityPath, allImports, lock)
	if err != nil {
		return err
	}
	byName := make(map[string]packman.UpstreamStatus, len(report.Statuses))
	for _, upstream := range report.Statuses {
		byName[upstream.Name] = upstream
	}
	for i := range status.Imports {
		upstream, ok := byName[status.Imports[i].Name]
		if !ok {
			continue
		}
		entry := &ImportStatusUpstream{
			Verdict:        string(upstream.Verdict),
			ResolvedRef:    upstream.ResolvedRef,
			ResolvedCommit: upstream.ResolvedCommit,
		}
		if upstream.Err != nil {
			entry.Error = upstream.Err.Error()
			if upstream.Verdict == packman.UpstreamUnreachable {
				status.upstreamErrors = append(status.upstreamErrors, upstream.Err)
			}
		}
		status.Imports[i].Upstream = entry
	}

	behind := report.Count(packman.UpstreamBehind)
	unreachable := report.Count(packman.UpstreamUnreachable)
	status.Upstream = &ImportStatusUpstreamSummary{
		// An unreachable import is only a failure when the caller asks for it:
		// a laptop offline in a tunnel should not fail the same gate a stale
		// pin does, but a CI job that wants "prove it" can say so.
		Passed:        behind == 0 && (unreachable == 0 || !opts.FailOnUnreachable),
		Checked:       report.Checked,
		Current:       report.Count(packman.UpstreamCurrent),
		Behind:        behind,
		Unreachable:   unreachable,
		NotApplicable: report.Count(packman.UpstreamNotApplicable),
	}
	return nil
}

// formatImportStatusTime renders a lock timestamp as RFC 3339 UTC, or
// "" for the zero value so omitempty drops it from the JSON output.
func formatImportStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeImportStatusText renders the human-readable status: the lock
// hash followed by one tab-separated line per declared import
// (name, source, constraint, kind, pinned version, pinned commit).
func writeImportStatusText(stdout io.Writer, status *ImportStatusJSON) {
	if status.PacksLockSHA256 != "" {
		fmt.Fprintf(stdout, "packs.lock sha256: %s\n", status.PacksLockSHA256) //nolint:errcheck
	} else {
		fmt.Fprintln(stdout, "packs.lock sha256: (missing)") //nolint:errcheck
	}
	for _, entry := range status.Imports {
		pinnedVersion, pinnedCommit := "", ""
		switch {
		case entry.Pin != nil:
			pinnedVersion = entry.Pin.Version
			pinnedCommit = entry.Pin.Commit
		case entry.Kind == "remote":
			pinnedVersion = "(unlocked)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			entry.Name, entry.Source, entry.Constraint, entry.Kind, pinnedVersion, pinnedCommit)
	}
	writeImportStatusUpstreamText(stdout, status)
}

// writeImportStatusUpstreamText renders the freshness block appended under
// --check-upstream. It is a no-op otherwise, which is what keeps the default
// text output byte-identical.
func writeImportStatusUpstreamText(stdout io.Writer, status *ImportStatusJSON) {
	if status.Upstream == nil {
		return
	}
	fmt.Fprintln(stdout, "\nupstream freshness:") //nolint:errcheck
	width := 0
	for _, entry := range status.Imports {
		if entry.Upstream != nil && len(entry.Name) > width {
			width = len(entry.Name)
		}
	}
	subpathBehind := false
	for _, entry := range status.Imports {
		if entry.Upstream == nil {
			continue
		}
		// Ask the source parser whether this import names a subpath rather
		// than matching "//" by hand: every https://, ssh:// and file://
		// source contains "//" in its scheme, so a hand-rolled test prints
		// the caveat for every behind import.
		if entry.Upstream.Verdict == string(packman.UpstreamBehind) &&
			remotesource.Parse(entry.Source).Subpath != "" {
			subpathBehind = true
		}
		fmt.Fprintf(stdout, "  %-*s  %-14s %s\n", //nolint:errcheck
			width, entry.Name, entry.Upstream.Verdict, importStatusUpstreamDetail(entry))
	}
	sum := status.Upstream
	fmt.Fprintf(stdout, "%d checked: %d current, %d behind, %d unreachable, %d not applicable\n", //nolint:errcheck
		sum.Checked, sum.Current, sum.Behind, sum.Unreachable, sum.NotApplicable)
	if subpathBehind {
		// Without this, a subpath import reported behind reads as "the files
		// under this subpath changed", which the walk never establishes.
		fmt.Fprintln(stdout, `note: freshness is measured per repository; a "behind" verdict does not`) //nolint:errcheck
		fmt.Fprintln(stdout, "      necessarily mean this pack's subpath changed")                      //nolint:errcheck
	}
}

// importStatusUpstreamDetail renders the trailing evidence for one freshness
// line: what is pinned, and what the source resolved to.
func importStatusUpstreamDetail(entry ImportStatusEntry) string {
	upstream := entry.Upstream
	if upstream.Verdict == string(packman.UpstreamNotApplicable) {
		if upstream.Error != "" {
			return upstream.Error
		}
		return "no upstream to compare (path source)"
	}
	var b strings.Builder
	if entry.Pin != nil {
		fmt.Fprintf(&b, "pinned %s", shortImportCommit(entry.Pin.Commit))
		if entry.Pin.Fetched != "" {
			fmt.Fprintf(&b, " (fetched %s)", entry.Pin.Fetched)
		}
	}
	if upstream.Verdict == string(packman.UpstreamUnreachable) {
		fmt.Fprintf(&b, "  source unresolved: %s", upstream.Error)
		return b.String()
	}
	fmt.Fprintf(&b, "  source")
	if upstream.ResolvedRef != "" {
		fmt.Fprintf(&b, " %s", upstream.ResolvedRef)
	}
	fmt.Fprintf(&b, " %s", shortImportCommit(upstream.ResolvedCommit))
	return b.String()
}

// writeImportStatusUpstreamStderr names every stale or unresolved import on
// stderr with both commits, so a CI log that only captures stderr still says
// which pin is stale and what it should move to.
func writeImportStatusUpstreamStderr(stderr io.Writer, status *ImportStatusJSON) {
	for _, entry := range status.Imports {
		if entry.Upstream == nil {
			continue
		}
		pinned := ""
		if entry.Pin != nil {
			pinned = entry.Pin.Commit
		}
		switch entry.Upstream.Verdict {
		case string(packman.UpstreamBehind):
			fmt.Fprintf(stderr, "gc import status: import %q is behind upstream: pinned %s, source %s\n", //nolint:errcheck
				entry.Name, pinned, strings.TrimSpace(entry.Upstream.ResolvedRef+" "+entry.Upstream.ResolvedCommit))
		case string(packman.UpstreamUnreachable):
			fmt.Fprintf(stderr, "gc import status: import %q upstream is unreachable (pinned %s): %s\n", //nolint:errcheck
				entry.Name, pinned, entry.Upstream.Error)
		}
	}
	for _, err := range status.upstreamErrors {
		printCredentialHint(stderr, err)
	}
}

// shortImportCommit abbreviates a commit for the human-readable block. The
// stderr lines and the JSON document always carry the full value.
func shortImportCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
