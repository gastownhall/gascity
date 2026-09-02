package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

const beadsUpdateCASMaxPatchBytes = 256 * 1024

type beadsUpdateCASRequest struct {
	beadID           string
	storeRef         string
	expectedRevision int64
	requestFile      string
	format           string
	jsonOut          bool

	storeRefSet         bool
	expectedRevisionSet bool
	requestFileSet      bool
	formatSet           bool
}

type beadsUpdateCASPatch struct {
	Title       *string           `json:"title,omitempty"`
	Status      *string           `json:"status,omitempty"`
	Type        *string           `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	Description *string           `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type beadsUpdateCASResult struct {
	SchemaVersion    string                 `json:"schema_version"`
	OK               bool                   `json:"ok"`
	BeadID           string                 `json:"bead_id"`
	StoreRef         string                 `json:"store_ref"`
	Outcome          beads.UpdateCASOutcome `json:"outcome"`
	ExpectedRevision int64                  `json:"expected_revision"`
	Revision         int64                  `json:"revision"`
}

func newBeadsUpdateCASCmd(stdout, stderr io.Writer) *cobra.Command {
	var request beadsUpdateCASRequest
	cmd := &cobra.Command{
		Use:   "update-cas <bead-id>",
		Short: "Atomically update row-backed fields in an exact local store",
		Long: `Atomically update row-backed fields in one exact local bead store.

The store must be selected explicitly with --store-ref=city:<name> or
--store-ref=rig:<name>. The JSON patch is read from --request-file; use - for
stdin so title and description do not appear in process arguments. Supported
fields are title, description, status, priority, type, and metadata. Unknown or
empty patches fail before the store opens.

The command never scans another store or falls back to an unconditional write.
A stale revision is a zero-exit conflict outcome. Capability, transport,
readback, close, and validation failures are non-zero. Results contain only the
identity, outcome, and revisions; patch content is never echoed.`,
		Example: `  gc beads update-cas tr-123 \
    --store-ref=rig:tributary \
    --expected-revision=42 \
    --request-file=- \
    --json < patch.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request.beadID = args[0]
			request.storeRefSet = cmd.Flags().Changed("store-ref")
			request.expectedRevisionSet = cmd.Flags().Changed("expected-revision")
			request.requestFileSet = cmd.Flags().Changed("request-file")
			request.formatSet = cmd.Flags().Changed("format")
			if err := resolveBeadsUpdateCASOutputMode(&request); err != nil {
				fmt.Fprintf(stderr, "gc beads update-cas: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := validateBeadsUpdateCASRequest(request); err != nil {
				fmt.Fprintf(stderr, "gc beads update-cas: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			patch, err := readBeadsUpdateCASPatch(request.requestFile, cmd.InOrStdin())
			if err != nil {
				fmt.Fprintf(stderr, "gc beads update-cas: reading patch: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdBeadsUpdateCAS(request, patch, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&request.storeRef, "store-ref", "", "exact local store: city:<name> or rig:<name>")
	cmd.Flags().Int64Var(&request.expectedRevision, "expected-revision", 0, "opaque revision returned by bd show --json")
	cmd.Flags().StringVar(&request.requestFile, "request-file", "", "JSON patch file, or - for stdin")
	cmd.Flags().StringVar(&request.format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&request.jsonOut, "json", false, "emit the canonical JSON result")
	return cmd
}

func resolveBeadsUpdateCASOutputMode(request *beadsUpdateCASRequest) error {
	if request == nil || !request.jsonOut {
		return nil
	}
	if request.formatSet {
		switch request.format {
		case "json":
		case "text":
			return fmt.Errorf("--json cannot be combined with --format=text")
		default:
			return fmt.Errorf("invalid --format %q: expected text or json", request.format)
		}
	}
	request.format = "json"
	return nil
}

func validateBeadsUpdateCASRequest(request beadsUpdateCASRequest) error {
	switch {
	case !request.storeRefSet:
		return fmt.Errorf("--store-ref is required")
	case !request.expectedRevisionSet:
		return fmt.Errorf("--expected-revision is required")
	case request.expectedRevision == 0:
		return fmt.Errorf("--expected-revision must be a nonzero opaque token")
	case !request.requestFileSet:
		return fmt.Errorf("--request-file is required")
	case strings.TrimSpace(request.requestFile) == "":
		return fmt.Errorf("--request-file must not be empty")
	}
	if !validMetadataCASToken(request.beadID, metadataCASMaxBeadIDBytes) {
		return fmt.Errorf("invalid bead id %q: must be 1-%d ASCII letters, digits, dot, underscore, or hyphen and start with a letter or digit",
			request.beadID, metadataCASMaxBeadIDBytes)
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(request.storeRef); err != nil {
		return err
	}
	if request.format != "text" && request.format != "json" {
		return fmt.Errorf("invalid --format %q: expected text or json", request.format)
	}
	return nil
}

func readBeadsUpdateCASPatch(path string, stdin io.Reader) (beadsUpdateCASPatch, error) {
	if path == "-" {
		return decodeBeadsUpdateCASPatch(stdin)
	}
	file, err := os.Open(path)
	if err != nil {
		return beadsUpdateCASPatch{}, err
	}
	patch, decodeErr := decodeBeadsUpdateCASPatch(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return beadsUpdateCASPatch{}, decodeErr
	}
	if closeErr != nil {
		return beadsUpdateCASPatch{}, closeErr
	}
	return patch, nil
}

func decodeBeadsUpdateCASPatch(reader io.Reader) (beadsUpdateCASPatch, error) {
	if reader == nil {
		return beadsUpdateCASPatch{}, fmt.Errorf("patch reader is nil")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, beadsUpdateCASMaxPatchBytes+1))
	if err != nil {
		return beadsUpdateCASPatch{}, err
	}
	if len(raw) > beadsUpdateCASMaxPatchBytes {
		return beadsUpdateCASPatch{}, fmt.Errorf("patch exceeds %d bytes", beadsUpdateCASMaxPatchBytes)
	}
	if !utf8.Valid(raw) {
		return beadsUpdateCASPatch{}, fmt.Errorf("patch must be valid UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var patch beadsUpdateCASPatch
	if err := decoder.Decode(&patch); err != nil {
		return beadsUpdateCASPatch{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return beadsUpdateCASPatch{}, fmt.Errorf("patch must contain a single JSON object")
		}
		return beadsUpdateCASPatch{}, fmt.Errorf("patch must contain a single JSON object: %w", err)
	}
	if err := validateBeadsUpdateCASPatch(patch); err != nil {
		return beadsUpdateCASPatch{}, err
	}
	return patch, nil
}

func validateBeadsUpdateCASPatch(patch beadsUpdateCASPatch) error {
	if patch.Title == nil && patch.Status == nil && patch.Type == nil && patch.Priority == nil &&
		patch.Description == nil && len(patch.Metadata) == 0 {
		return fmt.Errorf("patch must set at least one supported field")
	}
	for key, value := range patch.Metadata {
		if !validMetadataCASToken(key, metadataCASMaxKeyBytes) {
			return fmt.Errorf("invalid metadata key %q", key)
		}
		if len(value) > metadataCASMaxValueBytes {
			return fmt.Errorf("metadata value for %q exceeds %d bytes", key, metadataCASMaxValueBytes)
		}
	}
	return nil
}

func (patch beadsUpdateCASPatch) updateOpts() beads.UpdateOpts {
	return beads.UpdateOpts{
		Title:       patch.Title,
		Status:      patch.Status,
		Type:        patch.Type,
		Priority:    patch.Priority,
		Description: patch.Description,
		Metadata:    patch.Metadata,
	}
}

var (
	openBeadsUpdateCASStore  = openAuthoritativeStoreAtForCity
	closeBeadsUpdateCASStore = closeBeadStoreHandle
)

func cmdBeadsUpdateCAS(request beadsUpdateCASRequest, patch beadsUpdateCASPatch, stdout, stderr io.Writer) int {
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(ctx.CityPath, configWarnWriter(request.format == "json", stderr))
	if err != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	scopeRoot, canonicalRef, err := resolveBeadsMetadataCASStore(cfg, ctx.CityPath, request.storeRef)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	request.storeRef = canonicalRef

	store, err := openBeadsUpdateCASStore(scopeRoot, ctx.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: opening %s: %v\n", canonicalRef, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	result, applyErr := applyBeadsUpdateCAS(store, request, patch)
	closeErr := closeBeadsUpdateCASStore(store)
	if applyErr != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: %v\n", applyErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: closing %s after CAS: %v\n", canonicalRef, closeErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	return renderBeadsUpdateCAS(result, request.format, stdout, stderr)
}

func applyBeadsUpdateCAS(store beads.Store, request beadsUpdateCASRequest, patch beadsUpdateCASPatch) (beadsUpdateCASResult, error) {
	result, err := beads.ApplyUpdateCAS(store, request.beadID, request.expectedRevision, patch.updateOpts())
	if err != nil {
		return beadsUpdateCASResult{}, err
	}
	return beadsUpdateCASResult{
		SchemaVersion:    "1",
		OK:               true,
		BeadID:           request.beadID,
		StoreRef:         request.storeRef,
		Outcome:          result.Outcome,
		ExpectedRevision: request.expectedRevision,
		Revision:         result.Revision,
	}, nil
}

func renderBeadsUpdateCAS(result beadsUpdateCASResult, format string, stdout, stderr io.Writer) int {
	if format == "json" {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc beads update-cas", result)
	}
	if _, err := fmt.Fprintf(stdout, "bead=%s store=%s outcome=%s expected_revision=%d revision=%d\n",
		result.BeadID, result.StoreRef, result.Outcome, result.ExpectedRevision, result.Revision); err != nil {
		fmt.Fprintf(stderr, "gc beads update-cas: writing result: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
