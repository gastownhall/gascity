package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const productMetricsIndependenceText = "Gas City OTel, local costs, event export, and Beads telemetry are separate and unchanged."

type productMetricsStatusQueueJSON struct {
	Available        bool   `json:"available"`
	Events           uint64 `json:"events"`
	Bytes            uint64 `json:"bytes"`
	OldestAgeSeconds *int64 `json:"oldest_age_seconds"`
}

type productMetricsStatusDiagnosticsJSON struct {
	Available                bool    `json:"available"`
	DroppedEvents            uint64  `json:"dropped_events"`
	LastUploadAttemptHourUTC *string `json:"last_upload_attempt_hour_utc"`
	LastUploadSuccessHourUTC *string `json:"last_upload_success_hour_utc"`
	LastErrorClass           *string `json:"last_error_class"`
	SpawnThrottleAgeSeconds  *int64  `json:"spawn_throttle_age_seconds"`
}

type productMetricsStatusRetentionJSON struct {
	EdgeLogDays     uint64 `json:"edge_log_days"`
	RawEventDays    uint64 `json:"raw_event_days"`
	AggregateMonths uint64 `json:"aggregate_months"`
	PrivacyURL      string `json:"privacy_url"`
}

type productMetricsStatusJSON struct {
	OK                     bool                                `json:"ok"`
	State                  productMetricsEffectiveState        `json:"state"`
	Reason                 productMetricsStateReason           `json:"reason"`
	HomeStable             bool                                `json:"home_stable"`
	HomeReason             *string                             `json:"home_reason"`
	ConfigPath             string                              `json:"config_path"`
	ConfigPresent          bool                                `json:"config_present"`
	StateSchema            uint64                              `json:"state_schema"`
	RequiredNoticeVersion  uint64                              `json:"required_notice_version"`
	AcceptedNoticeVersion  uint64                              `json:"accepted_notice_version"`
	EndpointHostname       string                              `json:"endpoint_hostname"`
	InstallationIDPresent  bool                                `json:"installation_id_present"`
	SpoolGenerationPresent bool                                `json:"spool_generation_present"`
	CleanupPending         bool                                `json:"cleanup_pending"`
	Queue                  productMetricsStatusQueueJSON       `json:"queue"`
	Diagnostics            productMetricsStatusDiagnosticsJSON `json:"diagnostics"`
	Retention              productMetricsStatusRetentionJSON   `json:"retention"`
	Independence           string                              `json:"independence"`
}

func newMetricsCmd(stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:           "metrics",
		Short:         "Inspect or control Gas City command usage metrics",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			return runProductMetricsStatus(stdout, stderr, false, false)
		},
	}
	command.AddCommand(
		newMetricsStatusCmd(stdout, stderr),
		newMetricsOnCmd(stdout, stderr),
		newMetricsOffCmd(stdout, stderr),
		newMetricsExampleCmd(stdout),
	)
	registerProductMetricsBuildCommands(command)
	return command
}

func newMetricsStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	var showInstallationID bool
	command := &cobra.Command{
		Use:           "status",
		Short:         "Show redacted local command-usage metrics status",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			return runProductMetricsStatus(stdout, stderr, jsonOutput, showInstallationID)
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "write the redacted status as JSON")
	command.Flags().BoolVar(&showInstallationID, "show-installation-id", false,
		"print the stable linkable installation pseudonym with a warning")
	command.MarkFlagsMutuallyExclusive("json", "show-installation-id")
	return command
}

func newMetricsOnCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "on",
		Short:         "Read and accept the command-usage disclosure on a verified TTY",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			service, err := openProductMetricsControlService()
			if err != nil {
				fmt.Fprintln(stderr, "gc metrics on: product metrics are unavailable") //nolint:errcheck // bounded CLI error
				return errExit
			}
			ctx, cancel := productMetricsControlContext()
			defer cancel()
			if err := service.Enable(ctx, productMetricsExplicitEnableInvocation(), stderr); err != nil {
				status := service.Status(context.Background())
				fmt.Fprintf(stderr, "gc metrics on: cannot enable while state is %s (%s)\n", status.State, status.Reason) //nolint:errcheck // bounded enums only
				return errExit
			}
			fmt.Fprintln(stdout, "Gas City command usage metrics are enabled. This command was not recorded.") //nolint:errcheck // best-effort stdout
			return nil
		},
	}
}

func newMetricsOffCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "off",
		Short:         "Disable command usage metrics and delete local queued data",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			service, err := openProductMetricsControlService()
			if err != nil {
				fmt.Fprintln(stderr, "gc metrics off: product metrics are unavailable; durable opt-out was not proven. Retry `gc metrics off`.") //nolint:errcheck // bounded CLI error
				return errExit
			}
			result, disableErr := service.DisableAndPurge(context.Background())
			if disableErr != nil {
				writeProductMetricsOffFailure(stderr, result, disableErr)
				return errExit
			}
			if result.Outcome != productMetricsPurgeCompleted && result.Outcome != productMetricsPurgeAlreadyDisabled {
				fmt.Fprintln(stderr, "gc metrics off: local cleanup did not report a complete result. Retry `gc metrics off`.") //nolint:errcheck // bounded CLI error
				return errExit
			}
			writeProductMetricsOffSuccess(stdout, result)
			return nil
		},
	}
}

func newMetricsExampleCmd(stdout io.Writer) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:           "example",
		Short:         "Print the fixed state-independent command-usage request example",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			encoded, err := encodeProductMetricsExampleBatch()
			if err != nil {
				return errExit
			}
			if !jsonOutput {
				fmt.Fprintln(stdout, "Fixed placeholders below show the exact request shape; no local state, ID, clock, or random source was read.") //nolint:errcheck // best-effort stdout
			}
			if written, err := stdout.Write(encoded); err != nil || written != len(encoded) {
				return errExit
			}
			if !jsonOutput {
				_, _ = io.WriteString(stdout, "\n")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "write only the exact example JSON")
	return command
}

func openProductMetricsControlService() (productMetricsControlService, error) {
	if productMetricsControlServiceFactory == nil {
		return nil, errors.New("product metrics control service factory is unavailable")
	}
	service, err := productMetricsControlServiceFactory()
	if err != nil || service == nil {
		return nil, errors.New("product metrics control service is unavailable")
	}
	return service, nil
}

func runProductMetricsStatus(stdout, stderr io.Writer, jsonOutput, showInstallationID bool) error {
	if jsonOutput && showInstallationID {
		fmt.Fprintln(stderr, "gc metrics status: --json and --show-installation-id cannot be used together") //nolint:errcheck // bounded CLI error
		return errExit
	}
	service, err := openProductMetricsControlService()
	if err != nil {
		fmt.Fprintln(stderr, "gc metrics status: product metrics are unavailable") //nolint:errcheck // bounded CLI error
		return errExit
	}
	status := service.Status(context.Background())
	policy := service.PolicyMetadata()
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(productMetricsStatusForJSON(status, policy)); err != nil {
			return errExit
		}
		return nil
	}
	writeProductMetricsStatusText(stdout, status, policy)
	if showInstallationID {
		fmt.Fprintln(stdout, "Warning: this stable installation ID is a linkable pseudonym. Do not put it in public logs.")                              //nolint:errcheck // deliberate disclosure warning
		fmt.Fprintln(stdout, "`gc metrics off` deletes it locally, makes no remote request, and can make a later targeted deletion request impossible.") //nolint:errcheck // deliberate disclosure warning
		if installationID, present := service.InstallationIDForDisclosure(context.Background()); present {
			fmt.Fprintf(stdout, "Installation ID: %s\n", installationID) //nolint:errcheck // explicitly requested pseudonym disclosure
		} else {
			fmt.Fprintln(stdout, "Installation ID: not present") //nolint:errcheck // best-effort stdout
		}
	}
	return nil
}

func productMetricsStatusForJSON(status productMetricsStatus, policy productMetricsPolicyMetadata) productMetricsStatusJSON {
	var oldestAgeSeconds *int64
	if status.OldestQueuedEventPresent {
		value := durationSeconds(status.OldestQueuedEventAge)
		oldestAgeSeconds = &value
	}
	var throttleAgeSeconds *int64
	if status.SpawnThrottlePresent {
		value := durationSeconds(status.SpawnThrottleAge)
		throttleAgeSeconds = &value
	}
	return productMetricsStatusJSON{
		OK:                     true,
		State:                  status.State,
		Reason:                 status.Reason,
		HomeStable:             status.HomeStable,
		HomeReason:             optionalString(string(status.HomeReason)),
		ConfigPath:             status.ConfigPath,
		ConfigPresent:          status.ConfigPresent,
		StateSchema:            status.StateSchema,
		RequiredNoticeVersion:  status.RequiredNoticeVersion,
		AcceptedNoticeVersion:  status.AcceptedNoticeVersion,
		EndpointHostname:       policy.EndpointHostname,
		InstallationIDPresent:  status.InstallationIDPresent,
		SpoolGenerationPresent: status.SpoolGenerationPresent,
		CleanupPending:         status.CleanupPending,
		Queue: productMetricsStatusQueueJSON{
			Available:        status.QueueDiagnosticsAvailable,
			Events:           status.QueueEvents,
			Bytes:            status.QueueBytes,
			OldestAgeSeconds: oldestAgeSeconds,
		},
		Diagnostics: productMetricsStatusDiagnosticsJSON{
			Available:                status.StatusDiagnosticsAvailable,
			DroppedEvents:            status.DroppedEvents,
			LastUploadAttemptHourUTC: optionalString(status.LastUploadAttemptHourUTC),
			LastUploadSuccessHourUTC: optionalString(status.LastUploadSuccessHourUTC),
			LastErrorClass:           optionalString(string(status.LastErrorClass)),
			SpawnThrottleAgeSeconds:  throttleAgeSeconds,
		},
		Retention: productMetricsStatusRetentionJSON{
			EdgeLogDays:     policy.EdgeLogRetentionDays,
			RawEventDays:    policy.RawEventRetentionDays,
			AggregateMonths: policy.AggregateRetentionMonths,
			PrivacyURL:      policy.PrivacyURL,
		},
		Independence: productMetricsIndependenceText,
	}
}

