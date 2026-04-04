package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

var (
	ensureSupervisorRunningHook = ensureSupervisorRunning
	reloadSupervisorHook        = reloadSupervisor
	supervisorAliveHook         = supervisorAlive
)

func newSupervisorRunCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the machine-wide supervisor in the foreground",
		Long: `Run the machine-wide supervisor in the foreground.

This is the canonical long-running control loop. It reads ~/.gc/cities.toml
for registered cities, manages them from one process, and hosts the shared
API server.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorRun(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func doSupervisorRun(stdout, stderr io.Writer) int {
	return runSupervisor(stdout, stderr)
}

func doSupervisorStart(stdout, stderr io.Writer) int {
	if pid := supervisorAlive(); pid != 0 {
		fmt.Fprintf(stderr, "gc supervisor start: supervisor already running (PID %d)\n", pid) //nolint:errcheck // best-effort stderr
		return 1
	}

	lock, err := acquireSupervisorLock()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	lock.Close() //nolint:errcheck // release probe lock

	gcPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: finding executable: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	logPath := supervisorLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: creating log dir: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: opening log: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer logFile.Close() //nolint:errcheck // best-effort cleanup

	child := exec.Command(gcPath, "supervisor", "run")
	child.SysProcAttr = backgroundSysProcAttr()
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = os.Environ()

	if err := child.Start(); err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid := supervisorAlive(); pid != 0 {
			fmt.Fprintf(stdout, "Supervisor started (PID %d)\n", pid) //nolint:errcheck // best-effort stdout
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready; see %s\n", logPath) //nolint:errcheck // best-effort stderr
	return 1
}

func ensureSupervisorRunning(stdout, stderr io.Writer) int {
	// Always regenerate the service file so upgrades pick up template
	// changes (e.g. PATH captured from the user's shell).
	if doSupervisorInstall(stdout, stderr) != 0 {
		if supervisorAlive() != 0 {
			return 0
		}
		// Fall back to bare start if install fails (e.g., unsupported OS).
		return doSupervisorStart(stdout, stderr)
	}
	if supervisorAlive() != 0 {
		return 0
	}
	return waitForSupervisorReady(stderr)
}

// waitForSupervisorReady polls supervisorAlive with a 5s timeout.
func waitForSupervisorReady(stderr io.Writer) int {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if supervisorAlive() != 0 {
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "gc: supervisor did not become ready; see %s\n", supervisorLogPath()) //nolint:errcheck // best-effort stderr
	return 1
}

// unloadSupervisorService stops the platform service without removing
// the unit file, so gc start can reload it later.
func unloadSupervisorService() {
	switch goruntime.GOOS {
	case "darwin":
		exec.Command("launchctl", "unload", supervisorLaunchdPlistPath()).Run() //nolint:errcheck // best-effort
	case "linux":
		exec.Command("systemctl", "--user", "stop", "gascity-supervisor.service").Run() //nolint:errcheck // best-effort
	}
}

func newSupervisorLogsCmd(stdout, stderr io.Writer) *cobra.Command {
	var numLines int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the supervisor log file",
		Long: `Tail the machine-wide supervisor log file.

Shows recent log output from background and service-managed supervisor runs.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorLogs(numLines, follow, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&numLines, "lines", "n", 50, "number of lines to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return cmd
}

func doSupervisorLogs(numLines int, follow bool, stdout, stderr io.Writer) int {
	logPath := supervisorLogPath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor logs: log file not found: %s\n", logPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	args := []string{"-n", fmt.Sprintf("%d", numLines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logPath)

	cmd := exec.Command("tail", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "gc supervisor logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func newSupervisorInstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the supervisor as a platform service",
		Long: `Install the machine-wide supervisor as a platform service that
starts on login.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorInstall(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func doSupervisorInstall(stdout, stderr io.Writer) int {
	data, err := buildSupervisorServiceData()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch goruntime.GOOS {
	case "darwin":
		return installSupervisorLaunchd(data, stdout, stderr)
	case "linux":
		return installSupervisorSystemd(data, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc supervisor install: not supported on %s\n", goruntime.GOOS) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func newSupervisorUninstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the platform service",
		Long:  `Remove the platform service and stop the machine-wide supervisor.`,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorUninstall(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func doSupervisorUninstall(stdout, stderr io.Writer) int {
	data, err := buildSupervisorServiceData()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch goruntime.GOOS {
	case "darwin":
		return uninstallSupervisorLaunchd(data, stdout, stderr)
	case "linux":
		return uninstallSupervisorSystemd(data, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc supervisor uninstall: not supported on %s\n", goruntime.GOOS) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func supervisorLogPath() string {
	return filepath.Join(supervisor.DefaultHome(), "supervisor.log")
}

type supervisorServiceData struct {
	GCPath   string
	LogPath  string
	GCHome   string
	SafeName string
	Path     string
}

func buildSupervisorServiceData() (*supervisorServiceData, error) {
	gcPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding executable: %w", err)
	}
	home := supervisor.DefaultHome()
	return &supervisorServiceData{
		GCPath:   gcPath,
		LogPath:  supervisorLogPath(),
		GCHome:   home,
		SafeName: sanitizeServiceName(filepath.Base(home)),
		Path:     os.Getenv("PATH"),
	}, nil
}

func sanitizeServiceName(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	name = re.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

const supervisorLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.gascity.supervisor</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{xmlesc .GCPath}}</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>{{xmlesc .LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{xmlesc .LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>GC_HOME</key>
        <string>{{xmlesc .GCHome}}</string>
        <key>PATH</key>
        <string>{{xmlesc .Path}}</string>
    </dict>
</dict>
</plist>
`

const supervisorSystemdTemplate = `[Unit]
Description=Gas City machine supervisor

[Service]
Type=simple
ExecStart={{.GCPath}} supervisor run
Restart=always
RestartSec=5s
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}
Environment=GC_HOME="{{.GCHome}}"
Environment=PATH="{{.Path}}"

[Install]
WantedBy=default.target
`

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func renderSupervisorTemplate(tmplStr string, data *supervisorServiceData) (string, error) {
	funcMap := template.FuncMap{"xmlesc": xmlEscape}
	tmpl, err := template.New("service").Funcs(funcMap).Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func supervisorLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.gascity.supervisor.plist")
}

func supervisorSystemdServicePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user", "gascity-supervisor.service")
}

func installSupervisorLaunchd(data *supervisorServiceData, stdout, stderr io.Writer) int {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: rendering plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	path := supervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	exec.Command("launchctl", "unload", path).Run() //nolint:errcheck // best-effort cleanup
	if err := exec.Command("launchctl", "load", path).Run(); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: launchctl load: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Installed launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func uninstallSupervisorLaunchd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorLaunchdPlistPath()
	exec.Command("launchctl", "unload", path).Run() //nolint:errcheck // best-effort cleanup
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Uninstalled launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func installSupervisorSystemd(data *supervisorServiceData, stdout, stderr io.Writer) int {
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: rendering unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "gascity-supervisor.service"},
		{"--user", "start", "gascity-supervisor.service"},
	} {
		if err := exec.Command("systemctl", args...).Run(); err != nil {
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	fmt.Fprintf(stdout, "Installed systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func uninstallSupervisorSystemd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorSystemdServicePath()
	exec.Command("systemctl", "--user", "stop", "gascity-supervisor.service").Run()    //nolint:errcheck // best-effort cleanup
	exec.Command("systemctl", "--user", "disable", "gascity-supervisor.service").Run() //nolint:errcheck // best-effort cleanup
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()     //nolint:errcheck // best-effort cleanup
	fmt.Fprintf(stdout, "Uninstalled systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}
