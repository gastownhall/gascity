package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

const (
	metadataCASMaxBeadIDBytes    = 512
	metadataCASMaxKeyBytes       = 256
	metadataCASMaxStoreNameBytes = 255
	metadataCASMaxValueBytes     = 64 * 1024
)

var metadataCASSafeToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type beadsMetadataCASRequest struct {
	beadID   string
	storeRef string
	key      string
	expected string
	next     string
	format   string
	jsonOut  bool

	storeRefSet bool
	keySet      bool
	expectedSet bool
	nextSet     bool
	formatSet   bool
}

type beadsMetadataCASResult struct {
	SchemaVersion string                   `json:"schema_version"`
	OK            bool                     `json:"ok"`
	BeadID        string                   `json:"bead_id"`
	StoreRef      string                   `json:"store_ref"`
	Key           string                   `json:"key"`
	Outcome       beads.MetadataCASOutcome `json:"outcome"`
}

func newBeadsMetadataCASCmd(stdout, stderr io.Writer) *cobra.Command {
	var request beadsMetadataCASRequest
	cmd := &cobra.Command{
		Use:   "metadata-cas <bead-id>",
		Short: "Atomically compare and set one metadata key in an exact local store",
		Long: `Atomically compare and set one metadata key in one exact local bead store.

The store must be selected explicitly with --store-ref=city:<name> or
--store-ref=rig:<name>. This command never scans other stores, follows a
cross-store fallback, or operates on a remote city. A conflict is an ordinary
zero-exit outcome; capability, transport, readback, and validation failures are
non-zero.

Legacy file-provider cities created before scope-local file stores may map city
and rig references to the same shared city file. That provider-layout alias is
preserved for compatibility; it is not a search or cross-store fallback.

Use --json for the canonical machine-output contract. --format=json remains
accepted for compatibility. Combining --json with an explicit --format=text is
a usage error.`,
		Example: `  gc beads metadata-cas tr-123 \
    --store-ref=rig:tributary \
    --key=semantic_review_sha \
    --expected=old-sha \
    --next=new-sha \
    --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request.beadID = args[0]
			request.storeRefSet = cmd.Flags().Changed("store-ref")
			request.keySet = cmd.Flags().Changed("key")
			request.expectedSet = cmd.Flags().Changed("expected")
			request.nextSet = cmd.Flags().Changed("next")
			request.formatSet = cmd.Flags().Changed("format")
			if err := resolveBeadsMetadataCASOutputMode(&request); err != nil {
				fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := validateBeadsMetadataCASRequest(request); err != nil {
				fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdBeadsMetadataCAS(request, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&request.storeRef, "store-ref", "", "exact local store: city:<name> or rig:<name>")
	cmd.Flags().StringVar(&request.key, "key", "", "metadata key to compare and set")
	cmd.Flags().StringVar(&request.expected, "expected", "", "expected current value (explicit empty is allowed)")
	cmd.Flags().StringVar(&request.next, "next", "", "replacement value (explicit empty is allowed)")
	cmd.Flags().StringVar(&request.format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&request.jsonOut, "json", false, "emit the canonical JSON result")
	return cmd
}

func resolveBeadsMetadataCASOutputMode(request *beadsMetadataCASRequest) error {
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

func validateBeadsMetadataCASRequest(request beadsMetadataCASRequest) error {
	switch {
	case !request.storeRefSet:
		return fmt.Errorf("--store-ref is required")
	case !request.keySet:
		return fmt.Errorf("--key is required")
	case !request.expectedSet:
		return fmt.Errorf("--expected is required (use --expected= for an empty value)")
	case !request.nextSet:
		return fmt.Errorf("--next is required (use --next= for an empty value)")
	}
	if !validMetadataCASToken(request.beadID, metadataCASMaxBeadIDBytes) {
		return fmt.Errorf("invalid bead id %q: must be 1-%d ASCII letters, digits, dot, underscore, or hyphen and start with a letter or digit",
			request.beadID, metadataCASMaxBeadIDBytes)
	}
	if !validMetadataCASToken(request.key, metadataCASMaxKeyBytes) {
		return fmt.Errorf("invalid metadata key %q: must be 1-%d ASCII letters, digits, dot, underscore, or hyphen and start with a letter or digit",
			request.key, metadataCASMaxKeyBytes)
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(request.storeRef); err != nil {
		return err
	}
	if !utf8.ValidString(request.expected) {
		return fmt.Errorf("--expected must be valid UTF-8")
	}
	if len(request.expected) > metadataCASMaxValueBytes {
		return fmt.Errorf("--expected exceeds %d bytes", metadataCASMaxValueBytes)
	}
	if !utf8.ValidString(request.next) {
		return fmt.Errorf("--next must be valid UTF-8")
	}
	if len(request.next) > metadataCASMaxValueBytes {
		return fmt.Errorf("--next exceeds %d bytes", metadataCASMaxValueBytes)
	}
	if request.format != "text" && request.format != "json" {
		return fmt.Errorf("invalid --format %q: expected text or json", request.format)
	}
	return nil
}

func validMetadataCASToken(value string, maxBytes int) bool {
	return len(value) > 0 && len(value) <= maxBytes && metadataCASSafeToken.MatchString(value)
}

func parseBeadsMetadataCASStoreRef(storeRef string) (kind, name string, err error) {
	kind, name, ok := strings.Cut(storeRef, ":")
	if !ok || (kind != "city" && kind != "rig") ||
		!validMetadataCASToken(name, metadataCASMaxStoreNameBytes) {
		return "", "", fmt.Errorf("invalid --store-ref %q: expected city:<name> or rig:<name>", storeRef)
	}
	return kind, name, nil
}

func resolveBeadsMetadataCASStore(cfg *config.City, cityPath, storeRef string) (scopeRoot, canonicalRef string, err error) {
	kind, name, err := parseBeadsMetadataCASStoreRef(storeRef)
	if err != nil {
		return "", "", err
	}
	switch kind {
	case "city":
		cityName := loadedCityName(cfg, cityPath)
		if name != cityName {
			return "", "", fmt.Errorf("city store %q does not match the resolved local city %q", name, cityName)
		}
		return cityPath, "city:" + cityName, nil
	case "rig":
		for _, rig := range cfg.Rigs {
			if rig.Name != name {
				continue
			}
			if strings.TrimSpace(rig.Path) == "" {
				return "", "", fmt.Errorf("rig store %q has no configured local path", name)
			}
			return resolveStoreScopeRoot(cityPath, rig.Path), "rig:" + name, nil
		}
		return "", "", fmt.Errorf("rig store %q is not configured in city %q", name, loadedCityName(cfg, cityPath))
	default:
		return "", "", fmt.Errorf("invalid --store-ref %q: unsupported store kind %q", storeRef, kind)
	}
}

var (
	openBeadsMetadataCASStore  = openAuthoritativeStoreAtForCity
	closeBeadsMetadataCASStore = closeBeadStoreHandle
)

func cmdBeadsMetadataCAS(request beadsMetadataCASRequest, stdout, stderr io.Writer) int {
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(ctx.CityPath, configWarnWriter(request.format == "json", stderr))
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	scopeRoot, canonicalRef, err := resolveBeadsMetadataCASStore(cfg, ctx.CityPath, request.storeRef)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	request.storeRef = canonicalRef

	store, err := openBeadsMetadataCASStore(scopeRoot, ctx.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: opening %s: %v\n", canonicalRef, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	result, err := applyBeadsMetadataCAS(store, request)
	closeErr := closeBeadsMetadataCASStore(store)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: closing %s after CAS: %v\n", canonicalRef, closeErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	return renderBeadsMetadataCAS(result, request.format, stdout, stderr)
}

func applyBeadsMetadataCAS(store beads.Store, request beadsMetadataCASRequest) (beadsMetadataCASResult, error) {
	outcome, err := beads.ApplyMetadataCAS(store, request.beadID, request.key, request.expected, request.next)
	if err != nil {
		return beadsMetadataCASResult{}, err
	}
	return beadsMetadataCASResult{
		SchemaVersion: "1",
		OK:            true,
		BeadID:        request.beadID,
		StoreRef:      request.storeRef,
		Key:           request.key,
		Outcome:       outcome,
	}, nil
}

func doBeadsMetadataCAS(store beads.Store, request beadsMetadataCASRequest, stdout, stderr io.Writer) int {
	result, err := applyBeadsMetadataCAS(store, request)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return renderBeadsMetadataCAS(result, request.format, stdout, stderr)
}

func renderBeadsMetadataCAS(result beadsMetadataCASResult, format string, stdout, stderr io.Writer) int {
	if format == "json" {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc beads metadata-cas", result)
	}
	if _, err := fmt.Fprintf(stdout, "bead=%s store=%s key=%s outcome=%s\n",
		result.BeadID, result.StoreRef, result.Key, result.Outcome); err != nil {
		fmt.Fprintf(stderr, "gc beads metadata-cas: writing result: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
