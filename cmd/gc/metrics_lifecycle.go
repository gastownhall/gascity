package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
)

const productMetricsInvocationHourLayout = "2006-01-02T15:04:05Z"

var closeProductMetricsRecordingPermit = func(permit productMetricsRecordingPermit) error {
	return permit.Close()
}

var productMetricsInvocationNow = time.Now

type productMetricsInvocationLifecycle struct {
	service      productMetricsInvocationService
	permit       productMetricsRecordingPermit
	entryContext productMetricsInvocationContext
	policy       productMetricsPolicyContext
	noticed      atomic.Bool
	attempted    atomic.Bool
}

type productMetricsLifecycleBinding struct {
	lifecycle      *productMetricsInvocationLifecycle
	classification productMetricsClassification
}

type productMetricsEarlyOutcomeKind uint8

const (
	productMetricsEarlyOutcomeNone productMetricsEarlyOutcomeKind = iota
	productMetricsEarlyOutcomeJSONSchema
	productMetricsEarlyOutcomeJSONContractWarning
	productMetricsEarlyOutcomeJSONContractFailure
)

// productMetricsEarlyOutcome is the closed lifecycle projection for output
// paths that run before Cobra. It deliberately retains no argv, command,
// writer, error, output payload, path, or dynamic pack identity.
type productMetricsEarlyOutcome struct {
	kind           productMetricsEarlyOutcomeKind
	handled        bool
	exitCode       int
	classification productMetricsClassification
}

type productMetricsFinalOutcome struct {
	classification productMetricsClassification
}

type productMetricsDeferredOutcome struct {
	classification productMetricsClassification
}

// productMetricsDeferredAction is stack-local dispatch scaffolding. Callers
// construct and execute it in the same frame; only its closed outcome crosses
// into the lifecycle, and the invoke closure is never retained or returned.
type productMetricsDeferredAction struct {
	outcome productMetricsDeferredOutcome
	invoke  func() error
}

type productMetricsLifecycleContextKey struct{}

func openProductMetricsInvocationLifecycle(args []string) *productMetricsInvocationLifecycle {
	occurredHourUTC := productMetricsInvocationNow().UTC().Truncate(time.Hour).Format(productMetricsInvocationHourLayout)
	environment, policy := captureProductMetricsInvocationEnvironment()
	lifecycle := &productMetricsInvocationLifecycle{
		entryContext: productMetricsInvocationContext{
			DoNotTrack:          environment.doNotTrack,
			DisableUsageMetrics: environment.disableUsageMetrics,
			ManagedAutomation:   policy.ManagedAutomation || policy.ProviderHook,
			Recordable:          true,
			OccurredHourUTC:     occurredHourUTC,
		},
		policy: policy,
	}
	if command, ok := firstRootCommand(args); ok && command == "metrics" {
		return lifecycle
	}
	if productMetricsControlServiceFactory == nil {
		return lifecycle
	}
	control, err := productMetricsControlServiceFactory()
	if err != nil || control == nil {
		return lifecycle
	}
	service, ok := control.(productMetricsInvocationService)
	if !ok {
		return lifecycle
	}
	lifecycle.service = service
	lifecycle.permit = service.RecordingPermit(lifecycle.entryContext)
	return lifecycle
}

type productMetricsInvocationEnvironment struct {
	doNotTrack          string
	disableUsageMetrics string
}

func captureProductMetricsInvocationEnvironment() (productMetricsInvocationEnvironment, productMetricsPolicyContext) {
	environment := productMetricsInvocationEnvironment{
		doNotTrack:          os.Getenv("DO_NOT_TRACK"),
		disableUsageMetrics: os.Getenv("GC_DISABLE_USAGE_METRICS"),
	}
	managed := anyProductMetricsEnvironmentSet(
		"GC_SESSION_ID",
		"GC_SESSION_NAME",
		"GC_AGENT",
		"GC_TEMPLATE",
		"GC_MANAGED_SESSION_HOOK",
		"GC_HOOK_EVENT_NAME",
		"BEADS_ACTOR",
	)
	providerHook := anyProductMetricsEnvironmentSet(
		"GC_HOOK_SOURCE",
		"GC_PROVIDER_SESSION_ID",
		"GC_PROVIDER_SESSION_ID_REQUIRED",
	)
	return environment, productMetricsPolicyContext{ManagedAutomation: managed, ProviderHook: providerHook}
}