func writeProductMetricsStatusText(stdout io.Writer, status productMetricsStatus, policy productMetricsPolicyMetadata) {
	fmt.Fprintln(stdout, "Gas City command usage metrics")                 //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  State: %s (%s)\n", status.State, status.Reason) //nolint:errcheck // bounded enums
	if status.HomeStable {
		fmt.Fprintln(stdout, "  Home: stable") //nolint:errcheck // bounded provenance projection
	} else {
		fmt.Fprintf(stdout, "  Home: unavailable (%s)\n", valueOrNone(string(status.HomeReason))) //nolint:errcheck // bounded enum only
	}
	_, _ = fmt.Fprintf(stdout, "  State schema: %d; notice required/accepted: %d/%d\n",
		status.StateSchema, status.RequiredNoticeVersion, status.AcceptedNoticeVersion)
	configPresence := "absent"
	if status.ConfigPresent {
		configPresence = "present"
	}
	fmt.Fprintf(stdout, "  Config: %s (%s)\n", status.ConfigPath, configPresence) //nolint:errcheck // explicitly documented metrics path
	endpoint := policy.EndpointHostname
	if endpoint == "" {
		endpoint = "not configured"
	}
	fmt.Fprintf(stdout, "  Endpoint: %s\n", endpoint) //nolint:errcheck // hostname only
	installation := "absent"
	if status.InstallationIDPresent {
		installation = "present (redacted)"
	}
	fmt.Fprintf(stdout, "  Installation ID: %s\n", installation)                                  //nolint:errcheck // redacted by default
	fmt.Fprintf(stdout, "  Spool generation present: %s\n", yesNo(status.SpoolGenerationPresent)) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Cleanup pending: %s\n", yesNo(status.CleanupPending))                  //nolint:errcheck // best-effort stdout
	if status.QueueDiagnosticsAvailable {
		oldest := "none"
		if status.OldestQueuedEventPresent {
			oldest = status.OldestQueuedEventAge.String()
		}
		fmt.Fprintf(stdout, "  Queue: %d events, %s, oldest %s\n", status.QueueEvents, formatProductMetricsBytes(status.QueueBytes), oldest) //nolint:errcheck // bounded aggregates
	} else {
		fmt.Fprintln(stdout, "  Queue: unavailable") //nolint:errcheck // best-effort stdout
	}
	if status.StatusDiagnosticsAvailable {
		fmt.Fprintf(stdout, "  Dropped events: %d\n", status.DroppedEvents)                               //nolint:errcheck // bounded aggregate
		fmt.Fprintf(stdout, "  Last upload attempt: %s\n", valueOrNever(status.LastUploadAttemptHourUTC)) //nolint:errcheck // canonical hour or never
		fmt.Fprintf(stdout, "  Last upload success: %s\n", valueOrNever(status.LastUploadSuccessHourUTC)) //nolint:errcheck // canonical hour or never
		fmt.Fprintf(stdout, "  Last error: %s\n", valueOrNone(string(status.LastErrorClass)))             //nolint:errcheck // closed class or none
	} else {
		fmt.Fprintln(stdout, "  Diagnostics: unavailable") //nolint:errcheck // bounded read-only status
	}
	spawnAge := "none"
	if status.SpawnThrottlePresent {
		spawnAge = status.SpawnThrottleAge.String()
	}
	fmt.Fprintf(stdout, "  Spawn throttle age: %s\n", spawnAge)                                                                                                                                   //nolint:errcheck // bounded age
	fmt.Fprintln(stdout, "  Fields sent: schema_version, event_id, installation_id, app, release_version, os, occurred_hour_utc, command_id.")                                                    //nolint:errcheck // closed DTO disclosure
	fmt.Fprintln(stdout, "  Fields never sent: arguments, flag values, paths, names, prompts, output, error text, exact timestamps, durations, outcomes, models, tokens, costs, or credentials.") //nolint:errcheck // privacy disclosure
	_, _ = fmt.Fprintf(stdout, "  Retention: edge logs %d days; raw events %d days; aggregate facts %d months.\n",
		policy.EdgeLogRetentionDays, policy.RawEventRetentionDays, policy.AggregateRetentionMonths)
	privacyURL := policy.PrivacyURL
	if privacyURL == "" {
		privacyURL = "not configured in this build"
	}
	fmt.Fprintf(stdout, "  Privacy and deletion contact: %s\n", privacyURL) //nolint:errcheck // compiled policy only
	fmt.Fprintln(stdout, "  "+productMetricsIndependenceText)               //nolint:errcheck // best-effort stdout
}

