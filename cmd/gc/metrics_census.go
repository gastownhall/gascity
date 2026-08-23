package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	productMetricsIDAnnotation          = "gc.productmetrics.id"
	productMetricsModeAnnotation        = "gc.productmetrics.mode"
	productMetricsNoticeAnnotation      = "gc.productmetrics.notice"
	productMetricsRecordingAnnotation   = "gc.productmetrics.recording"
	productMetricsOwnerAnnotation       = "gc.productmetrics.owner"
	productMetricsResolverAnnotation    = "gc.productmetrics.resolver"
	productMetricsExclusionAnnotation   = "gc.productmetrics.exclusion"
	productMetricsConditionalAnnotation = "gc.productmetrics.conditional"
	productMetricsCensusValidAnnotation = "gc.productmetrics.census-valid"
	cobraForcedDefaultAnnotation        = "gc.cobra.forced-default"
)

func materializeProductMetricsCobraDefaults(root *cobra.Command) {
	existing := make(map[*cobra.Command]struct{})
	walkProductMetricsCommands(root, false, func(command *cobra.Command, _ bool) {
		existing[command] = struct{}{}
	})
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, command := range root.Commands() {
		if _, alreadyPresent := existing[command]; alreadyPresent {
			continue
		}
		markCobraForcedDefault(command)
	}
}

func markCobraForcedDefault(command *cobra.Command) {
	annotations := make(map[string]string, len(command.Annotations)+1)
	for key, value := range command.Annotations {
		annotations[key] = value
	}
	annotations[cobraForcedDefaultAnnotation] = "true"
	command.Annotations = annotations
	for _, child := range command.Commands() {
		markCobraForcedDefault(child)
	}
}

type productMetricsCommandShape string

const (
	productMetricsShapeStructural    productMetricsCommandShape = "structural"
	productMetricsShapeRunnable      productMetricsCommandShape = "runnable"
	productMetricsShapeRunnableGroup productMetricsCommandShape = "runnable-group"
)

type productMetricsMode string

const (
	productMetricsModeStandard          productMetricsMode = "standard"
	productMetricsModeCompletion        productMetricsMode = "completion"
	productMetricsModeVersion           productMetricsMode = "version"
	productMetricsModeBdPassthrough     productMetricsMode = "bd-passthrough"
	productMetricsModeEventsStream      productMetricsMode = "events-stream"
	productMetricsModePerfWrapper       productMetricsMode = "perf-wrapper"
	productMetricsModeWorkflowCompat    productMetricsMode = "workflow-compat"
	productMetricsModeSupervisorService productMetricsMode = "supervisor-service"
	productMetricsModePackCommand       productMetricsMode = "pack-command"
	productMetricsModeHiddenPrivate     productMetricsMode = "hidden-private"
	productMetricsModeMetricsControl    productMetricsMode = "metrics-control"
	productMetricsModeHookProtocol      productMetricsMode = "hook-protocol"
	productMetricsModeEventEmit         productMetricsMode = "event-emit"
	productMetricsModeCredentialHelper  productMetricsMode = "credential-helper"
	productMetricsModePrivateCompletion productMetricsMode = "private-completion"
)

type productMetricsNoticePolicy string

const (
	productMetricsNoticeEligible   productMetricsNoticePolicy = "eligible"
	productMetricsNoticeIneligible productMetricsNoticePolicy = "ineligible"
)

type productMetricsRecordingPolicy string

const (
	productMetricsRecordingRecordable productMetricsRecordingPolicy = "recordable"
	productMetricsRecordingExcluded   productMetricsRecordingPolicy = "excluded"
)

type productMetricsExclusionReason string

const (
	productMetricsExclusionHiddenPrivate     productMetricsExclusionReason = "hidden-private"
	productMetricsExclusionMetricsControl    productMetricsExclusionReason = "metrics-control"
	productMetricsExclusionHookProtocol      productMetricsExclusionReason = "hook-protocol"
	productMetricsExclusionEventEmit         productMetricsExclusionReason = "event-emit"
	productMetricsExclusionCredentialHelper  productMetricsExclusionReason = "credential-helper"
	productMetricsExclusionPrivateCompletion productMetricsExclusionReason = "private-completion"
	productMetricsExclusionPrimeHook         productMetricsExclusionReason = "prime-hook"
	productMetricsExclusionHandoffAutomation productMetricsExclusionReason = "handoff-automation"
	productMetricsExclusionMailHookFormat    productMetricsExclusionReason = "mail-hook-format"
	productMetricsExclusionManagedContext    productMetricsExclusionReason = "managed-context"
	productMetricsExclusionProviderHook      productMetricsExclusionReason = "provider-hook"
	productMetricsExclusionCensusMismatch    productMetricsExclusionReason = "census-mismatch"
)

