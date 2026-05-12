package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

type reloadOutcome string

const (
	reloadOutcomeApplied  reloadOutcome = "applied"
	reloadOutcomeNoChange reloadOutcome = "no_change"
	reloadOutcomeAccepted reloadOutcome = "accepted"
	reloadOutcomeFailed   reloadOutcome = "failed"
	reloadOutcomeBusy     reloadOutcome = "busy"
	reloadOutcomeTimeout  reloadOutcome = "timeout"
)

type reloadSource string

const (
	reloadSourceWatch  reloadSource = "watch"
	reloadSourceManual reloadSource = "manual"
)

var (
	// controllerReloadAcceptTimeout is how long a reload request waits for
	// the controller's main goroutine to drain it from reloadReqCh. The
	// main goroutine is blocked while a reconcile tick runs, and ticks can
	// take 30s–90s+ under bead-store churn (see issue #1560). 5s was
	// dramatically too short and produced "controller is busy" rejections
	// for many minutes at a time. 60s gives the controller enough headroom
	// to finish a tick before the reload is rejected, while still bounding
	// the wait for genuinely deadlocked controllers.
	controllerReloadAcceptTimeout = 60 * time.Second
	sendReloadControlRequestHook  = sendReloadControlRequest
	reloadUnavailableMessageHook  = reloadUnavailableMessage
	supervisorAPIBaseURLHook      = supervisorAPIBaseURL
)

type reloadControlRequest struct {
	Wait    bool   `json:"wait"`
	Timeout string `json:"timeout,omitempty"`
	// Soft requests that the reload tick accept any detected per-session
	// config drift instead of draining the drifted sessions. The
	// controller updates each open session's started_config_hash to the
	// hash that the freshly reloaded config produces for that session,
	// so the immediately-following reconcile tick sees no drift and
	// doesn't fire config-drift drains. Sessions whose template no
	// longer maps to a configured agent are NOT updated; normal
	// orphan/suspended drain handles them on the next tick.
	Soft bool `json:"soft,omitempty"`
}

type reloadControlReply struct {
	Outcome  reloadOutcome `json:"outcome,omitempty"`
	Message  string        `json:"message,omitempty"`
	Revision string        `json:"revision,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type reloadRequest struct {
	wait       bool
	timeout    time.Duration
	soft       bool
	acceptedCh chan reloadControlReply
	doneCh     chan reloadControlReply
}

func newReloadCmd(stdout, stderr io.Writer) *cobra.Command {
	var async bool
	var soft bool
	var timeoutValue string
	cmd := &cobra.Command{
		Use:   "reload [path]",
		Short: "Reload the current city's config without restarting the city/controller",
		Long: `Force the current city controller to re-read effective config and
process one reload tick without restarting the city/controller.

Reload may fetch configured remote packs before recomputing effective
config. By default, per-session restarts may still happen if normal
config drift rules require them.

With --soft, the controller accepts any detected per-session config
drift instead of draining the drifted sessions: each open session's
recorded config hash is updated to the hash the freshly reloaded
config produces for it, so the immediately-following reconcile tick
sees no drift and no config-drift drains fire. Useful when editing a
running city's .gc/settings.json without disrupting in-flight work.
Sessions whose template no longer maps to a configured agent are
NOT updated; normal orphan/suspended drain handles them on the next
tick.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			timeoutChanged := cmd.Flags().Changed("timeout")
			if cmdReload(args, async, soft, timeoutValue, timeoutChanged, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&async, "async", false, "Return after the controller accepts the reload request")
	cmd.Flags().BoolVar(&soft, "soft", false, "Accept config drift on open sessions instead of draining them")
	cmd.Flags().StringVar(&timeoutValue, "timeout", "5m", "How long to wait for reload completion")
	return cmd
}

