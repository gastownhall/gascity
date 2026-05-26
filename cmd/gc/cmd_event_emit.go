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

Best-effort: always exits 0 so bead hooks never fail. Supports
attaching arbitrary JSON payloads. JSON summaries report whether submission to
the configured provider was attempted; the event bus does not acknowledge
durable persistence.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			effectiveActor := actor
			if effectiveActor == "" {
				effectiveActor = eventActor()
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
	cmd.Flags().StringVar(&beadPayload, "bead-payload", "", "Best-effort bead ID fallback for hook payloads")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
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