type productMetricsConditionalMode string

const (
	productMetricsConditionalGenericMachineOutput productMetricsConditionalMode = "generic-machine-output"
	productMetricsConditionalManagedContext       productMetricsConditionalMode = "managed-context"
	productMetricsConditionalProviderHook         productMetricsConditionalMode = "provider-hook"
	productMetricsConditionalBeadsMachineOutput   productMetricsConditionalMode = "beads-machine-output"
	productMetricsConditionalPrimeHook            productMetricsConditionalMode = "prime-hook"
	productMetricsConditionalHandoffAutomation    productMetricsConditionalMode = "handoff-automation"
	productMetricsConditionalMailHookFormat       productMetricsConditionalMode = "mail-hook-format"
)

type productMetricsDeferredDefault string

const (
	productMetricsDeferredHelp    productMetricsDeferredDefault = "help"
	productMetricsDeferredUnknown productMetricsDeferredDefault = "unknown"
)

type productMetricsOwner string

const (
	productMetricsOwnerStructural productMetricsOwner = "structural"
	productMetricsOwnerImmediate  productMetricsOwner = "immediate"
	productMetricsOwnerDeferred   productMetricsOwner = "deferred"
	productMetricsOwnerExcluded   productMetricsOwner = "excluded"
)

type productMetricsResolverKey string

const (
	productMetricsResolverRootDispatch  productMetricsResolverKey = "root-dispatch"
	productMetricsResolverGroupDispatch productMetricsResolverKey = "group-dispatch"
	productMetricsResolverPackDispatch  productMetricsResolverKey = "pack-dispatch"
)

type productMetricsCommandCensusEntry struct {
	Path               string
	Aliases            []string
	ConditionalModes   []productMetricsConditionalMode
	Hidden             bool
	EffectiveHidden    bool
	DisableFlagParsing bool
	Shape              productMetricsCommandShape
	Classification     string
	Mode               productMetricsMode
	Notice             productMetricsNoticePolicy
	Recording          productMetricsRecordingPolicy
	Owner              productMetricsOwner
	Resolver           productMetricsResolverKey
	Exclusion          productMetricsExclusionReason
	DeferredDefault    productMetricsDeferredDefault
	ID                 productMetricsCommandID
}

type productMetricsSyntheticCensusEntry = productMetricsCommandCensusEntry

// applyProductionProductMetricsCommandCensus is deliberately fail-closed for
// product metrics and fail-open for the CLI. A stale generated table never
// prevents ordinary command execution; it simply leaves the built-in tree
// without product-metrics annotations. The structural test below turns the
// same mismatch into a loud CI failure.
func applyProductionProductMetricsCommandCensus(root *cobra.Command) {
	clearProductMetricsCensusAnnotations(root)
	if err := validateProductMetricsCommandCensus(root, generatedProductMetricsCommandCensus); err != nil {
		return
	}
	if root.Annotations == nil {
		root.Annotations = make(map[string]string)
	}

	byPath := make(map[string]productMetricsCommandCensusEntry, len(generatedProductMetricsCommandCensus))
	for _, entry := range generatedProductMetricsCommandCensus {
		byPath[entry.Path] = entry
	}
	walkProductMetricsCommands(root, false, func(cmd *cobra.Command, _ bool) {
		if ignoreProductMetricsCensusCommand(cmd) {
			return
		}
		entry := byPath[cmd.CommandPath()]
		if cmd.Annotations == nil {
			cmd.Annotations = make(map[string]string)
		}
		cmd.Annotations[productMetricsClassAnnotation] = entry.Classification
		cmd.Annotations[productMetricsModeAnnotation] = string(entry.Mode)
		cmd.Annotations[productMetricsNoticeAnnotation] = string(entry.Notice)
		cmd.Annotations[productMetricsRecordingAnnotation] = string(entry.Recording)
		cmd.Annotations[productMetricsOwnerAnnotation] = string(entry.Owner)
		if len(entry.ConditionalModes) > 0 {
			values := make([]string, len(entry.ConditionalModes))
			for index, mode := range entry.ConditionalModes {
				values[index] = string(mode)
			}
			cmd.Annotations[productMetricsConditionalAnnotation] = strings.Join(values, ",")
		}
		if entry.ID != 0 {
			cmd.Annotations[productMetricsIDAnnotation] = strconv.FormatUint(uint64(entry.ID), 10)
		}
		if entry.Resolver != "" {
			cmd.Annotations[productMetricsResolverAnnotation] = string(entry.Resolver)
		}
		if entry.Exclusion != "" {
			cmd.Annotations[productMetricsExclusionAnnotation] = string(entry.Exclusion)
		}
	})
	root.Annotations[productMetricsCensusValidAnnotation] = "true"
}

