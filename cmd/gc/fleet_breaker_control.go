package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/resilience"
)

const (
	// fleetBreakerCommandPrefix is the controller-socket prefix of the fleet
	// start breaker commands: "fleet-breaker:status" and "fleet-breaker:reset".
	fleetBreakerCommandPrefix = "fleet-breaker:"
	fleetBreakerStatusCommand = "status"
	fleetBreakerResetCommand  = "reset"
)

// fleetBreakerReply is the controller's answer to a fleet breaker command.
type fleetBreakerReply struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	// Enabled is false when the kill switch is off or the controller runs no
	// guard; the remaining fields are then zero.
	Enabled       bool   `json:"enabled"`
	State         string `json:"state,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Trips         int    `json:"trips,omitempty"`
	BackoffCapMs  int64  `json:"backoff_cap_ms,omitempty"`
	NextProbeAt   string `json:"next_probe_at,omitempty"`
	OpenSince     string `json:"open_since,omitempty"`
	ProbeInFlight bool   `json:"probe_in_flight,omitempty"`
	// WasOpen is set on a reset reply when the breaker was open or half-open.
	WasOpen bool `json:"was_open,omitempty"`
}

// fleetGuardDirectory lets the controller socket reach a city's fleet start
// breaker. The breaker lives in the city's runtime, which the socket handler
// does not hold; the runtime publishes its guard here, keyed by city path. It
// holds a pointer to live in-memory state, not a copy of it.
var fleetGuardDirectory = struct {
	mu     sync.Mutex
	guards map[string]*fleetStartGuard
}{guards: make(map[string]*fleetStartGuard)}

// publishFleetGuard records guard as the city's fleet start breaker, or
// removes the city's entry when guard is nil.
func publishFleetGuard(cityPath string, guard *fleetStartGuard) {
	key := normalizePathForCompare(cityPath)
	fleetGuardDirectory.mu.Lock()
	defer fleetGuardDirectory.mu.Unlock()
	if guard == nil {
		delete(fleetGuardDirectory.guards, key)
		return
	}
	fleetGuardDirectory.guards[key] = guard
}

func publishedFleetGuard(cityPath string) *fleetStartGuard {
	fleetGuardDirectory.mu.Lock()
	defer fleetGuardDirectory.mu.Unlock()
	return fleetGuardDirectory.guards[normalizePathForCompare(cityPath)]
}

// fleetBreakerReplyFor reads guard's status into a reply. A nil guard replies
// disabled.
func fleetBreakerReplyFor(guard *fleetStartGuard) fleetBreakerReply {
	if guard == nil {
		return fleetBreakerReply{Outcome: "ok"}
	}
	st := guard.Status()
	return fleetBreakerReply{
		Outcome:       "ok",
		Enabled:       true,
		State:         st.State.String(),
		Reason:        st.Reason,
		Trips:         st.Trips,
		BackoffCapMs:  st.BackoffCap.Milliseconds(),
		NextProbeAt:   traceTime(st.NextProbeAt),
		OpenSince:     traceTime(st.OpenSince),
		ProbeInFlight: st.ProbeInFlight,
	}
}

// handleFleetBreakerSocketCmd serves one fleet breaker command. A reset queues
// the close transition; the next reconcile tick emits its event and log line.
func handleFleetBreakerSocketCmd(conn net.Conn, cityPath, command string) {
	guard := publishedFleetGuard(cityPath)
	switch command {
	case fleetBreakerStatusCommand:
		writeJSONLine(conn, fleetBreakerReplyFor(guard))
	case fleetBreakerResetCommand:
		wasOpen := guard.Reset()
		reply := fleetBreakerReplyFor(guard)
		reply.WasOpen = wasOpen
		writeJSONLine(conn, reply)
	default:
		writeJSONLine(conn, fleetBreakerReply{
			Outcome: "failed",
			Error:   fmt.Sprintf("unknown fleet breaker command %q", command),
		})
	}
}

// sendFleetBreakerCommand asks the city's controller to run a fleet breaker
// command.
func sendFleetBreakerCommand(cityPath, command string) (fleetBreakerReply, error) {
	resp, err := sendControllerCommand(cityPath, fleetBreakerCommandPrefix+command)
	return decodeFleetBreakerReply(command, resp, err)
}

// Bounds on the courtesy breaker query gc status and gc doctor make: a
// controller that does not answer promptly must not stall them.
const (
	fleetBreakerQueryDial  = 500 * time.Millisecond
	fleetBreakerQueryWrite = 500 * time.Millisecond
	fleetBreakerQueryRead  = 2 * time.Second
)

// queryFleetBreakerStatus is the status command with short timeouts, for the
// callers that only decorate their own output with it.
func queryFleetBreakerStatus(cityPath string) (fleetBreakerReply, error) {
	resp, err := sendControllerCommandWithTimeouts(cityPath, fleetBreakerCommandPrefix+fleetBreakerStatusCommand,
		fleetBreakerQueryDial, fleetBreakerQueryWrite, fleetBreakerQueryRead)
	return decodeFleetBreakerReply(fleetBreakerStatusCommand, resp, err)
}

func decodeFleetBreakerReply(command string, resp []byte, err error) (fleetBreakerReply, error) {
	var reply fleetBreakerReply
	if err != nil {
		return reply, err
	}
	if err := json.Unmarshal(resp, &reply); err != nil {
		return reply, fmt.Errorf("decoding fleet breaker reply: %w", err)
	}
	if reply.Outcome != "ok" {
		if reply.Error != "" {
			return reply, fmt.Errorf("%s", reply.Error)
		}
		return reply, fmt.Errorf("fleet breaker %s failed", command)
	}
	return reply, nil
}

// describe renders a status reply as one line for gc status and gc doctor. It
// is empty while the breaker is disabled or closed.
func (r fleetBreakerReply) describe() string {
	if !r.Enabled || r.State == "" || r.State == resilience.StateClosed.String() {
		return ""
	}
	msg := fmt.Sprintf("fleet start breaker %s (%s)", r.State, r.Reason)
	if r.NextProbeAt != "" {
		msg += ", next probe " + r.NextProbeAt
	}
	return msg + "; automatic session starts are limited to probes; clear with `gc breaker reset`"
}

func newBreakerCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "breaker",
		Short: "Inspect and clear the fleet start breaker",
		Long: `Inspect and clear the fleet start breaker.