func writeProductMetricsOffSuccess(stdout io.Writer, result productMetricsPurgeResult) {
	if result.Outcome == productMetricsPurgeAlreadyDisabled {
		fmt.Fprintln(stdout, "Gas City command usage metrics were already disabled and locally clean; uploader quiescence was rechecked.") //nolint:errcheck // best-effort stdout
		return
	}
	_, _ = fmt.Fprintf(stdout, "Gas City command usage metrics are disabled. Removed %d queued events (%s) and deleted this installation ID. ",
		result.RemovedEvents, formatProductMetricsBytes(result.RemovedBytes))
	fmt.Fprintln(stdout, "Data accepted before or while this command waited was not deleted: raw events expire within 90 days and pseudonymous aggregate facts within 13 months. This command made no server request; use the published deletion contact with an ID you saved before opt-out for a targeted request. Gas City OTel, redacted event export, local cost records, and Beads telemetry were not changed.") //nolint:errcheck // approved opt-out disclosure
	if result.RecoveredState {
		fmt.Fprintln(stdout, "Recovered a corrupt local consent record while completing the disable barrier.") //nolint:errcheck // bounded recovery result
	}
}

func writeProductMetricsOffFailure(stderr io.Writer, result productMetricsPurgeResult, err error) {
	class := productMetricsPurgeErrorClass(err)
	phase := productMetricsPurgeIncompletePhase(result)
	phaseText := ""
	if phase != "" {
		phaseText = " (phase " + phase + ")"
	}
	if result.DisabledDurable {
		fmt.Fprintf(stderr, "gc metrics off: %s%s. Future collection and new uploads are already disabled; only local quiescence/deletion proof remains. Retry `gc metrics off`.\n", class, phaseText) //nolint:errcheck // bounded classes only
		if result.ManualCleanupRequired {
			reason := productMetricsPurgeManualReason(result)
			fmt.Fprintf(stderr, "Manual cleanup is required (%s): a same-UID filesystem change left residue this binary cannot safely delete. Inspect only the product-usage root shown by `gc metrics status`, verify ownership, remove the residue, then retry `gc metrics off`.\n", reason) //nolint:errcheck // bounded guidance only
		}
		return
	}
	fmt.Fprintf(stderr, "gc metrics off: %s%s; could not prove durable opt-out. Previous state may remain. Retry `gc metrics off`.\n", class, phaseText) //nolint:errcheck // bounded classes only
}

func productMetricsPurgeIncompletePhase(result productMetricsPurgeResult) string {
	switch result.IncompletePhase {
	case productMetricsPurgeIncompleteDisableWrite:
		return "disable-write"
	case productMetricsPurgeIncompleteUploaderQuiescence:
		return "uploader-quiescence"
	case productMetricsPurgeIncompleteLocalCleanup:
		return "local-cleanup"
	case productMetricsPurgeIncompleteFinalProof:
		return "final-proof"
	default:
		return ""
	}
}

func productMetricsPurgeManualReason(result productMetricsPurgeResult) string {
	switch result.ManualCleanupReason {
	case productMetricsPurgeManualUnsettledJournal:
		return "unsettled-root-temp-journal"
	case productMetricsPurgeManualUnrecognizedEntry:
		return "unrecognized-root-entry"
	default:
		return "local-residue"
	}
}

func productMetricsPurgeErrorClass(err error) productMetricsPurgeClass {
	var purgeErr *productMetricsPurgeError
	if errors.As(err, &purgeErr) && purgeErr != nil {
		switch purgeErr.Class {
		case productMetricsPurgeErrorInvalidRequest,
			productMetricsPurgeErrorDisableWrite,
			productMetricsPurgeErrorUploaderQuiescence,
			productMetricsPurgeErrorCleanupIncomplete,
			productMetricsPurgeErrorStateChanged,
			productMetricsPurgeErrorStorage:
			return purgeErr.Class
		}
	}
	return productMetricsPurgeErrorStorage
}

func formatProductMetricsBytes(value uint64) string {
	if value > math.MaxInt64 {
		return "more than 8.0 EiB"
	}
	return formatBytes(int64(value))
}

func durationSeconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value / time.Second)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	cloned := strings.Clone(value)
	return &cloned
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func valueOrNever(value string) string {
	if value == "" {
		return "never"
	}
	return value
}

func valueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