func cmdReload(args []string, async bool, soft bool, timeoutValue string, timeoutChanged bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc reload: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if async && timeoutChanged {
		fmt.Fprintln(stderr, "gc reload: --async and --timeout cannot be used together") //nolint:errcheck // best-effort stderr
		return 1
	}

	req := reloadControlRequest{Wait: !async, Soft: soft}
	if !async {
		timeout, err := time.ParseDuration(timeoutValue)
		if err != nil {
			fmt.Fprintf(stderr, "gc reload: invalid --timeout %q: %v\n", timeoutValue, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if timeout <= 0 {
			fmt.Fprintln(stderr, "gc reload: --timeout must be greater than 0") //nolint:errcheck // best-effort stderr
			return 1
		}
		req.Timeout = timeout.String()
	}

	reply, err := sendReloadControlRequestHook(cityPath, req)
	if err != nil {
		if isControllerUnavailableError(err) {
			if msg := reloadUnavailableMessageHook(cityPath); msg != "" {
				fmt.Fprintf(stderr, "gc reload: %s: %v\n", msg, err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		fmt.Fprintf(stderr, "gc reload: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch reply.Outcome {
	case reloadOutcomeAccepted, reloadOutcomeApplied, reloadOutcomeNoChange:
		if strings.TrimSpace(reply.Message) != "" {
			fmt.Fprintln(stdout, strings.TrimSpace(reply.Message)) //nolint:errcheck // best-effort stdout
		}
		for _, warning := range reply.Warnings {
			fmt.Fprintf(stderr, "gc reload: warning: %s\n", warning) //nolint:errcheck // best-effort stderr
		}
		return 0
	case reloadOutcomeFailed:
		for _, warning := range reply.Warnings {
			fmt.Fprintf(stderr, "gc reload: warning: %s\n", warning) //nolint:errcheck // best-effort stderr
		}
		switch {
		case strings.TrimSpace(reply.Error) != "":
			fmt.Fprintln(stderr, strings.TrimSpace(reply.Error)) //nolint:errcheck // best-effort stderr
		case strings.TrimSpace(reply.Message) != "":
			fmt.Fprintln(stderr, strings.TrimSpace(reply.Message)) //nolint:errcheck // best-effort stderr
		default:
			fmt.Fprintln(stderr, "gc reload: reload failed") //nolint:errcheck // best-effort stderr
		}
		return 1
	case reloadOutcomeBusy, reloadOutcomeTimeout:
		if strings.TrimSpace(reply.Message) != "" {
			fmt.Fprintln(stderr, strings.TrimSpace(reply.Message)) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc reload: %s\n", reply.Outcome) //nolint:errcheck // best-effort stderr
		}
		return 1
	default:
		fmt.Fprintf(stderr, "gc reload: unexpected controller outcome %q\n", reply.Outcome) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func isControllerUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errControllerUnavailable) || errors.Is(err, errControllerUnresponsive)
}

func sendReloadControlRequest(cityPath string, req reloadControlRequest) (reloadControlReply, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return reloadControlReply{}, fmt.Errorf("marshaling request: %w", err)
	}
	readTimeout, err := reloadControlReadTimeout(req)
	if err != nil {
		return reloadControlReply{}, err
	}
	resp, err := sendControllerCommandWithReadTimeout(cityPath, "reload:"+string(data), readTimeout)
	if err != nil {
		return reloadControlReply{}, err
	}
	var reply reloadControlReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		return reloadControlReply{}, fmt.Errorf("parsing response: %w", err)
	}
	return reply, nil
}

func reloadControlReadTimeout(req reloadControlRequest) (time.Duration, error) {
	readTimeout := 2*controllerReloadAcceptTimeout + 10*time.Second
	if req.Wait && req.Timeout != "" {
		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return 0, fmt.Errorf("parsing request timeout: %w", err)
		}
		readTimeout += timeout
	}
	return readTimeout, nil
}

func reloadUnavailableMessage(cityPath string) string {
	info, ok := supervisorCityInfo(cityPath)
	if !ok {
		return ""
	}
	switch {
	case info.Running:
		return "controller is running but not responding"
	case info.Status == "init_failed" && strings.TrimSpace(info.Error) != "":
		return fmt.Sprintf("city failed to start under supervisor: %s", strings.TrimSpace(info.Error))
	case info.Status == "init_failed":
		return "city failed to start under supervisor"
	case strings.TrimSpace(info.Status) != "":
		return fmt.Sprintf("city is still starting under supervisor (%s)", controllerSupervisorStatusText(info.Status))
	default:
		return "city controller is not running"
	}
}

func supervisorCityInfo(cityPath string) (api.CityInfo, bool) {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered || supervisorAliveHook() == 0 {
		return api.CityInfo{}, false
	}
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return api.CityInfo{}, false
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return api.CityInfo{}, false
	}
	for _, city := range cities {
		if samePath(city.Path, entry.Path) {
			return city, true
		}
	}
	return api.CityInfo{}, false
}