The fleet start breaker pauses automatic session starts when several
different sessions fail to start (or stay up) within a short window, or when
the runtime's state cache has gone stale. While it is open, automatic starts
are limited to a probe at a time until a started session stays up. User
initiated starts (attach, new, nudge) are never gated.

The breaker is in-memory in the running controller, so these commands need a
running controller. Disable it with [daemon] fleet_start_breaker = false.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc breaker: unknown subcommand %q\n", args[0]) //nolint:errcheck
			return errExit
		},
	}
	cmd.AddCommand(newBreakerStatusCmd(stdout, stderr), newBreakerResetCmd(stdout, stderr))
	return cmd
}

func newBreakerStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the fleet start breaker's state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBreakerStatus(jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func newBreakerResetCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Close the fleet start breaker and forget its failure window",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBreakerReset(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdBreakerStatus(jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc breaker status: %v\n", err) //nolint:errcheck
		return 1
	}
	reply, err := sendFleetBreakerCommand(cityPath, fleetBreakerStatusCommand)
	if err != nil {
		fmt.Fprintf(stderr, "gc breaker status: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, reply); err != nil {
			fmt.Fprintf(stderr, "gc breaker status: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if !reply.Enabled {
		fmt.Fprintln(stdout, "Fleet start breaker: disabled ([daemon] fleet_start_breaker = false)") //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "Fleet start breaker: %s\n", reply.State) //nolint:errcheck
	if reply.State == resilience.StateClosed.String() {
		return 0
	}
	fmt.Fprintf(stdout, "Reason: %s\n", reply.Reason)                                                                    //nolint:errcheck
	fmt.Fprintf(stdout, "Open since: %s\n", reply.OpenSince)                                                             //nolint:errcheck
	fmt.Fprintf(stdout, "Trips: %d (backoff cap %s)\n", reply.Trips, time.Duration(reply.BackoffCapMs)*time.Millisecond) //nolint:errcheck
	fmt.Fprintf(stdout, "Next probe: %s (probe in flight: %t)\n", reply.NextProbeAt, reply.ProbeInFlight)                //nolint:errcheck
	fmt.Fprintln(stdout, "Clear with: gc breaker reset")                                                                 //nolint:errcheck
	return 0
}

func cmdBreakerReset(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc breaker reset: %v\n", err) //nolint:errcheck
		return 1
	}
	reply, err := sendFleetBreakerCommand(cityPath, fleetBreakerResetCommand)
	if err != nil {
		fmt.Fprintf(stderr, "gc breaker reset: %v\n", err) //nolint:errcheck
		return 1
	}
	switch {
	case !reply.Enabled:
		fmt.Fprintln(stdout, "Fleet start breaker is disabled; nothing to reset") //nolint:errcheck
	case reply.WasOpen:
		fmt.Fprintln(stdout, "Fleet start breaker reset: automatic session starts resumed") //nolint:errcheck
	default:
		fmt.Fprintln(stdout, "Fleet start breaker was already closed; its failure window was cleared") //nolint:errcheck
	}
	return 0
}

// fleetBreakerLine reads the controller's breaker state for gc status and gc
// doctor. It is empty when the breaker is closed or disabled, or when the
// controller cannot be asked; the second result is the query error.
func fleetBreakerLine(cityPath string) (string, error) {
	reply, err := queryFleetBreakerStatus(cityPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(reply.describe()), nil
}

// fleetBreakerNote is fleetBreakerLine gated on a running controller, for the
// status snapshot. A controller that cannot answer adds no note.
func fleetBreakerNote(ctrl ControllerJSON, cityPath string) string {
	if !ctrl.Running || cityPath == "" {
		return ""
	}
	line, err := fleetBreakerLine(cityPath)
	if err != nil {
		return ""
	}
	return line
}

// fleetBreakerDoctorCheck reports an open or half-open fleet start breaker.
type fleetBreakerDoctorCheck struct {
	cfg   *config.City
	query func(cityPath string) (fleetBreakerReply, error)
}

func newFleetBreakerDoctorCheck(cfg *config.City) *fleetBreakerDoctorCheck {
	return &fleetBreakerDoctorCheck{cfg: cfg, query: queryFleetBreakerStatus}
}

// Name returns the check's identifier.
func (*fleetBreakerDoctorCheck) Name() string { return "fleet-start-breaker" }

// CanFix reports false: clearing the breaker is an operator decision.
func (*fleetBreakerDoctorCheck) CanFix() bool { return false }

// WarmupEligible reports false: the check needs a running controller.
func (*fleetBreakerDoctorCheck) WarmupEligible() bool { return false }

// Fix is a no-op; see CanFix.
func (*fleetBreakerDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run reports the breaker's state. An open or half-open breaker is a warning
// (automatic starts are being limited); an unreachable controller is not a
// finding here, since other checks report on the controller itself.
func (c *fleetBreakerDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if c.cfg != nil && !c.cfg.Daemon.FleetStartBreakerEnabled() {
		r.Message = "fleet start breaker: disabled ([daemon] fleet_start_breaker = false)"
		return r
	}
	if ctx == nil || ctx.CityPath == "" {
		r.Message = "fleet start breaker: not checked"
		return r
	}
	reply, err := c.query(ctx.CityPath)
	if err != nil {
		r.Message = "fleet start breaker: not checked (controller not reachable)"
		r.Details = append(r.Details, err.Error())
		return r
	}
	if line := reply.describe(); line != "" {
		r.Status = doctor.StatusWarning
		r.Message = line
		r.FixHint = "find why sessions fail to start (gc trace show --reason fleet_start_breaker_open), then run gc breaker reset"
		return r
	}
	r.Message = "fleet start breaker: closed"
	return r
}