func anyProductMetricsEnvironmentSet(names ...string) bool {
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func (lifecycle *productMetricsInvocationLifecycle) Close() {
	if lifecycle == nil {
		return
	}
	_ = closeProductMetricsRecordingPermit(lifecycle.permit)
}

func (lifecycle *productMetricsInvocationLifecycle) prepareNotice(classification productMetricsClassification, writer io.Writer) {
	if classification.ID == productMetricsCommandUnknown && classification.Notice == productMetricsNoticeEligible &&
		classification.Owner == productMetricsOwnerDeferred && classification.Resolver == productMetricsResolverRootDispatch {
		return
	}
	lifecycle.prepareResolvedNotice(classification, writer)
}

func (lifecycle *productMetricsInvocationLifecycle) prepareResolvedNotice(classification productMetricsClassification, writer io.Writer) {
	if lifecycle == nil || lifecycle.service == nil {
		return
	}
	if !lifecycle.noticed.CompareAndSwap(false, true) {
		return
	}
	invocation := lifecycle.entryContext
	invocation.NoticeEligible = classification.Notice == productMetricsNoticeEligible
	invocation.Recordable = classification.Recording == productMetricsRecordingRecordable
	_ = lifecycle.service.MaybeActivateNotice(invocation, writer)
}

func (lifecycle *productMetricsInvocationLifecycle) attemptClassification(classification productMetricsClassification) {
	if lifecycle == nil || !lifecycle.attempted.CompareAndSwap(false, true) {
		return
	}
	if lifecycle.service == nil || classification.Recording != productMetricsRecordingRecordable || classification.ID == 0 {
		return
	}
	_ = lifecycle.service.RecordOnce(lifecycle.permit, classification.ID)
}

func (lifecycle *productMetricsInvocationLifecycle) attemptPackOutcome(outcome packCommandOutcome) {
	if lifecycle == nil {
		return
	}
	lifecycle.attemptClassification(classifyProductMetricsPackOutcome(outcome, lifecycle.policy))
}

func (lifecycle *productMetricsInvocationLifecycle) attemptEarlyOutcome(outcome productMetricsEarlyOutcome) {
	if lifecycle == nil || outcome.kind == productMetricsEarlyOutcomeNone {
		return
	}
	lifecycle.attemptClassification(outcome.classification)
}

func (lifecycle *productMetricsInvocationLifecycle) attemptFinalOutcome(outcome productMetricsFinalOutcome) {
	if lifecycle == nil {
		return
	}
	lifecycle.attemptClassification(outcome.classification)
}

func (lifecycle *productMetricsInvocationLifecycle) attemptDeferredOutcome(outcome productMetricsDeferredOutcome) {
	if lifecycle == nil {
		return
	}
	lifecycle.attemptClassification(outcome.classification)
}

func resolveProductMetricsFinalOutcome(command *cobra.Command, initial productMetricsClassification) productMetricsFinalOutcome {
	if initial.Recording == productMetricsRecordingExcluded || initial.ID == productMetricsCommandHelp ||
		initial.ID == productMetricsCommandUnknown || initial.ID == productMetricsCommandPackCommand || command == nil {
		return productMetricsFinalOutcome{classification: initial}
	}
	resolved, ok := classificationFromCommandAnnotations(command)
	if !ok {
		return productMetricsFinalOutcome{classification: failClosedProductMetricsClassification()}
	}
	resolved.Notice = initial.Notice
	resolved.Recording = initial.Recording
	resolved.Exclusion = initial.Exclusion
	if resolved.Recording == productMetricsRecordingExcluded {
		resolved.ID = 0
		resolved.Owner = productMetricsOwnerExcluded
		resolved.Resolver = ""
	}
	return productMetricsFinalOutcome{classification: resolved}
}

func executeProductMetricsDeferredAction(command *cobra.Command, action productMetricsDeferredAction) error {
	binding := productMetricsLifecycleBindingForCommand(command)
	if binding.lifecycle != nil {
		binding.lifecycle.attemptDeferredOutcome(action.outcome)
	}
	if action.invoke == nil {
		return nil
	}
	return action.invoke()
}

func resolveProductMetricsEarlyOutcome(action jsonPreparedEarlyAction, classification productMetricsClassification) productMetricsEarlyOutcome {
	kind := productMetricsEarlyOutcomeNone
	switch action.kind {
	case jsonPreparedEarlySchema:
		kind = productMetricsEarlyOutcomeJSONSchema
	case jsonPreparedEarlyContractWarning:
		kind = productMetricsEarlyOutcomeJSONContractWarning
	case jsonPreparedEarlyContractFailure:
		kind = productMetricsEarlyOutcomeJSONContractFailure
	}
	return productMetricsEarlyOutcome{
		kind:           kind,
		handled:        action.handled,
		exitCode:       action.exitCode,
		classification: classification,
	}
}

func executeProductMetricsEarlyOutcome(outcome productMetricsEarlyOutcome, action jsonPreparedEarlyAction, stdout, stderr io.Writer) (bool, int) {
	if action.emit == nil {
		return outcome.handled, outcome.exitCode
	}
	_, code := action.execute(stdout, stderr)
	return outcome.handled, code
}

func bindProductMetricsInvocationLifecycle(root *cobra.Command, args []string, lifecycle *productMetricsInvocationLifecycle) productMetricsLifecycleBinding {
	if root == nil || lifecycle == nil {
		return productMetricsLifecycleBinding{}
	}
	binding := productMetricsLifecycleBinding{
		lifecycle:      lifecycle,
		classification: classifyProductMetricsCommand(root, args, lifecycle.policy),
	}
	ctx := root.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	root.SetContext(context.WithValue(ctx, productMetricsLifecycleContextKey{}, binding))
	installProductMetricsInvocationWrappers(root, binding)
	return binding
}

func installProductMetricsInvocationWrappers(root *cobra.Command, binding productMetricsLifecycleBinding) {
	if root == nil || binding.lifecycle == nil {
		return
	}
	helpFunctions := make(map[*cobra.Command]func(*cobra.Command, []string))
	walkProductMetricsCommands(root, false, func(command *cobra.Command, _ bool) {
		helpFunctions[command] = command.HelpFunc()
	})
	walkProductMetricsCommands(root, false, func(command *cobra.Command, _ bool) {
		if command.Annotations[productMetricsClassAnnotation] == packCommandClassificationValue {
			return
		}
		originalHelp := helpFunctions[command]
		command.SetHelpFunc(func(helpCommand *cobra.Command, helpArgs []string) {
			outcome := resolveProductMetricsFinalOutcome(helpCommand, binding.classification)
			if productMetricsOwner(command.Annotations[productMetricsOwnerAnnotation]) == productMetricsOwnerDeferred {
				binding.lifecycle.attemptDeferredOutcome(productMetricsDeferredOutcome(outcome))
			} else {
				binding.lifecycle.attemptFinalOutcome(outcome)
			}
			originalHelp(helpCommand, helpArgs)
		})
		owner := productMetricsOwner(command.Annotations[productMetricsOwnerAnnotation])
		if owner == productMetricsOwnerImmediate {
			wrapProductMetricsPreRuns(command, binding)
			if originalRunE := command.RunE; originalRunE != nil {
				command.RunE = func(runCommand *cobra.Command, runArgs []string) error {
					binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
					return originalRunE(runCommand, runArgs)
				}
			}
			if originalRun := command.Run; originalRun != nil {
				command.Run = func(runCommand *cobra.Command, runArgs []string) {
					binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
					originalRun(runCommand, runArgs)
				}
			}
			return
		}
		if owner == productMetricsOwnerDeferred && command != root {
			wrapProductMetricsDeferredRun(command, binding)
		}
	})
}

func wrapProductMetricsDeferredRun(command *cobra.Command, binding productMetricsLifecycleBinding) {
	outcome := resolveProductMetricsFinalOutcome(command, binding.classification)
	deferred := productMetricsDeferredOutcome(outcome)
	if original := command.RunE; original != nil {
		command.RunE = func(runCommand *cobra.Command, runArgs []string) error {
			return executeProductMetricsDeferredAction(runCommand, productMetricsDeferredAction{
				outcome: deferred,
				invoke:  func() error { return original(runCommand, runArgs) },
			})
		}
	}
	if original := command.Run; original != nil {
		command.Run = func(runCommand *cobra.Command, runArgs []string) {
			_ = executeProductMetricsDeferredAction(runCommand, productMetricsDeferredAction{
				outcome: deferred,
				invoke: func() error {
					original(runCommand, runArgs)
					return nil
				},
			})
		}
	}
}

func wrapProductMetricsPreRuns(command *cobra.Command, binding productMetricsLifecycleBinding) {
	if original := command.PersistentPreRunE; original != nil {
		command.PersistentPreRunE = func(runCommand *cobra.Command, runArgs []string) error {
			binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
			return original(runCommand, runArgs)
		}
	}
	if original := command.PersistentPreRun; original != nil {
		command.PersistentPreRun = func(runCommand *cobra.Command, runArgs []string) {
			binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
			original(runCommand, runArgs)
		}
	}
	if original := command.PreRunE; original != nil {
		command.PreRunE = func(runCommand *cobra.Command, runArgs []string) error {
			binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
			return original(runCommand, runArgs)
		}
	}
	if original := command.PreRun; original != nil {
		command.PreRun = func(runCommand *cobra.Command, runArgs []string) {
			binding.lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(runCommand, binding.classification))
			original(runCommand, runArgs)
		}
	}
}

