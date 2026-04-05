package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/spf13/cobra"
)

type traceControlRequest struct {
	Action         string            `json:"action"`
	ScopeType      TraceArmScopeType `json:"scope_type"`
	ScopeValue     string            `json:"scope_value"`
	Source         TraceArmSource    `json:"source"`
	Level          TraceMode         `json:"level"`
	For            string            `json:"for,omitempty"`
	All            bool              `json:"all,omitempty"`
	TriggerReason  string            `json:"trigger_reason,omitempty"`
	ActorKind      string            `json:"actor_kind,omitempty"`
	ActorUser      string            `json:"actor_user,omitempty"`
	ActorHost      string            `json:"actor_host,omitempty"`
	ActorPID       int               `json:"actor_pid,omitempty"`
	CommandSummary string            `json:"command_summary,omitempty"`
	RequestedAt    time.Time         `json:"requested_at,omitempty"`
}

type traceControlReply struct {
	OK      bool             `json:"ok"`
	Message string           `json:"message,omitempty"`
	Status  *traceStatusJSON `json:"status,omitempty"`
	Error   string           `json:"error,omitempty"`
}

type traceStatusJSON struct {
	CityPath          string     `json:"city_path"`
	AsOf              time.Time  `json:"as_of"`
	ControllerRunning bool       `json:"controller_running"`
	ControllerPID     int        `json:"controller_pid,omitempty"`
	ActiveArms        []TraceArm `json:"active_arms"`
}

func newTraceCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Inspect and control session reconciler tracing",
		Long: `Inspect and control the session reconciler trace stream.

Trace state is persisted locally under .gc/runtime/session-reconciler-trace
and can be managed even when the controller is offline.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc trace: unknown subcommand %q\n", args[0]) //nolint:errcheck
			return errExit
		},
	}
	cmd.AddCommand(
		newTraceStartCmd(stdout, stderr),
		newTraceStopCmd(stdout, stderr),
		newTraceStatusCmd(stdout, stderr),
		newTraceShowCmd(stdout, stderr),
		newTraceCycleCmd(stdout, stderr),
		newTraceReasonsCmd(stdout, stderr),
		newTraceTailCmd(stdout, stderr),
	)
	return cmd
}

func newTraceStartCmd(stdout, stderr io.Writer) *cobra.Command {
	var template string
	var forDuration string
	var auto bool
	var level string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start or extend tracing for a template",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceStart(template, forDuration, auto, level, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&template, "template", "", "exact normalized template selector")
	cmd.Flags().StringVar(&forDuration, "for", "15m", "trace arm duration (e.g. 15m)")
	cmd.Flags().BoolVar(&auto, "auto", false, "mark the arm as auto-triggered")
	cmd.Flags().StringVar(&level, "level", string(TraceModeDetail), "trace level: baseline or detail")
	return cmd
}

func newTraceStopCmd(stdout, stderr io.Writer) *cobra.Command {
	var template string
	var all bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop tracing for a template",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceStop(template, all, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&template, "template", "", "exact normalized template selector")
	cmd.Flags().BoolVar(&all, "all", false, "remove both manual and auto arms")
	return cmd
}

func newTraceStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show trace arms and stream state",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceStatus(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func newTraceShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var template string
	var since string
	var traceID string
	var tickID string
	var recordType string
	var reason string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show trace records",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceShow(template, since, traceID, tickID, recordType, reason, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&template, "template", "", "exact normalized template selector")
	cmd.Flags().StringVar(&since, "since", "", "show records since duration ago")
	cmd.Flags().StringVar(&traceID, "trace-id", "", "filter by trace id")
	cmd.Flags().StringVar(&tickID, "tick", "", "filter by tick id")
	cmd.Flags().StringVar(&recordType, "type", "", "filter by record type")
	cmd.Flags().StringVar(&reason, "reason", "", "filter by reason code")
	cmd.Flags().BoolVar(&jsonOut, "json", true, "emit JSON array")
	return cmd
}

func newTraceCycleCmd(stdout, stderr io.Writer) *cobra.Command {
	var tickID string
	cmd := &cobra.Command{
		Use:   "cycle",
		Short: "Show a cycle by tick id",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceCycle(tickID, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tickID, "tick", "", "tick id to display")
	return cmd
}

func newTraceReasonsCmd(stdout, stderr io.Writer) *cobra.Command {
	var template string
	var since string
	cmd := &cobra.Command{
		Use:   "reasons",
		Short: "Show reason codes observed in trace records",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceReasons(template, since, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&template, "template", "", "exact normalized template selector")
	cmd.Flags().StringVar(&since, "since", "", "show reasons since duration ago")
	return cmd
}

func newTraceTailCmd(stdout, stderr io.Writer) *cobra.Command {
	var template string
	var since string
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Follow trace records",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdTraceTail(template, since, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&template, "template", "", "exact normalized template selector")
	cmd.Flags().StringVar(&since, "since", "", "follow from duration ago")
	return cmd
}

func cmdTraceStart(template, forDuration string, auto bool, level string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(template) == "" {
		fmt.Fprintln(stderr, "gc trace start: missing --template") //nolint:errcheck
		return 1
	}
	dur, err := time.ParseDuration(forDuration)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace start: invalid --for %q: %v\n", forDuration, err) //nolint:errcheck
		return 1
	}
	if dur <= 0 {
		fmt.Fprintf(stderr, "gc trace start: invalid duration %q\n", forDuration) //nolint:errcheck
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace start: %v\n", err) //nolint:errcheck
		return 1
	}
	now := time.Now().UTC()
	req := traceControlRequest{
		Action:         "start",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     normalizedTraceTemplate(template),
		Source:         TraceArmSourceManual,
		Level:          TraceMode(level),
		For:            forDuration,
		ActorKind:      "cli",
		CommandSummary: traceCommandSummary("trace.start", template, forDuration, false),
		RequestedAt:    now,
	}
	if auto {
		req.Source = TraceArmSourceAuto
	}
	usr, _ := user.Current()
	if usr != nil {
		req.ActorUser = usr.Username
	}
	if host, err := os.Hostname(); err == nil {
		req.ActorHost = host
	}
	req.ActorPID = os.Getpid()
	status, msg, err := applyTraceControlMaybeRemote(cityPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace start: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, msg) //nolint:errcheck
	if status != nil {
		fmt.Fprintf(stdout, "active trace arms: %d\n", len(status.ActiveArms)) //nolint:errcheck
	}
	return 0
}

func cmdTraceStop(template string, all bool, stdout, stderr io.Writer) int {
	if strings.TrimSpace(template) == "" {
		fmt.Fprintln(stderr, "gc trace stop: missing --template") //nolint:errcheck
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace stop: %v\n", err) //nolint:errcheck
		return 1
	}
	req := traceControlRequest{
		Action:         "stop",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     normalizedTraceTemplate(template),
		Source:         TraceArmSourceManual,
		All:            all,
		ActorKind:      "cli",
		RequestedAt:    time.Now().UTC(),
		CommandSummary: traceCommandSummary("trace.stop", template, "", all),
	}
	usr, _ := user.Current()
	if usr != nil {
		req.ActorUser = usr.Username
	}
	if host, err := os.Hostname(); err == nil {
		req.ActorHost = host
	}
	req.ActorPID = os.Getpid()
	status, msg, err := applyTraceControlMaybeRemote(cityPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace stop: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, msg) //nolint:errcheck
	if status != nil {
		fmt.Fprintf(stdout, "active trace arms: %d\n", len(status.ActiveArms)) //nolint:errcheck
	}
	return 0
}

func cmdTraceStatus(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace status: %v\n", err) //nolint:errcheck
		return 1
	}
	status, err := traceStatusMaybeRemote(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace status: %v\n", err) //nolint:errcheck
		return 1
	}
	head, err := traceHeadSeq(traceCityRuntimeDir(cityPath))
	if err != nil {
		fmt.Fprintf(stderr, "gc trace status: %v\n", err) //nolint:errcheck
		return 1
	}
	payload := map[string]any{
		"city_path": cityPath,
		"head_seq":  head,
		"arms":      status.ActiveArms,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc trace status: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck
	return 0
}

func cmdTraceShow(template, since, traceID, tickID, recordType, reason string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace show: %v\n", err) //nolint:errcheck
		return 1
	}
	filter := TraceFilter{
		Template:   normalizedTraceTemplate(template),
		TraceID:    traceID,
		TickID:     tickID,
		RecordType: TraceRecordType(recordType),
		ReasonCode: TraceReasonCode(reason),
	}
	if since != "" {
		d, err := time.ParseDuration(since)
		if err != nil {
			fmt.Fprintf(stderr, "gc trace show: invalid --since %q: %v\n", since, err) //nolint:errcheck
			return 1
		}
		filter.Since = time.Now().Add(-d)
	}
	recs, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), filter)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace show: %v\n", err) //nolint:errcheck
		return 1
	}
	if !jsonOut {
		for _, rec := range recs {
			fmt.Fprintln(stdout, traceRecordSummary(rec)) //nolint:errcheck
		}
		return 0
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc trace show: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck
	return 0
}

func cmdTraceCycle(tickID string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace cycle: %v\n", err) //nolint:errcheck
		return 1
	}
	recs, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{TickID: tickID})
	if err != nil {
		fmt.Fprintf(stderr, "gc trace cycle: %v\n", err) //nolint:errcheck
		return 1
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc trace cycle: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck
	return 0
}

func cmdTraceReasons(template, since string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace reasons: %v\n", err) //nolint:errcheck
		return 1
	}
	filter := TraceFilter{Template: normalizedTraceTemplate(template)}
	if since != "" {
		d, err := time.ParseDuration(since)
		if err != nil {
			fmt.Fprintf(stderr, "gc trace reasons: invalid --since %q: %v\n", since, err) //nolint:errcheck
			return 1
		}
		filter.Since = time.Now().Add(-d)
	}
	recs, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), filter)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace reasons: %v\n", err) //nolint:errcheck
		return 1
	}
	reasons := make(map[string]int)
	for _, rec := range recs {
		if rec.ReasonCode != "" {
			reasons[string(rec.ReasonCode)]++
		}
	}
	data, err := json.MarshalIndent(reasons, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc trace reasons: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck
	return 0
}

func cmdTraceTail(template, since string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc trace tail: %v\n", err) //nolint:errcheck
		return 1
	}
	filter := TraceFilter{Template: normalizedTraceTemplate(template)}
	if since != "" {
		d, err := time.ParseDuration(since)
		if err != nil {
			fmt.Fprintf(stderr, "gc trace tail: invalid --since %q: %v\n", since, err) //nolint:errcheck
			return 1
		}
		filter.Since = time.Now().Add(-d)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), filter)
	if err != nil {
		fmt.Fprintf(stderr, "gc trace tail: %v\n", err) //nolint:errcheck
		return 1
	}
	records = traceRecentRecords(records, 20)
	var lastSeq uint64
	for _, rec := range records {
		if err := writeTraceTailRecord(stdout, rec); err != nil {
			fmt.Fprintf(stderr, "gc trace tail: %v\n", err) //nolint:errcheck
			return 1
		}
		if rec.Seq > lastSeq {
			lastSeq = rec.Seq
		}
	}
	if lastSeq == 0 {
		if head, err := traceHeadSeq(traceCityRuntimeDir(cityPath)); err == nil {
			lastSeq = head
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		filter.SeqAfter = lastSeq
		next, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), filter)
		if err != nil {
			fmt.Fprintf(stderr, "gc trace tail: %v\n", err) //nolint:errcheck
			return 1
		}
		for _, rec := range next {
			if err := writeTraceTailRecord(stdout, rec); err != nil {
				fmt.Fprintf(stderr, "gc trace tail: %v\n", err) //nolint:errcheck
				return 1
			}
			if rec.Seq > lastSeq {
				lastSeq = rec.Seq
			}
		}
	}
	return 0
}

func writeTraceTailRecord(stdout io.Writer, rec SessionReconcilerTraceRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(data))
	return err
}

func traceCityRuntimeDir(cityPath string) string {
	return filepath.Join(citylayout.RuntimeDataDir(cityPath), sessionReconcilerTraceRootDir)
}

func traceHeadSeq(rootDir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(rootDir, sessionReconcilerTraceHeadFile))
	if err != nil {
		if os.IsNotExist(err) {
			records, readErr := ReadTraceRecords(rootDir, TraceFilter{})
			if readErr != nil {
				return 0, readErr
			}
			var maxSeq uint64
			for _, record := range records {
				if record.Seq > maxSeq {
					maxSeq = record.Seq
				}
			}
			return maxSeq, nil
		}
		return 0, err
	}
	var head sessionReconcilerTraceHead
	if err := json.Unmarshal(data, &head); err != nil {
		return 0, err
	}
	return head.Seq, nil
}

func applyTraceControlMaybeRemote(cityPath string, req traceControlRequest) (*traceStatusJSON, string, error) {
	var (
		status *traceStatusJSON
		msg    string
		err    error
	)
	switch req.Action {
	case "start":
		status, msg, err = traceSocketControl(cityPath, "trace-arm", req)
	case "stop":
		status, msg, err = traceSocketControl(cityPath, "trace-stop", req)
	case "status":
		status, msg, err = traceSocketStatus(cityPath)
	default:
		return nil, "", fmt.Errorf("unsupported action %q", req.Action)
	}
	if err == nil {
		return status, msg, nil
	}
	if !traceControllerUnavailable(err) {
		return nil, "", err
	}
	return applyTraceControlLocal(cityPath, req)
}

func traceStatusMaybeRemote(cityPath string) (traceStatusJSON, error) {
	status, _, err := traceSocketStatus(cityPath)
	if err != nil {
		if !traceControllerUnavailable(err) {
			return traceStatusJSON{}, err
		}
		status, _, err = traceStatusLocal(cityPath)
		if err != nil {
			return traceStatusJSON{}, err
		}
		if status == nil {
			return traceStatusJSON{}, nil
		}
		return *status, nil
	}
	if status == nil {
		return traceStatusJSON{}, fmt.Errorf("empty trace status")
	}
	return *status, nil
}

func applyTraceControlLocal(cityPath string, req traceControlRequest) (*traceStatusJSON, string, error) {
	store := newSessionReconcilerTraceArmStore(cityPath)
	now := time.Now().UTC()
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	if req.ScopeType == "" {
		req.ScopeType = TraceArmScopeTemplate
	}
	if req.Source == "" {
		req.Source = TraceArmSourceManual
	}
	if req.Level == "" {
		req.Level = TraceModeDetail
	}
	switch req.Action {
	case "start":
		dur, err := time.ParseDuration(req.For)
		if err != nil {
			return nil, "", err
		}
		if dur <= 0 {
			return nil, "", fmt.Errorf("duration must be > 0")
		}
		arm := TraceArm{
			ScopeType:      req.ScopeType,
			ScopeValue:     normalizedTraceTemplate(req.ScopeValue),
			Source:         req.Source,
			Level:          req.Level,
			ArmedAt:        now,
			ExpiresAt:      now.Add(dur),
			LastExtendedAt: now,
			TriggerReason:  req.TriggerReason,
			ActorKind:      req.ActorKind,
			ActorUser:      req.ActorUser,
			ActorHost:      req.ActorHost,
			ActorPID:       req.ActorPID,
			CommandSummary: req.CommandSummary,
			UpdatedAt:      now,
		}
		if !req.RequestedAt.IsZero() {
			arm.RequestedAt = &req.RequestedAt
		}
		state, err := store.upsertArm(arm)
		if err != nil {
			return nil, "", err
		}
		status := traceStatusFromState(cityPath, state, now)
		return &status, fmt.Sprintf("armed %s %s for %s", req.Source, arm.ScopeValue, dur.Round(time.Second)), nil
	case "stop":
		state, err := store.remove(req.ScopeType, normalizedTraceTemplate(req.ScopeValue), req.All)
		if err != nil {
			return nil, "", err
		}
		status := traceStatusFromState(cityPath, state, now)
		if req.All {
			return &status, fmt.Sprintf("cleared trace arms for %s", req.ScopeValue), nil
		}
		return &status, fmt.Sprintf("cleared manual trace arm for %s", req.ScopeValue), nil
	case "status":
		return traceStatusLocal(cityPath)
	default:
		return nil, "", fmt.Errorf("unsupported action %q", req.Action)
	}
}

func traceStatusLocal(cityPath string) (*traceStatusJSON, string, error) {
	store := newSessionReconcilerTraceArmStore(cityPath)
	state, err := store.list()
	if err != nil {
		return nil, "", err
	}
	status := traceStatusFromState(cityPath, state, time.Now().UTC())
	return &status, "ok", nil
}

func traceStatusFromState(cityPath string, state TraceArmState, now time.Time) traceStatusJSON {
	arms := traceArmStatus(state, now)
	pid := controllerAlive(cityPath)
	return traceStatusJSON{
		CityPath:          cityPath,
		AsOf:              now,
		ControllerRunning: pid != 0,
		ControllerPID:     pid,
		ActiveArms:        arms,
	}
}

func traceSocketControl(cityPath, command string, req traceControlRequest) (*traceStatusJSON, string, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	resp, err := sendControllerCommand(cityPath, command+":"+string(payload))
	if err != nil {
		return nil, "", err
	}
	var reply traceControlReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		return nil, "", err
	}
	if !reply.OK {
		if reply.Error == "" {
			reply.Error = "trace command failed"
		}
		return nil, "", fmt.Errorf("%s", reply.Error)
	}
	return reply.Status, reply.Message, nil
}

func traceSocketStatus(cityPath string) (*traceStatusJSON, string, error) {
	resp, err := sendControllerCommand(cityPath, "trace-status")
	if err != nil {
		return nil, "", err
	}
	var reply traceControlReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		return nil, "", err
	}
	if !reply.OK {
		if reply.Error == "" {
			reply.Error = "trace status failed"
		}
		return nil, "", fmt.Errorf("%s", reply.Error)
	}
	return reply.Status, reply.Message, nil
}

func traceControllerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connecting to controller") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "refused")
}

func handleTraceSocketCmd(conn net.Conn, cityPath, action, payload string) bool {
	var req traceControlRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		writeJSONLine(conn, traceControlReply{OK: false, Error: fmt.Sprintf("invalid trace request: %v", err)})
		return false
	}
	req.Action = action
	status, msg, err := applyTraceControlLocal(cityPath, req)
	if err != nil {
		writeJSONLine(conn, traceControlReply{OK: false, Error: err.Error()})
		return false
	}
	writeJSONLine(conn, traceControlReply{OK: true, Message: msg, Status: status})
	return true
}

func handleTraceStatusSocketCmd(conn net.Conn, cityPath string) {
	status, _, err := traceStatusLocal(cityPath)
	if err != nil {
		writeJSONLine(conn, traceControlReply{OK: false, Error: err.Error()})
		return
	}
	writeJSONLine(conn, traceControlReply{OK: true, Message: "ok", Status: status})
}
