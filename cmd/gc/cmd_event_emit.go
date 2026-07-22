package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

func newEventCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "event",
		Short: "Event operations",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc event: missing subcommand (emit)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc event: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newEventEmitCmd(stdout, stderr))
	return cmd
}

type eventEmitJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	EventType     string `json:"event_type"`
	Actor         string `json:"actor"`
	Subject       string `json:"subject,omitempty"`
	Message       string `json:"message,omitempty"`
	HasPayload    bool   `json:"has_payload"`
	Submitted     bool   `json:"submitted"`
}

func newEventEmitCmd(stdout, stderr io.Writer) *cobra.Command {
	var subject, message, actor, payload, beadPayload string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "emit <type>",
		Short: "Emit an event to the city event log",
		Long: `Record a custom event to the city event log.

Best-effort: always exits 0 so bead hooks never fail. Supports attaching
arbitrary JSON payloads to custom event types. bead.created, bead.updated, and
bead.closed resolve a bead ID from --bead-payload, --subject, or the caller JSON
and replace the caller JSON with an authoritative snapshot from the owning bead
store. bead.deleted accepts a decodable ID-only payload. bead.closed also
requires the authoritative status to be "closed". For lifecycle events,
--subject defaults to the resolved bead ID when omitted, and explicitly
supplied identities must agree. An explicitly empty subject counts as supplied
and is rejected. JSON summaries report whether submission to the configured
provider was attempted; the event bus does not acknowledge durable persistence.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			effectiveActor := actor
			if effectiveActor == "" {
				effectiveActor = eventActor()
			}
			subjectSupplied := cmd.Flags().Changed("subject")
			finalPayload, effectiveSubject, prepared := eventPayloadAndSubjectForEmit(
				args[0], subject, subjectSupplied, payload, beadPayload, stderr,
			)
			submitted := false
			if jsonOut {
				if prepared {
					submitted = cmdEventEmitSubmittedWithSubjectPresence(args[0], effectiveSubject, subjectSupplied, message, effectiveActor, finalPayload, stderr)
				}
				return writeCLIJSONLineOrErr(stdout, stderr, "gc event emit", eventEmitJSONResult{
					SchemaVersion: "1",
					OK:            true,
					EventType:     args[0],
					Actor:         effectiveActor,
					Subject:       effectiveSubject,
					Message:       message,
					HasPayload:    finalPayload != "",
					Submitted:     submitted,
				})
			}
			if !prepared {
				return nil
			}
			if cmdEventEmitWithSubjectPresence(args[0], effectiveSubject, subjectSupplied, message, effectiveActor, finalPayload, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "Event subject (e.g. bead ID)")
	cmd.Flags().StringVar(&message, "message", "", "Event message")
	cmd.Flags().StringVar(&actor, "actor", "", "Actor name (default: $GC_ALIAS, else $GC_AGENT, else $GC_SESSION_ID, else \"human\")")
	cmd.Flags().StringVar(&payload, "payload", "", "JSON payload to attach to the event")
	cmd.Flags().StringVar(&beadPayload, "bead-payload", "", "Owning-store bead ID for lifecycle payloads")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

func eventPayloadAndSubjectForEmit(eventType, subject string, subjectSupplied bool, payload, beadPayload string, stderr io.Writer) (string, string, bool) {
	if !requiresAuthoritativeLifecyclePayload(eventType) {
		// A deleted bead can no longer be hydrated from its store, so bead.deleted
		// resolves its snapshot without a store round-trip: an explicit valid
		// --payload wins, otherwise the ID-only snapshot the contract accepts is
		// synthesized from --bead-payload (or an explicit --subject). This keeps
		// the natural `gc event emit bead.deleted --bead-payload <id>` invocation
		// from being silently dropped when hydration of the gone bead fails.
		if eventType == events.BeadDeleted && (payload == "" || !json.Valid([]byte(payload))) {
			if id := beadDeletedFallbackID(beadPayload, subject, subjectSupplied); id != "" {
				if synthesized, err := json.Marshal(map[string]string{"id": id}); err == nil {
					return string(synthesized), eventSubjectForEmitWithPresence(eventType, subject, string(synthesized), subjectSupplied), true
				}
			}
		}
		finalPayload := eventPayloadForEmit(payload, beadPayload, stderr)
		return finalPayload, eventSubjectForEmitWithPresence(eventType, subject, finalPayload, subjectSupplied), true
	}

	beadID, ok := eventLifecycleBeadID(eventType, subject, subjectSupplied, payload, beadPayload, stderr)
	if !ok {
		return "", subject, false
	}
	effectiveSubject := subject
	if !subjectSupplied {
		effectiveSubject = beadID
	}
	canonicalPayload, err := loadAuthoritativeEventBeadPayload(beadID)
	if err != nil {
		fmt.Fprintf(stderr, "gc event emit: %s cannot load authoritative bead payload for %s: %v\n", eventType, beadID, err) //nolint:errcheck // best-effort stderr
		return "", effectiveSubject, false
	}
	canonicalBead, decoded := beads.DecodeBeadEventPayload(canonicalPayload)
	if !decoded || canonicalBead.ID != beadID {
		fmt.Fprintf(stderr, "gc event emit: %s owning store returned an invalid bead snapshot for %s\n", eventType, beadID) //nolint:errcheck // best-effort stderr
		return "", effectiveSubject, false
	}
	return string(canonicalPayload), effectiveSubject, true
}

func eventLifecycleBeadID(eventType, subject string, subjectSupplied bool, payload, beadPayload string, stderr io.Writer) (string, bool) {
	if subjectSupplied && strings.TrimSpace(subject) == "" {
		if callerBead, ok := beads.DecodeBeadEventPayload(json.RawMessage(payload)); ok {
			fmt.Fprintf(stderr, "gc event emit: %s subject %q does not match payload bead id %q\n", eventType, subject, callerBead.ID) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc event emit: %s requires a non-empty --subject when the flag is supplied\n", eventType) //nolint:errcheck // best-effort stderr
		}
		return "", false
	}

	type identity struct {
		source string
		value  string
	}
	identities := make([]identity, 0, 3)
	if id := strings.TrimSpace(beadPayload); id != "" {
		identities = append(identities, identity{source: "--bead-payload", value: id})
	}
	if subjectSupplied {
		identities = append(identities, identity{source: "--subject", value: subject})
	}
	if callerBead, ok := beads.DecodeBeadEventPayload(json.RawMessage(payload)); ok && strings.TrimSpace(callerBead.ID) != "" {
		identities = append(identities, identity{source: "--payload", value: callerBead.ID})
	}
	if len(identities) == 0 {
		fmt.Fprintf(stderr, "gc event emit: %s authoritative payload requires a bead ID from --bead-payload, --subject, or --payload\n", eventType) //nolint:errcheck // best-effort stderr
		return "", false
	}
	beadID := identities[0].value
	for _, candidate := range identities[1:] {
		if candidate.value != beadID {
			fmt.Fprintf(stderr, "gc event emit: %s identity mismatch: %s=%q, %s=%q\n", eventType, identities[0].source, beadID, candidate.source, candidate.value) //nolint:errcheck // best-effort stderr
			return "", false
		}
	}
	return beadID, true
}

// beadDeletedFallbackID resolves the bead ID used to synthesize an ID-only
// bead.deleted snapshot when the row can no longer be hydrated. --bead-payload
// is the documented lifecycle-payload source; an explicitly supplied --subject
// is honored as a fallback so both documented invocation forms carry the
// deletion signal.
func beadDeletedFallbackID(beadPayload, subject string, subjectSupplied bool) string {
	if id := strings.TrimSpace(beadPayload); id != "" {
		return id
	}
	if subjectSupplied {
		return strings.TrimSpace(subject)
	}
	return ""
}

func requiresAuthoritativeLifecyclePayload(eventType string) bool {
	switch eventType {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
		return true
	default:
		return false
	}
}

func eventPayloadForEmit(payload, beadID string, stderr io.Writer) string {
	if payload == "" || !json.Valid([]byte(payload)) {
		beadID = strings.TrimSpace(beadID)
		if beadID == "" {
			return payload
		}
		beadPayload, err := loadEventBeadPayload(beadID)
		if err != nil {
			fmt.Fprintf(stderr, "gc event emit: bead payload %s: %v\n", beadID, err) //nolint:errcheck // best-effort stderr
			if payload != "" && !json.Valid([]byte(payload)) {
				return ""
			}
			return payload
		}
		return string(beadPayload)
	}
	return payload
}

func eventSubjectForEmitWithPresence(eventType, subject, payload string, subjectSupplied bool) string {
	if subjectSupplied || !isBeadLifecycleEvent(eventType) {
		return subject
	}
	bead, ok := beads.DecodeBeadEventPayload(json.RawMessage(payload))
	if !ok {
		return subject
	}
	return bead.ID
}

func loadEventBeadPayload(beadID string) (json.RawMessage, error) {
	return loadEventBeadPayloadWithDependencies(beadID, false)
}

func loadAuthoritativeEventBeadPayload(beadID string) (json.RawMessage, error) {
	return loadEventBeadPayloadWithDependencies(beadID, true)
}

func loadEventBeadPayloadWithDependencies(beadID string, hydrateDependencies bool) (json.RawMessage, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	scopeRoot, err := eventBeadPayloadScopeRoot()
	if err != nil {
		return nil, fmt.Errorf("resolving current scope: %w", err)
	}
	store, err := openStoreAtForCity(scopeRoot, cityPath)
	if err != nil {
		return nil, fmt.Errorf("opening bead store: %w", err)
	}
	return loadEventBeadPayloadFromStore(store, beadID, hydrateDependencies)
}

func loadEventBeadPayloadFromStore(store beads.Store, beadID string, hydrateDependencies bool) (json.RawMessage, error) {
	bead, err := beads.GetCanonical(store, beadID)
	if err != nil {
		return nil, fmt.Errorf("loading bead: %w", err)
	}
	if hydrateDependencies {
		dependencies, err := store.DepList(beadID, "down")
		if err != nil {
			return nil, fmt.Errorf("loading bead dependencies: %w", err)
		}
		bead.Dependencies = dependencies
		// Mirror the canonical CachingStore snapshot (readBeadWithDeps): the
		// authoritative dependency edges live in Dependencies, and Needs is nulled
		// so both event producers publish structurally identical snapshots. Leaving
		// Needs populated makes run-detail draw duplicate prerequisite edges that
		// the next in-process snapshot then flips back.
		bead.Needs = nil
	}
	payload, err := json.Marshal(map[string]beads.Bead{"bead": bead})
	if err != nil {
		return nil, fmt.Errorf("marshaling bead payload: %w", err)
	}
	return payload, nil
}

func eventBeadPayloadScopeRoot() (string, error) {
	if beadsDir := strings.TrimSpace(os.Getenv("BEADS_DIR")); beadsDir != "" {
		return cleanAbsPath(filepath.Dir(beadsDir))
	}
	if rigRoot := strings.TrimSpace(os.Getenv("GC_RIG_ROOT")); rigRoot != "" {
		return cleanAbsPath(rigRoot)
	}
	return os.Getwd()
}

func cleanAbsPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func cmdEventEmitWithSubjectPresence(eventType, subject string, subjectSupplied bool, message, actor, payload string, stderr io.Writer) int {
	cmdEventEmitSubmittedWithSubjectPresence(eventType, subject, subjectSupplied, message, actor, payload, stderr)
	return 0
}

func cmdEventEmitSubmittedWithSubjectPresence(eventType, subject string, subjectSupplied bool, message, actor, payload string, stderr io.Writer) bool {
	ep, code := openCityEventEmitProvider(stderr, "gc event emit")
	if ep == nil {
		// Best-effort: if we can't open the provider, still exit 0.
		_ = code
		return false
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doEventEmitWithSubjectPresence(ep, eventType, subject, subjectSupplied, message, actor, payload, stderr)
}

// doEventEmit is the pure logic for "gc event emit". Accepts the provider
// directly for testability. Best-effort: never fails.
func doEventEmit(ep events.Provider, eventType, subject, message, actor, payload string, stderr io.Writer) bool {
	return doEventEmitWithSubjectPresence(ep, eventType, subject, subject != "", message, actor, payload, stderr)
}

func doEventEmitWithSubjectPresence(ep events.Provider, eventType, subject string, subjectSupplied bool, message, actor, payload string, stderr io.Writer) bool {
	if actor == "" {
		actor = eventActor()
	}
	subject = eventSubjectForEmitWithPresence(eventType, subject, payload, subjectSupplied)

	e := events.Event{
		Type:    eventType,
		Actor:   actor,
		Subject: subject,
		Message: message,
	}
	if payload != "" {
		if !json.Valid([]byte(payload)) {
			fmt.Fprintf(stderr, "gc event emit: --payload is not valid JSON\n") //nolint:errcheck // best-effort stderr
			return false                                                        // best-effort — never fail
		}
		e.Payload = json.RawMessage(payload)
	}
	if isBeadLifecycleEvent(eventType) {
		bead, ok := beads.DecodeBeadEventPayload(e.Payload)
		if !ok || strings.TrimSpace(bead.ID) == "" {
			fmt.Fprintf(stderr, "gc event emit: %s requires --payload with a decodable bead snapshot and non-empty id\n", eventType) //nolint:errcheck // best-effort stderr
			return false
		}
		if eventType != events.BeadDeleted {
			if invalid := invalidBeadSnapshotFields(bead); len(invalid) > 0 {
				fmt.Fprintf(stderr, "gc event emit: %s requires a complete bead snapshot; empty or invalid fields: %s\n", eventType, strings.Join(invalid, ", ")) //nolint:errcheck // best-effort stderr
				return false
			}
		}
		if eventType == events.BeadClosed && bead.Status != "closed" {
			fmt.Fprintf(stderr, "gc event emit: %s requires a payload snapshot with status closed (got %q)\n", eventType, bead.Status) //nolint:errcheck // best-effort stderr
			return false
		}
		if subjectSupplied && bead.ID != subject {
			fmt.Fprintf(stderr, "gc event emit: %s subject %q does not match payload bead id %q\n", eventType, subject, bead.ID) //nolint:errcheck // best-effort stderr
			return false
		}
		if eventType != events.BeadDeleted {
			stampBeadSnapshotCorrelation(&e, bead)
		}
	}

	ep.Record(e)
	return true
}

// invalidBeadSnapshotFields checks the decoded values that replace the
// projector's prior snapshot. Presence-only validation is insufficient:
// null and whitespace string fields decode to zero values and erase the same
// projection data as an ID-only object. UpdatedAt remains optional for legacy
// beads because Bead deliberately omits its zero value on the wire.
func invalidBeadSnapshotFields(bead beads.Bead) []string {
	invalid := make([]string, 0, 4)
	if strings.TrimSpace(bead.Title) == "" {
		invalid = append(invalid, "title")
	}
	if strings.TrimSpace(bead.Status) == "" {
		invalid = append(invalid, "status")
	}
	if bead.CreatedAt.IsZero() {
		invalid = append(invalid, "created_at")
	}
	if strings.TrimSpace(bead.Type) == "" {
		invalid = append(invalid, "issue_type/type")
	}
	return invalid
}

func isBeadLifecycleEvent(eventType string) bool {
	switch eventType {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
		return true
	default:
		return false
	}
}