func clearProductMetricsCensusAnnotations(root *cobra.Command) {
	walkProductMetricsCommands(root, false, func(cmd *cobra.Command, _ bool) {
		cmd.Annotations = cloneCommandAnnotations(cmd.Annotations)
		for _, key := range []string{
			productMetricsIDAnnotation,
			productMetricsModeAnnotation,
			productMetricsNoticeAnnotation,
			productMetricsRecordingAnnotation,
			productMetricsOwnerAnnotation,
			productMetricsResolverAnnotation,
			productMetricsExclusionAnnotation,
			productMetricsConditionalAnnotation,
			productMetricsCensusValidAnnotation,
		} {
			delete(cmd.Annotations, key)
		}
		// E1 exclusively owns the pack wildcard annotation. Never clear it.
		if cmd.Annotations[productMetricsClassAnnotation] != packCommandClassificationValue {
			delete(cmd.Annotations, productMetricsClassAnnotation)
		}
	})
}

func cloneCommandAnnotations(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func validateProductMetricsCommandCensus(root *cobra.Command, census []productMetricsCommandCensusEntry) error {
	if root == nil {
		return fmt.Errorf("product-metrics census: nil root")
	}
	if err := validateDeferredProductMetricsResolvers(census, generatedProductMetricsSyntheticCensus); err != nil {
		return err
	}

	live := make(map[string]productMetricsCommandCensusEntry)
	var liveErr error
	walkProductMetricsCommands(root, false, func(cmd *cobra.Command, effectiveHidden bool) {
		if liveErr != nil {
			return
		}
		if ignoreProductMetricsCensusCommand(cmd) {
			return
		}
		path := cmd.CommandPath()
		if _, exists := live[path]; exists {
			liveErr = fmt.Errorf("product-metrics census: duplicate live path %q", path)
			return
		}
		if err := validateSiblingCommandCollisions(cmd); err != nil {
			liveErr = err
			return
		}
		live[path] = productMetricsCommandCensusEntry{
			Path:               path,
			Aliases:            sortedStrings(cmd.Aliases),
			Hidden:             cmd.Hidden,
			EffectiveHidden:    effectiveHidden,
			DisableFlagParsing: cmd.DisableFlagParsing,
			Shape:              productMetricsShape(cmd),
		}
	})
	if liveErr != nil {
		return liveErr
	}

	declared := make(map[string]productMetricsCommandCensusEntry, len(census))
	for _, entry := range census {
		if err := validateProductMetricsCensusEntry(entry); err != nil {
			return err
		}
		if _, exists := declared[entry.Path]; exists {
			return fmt.Errorf("product-metrics census: duplicate manifest path %q", entry.Path)
		}
		declared[entry.Path] = entry
	}

	for path, got := range live {
		want, ok := declared[path]
		if !ok {
			return fmt.Errorf("product-metrics census: live command %q is missing", path)
		}
		if strings.Join(got.Aliases, "\x00") != strings.Join(sortedStrings(want.Aliases), "\x00") {
			return fmt.Errorf("product-metrics census: %q aliases = %q, want %q", path, got.Aliases, want.Aliases)
		}
		if got.Hidden != want.Hidden || got.EffectiveHidden != want.EffectiveHidden {
			return fmt.Errorf("product-metrics census: %q hidden state = (%t,%t), want (%t,%t)", path, got.Hidden, got.EffectiveHidden, want.Hidden, want.EffectiveHidden)
		}
		if got.Shape != want.Shape {
			return fmt.Errorf("product-metrics census: %q shape = %q, want %q", path, got.Shape, want.Shape)
		}
		if got.DisableFlagParsing != want.DisableFlagParsing {
			return fmt.Errorf("product-metrics census: %q DisableFlagParsing = %t, want %t", path, got.DisableFlagParsing, want.DisableFlagParsing)
		}
	}
	for path := range declared {
		if _, ok := live[path]; !ok {
			return fmt.Errorf("product-metrics census: manifest command %q is not live", path)
		}
	}
	return nil
}

func validateDeferredProductMetricsResolvers(census []productMetricsCommandCensusEntry, synthetic []productMetricsSyntheticCensusEntry) error {
	staticModes := make(map[productMetricsMode]struct{}, len(productMetricsStaticModeRegistry))
	for _, registration := range productMetricsStaticModeRegistry {
		if registration.Mode == "" || registration.Resolve == nil {
			return fmt.Errorf("product-metrics census: invalid static mode registration %q", registration.Mode)
		}
		if _, duplicate := staticModes[registration.Mode]; duplicate {
			return fmt.Errorf("product-metrics census: duplicate static mode registration %q", registration.Mode)
		}
		staticModes[registration.Mode] = struct{}{}
	}
	conditionalModes := make(map[productMetricsConditionalMode]struct{}, len(productMetricsConditionalRegistry))
	for _, registration := range productMetricsConditionalRegistry {
		if registration.Mode == "" || registration.Apply == nil {
			return fmt.Errorf("product-metrics census: invalid conditional mode registration %q", registration.Mode)
		}
		if _, duplicate := conditionalModes[registration.Mode]; duplicate {
			return fmt.Errorf("product-metrics census: duplicate conditional mode registration %q", registration.Mode)
		}
		conditionalModes[registration.Mode] = struct{}{}
	}
	resolvers := make(map[productMetricsResolverKey]struct{}, len(productMetricsResolverRegistry))
	for _, registration := range productMetricsResolverRegistry {
		if registration.Key == "" || registration.Resolve == nil {
			return fmt.Errorf("product-metrics census: invalid resolver registration %q", registration.Key)
		}
		if _, duplicate := resolvers[registration.Key]; duplicate {
			return fmt.Errorf("product-metrics census: duplicate resolver registration %q", registration.Key)
		}
		resolvers[registration.Key] = struct{}{}
	}
	for _, mode := range generatedProductMetricsGlobalConditionalModes {
		if _, ok := conditionalModes[mode]; !ok {
			return fmt.Errorf("product-metrics census: global conditional mode %q has no callback", mode)
		}
	}
	all := append(append([]productMetricsCommandCensusEntry(nil), census...), synthetic...)
	usedStatic := make(map[productMetricsMode]struct{})
	usedConditional := make(map[productMetricsConditionalMode]struct{})
	usedResolvers := make(map[productMetricsResolverKey]struct{})
	for _, mode := range generatedProductMetricsGlobalConditionalModes {
		usedConditional[mode] = struct{}{}
	}
	for _, entry := range all {
		decision, ok := lookupProductMetricsStaticMode(entry.Mode)
		if !ok || decision.Notice != entry.Notice || decision.Recording != entry.Recording || decision.Exclusion != entry.Exclusion {
			return fmt.Errorf("product-metrics census: %q mode %q policy drift", entry.Path, entry.Mode)
		}
		usedStatic[entry.Mode] = struct{}{}
		for _, mode := range entry.ConditionalModes {
			if _, ok := conditionalModes[mode]; !ok {
				return fmt.Errorf("product-metrics census: %q conditional mode %q has no callback", entry.Path, mode)
			}
			usedConditional[mode] = struct{}{}
		}
		if entry.Owner == productMetricsOwnerDeferred {
			if _, ok := resolvers[entry.Resolver]; !ok {
				return fmt.Errorf("product-metrics census: %q deferred resolver %q has no callback", entry.Path, entry.Resolver)
			}
			usedResolvers[entry.Resolver] = struct{}{}
		} else if entry.Resolver != "" {
			return fmt.Errorf("product-metrics census: %q has resolver without deferred ownership", entry.Path)
		}
	}
	if len(usedStatic) != len(staticModes) || len(usedConditional) != len(conditionalModes) || len(usedResolvers) != len(resolvers) {
		return fmt.Errorf("product-metrics census: registry coverage static=%d/%d conditional=%d/%d resolver=%d/%d", len(usedStatic), len(staticModes), len(usedConditional), len(conditionalModes), len(usedResolvers), len(resolvers))
	}
	return nil
}

func validateProductMetricsCensusEntry(entry productMetricsCommandCensusEntry) error {
	if entry.Path == "" || entry.Classification == "" {
		return fmt.Errorf("product-metrics census: %q has empty path or classification", entry.Path)
	}
	switch entry.Shape {
	case productMetricsShapeStructural, productMetricsShapeRunnable, productMetricsShapeRunnableGroup:
	default:
		return fmt.Errorf("product-metrics census: %q has invalid shape %q", entry.Path, entry.Shape)
	}
	if entry.Notice != productMetricsNoticeEligible && entry.Notice != productMetricsNoticeIneligible {
		return fmt.Errorf("product-metrics census: %q has invalid notice policy %q", entry.Path, entry.Notice)
	}
	if _, ok := lookupProductMetricsStaticMode(entry.Mode); !ok {
		return fmt.Errorf("product-metrics census: %q has invalid mode %q", entry.Path, entry.Mode)
	}
	if entry.Recording != productMetricsRecordingRecordable && entry.Recording != productMetricsRecordingExcluded {
		return fmt.Errorf("product-metrics census: %q has invalid recording policy %q", entry.Path, entry.Recording)
	}
	switch entry.Owner {
	case productMetricsOwnerStructural:
		if entry.Shape != productMetricsShapeStructural || entry.Resolver != "" {
			return fmt.Errorf("product-metrics census: %q has invalid structural owner", entry.Path)
		}
	case productMetricsOwnerImmediate:
		if entry.Shape == productMetricsShapeStructural || entry.Resolver != "" || entry.Recording == productMetricsRecordingExcluded {
			return fmt.Errorf("product-metrics census: %q has invalid immediate owner", entry.Path)
		}
	case productMetricsOwnerDeferred:
		if entry.Shape != productMetricsShapeRunnableGroup || entry.Resolver == "" || entry.Recording == productMetricsRecordingExcluded {
			return fmt.Errorf("product-metrics census: %q has invalid deferred owner", entry.Path)
		}
	case productMetricsOwnerExcluded:
		if entry.Recording != productMetricsRecordingExcluded || entry.Resolver != "" {
			return fmt.Errorf("product-metrics census: %q has invalid excluded owner", entry.Path)
		}
	default:
		return fmt.Errorf("product-metrics census: %q has invalid owner %q", entry.Path, entry.Owner)
	}
	if entry.Recording == productMetricsRecordingExcluded {
		if entry.Classification != "excluded" || entry.ID != 0 || entry.Exclusion == "" {
			return fmt.Errorf("product-metrics census: %q excluded policy has a recordable classification", entry.Path)
		}
	} else if entry.ID == 0 || entry.Exclusion != "" {
		return fmt.Errorf("product-metrics census: %q recordable policy has zero ID", entry.Path)
	}
	return nil
}

func productMetricsShape(cmd *cobra.Command) productMetricsCommandShape {
	switch {
	case cmd.Runnable() && cmd.HasSubCommands():
		return productMetricsShapeRunnableGroup
	case cmd.Runnable():
		return productMetricsShapeRunnable
	default:
		return productMetricsShapeStructural
	}
}

func walkProductMetricsCommands(root *cobra.Command, parentHidden bool, visit func(*cobra.Command, bool)) {
	effectiveHidden := parentHidden || root.Hidden
	visit(root, effectiveHidden)
	for _, child := range root.Commands() {
		walkProductMetricsCommands(child, effectiveHidden, visit)
	}
}

func validateSiblingCommandCollisions(parent *cobra.Command) error {
	seen := make(map[string]string)
	for _, child := range parent.Commands() {
		canonical := child.Name()
		for _, name := range append([]string{canonical}, child.Aliases...) {
			if previous, exists := seen[name]; exists {
				return fmt.Errorf("product-metrics census: sibling name/alias %q collides between %q and %q", name, previous, canonical)
			}
			seen[name] = canonical
		}
	}
	return nil
}

func findCommandByCanonicalPath(root *cobra.Command, path string) (*cobra.Command, bool) {
	var found *cobra.Command
	walkProductMetricsCommands(root, false, func(cmd *cobra.Command, _ bool) {
		if cmd.CommandPath() == path {
			found = cmd
		}
	})
	return found, found != nil
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
