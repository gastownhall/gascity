package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gastownhall/gascity/internal/searchpath"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

var (
	ensureSupervisorRunningHook = ensureSupervisorRunning
	reloadSupervisorHook        = reloadSupervisor
	supervisorAliveHook         = supervisorAlive
	supervisorReadyTimeout      = 15 * time.Second
	supervisorReadyPollInterval = 100 * time.Millisecond
	supervisorLaunchctlRun      = func(args ...string) error {
		return exec.Command("launchctl", args...).Run()
	}
	supervisorSystemctlRun = func(args ...string) error {
		return exec.Command("systemctl", args...).Run()
	}
	supervisorSystemctlActive = func(service string) bool {
		return exec.Command("systemctl", "--user", "is-active", "--quiet", service).Run() == nil
	}
)

const supervisorServiceFileMode os.FileMode = 0o600

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
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor start: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
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

	deadline := time.Now().Add(supervisorReadyTimeout)
	for time.Now().Before(deadline) {
		if pid := supervisorAliveHook(); pid != 0 {
			fmt.Fprintf(stdout, "Supervisor started (PID %d)\n", pid) //nolint:errcheck // best-effort stdout
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}

	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready; see %s\n", logPath) //nolint:errcheck // best-effort stderr
	return 1
}

func ensureSupervisorRunning(stdout, stderr io.Writer) int {
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor start: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Always regenerate the service file so upgrades pick up template
	// changes (e.g. PATH captured from the user's shell).
	if doSupervisorInstall(stdout, stderr) != 0 {
		if supervisorAlive() != 0 {
			return 0
		}
		// Fall back to bare start if install fails (e.g., unsupported OS).
		return doSupervisorStart(stdout, stderr)
	}
	if supervisorAliveHook() != 0 {
		return 0
	}
	return waitForSupervisorReady(stderr)
}

func platformSupervisorHomeOverrideError() (string, bool) {
	switch goruntime.GOOS {
	case "darwin", "linux":
	default:
		return "", false
	}
	envHome, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(envHome) == "" {
		return "", false
	}
	lookup, err := osuser.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		return "", false
	}
	if filepath.Clean(envHome) == filepath.Clean(lookup.HomeDir) {
		return "", false
	}
	return fmt.Sprintf("HOME override %q differs from the user home %q; platform supervisor requires the real HOME. Keep HOME unchanged and use GC_HOME for isolated runs", envHome, lookup.HomeDir), true
}

