package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func newDoltStateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dolt-state",
		Short:  "Internal Dolt runtime state helpers",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var (
		stateFile     string
		pidText       string
		runText       string
		portText      string
		dataDir       string
		startedAt     string
		field         string
		cityPath      string
		hostText      string
		userText      string
		checkReadOnly bool
		checkDeleted  bool
		forceReset    bool
		logLevel      string
		timeoutMS     int
	)

	writeProvider := &cobra.Command{
		Use:    "write-provider",
		Short:  "Write provider-managed Dolt runtime state",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			state, err := parseDoltRuntimeStateFlags(pidText, runText, portText, dataDir, startedAt)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state write-provider: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := writeDoltRuntimeStateFile(stateFile, state); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state write-provider: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	writeProvider.Flags().StringVar(&stateFile, "file", "", "path to provider runtime state json")
	writeProvider.Flags().StringVar(&pidText, "pid", "", "server pid")
	writeProvider.Flags().StringVar(&runText, "running", "", "true or false")
	writeProvider.Flags().StringVar(&portText, "port", "", "server port")
	writeProvider.Flags().StringVar(&dataDir, "data-dir", "", "Dolt data directory")
	writeProvider.Flags().StringVar(&startedAt, "started-at", "", "RFC3339 start time (default: now UTC)")
	_ = writeProvider.MarkFlagRequired("file")
	_ = writeProvider.MarkFlagRequired("pid")
	_ = writeProvider.MarkFlagRequired("running")
	_ = writeProvider.MarkFlagRequired("port")
	_ = writeProvider.MarkFlagRequired("data-dir")
	cmd.AddCommand(writeProvider)

	readProvider := &cobra.Command{
		Use:    "read-provider",
		Short:  "Read a field from provider-managed Dolt runtime state",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			state, err := readDoltRuntimeStateFile(stateFile)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				fmt.Fprintf(stderr, "gc dolt-state read-provider: %v\n", err) //nolint:errcheck
				return errExit
			}
			value, err := doltRuntimeStateField(state, field)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state read-provider: %v\n", err) //nolint:errcheck
				return errExit
			}
			if _, err := io.WriteString(stdout, value); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state read-provider: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	readProvider.Flags().StringVar(&stateFile, "file", "", "path to provider runtime state json")
	readProvider.Flags().StringVar(&field, "field", "", "state field to read")
	_ = readProvider.MarkFlagRequired("file")
	_ = readProvider.MarkFlagRequired("field")
	cmd.AddCommand(readProvider)

	runtimeLayout := &cobra.Command{
		Use:    "runtime-layout",
		Short:  "Resolve provider-managed Dolt runtime layout",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			layout, err := resolveManagedDoltRuntimeLayout(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state runtime-layout: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range doltRuntimeLayoutFields(layout) {
				if _, err := fmt.Fprintln(stdout, line); err != nil {
					fmt.Fprintf(stderr, "gc dolt-state runtime-layout: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	runtimeLayout.Flags().StringVar(&cityPath, "city", "", "city root")
	_ = runtimeLayout.MarkFlagRequired("city")
	cmd.AddCommand(runtimeLayout)

	allocatePort := &cobra.Command{
		Use:    "allocate-port",
		Short:  "Choose the managed Dolt port for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			port, err := chooseManagedDoltPort(cityPath, stateFile)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state allocate-port: %v\n", err) //nolint:errcheck
				return errExit
			}
			if _, err := fmt.Fprintln(stdout, port); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state allocate-port: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	allocatePort.Flags().StringVar(&cityPath, "city", "", "city root")
	allocatePort.Flags().StringVar(&stateFile, "state-file", "", "path to provider runtime state json")
	_ = allocatePort.MarkFlagRequired("city")
	cmd.AddCommand(allocatePort)

	inspectManaged := &cobra.Command{
		Use:    "inspect-managed",
		Short:  "Inspect the managed Dolt process for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info, err := inspectManagedDoltProcess(cityPath, portText)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state inspect-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range doltProcessInspectionFields(info) {
				if _, err := fmt.Fprintln(stdout, line); err != nil {
					fmt.Fprintf(stderr, "gc dolt-state inspect-managed: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	inspectManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	inspectManaged.Flags().StringVar(&portText, "port", "", "selected Dolt port")
	_ = inspectManaged.MarkFlagRequired("city")
	cmd.AddCommand(inspectManaged)

	probeManaged := &cobra.Command{
		Use:    "probe-managed",
		Short:  "Probe the managed Dolt listener for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := probeManagedDolt(cityPath, hostText, portText)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state probe-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range managedDoltProbeFields(report) {
				if _, err := fmt.Fprintln(stdout, line); err != nil {
					fmt.Fprintf(stderr, "gc dolt-state probe-managed: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	probeManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	probeManaged.Flags().StringVar(&hostText, "host", "", "Dolt host")
	probeManaged.Flags().StringVar(&portText, "port", "", "selected Dolt port")
	_ = probeManaged.MarkFlagRequired("city")
	_ = probeManaged.MarkFlagRequired("port")
	cmd.AddCommand(probeManaged)

	existingManaged := &cobra.Command{
		Use:    "existing-managed",
		Short:  "Assess whether the current managed Dolt process can be reused",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := assessExistingManagedDolt(cityPath, hostText, portText, userText, time.Duration(timeoutMS)*time.Millisecond)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state existing-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range managedDoltExistingFields(report) {
				if _, err := fmt.Fprintln(stdout, line); err != nil {
					fmt.Fprintf(stderr, "gc dolt-state existing-managed: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	existingManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	existingManaged.Flags().StringVar(&hostText, "host", "", "Dolt host")
	existingManaged.Flags().StringVar(&portText, "port", "", "selected Dolt port")
	existingManaged.Flags().StringVar(&userText, "user", "", "Dolt user")
	existingManaged.Flags().IntVar(&timeoutMS, "timeout-ms", 30000, "reusability timeout in milliseconds")
	_ = existingManaged.MarkFlagRequired("city")
	_ = existingManaged.MarkFlagRequired("port")
	cmd.AddCommand(existingManaged)

	nowMS := &cobra.Command{
		Use:    "now-ms",
		Short:  "Print the current Unix time in milliseconds",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := fmt.Fprintln(stdout, time.Now().UnixMilli()); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state now-ms: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	cmd.AddCommand(nowMS)

	queryProbe := &cobra.Command{
		Use:    "query-probe",
		Short:  "Probe managed Dolt SQL readiness",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := managedDoltQueryProbe(hostText, portText, userText); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state query-probe: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	queryProbe.Flags().StringVar(&hostText, "host", "", "Dolt host")
	queryProbe.Flags().StringVar(&portText, "port", "", "Dolt port")
	queryProbe.Flags().StringVar(&userText, "user", "", "Dolt user")
	_ = queryProbe.MarkFlagRequired("port")
	cmd.AddCommand(queryProbe)

	readOnlyCheck := &cobra.Command{
		Use:    "read-only-check",
		Short:  "Detect managed Dolt read-only mode",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			state, err := managedDoltReadOnlyState(hostText, portText, userText)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state read-only-check: %v\n", err) //nolint:errcheck
				return errExit
			}
			if state == "true" {
				return nil
			}
			return errExit
		},
	}
	readOnlyCheck.Flags().StringVar(&hostText, "host", "", "Dolt host")
	readOnlyCheck.Flags().StringVar(&portText, "port", "", "Dolt port")
	readOnlyCheck.Flags().StringVar(&userText, "user", "", "Dolt user")
	_ = readOnlyCheck.MarkFlagRequired("port")
	cmd.AddCommand(readOnlyCheck)

	resetProbe := &cobra.Command{
		Use:    "reset-probe",
		Short:  "Reset managed Dolt health probe artifacts",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !forceReset {
				fmt.Fprintf(stderr, "gc dolt-state reset-probe: refusing to reset health probe artifacts without --force; %s may contain a legacy bead store in old metadata\n", managedDoltProbeDatabase) //nolint:errcheck
				return errExit
			}
			if err := managedDoltResetProbe(hostText, portText, userText); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state reset-probe: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	resetProbe.Flags().StringVar(&hostText, "host", "", "Dolt host")
	resetProbe.Flags().StringVar(&portText, "port", "", "Dolt port")
	resetProbe.Flags().StringVar(&userText, "user", "", "Dolt user")
	resetProbe.Flags().BoolVar(&forceReset, "force", false, "acknowledge dropping the legacy probe database and GC-owned probe table")
	_ = resetProbe.MarkFlagRequired("port")
	cmd.AddCommand(resetProbe)

	healthCheck := &cobra.Command{
		Use:    "health-check",
		Short:  "Report managed Dolt SQL health",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := managedDoltHealthCheck(hostText, portText, userText, checkReadOnly)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state health-check: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range managedDoltHealthCheckFields(report) {
				if _, err := fmt.Fprintln(stdout, line); err != nil {
					fmt.Fprintf(stderr, "gc dolt-state health-check: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	healthCheck.Flags().StringVar(&hostText, "host", "", "Dolt host")
	healthCheck.Flags().StringVar(&portText, "port", "", "Dolt port")
	healthCheck.Flags().StringVar(&userText, "user", "", "Dolt user")
	healthCheck.Flags().BoolVar(&checkReadOnly, "check-read-only", false, "include read-only status")
	_ = healthCheck.MarkFlagRequired("port")
	cmd.AddCommand(healthCheck)

	waitReady := &cobra.Command{
		Use:    "wait-ready",
		Short:  "Wait for managed Dolt SQL readiness",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, err := strconv.Atoi(pidText)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state wait-ready: invalid --pid %q: %v\n", pidText, err) //nolint:errcheck
				return errExit
			}
			report, err := waitForManagedDoltReady(cityPath, hostText, portText, userText, pid, time.Duration(timeoutMS)*time.Millisecond, checkDeleted)
			for _, line := range managedDoltWaitReadyFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state wait-ready: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state wait-ready: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	waitReady.Flags().StringVar(&cityPath, "city", "", "city root")
	waitReady.Flags().StringVar(&hostText, "host", "", "Dolt host")
	waitReady.Flags().StringVar(&portText, "port", "", "Dolt port")
	waitReady.Flags().StringVar(&userText, "user", "", "Dolt user")
	waitReady.Flags().StringVar(&pidText, "pid", "", "managed Dolt pid")
	waitReady.Flags().IntVar(&timeoutMS, "timeout-ms", 30000, "readiness timeout in milliseconds")
	waitReady.Flags().BoolVar(&checkDeleted, "check-deleted", false, "fail if the process holds deleted data inodes under the managed data dir")
	_ = waitReady.MarkFlagRequired("city")
	_ = waitReady.MarkFlagRequired("port")
	_ = waitReady.MarkFlagRequired("pid")
	cmd.AddCommand(waitReady)
	cmd.AddCommand(newEnsureProjectIDCmd(stdout, stderr))

	stopManaged := &cobra.Command{
		Use:    "stop-managed",
		Short:  "Stop the managed Dolt process for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := stopManagedDoltProcess(cityPath, portText)
			for _, line := range managedDoltStopFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state stop-managed: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state stop-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	stopManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	stopManaged.Flags().StringVar(&portText, "port", "", "managed Dolt port")
	_ = stopManaged.MarkFlagRequired("city")
	cmd.AddCommand(stopManaged)

	startManaged := &cobra.Command{
		Use:    "start-managed",
		Short:  "Start the managed Dolt process for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := startManagedDoltProcess(cityPath, hostText, portText, userText, logLevel, time.Duration(timeoutMS)*time.Millisecond)
			for _, line := range managedDoltStartFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state start-managed: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state start-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	startManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	startManaged.Flags().StringVar(&hostText, "host", "", "listener host")
	startManaged.Flags().StringVar(&portText, "port", "", "managed Dolt port")
	startManaged.Flags().StringVar(&userText, "user", "", "Dolt user")
	startManaged.Flags().StringVar(&logLevel, "log-level", "warning", "Dolt log level")
	startManaged.Flags().IntVar(&timeoutMS, "timeout-ms", 30000, "readiness timeout in milliseconds")
	_ = startManaged.MarkFlagRequired("city")
	_ = startManaged.MarkFlagRequired("port")
	cmd.AddCommand(startManaged)

	recoverManaged := &cobra.Command{
		Use:    "recover-managed",
		Short:  "Recover or reuse the managed Dolt process for a city",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := recoverManagedDoltProcess(cityPath, hostText, portText, userText, logLevel, time.Duration(timeoutMS)*time.Millisecond)
			for _, line := range managedDoltRecoverFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state recover-managed: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state recover-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	recoverManaged.Flags().StringVar(&cityPath, "city", "", "city root")
	recoverManaged.Flags().StringVar(&hostText, "host", "", "listener host")
	recoverManaged.Flags().StringVar(&portText, "port", "", "managed Dolt port")
	recoverManaged.Flags().StringVar(&userText, "user", "", "Dolt user")
	recoverManaged.Flags().StringVar(&logLevel, "log-level", "warning", "Dolt log level")
	recoverManaged.Flags().IntVar(&timeoutMS, "timeout-ms", 30000, "readiness timeout in milliseconds")
	_ = recoverManaged.MarkFlagRequired("city")
	_ = recoverManaged.MarkFlagRequired("port")
	cmd.AddCommand(recoverManaged)

	preflightClean := &cobra.Command{
		Use:    "preflight-clean",
		Short:  "Run managed Dolt preflight cleanup",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := preflightManagedDoltCleanup(cityPath); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state preflight-clean: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	preflightClean.Flags().StringVar(&cityPath, "city", "", "city root")
	_ = preflightClean.MarkFlagRequired("city")
	cmd.AddCommand(preflightClean)

	return cmd
}

func parseDoltRuntimeStateFlags(pidText, runText, portText, dataDir, startedAt string) (doltRuntimeState, error) {
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return doltRuntimeState{}, fmt.Errorf("invalid --pid %q: %w", pidText, err)
	}
	running, err := strconv.ParseBool(runText)
	if err != nil {
		return doltRuntimeState{}, fmt.Errorf("invalid --running %q: %w", runText, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return doltRuntimeState{}, fmt.Errorf("invalid --port %q: %w", portText, err)
	}
	if dataDir == "" {
		return doltRuntimeState{}, fmt.Errorf("missing --data-dir")
	}
	if startedAt == "" {
		startedAt = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
		return doltRuntimeState{}, fmt.Errorf("invalid --started-at %q: %w", startedAt, err)
	}
	return doltRuntimeState{
		Running:   running,
		PID:       pid,
		Port:      port,
		DataDir:   dataDir,
		StartedAt: startedAt,
	}, nil
}

func doltRuntimeStateField(state doltRuntimeState, field string) (string, error) {
	switch field {
	case "running":
		return strconv.FormatBool(state.Running) + "\n", nil
	case "pid":
		return strconv.Itoa(state.PID) + "\n", nil
	case "port":
		return strconv.Itoa(state.Port) + "\n", nil
	case "data_dir":
		return state.DataDir + "\n", nil
	case "started_at":
		return state.StartedAt + "\n", nil
	default:
		return "", fmt.Errorf("unsupported --field %q", field)
	}
}