func attemptProductMetricsForCommand(command *cobra.Command) {
	binding := productMetricsLifecycleBindingForCommand(command)
	if binding.lifecycle == nil {
		return
	}
	outcome := resolveProductMetricsFinalOutcome(command, binding.classification)
	binding.lifecycle.prepareResolvedNotice(outcome.classification, command.ErrOrStderr())
	binding.lifecycle.attemptFinalOutcome(outcome)
}

func productMetricsLifecycleBindingForCommand(command *cobra.Command) productMetricsLifecycleBinding {
	if command == nil {
		return productMetricsLifecycleBinding{}
	}
	if command.Context() != nil {
		if binding, ok := command.Context().Value(productMetricsLifecycleContextKey{}).(productMetricsLifecycleBinding); ok {
			return binding
		}
	}
	root := command.Root()
	if root != nil && root != command && root.Context() != nil {
		binding, _ := root.Context().Value(productMetricsLifecycleContextKey{}).(productMetricsLifecycleBinding)
		return binding
	}
	return productMetricsLifecycleBinding{}
}

func executeProductMetricsPackAction(command *cobra.Command, action packCommandAction) packCommandOutcome {
	binding := productMetricsLifecycleBindingForCommand(command)
	if binding.lifecycle == nil {
		return action.execute()
	}
	return action.executeReporting(func(outcome packCommandOutcome) {
		classification := classifyProductMetricsPackOutcome(outcome, binding.lifecycle.policy)
		noticeClassification := classification
		if action.selected {
			noticeClassification.Notice = productMetricsNoticeIneligible
		}
		binding.lifecycle.prepareResolvedNotice(noticeClassification, command.ErrOrStderr())
		binding.lifecycle.attemptClassification(classification)
	})
}