func waitForSupervisorPID() int {
	deadline := time.Now().Add(supervisorReadyTimeout)
	for {
		if pid := supervisorAliveHook(); pid != 0 {
			return pid
		}
		if !time.Now().Before(deadline) {
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}
}

// waitForSupervisorReady polls supervisorAlive until the configured timeout.
func waitForSupervisorReady(stderr io.Writer) int {
	if waitForSupervisorPID() != 0 {
		return 0
	}
	fmt.Fprintf(stderr, "gc: supervisor did not become ready; see %s\n", supervisorLogPath()) //nolint:errcheck // best-effort stderr
	return 1
}

// unloadSupervisorService stops the platform service without removing
// the unit file, so gc start can reload it later. It is a no-op when
// the platform unit/plist is not installed — this keeps unit tests that
// invoke the stop helper hermetic on machines where the service has
// never been registered.
func unloadSupervisorService() {
	switch goruntime.GOOS {
	case "darwin":
		path := supervisorLaunchdPlistPath()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			_ = supervisorLaunchctlRun("unload", path)
		}
		_ = unloadLegacySupervisorLaunchd(false)
	case "linux":
		service := supervisorSystemdServiceName()
		if _, err := os.Stat(supervisorSystemdServicePath()); !errors.Is(err, os.ErrNotExist) {
			_ = supervisorSystemctlRun("--user", "stop", service)
		}
		_ = unloadLegacySupervisorSystemd(false)
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
	if msg, blocked := platformSupervisorHomeOverrideError(); blocked {
		fmt.Fprintf(stderr, "gc supervisor install: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
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
	GCPath        string
	LogPath       string
	GCHome        string
	XDGRuntimeDir string
	LaunchdLabel  string
	SafeName      string
	Path          string
	ExtraEnv      []supervisorServiceEnvVar
}

type supervisorServiceEnvVar struct {
	Name  string
	Value string
}

func buildSupervisorServiceData() (*supervisorServiceData, error) {
	gcPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding executable: %w", err)
	}
	homeDir, _ := os.UserHomeDir()
	home := supervisor.DefaultHome()
	xdgRuntimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if supervisor.UsesIsolatedGCHomeOverride() {
		xdgRuntimeDir = ""
	}
	return &supervisorServiceData{
		GCPath:        gcPath,
		LogPath:       supervisorLogPath(),
		GCHome:        home,
		XDGRuntimeDir: xdgRuntimeDir,
		LaunchdLabel:  supervisorLaunchdLabel(),
		SafeName:      sanitizeServiceName(filepath.Base(home)),
		Path:          searchpath.ExpandPath(homeDir, goruntime.GOOS, os.Getenv("PATH")),
		ExtraEnv:      supervisorServiceExtraEnv(),
	}, nil
}

func sanitizeServiceName(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	name = re.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

var supervisorServiceEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Keep persistent service-file env narrow. Provider credentials and user
// context need to survive launchd/systemd startup; arbitrary shell state can
// be opted in with GC_SUPERVISOR_ENV.
var supervisorServiceEnvKeys = map[string]bool{
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true,
	"CLAUDE_CODE_EFFORT_LEVEL":                 true,
	"CLAUDE_CODE_OAUTH_TOKEN":                  true,
	"CLAUDE_CODE_SUBAGENT_MODEL":               true,
	"CLAUDE_CONFIG_DIR":                        true,
	"HOME":                                     true,
	"LANG":                                     true,
	"LC_ALL":                                   true,
	"LC_CTYPE":                                 true,
	"LOGNAME":                                  true,
	"SHELL":                                    true,
	"USER":                                     true,
	"XDG_CONFIG_HOME":                          true,
	"XDG_STATE_HOME":                           true,
}

var providerCredentialEnvPrefixes = []string{
	"ANTHROPIC_",
	"GEMINI_",
	"GOOGLE_",
	"OPENAI_",
}

var supervisorServiceFixedEnvKeys = map[string]bool{
	"GC_HOME":         true,
	"PATH":            true,
	"XDG_RUNTIME_DIR": true,
}

func supervisorServiceExtraEnv() []supervisorServiceEnvVar {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || val == "" || !shouldPersistSupervisorEnv(key) {
			continue
		}
		env[key] = val
	}
	for _, key := range supervisorServiceExplicitEnvKeys(os.Getenv("GC_SUPERVISOR_ENV")) {
		if val := os.Getenv(key); val != "" {
			env[key] = val
		}
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]supervisorServiceEnvVar, 0, len(keys))
	for _, key := range keys {
		out = append(out, supervisorServiceEnvVar{Name: key, Value: env[key]})
	}
	return out
}

func shouldPersistSupervisorEnv(key string) bool {
	if !supervisorServiceEnvNameRE.MatchString(key) || supervisorServiceFixedEnvKeys[key] {
		return false
	}
	if supervisorServiceEnvKeys[key] {
		return true
	}
	return isProviderCredentialEnv(key)
}

func isProviderCredentialEnv(key string) bool {
	for _, prefix := range providerCredentialEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func supervisorServiceExplicitEnvKeys(raw string) []string {
	fields := strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(raw))
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		key := strings.TrimSpace(field)
		if key == "" || seen[key] || !supervisorServiceEnvNameRE.MatchString(key) || supervisorServiceFixedEnvKeys[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

const (
	defaultSupervisorLaunchdLabel = "com.gascity.supervisor"
	defaultSupervisorSystemdUnit  = "gascity-supervisor.service"
)

func supervisorServiceSuffix() string {
	if !supervisor.UsesIsolatedGCHomeOverride() {
		return ""
	}
	gcHome := isolatedSupervisorHome()
	base := sanitizeServiceName(filepath.Base(gcHome))
	sum := sha1.Sum([]byte(gcHome))
	hash := hex.EncodeToString(sum[:])[:8]
	if base == "" {
		return "isolated-" + hash
	}
	return base + "-" + hash
}

func supervisorLaunchdLabel() string {
	if suffix := supervisorServiceSuffix(); suffix != "" {
		return defaultSupervisorLaunchdLabel + "." + suffix
	}
	return defaultSupervisorLaunchdLabel
}

func supervisorSystemdServiceName() string {
	if suffix := supervisorServiceSuffix(); suffix != "" {
		return "gascity-supervisor-" + suffix + ".service"
	}
	return defaultSupervisorSystemdUnit
}

const supervisorLaunchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xmlesc .LaunchdLabel}}</string>
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
        {{if .XDGRuntimeDir}}
        <key>XDG_RUNTIME_DIR</key>
        <string>{{xmlesc .XDGRuntimeDir}}</string>
        {{end}}
        <key>PATH</key>
        <string>{{xmlesc .Path}}</string>
        {{range .ExtraEnv}}
        <key>{{xmlesc .Name}}</key>
        <string>{{xmlesc .Value}}</string>
        {{end}}
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
{{if .XDGRuntimeDir}}Environment=XDG_RUNTIME_DIR="{{.XDGRuntimeDir}}"
{{end}}Environment=PATH="{{.Path}}"
{{range .ExtraEnv}}Environment={{systemdenv .Name .Value}}
{{end}}

[Install]
WantedBy=default.target
`

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func systemdEnv(name, value string) string {
	return name + "=" + strconv.Quote(value)
}

func renderSupervisorTemplate(tmplStr string, data *supervisorServiceData) (string, error) {
	funcMap := template.FuncMap{"xmlesc": xmlEscape, "systemdenv": systemdEnv}
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

func writeSupervisorServiceFile(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		if err := os.Chmod(path, supervisorServiceFileMode); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, content, supervisorServiceFileMode); err != nil {
		return err
	}
	return os.Chmod(path, supervisorServiceFileMode)
}

func supervisorLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", supervisorLaunchdLabel()+".plist")
}

func supervisorLaunchdServiceTarget(label string) string {
	if label == "" {
		label = supervisorLaunchdLabel()
	}
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
}

func loadAndStartSupervisorLaunchd(path, label string) error {
	if err := supervisorLaunchctlRun("load", path); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	target := supervisorLaunchdServiceTarget(label)
	if err := supervisorLaunchctlRun("enable", target); err != nil {
		return fmt.Errorf("enable %s: %w", target, err)
	}
	if err := supervisorLaunchctlRun("kickstart", "-p", target); err != nil {
		return fmt.Errorf("kickstart -p %s: %w", target, err)
	}
	return nil
}

func legacySupervisorLaunchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", defaultSupervisorLaunchdLabel+".plist")
}

func supervisorSystemdServicePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
}

func legacySupervisorSystemdServicePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user", defaultSupervisorSystemdUnit)
}

func isolatedSupervisorHome() string {
	return normalizePathForCompare(strings.TrimSpace(os.Getenv("GC_HOME")))
}

func legacySupervisorTargetsCurrentHome(path string) bool {
	if !supervisor.UsesIsolatedGCHomeOverride() {
		return false
	}
	gcHome := isolatedSupervisorHome()
	if gcHome == "" {
		return false
	}
	legacyHome, ok := legacySupervisorHome(path)
	return ok && samePath(legacyHome, gcHome)
}

func legacySupervisorHome(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	switch filepath.Ext(path) {
	case ".plist":
		return launchdSupervisorHome(data)
	case ".service":
		return systemdSupervisorHome(data)
	default:
		return "", false
	}
}

type plistValue struct {
	text string
	dict map[string]plistValue
}

func launchdSupervisorHome(data []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "dict" {
			continue
		}
		root, err := parsePlistDict(dec)
		if err != nil {
			return "", false
		}
		env, ok := root["EnvironmentVariables"]
		if !ok || env.dict == nil {
			return "", false
		}
		gcHome, ok := env.dict["GC_HOME"]
		if !ok || gcHome.text == "" {
			return "", false
		}
		return filepath.Clean(gcHome.text), true
	}
}

func parsePlistDict(dec *xml.Decoder) (map[string]plistValue, error) {
	dict := make(map[string]plistValue)
	currentKey := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			switch tok.Name.Local {
			case "key":
				var key string
				if err := dec.DecodeElement(&key, &tok); err != nil {
					return nil, err
				}
				currentKey = key
			case "string":
				var value string
				if err := dec.DecodeElement(&value, &tok); err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{text: value}
					currentKey = ""
				}
			case "dict":
				nested, err := parsePlistDict(dec)
				if err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{dict: nested}
					currentKey = ""
				}
			default:
				if err := skipXMLElement(dec); err != nil {
					return nil, err
				}
				if currentKey != "" {
					dict[currentKey] = plistValue{}
					currentKey = ""
				}
			}
		case xml.EndElement:
			if tok.Name.Local == "dict" {
				return dict, nil
			}
		}
	}
}

func skipXMLElement(dec *xml.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

func systemdSupervisorHome(data []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Environment=GC_HOME=") {
			continue
		}
		value := strings.TrimPrefix(line, "Environment=GC_HOME=")
		if unquoted, err := strconv.Unquote(value); err == nil {
			return filepath.Clean(unquoted), true
		}
		return filepath.Clean(value), true
	}
	return "", false
}

func unloadLegacySupervisorLaunchd(remove bool) error {
	path := legacySupervisorLaunchdPlistPath()
	if samePath(path, supervisorLaunchdPlistPath()) || !legacySupervisorTargetsCurrentHome(path) {
		return nil
	}
	_ = supervisorLaunchctlRun("unload", path)
	if remove {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy plist %s: %w", path, err)
		}
	}
	return nil
}

func unloadLegacySupervisorSystemd(remove bool) error {
	path := legacySupervisorSystemdServicePath()
	if samePath(path, supervisorSystemdServicePath()) || !legacySupervisorTargetsCurrentHome(path) {
		return nil
	}
	_ = supervisorSystemctlRun("--user", "stop", defaultSupervisorSystemdUnit)
	if remove {
		_ = supervisorSystemctlRun("--user", "disable", defaultSupervisorSystemdUnit)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy unit %s: %w", path, err)
		}
	}
	return nil
}

func rollbackNewSupervisorLaunchdInstall(path string, restoreLegacy bool) error {
	var errs []error
	_ = supervisorLaunchctlRun("unload", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing failed plist %s during rollback: %w", path, err))
	}
	if restoreLegacy {
		if err := loadAndStartSupervisorLaunchd(legacySupervisorLaunchdPlistPath(), defaultSupervisorLaunchdLabel); err != nil {
			errs = append(errs, fmt.Errorf("restoring legacy plist %s: %w", legacySupervisorLaunchdPlistPath(), err))
		}
	}
	return errors.Join(errs...)
}

func restorePreviousSupervisorLaunchdInstall(path string, previousContent []byte) error {
	var errs []error
	_ = supervisorLaunchctlRun("unload", path)
	if err := writeSupervisorServiceFile(path, previousContent); err != nil {
		errs = append(errs, fmt.Errorf("restoring previous plist %s: %w", path, err))
	} else if err := loadAndStartSupervisorLaunchd(path, supervisorLaunchdLabel()); err != nil {
		errs = append(errs, fmt.Errorf("reloading previous plist %s: %w", path, err))
	}
	return errors.Join(errs...)
}

func rollbackNewSupervisorSystemdInstall(path, service string, restoreLegacy bool) error {
	var errs []error
	_ = supervisorSystemctlRun("--user", "stop", service)
	_ = supervisorSystemctlRun("--user", "disable", service)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing failed unit %s during rollback: %w", path, err))
	}
	if err := supervisorSystemctlRun("--user", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl --user daemon-reload during rollback: %w", err))
	}
	if restoreLegacy {
		if err := supervisorSystemctlRun("--user", "start", defaultSupervisorSystemdUnit); err != nil {
			errs = append(errs, fmt.Errorf("restoring legacy unit %s: %w", defaultSupervisorSystemdUnit, err))
		}
	}
	return errors.Join(errs...)
}

func restorePreviousSupervisorSystemdInstall(path, service string, previousContent []byte, restart bool) error {
	var errs []error
	if restart {
		_ = supervisorSystemctlRun("--user", "stop", service)
	}
	if err := writeSupervisorServiceFile(path, previousContent); err != nil {
		errs = append(errs, fmt.Errorf("restoring previous unit %s: %w", path, err))
		return errors.Join(errs...)
	}
	if err := supervisorSystemctlRun("--user", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl --user daemon-reload during rollback: %w", err))
	}
	if restart {
		if err := supervisorSystemctlRun("--user", "enable", service); err != nil {
			errs = append(errs, fmt.Errorf("restoring previous unit enable %s: %w", service, err))
		}
		if err := supervisorSystemctlRun("--user", "start", service); err != nil {
			errs = append(errs, fmt.Errorf("restoring previous unit start %s: %w", service, err))
		}
	}
	return errors.Join(errs...)
}

func installSupervisorLaunchd(data *supervisorServiceData, stdout, stderr io.Writer) int {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: rendering plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	path := supervisorLaunchdPlistPath()
	legacyPresent := legacySupervisorTargetsCurrentHome(legacySupervisorLaunchdPlistPath())
	existing, err := os.ReadFile(path)
	hadCurrent := err == nil
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeSupervisorServiceFile(path, []byte(content)); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(false); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = supervisorLaunchctlRun("unload", path)
	if err := loadAndStartSupervisorLaunchd(path, data.LaunchdLabel); err != nil {
		var rollbackErr error
		if hadCurrent {
			rollbackErr = restorePreviousSupervisorLaunchdInstall(path, existing)
		} else {
			rollbackErr = rollbackNewSupervisorLaunchdInstall(path, legacyPresent)
		}
		if rollbackErr != nil {
			fmt.Fprintf(stderr, "gc supervisor install: rollback after launchctl failure: %v\n", rollbackErr) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintf(stderr, "gc supervisor install: launchctl %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: warning: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	fmt.Fprintf(stdout, "Installed launchd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func uninstallSupervisorLaunchd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorLaunchdPlistPath()
	_ = supervisorLaunchctlRun("unload", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing plist: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorLaunchd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
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
	service := supervisorSystemdServiceName()
	legacyPresent := legacySupervisorTargetsCurrentHome(legacySupervisorSystemdServicePath())
	existing, err := os.ReadFile(path)
	hadCurrent := err == nil
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor install: reading existing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	contentChanged := string(existing) != content
	if err := writeSupervisorServiceFile(path, []byte(content)); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: writing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", service},
	} {
		if err := supervisorSystemctlRun(args...); err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, false)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, false)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(args, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := unloadLegacySupervisorSystemd(false); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if contentChanged && supervisorSystemctlActive(service) {
		args := []string{"--user", "restart", service}
		if err := supervisorSystemctlRun(args...); err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, true)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, legacyPresent)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(args, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if !supervisorSystemctlActive(service) {
		args := []string{"--user", "start", service}
		if err := supervisorSystemctlRun(args...); err != nil {
			var rollbackErr error
			if hadCurrent {
				rollbackErr = restorePreviousSupervisorSystemdInstall(path, service, existing, true)
			} else {
				rollbackErr = rollbackNewSupervisorSystemdInstall(path, service, legacyPresent)
			}
			if rollbackErr != nil {
				fmt.Fprintf(stderr, "gc supervisor install: rollback after systemctl %s failure: %v\n", strings.Join(args, " "), rollbackErr) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintf(stderr, "gc supervisor install: systemctl %s: %v\n", strings.Join(args, " "), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := unloadLegacySupervisorSystemd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor install: warning: %v\n", err) //nolint:errcheck // best-effort stderr
	} else {
		_ = supervisorSystemctlRun("--user", "daemon-reload")
	}

	fmt.Fprintf(stdout, "Installed systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}

func uninstallSupervisorSystemd(_ *supervisorServiceData, stdout, stderr io.Writer) int {
	path := supervisorSystemdServicePath()
	service := supervisorSystemdServiceName()
	_ = supervisorSystemctlRun("--user", "stop", service)
	_ = supervisorSystemctlRun("--user", "disable", service)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc supervisor uninstall: removing unit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := unloadLegacySupervisorSystemd(true); err != nil {
		fmt.Fprintf(stderr, "gc supervisor uninstall: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	_ = supervisorSystemctlRun("--user", "daemon-reload")
	fmt.Fprintf(stdout, "Uninstalled systemd service: %s\n", path) //nolint:errcheck // best-effort stdout
	return 0
}
