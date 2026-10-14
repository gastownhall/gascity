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

Recording is best-effort: once an event is accepted, a provider or payload
error is reported on stderr and the command still exits 0, so bead hooks never
fail on it. JSON summaries report whether submission to the configured provider
was attempted; the event bus does not acknowledge durable persistence.

Two requests are refused with exit 1 and nothing is recorded. In --json mode
the failure object carries the code.

- reserved_actor: --actor starts with "cache-", or the default actor is one
  of the controller cache's own actors (cache-local, cache-reconcile).
- bead_payload_refused: --payload on bead.created, bead.updated, bead.closed
  or bead.deleted. Their payload is the bead row; use --bead-payload <id> so
  gc reads it from the store.

--payload attaches arbitrary JSON to any other event type.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			effectiveActor := actor
			if effectiveActor == "" {
				effectiveActor = eventActor()
			}
			if refusal := eventEmitRefusal(args[0], actor, effectiveActor, payload); refusal != nil {
				if jsonOut {
					return exitForCode(writeJSONError(stdout, stderr, refusal.code, refusal.reason, 1))
				}
				fmt.Fprintf(stderr, "gc event emit: %s\n", refusal.reason) //nolint:errcheck // best-effort stderr
				return errExit
			}
			finalPayload := eventPayloadForEmit(payload, beadPayload, stderr)
			submitted := false
			if jsonOut {
				submitted = cmdEventEmitSubmitted(args[0], subject, message, effectiveActor, finalPayload, stderr)
				return writeCLIJSONLineOrErr(stdout, stderr, "gc event emit", eventEmitJSONResult{
					SchemaVersion: "1",
					OK:            true,
					EventType:     args[0],
					Actor:         effectiveActor,
					Subject:       subject,
					Message:       message,
					HasPayload:    finalPayload != "",
					Submitted:     submitted,
				})
			}
			if cmdEventEmit(args[0], subject, message, effectiveActor, finalPayload, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "Event subject (e.g. bead ID)")
	cmd.Flags().StringVar(&message, "message", "", "Event message")
	cmd.Flags().StringVar(&actor, "actor", "", "Actor name (default: $GC_ALIAS, else $GC_AGENT, else $GC_SESSION_ID, else \"human\")")
	cmd.Flags().StringVar(&payload, "payload", "", "JSON payload to attach to the event")
	cmd.Flags().StringVar(&beadPayload, "bead-payload", "", "Bead ID whose stored row becomes the payload (the only payload source for bead.created/updated/closed/deleted)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

// cacheActorPrefix names the actor class the controller stamps on its own
// cache's notifications (cacheLocalActor, cacheReconcileActor).
const cacheActorPrefix = "cache-"

// Typed codes for the --json failure object of a refused gc event emit.
const (
	eventEmitReservedActorCode = "reserved_actor"
	eventEmitBeadPayloadCode   = "bead_payload_refused"
)

// beadSnapshotEventTypes are the bead lifecycle types whose payload is a row
// snapshot the controller may read as the row's state. The BeadEventPayload
// registrations in internal/api/event_payloads.go are the source of truth;
// TestBeadSnapshotEventTypesMatchBeadEventPayloadRegistrations pins the match.
var beadSnapshotEventTypes = map[string]bool{
	events.BeadCreated: true,
	events.BeadUpdated: true,
	events.BeadClosed:  true,
	events.BeadDeleted: true,
}

// eventEmitRefused is why gc event emit refused to record an event.
type eventEmitRefused struct {
	code   string
	reason string
}

// eventEmitRefusal reports why gc event emit must not record an event, or nil.
// An explicit --actor may not claim the cache-* class at all; the implicit
// actor (GC_ALIAS and friends, which are user-configured names) may not be one
// of the cache's own actors. A row-snapshot event's payload must come from
// the store (--bead-payload), not from the caller.
func eventEmitRefusal(eventType, explicitActor, effectiveActor, payload string) *eventEmitRefused {
	if strings.HasPrefix(explicitActor, cacheActorPrefix) || isCacheActor(effectiveActor) {
		return &eventEmitRefused{
			code:   eventEmitReservedActorCode,
			reason: fmt.Sprintf("actor %q: the %s* actor class is reserved for the controller's cache", effectiveActor, cacheActorPrefix),
		}
	}
	if payload != "" && beadSnapshotEventTypes[eventType] {
		return &eventEmitRefused{
			code:   eventEmitBeadPayloadCode,
			reason: fmt.Sprintf("--payload is not accepted for %s events; use --bead-payload <id> to attach the bead as the store holds it", eventType),
		}
	}
	return nil
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

func loadEventBeadPayload(beadID string) (json.RawMessage, error) {
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
	bead, err := store.Get(beadID)
	if err != nil {
		return nil, fmt.Errorf("loading bead: %w", err)
	}
	payload, err := beads.EncodeBeadEventPayload(bead)
	if err != nil {
		return nil, fmt.Errorf("marshaling bead payload: %w", err)
	}
	return payload, nil
}

func eventBeadPayloadScopeRoot() (string, error) {
	if beadsDir := strings.TrimSpace(os.Getenv("BEADS_DIR")); beadsDir != "" {
		// filepath.Clean beadsDir before Dir: a trailing-slash BEADS_DIR
		// (e.g. "/root/.beads/") makes filepath.Dir return beadsDir itself
		// rather than its parent, since Split treats the trailing slash as
		// already naming an (empty) final component. cleanAbsPath's own
		// Clean runs on the (already wrong) Dir result, too late to fix it.
		return cleanAbsPath(filepath.Dir(filepath.Clean(beadsDir)))
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

// cmdEventEmit records a single event to the city event log. Best-effort:
// errors go to stderr but exit code is always 0 so bd hooks never fail.
func cmdEventEmit(eventType, subject, message, actor, payload string, stderr io.Writer) int {
	cmdEventEmitSubmitted(eventType, subject, message, actor, payload, stderr)
	return 0
}

func cmdEventEmitSubmitted(eventType, subject, message, actor, payload string, stderr io.Writer) bool {
	ep, code := openCityEventEmitProvider(stderr, "gc event emit")
	if ep == nil {
		// Best-effort: if we can't open the provider, still exit 0.
		_ = code
		return false
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doEventEmit(ep, eventType, subject, message, actor, payload, stderr)
}

// doEventEmit is the pure logic for "gc event emit". Accepts the provider
// directly for testability. Best-effort: never fails.
func doEventEmit(ep events.Provider, eventType, subject, message, actor, payload string, stderr io.Writer) bool {
	if actor == "" {
		actor = eventActor()
	}

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

	ep.Record(e)
	return true
}
