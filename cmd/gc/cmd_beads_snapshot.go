package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

type beadsSnapshotRequest struct {
	storeRef string
	format   string
	jsonOut  bool

	storeRefSet bool
	formatSet   bool
}

type beadsSnapshotDependency struct {
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
}

// beadsSnapshotRow is the private runtime read boundary consumed by the
// Agent Platform normalizer. It deliberately omits assignee, labels, and other
// execution-only fields while promoting the store-internal CAS revision.
type beadsSnapshotRow struct {
	ID                 string                    `json:"id"`
	Title              string                    `json:"title"`
	Description        string                    `json:"description"`
	AcceptanceCriteria string                    `json:"acceptance_criteria"`
	IssueType          string                    `json:"issue_type"`
	Status             string                    `json:"status"`
	Priority           *int                      `json:"priority,omitempty"`
	Revision           int64                     `json:"revision"`
	ExternalRef        string                    `json:"external_ref,omitempty"`
	DependencyCount    int                       `json:"dependency_count"`
	Dependencies       []beadsSnapshotDependency `json:"dependencies"`
	Metadata           beads.StringMap           `json:"metadata"`
}

type beadsSnapshotResult struct {
	SchemaVersion string             `json:"schema_version"`
	OK            bool               `json:"ok"`
	StoreRef      string             `json:"store_ref"`
	Beads         []beadsSnapshotRow `json:"beads"`
}

func newBeadsSnapshotCmd(stdout, stderr io.Writer) *cobra.Command {
	request := beadsSnapshotRequest{format: "text"}
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Read a CAS-ready private snapshot from one exact local store",
		Long: `Read durable issues and their typed dependencies from one exact local
bead store. The command promotes the same authoritative store revision used by
gc beads update-cas and never scans another store. Output is JSON-only because
it contains private Issue title/body content intended for a captured runtime
pipe into the Agent Platform planner, not terminal logs. Any partial read,
dependency failure, or zero revision fails closed.`,
		Example: `  gc beads snapshot --store-ref=rig:tributary --json`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request.storeRefSet = cmd.Flags().Changed("store-ref")
			request.formatSet = cmd.Flags().Changed("format")
			if err := resolveBeadsSnapshotOutputMode(&request); err != nil {
				fmt.Fprintf(stderr, "gc beads snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := validateBeadsSnapshotRequest(request); err != nil {
				fmt.Fprintf(stderr, "gc beads snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdBeadsSnapshot(request, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&request.storeRef, "store-ref", "", "exact local store: city:<name> or rig:<name>")
	cmd.Flags().StringVar(&request.format, "format", "text", "output format: json")
	cmd.Flags().BoolVar(&request.jsonOut, "json", false, "emit the canonical JSON snapshot")
	return cmd
}

func resolveBeadsSnapshotOutputMode(request *beadsSnapshotRequest) error {
	if request == nil || !request.jsonOut {
		return nil
	}
	if request.formatSet && request.format != "json" {
		return fmt.Errorf("--json cannot be combined with --format=%s", request.format)
	}
	request.format = "json"
	return nil
}

func validateBeadsSnapshotRequest(request beadsSnapshotRequest) error {
	if !request.storeRefSet {
		return fmt.Errorf("--store-ref is required")
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(request.storeRef); err != nil {
		return err
	}
	if request.format != "json" {
		return fmt.Errorf("private bead snapshot requires JSON output (--json or --format=json)")
	}
	return nil
}

var (
	openBeadsSnapshotStore  = openAuthoritativeStoreAtForCity
	closeBeadsSnapshotStore = closeBeadStoreHandle
)

func cmdBeadsSnapshot(request beadsSnapshotRequest, stdout, stderr io.Writer) int {
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(ctx.CityPath, configWarnWriter(true, stderr))
	if err != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	scopeRoot, canonicalRef, err := resolveBeadsMetadataCASStore(cfg, ctx.CityPath, request.storeRef)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, err := openBeadsSnapshotStore(scopeRoot, ctx.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: opening %s: %v\n", canonicalRef, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	result, snapshotErr := collectBeadsSnapshot(store, canonicalRef)
	closeErr := closeBeadsSnapshotStore(store)
	if snapshotErr != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: %v\n", snapshotErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "gc beads snapshot: closing %s: %v\n", canonicalRef, closeErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	return writeCLIJSONLineOrExit(stdout, stderr, "gc beads snapshot", result)
}

func collectBeadsSnapshot(store beads.Store, storeRef string) (beadsSnapshotResult, error) {
	reader, ok := beads.IssueGraphSnapshotFor(store)
	if !ok {
		return beadsSnapshotResult{}, fmt.Errorf("reading exact store %s: %w", storeRef, beads.ErrIssueGraphSnapshotUnsupported)
	}
	rows, depsByID, err := reader.IssueGraphSnapshot(beads.ListQuery{
		IncludeClosed: true,
		AllowScan:     true,
		Live:          true,
		TierMode:      beads.TierIssues,
	})
	if err != nil {
		return beadsSnapshotResult{}, fmt.Errorf("reading exact store %s atomically: %w", storeRef, err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	result := beadsSnapshotResult{
		SchemaVersion: "1",
		OK:            true,
		StoreRef:      storeRef,
		Beads:         make([]beadsSnapshotRow, 0, len(rows)),
	}
	for _, bead := range rows {
		if strings.TrimSpace(bead.ID) == "" || bead.Revision == 0 {
			return beadsSnapshotResult{}, fmt.Errorf("bead %q has no usable authoritative revision", bead.ID)
		}
		deps, present := depsByID[bead.ID]
		if !present {
			return beadsSnapshotResult{}, fmt.Errorf("atomic snapshot omitted dependencies for %s", bead.ID)
		}
		dependencies := make([]beadsSnapshotDependency, 0, len(deps))
		seen := make(map[string]struct{}, len(deps))
		for _, dep := range deps {
			if dep.IssueID != bead.ID || dep.DependsOnID == "" || dep.Type == "" {
				return beadsSnapshotResult{}, fmt.Errorf("bead %s returned an invalid dependency row", bead.ID)
			}
			key := dep.DependsOnID + "\x00" + dep.Type
			if _, duplicate := seen[key]; duplicate {
				return beadsSnapshotResult{}, fmt.Errorf("bead %s returned duplicate dependency rows", bead.ID)
			}
			seen[key] = struct{}{}
			dependencies = append(dependencies, beadsSnapshotDependency{
				ID:             dep.DependsOnID,
				DependencyType: dep.Type,
			})
		}
		sort.Slice(dependencies, func(i, j int) bool {
			if dependencies[i].ID == dependencies[j].ID {
				return dependencies[i].DependencyType < dependencies[j].DependencyType
			}
			return dependencies[i].ID < dependencies[j].ID
		})
		metadata := bead.Metadata
		if metadata == nil {
			metadata = beads.StringMap{}
		}
		result.Beads = append(result.Beads, beadsSnapshotRow{
			ID:                 bead.ID,
			Title:              bead.Title,
			Description:        bead.Description,
			AcceptanceCriteria: bead.AcceptanceCriteria,
			IssueType:          bead.Type,
			Status:             bead.Status,
			Priority:           bead.Priority,
			Revision:           bead.Revision,
			ExternalRef:        bead.ExternalRef,
			DependencyCount:    len(dependencies),
			Dependencies:       dependencies,
			Metadata:           metadata,
		})
	}
	return result, nil
}
