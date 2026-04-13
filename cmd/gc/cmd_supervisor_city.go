package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

var (
	supervisorCityReadyTimeout = 180 * time.Second
	supervisorCityPollInterval = 100 * time.Millisecond
)

// registerCityWithSupervisorTestHook lets tests intercept registration after
// the registry entry is written but before any real supervisor lifecycle runs.
// It is nil in production.
var registerCityWithSupervisorTestHook func(cityPath, commandName string, stdout, stderr io.Writer) (bool, int)

func supervisorCityStartTimeout(cityPath string) time.Duration {
	timeout := supervisorCityReadyTimeout
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return timeout
	}
	if startup := cfg.Session.StartupTimeoutDuration(); startup > timeout {
		timeout = startup
	}
	return timeout
}

func supervisorCityStopTimeout(cityPath string) time.Duration {
	timeout := supervisorCityReadyTimeout
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return timeout
	}
	if shutdown := cfg.Daemon.ShutdownTimeoutDuration() + 5*time.Second; shutdown > timeout {
		timeout = shutdown
	}
	return timeout
}

func fetchCityPacksIfNeeded(cityPath string) error {
	tomlPath := filepath.Join(cityPath, "city.toml")
	if quickCfg, qErr := config.Load(fsys.OSFS{}, tomlPath); qErr == nil && len(quickCfg.Packs) > 0 {
		if err := config.FetchPacks(quickCfg.Packs, cityPath); err != nil {
			return err
		}
	}
	return nil
}

func effectiveCityName(cityPath string) (string, error) {
	name := filepath.Base(cityPath)
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		return "", err
	}
	if cfg.Workspace.Name != "" {
		name = cfg.Workspace.Name
	}
	return name, nil
}

func registeredCityName(cityPath, nameOverride string) (string, error) {
	if alias := strings.TrimSpace(nameOverride); alias != "" {
		return alias, nil
	}
	return effectiveCityName(cityPath)
}

func normalizeRegisteredCityPath(cityPath string) (string, error) {
	abs, err := filepath.Abs(cityPath)
	if err != nil {
		return "", err
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	return abs, nil
}

func registeredCityEntry(cityPath string) (supervisor.CityEntry, bool, error) {
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.Path == normalized {
			return entry, true, nil
		}
	}
	return supervisor.CityEntry{}, false, nil
}

func cityUsesManagedReconciler(cityPath string) bool {
	if controllerAlive(cityPath) != 0 {
		return true
	}
	_, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered {
		return false
	}
	return supervisorAlive() != 0
}

func ensureNoStandaloneController(cityPath string) (int, error) {
	if pid := controllerAlive(cityPath); pid != 0 {
		return pid, errControllerAlreadyRunning
	}
	gcDir := filepath.Join(cityPath, ".gc")
	if fi, err := os.Stat(gcDir); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	} else if !fi.IsDir() {
		return 0, nil
	}
	lock, err := acquireControllerLock(cityPath)
	if err == nil {
		lock.Close() //nolint:errcheck // best-effort probe cleanup
		return 0, nil
	}
	if errors.Is(err, errControllerAlreadyRunning) {
		return 0, err
	}
	return 0, err
}

func registerCityWithSupervisor(cityPath string, stdout, stderr io.Writer, commandName string, showProgress bool) int {
	return registerCityWithSupervisorNamed(cityPath, "", stdout, stderr, commandName, showProgress)
}

func registerCityWithSupervisorNamed(cityPath, nameOverride string, stdout, stderr io.Writer, commandName string, showProgress bool) int {
	cityPath = normalizePathForCompare(cityPath)
	if pid, err := ensureNoStandaloneController(cityPath); err != nil {
		if errors.Is(err, errControllerAlreadyRunning) {
			if pid != 0 {
				fmt.Fprintf(stderr, "%s: standalone controller already running for %s (PID %d); stop it before registering with the supervisor\n", commandName, cityPath, pid) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "%s: standalone controller already running for %s; stop it before registering with the supervisor\n", commandName, cityPath) //nolint:errcheck // best-effort stderr
			}
		} else {
			fmt.Fprintf(stderr, "%s: probing standalone controller: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		}
		return 1
	}
	// Materialize gastown packs before config load if the city references them.
	// This must succeed — without packs, config.LoadWithIncludes will fail
	// with a confusing "pack.toml: no such file" error downstream.
	if quickCfg, qErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); qErr == nil && usesGastownPack(quickCfg) {
		if err := MaterializeGastownPacks(cityPath); err != nil {
			fmt.Fprintf(stderr, "%s: materializing gastown packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := fetchCityPacksIfNeeded(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: fetching packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	name, err := registeredCityName(cityPath, nameOverride)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Test hook: intercept before writing to the real registry so tests
	// don't pollute the production cities.toml.
	if registerCityWithSupervisorTestHook != nil {
		if handled, code := registerCityWithSupervisorTestHook(cityPath, commandName, stdout, stderr); handled {
			return code
		}
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, name); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	entry, _, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Registered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	if ensureSupervisorRunningHook(stdout, stderr) != 0 {
		keepRegisteredCity(entry, stderr, commandName, "supervisor did not start")
		return 1
	}
	if reloadSupervisorHook(io.Discard, io.Discard) != 0 {
		// The supervisor may be a zombie from a recent "gc supervisor stop" —
		// alive enough to accept connections but unable to process reload
		// because its main loop has exited. Poll for it to finish dying,
		// start a fresh supervisor, and retry.
		deadline := time.Now().Add(10 * time.Second)
		for supervisorAliveHook() != 0 && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if ensureSupervisorRunningHook(stdout, stderr) != 0 {
			keepRegisteredCity(entry, stderr, commandName, "supervisor did not start after retry")
			return 1
		}
		if reloadSupervisorHook(stdout, stderr) != 0 {
			keepRegisteredCity(entry, stderr, commandName, "reconcile failed")
			return 1
		}
	}
	if supervisorAliveHook() != 0 {
		if showProgress {
			logInitProgress(stdout, 8, "Waiting for supervisor to start city")
		} else if stdout != nil {
			fmt.Fprintln(stdout, "Waiting for supervisor to start city...") //nolint:errcheck // best-effort stdout
		}
		if err := waitForSupervisorCity(cityPath, true, supervisorCityStartTimeout(cityPath), stdout); err != nil {
			keepRegisteredCity(entry, stderr, commandName, err.Error())
			fmt.Fprintf(stderr, "%s: check 'gc supervisor logs' for details\n", commandName) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

func keepRegisteredCity(entry supervisor.CityEntry, stderr io.Writer, commandName, reason string) {
	fmt.Fprintf(stderr, "%s: %s; keeping registration for '%s' so the supervisor can retry automatically\n", //nolint:errcheck // best-effort stderr
		commandName, reason, entry.EffectiveName())
}

func waitForSupervisorCity(cityPath string, wantRunning bool, timeout time.Duration, stdout io.Writer) error {
	deadline := time.Now().Add(timeout)
	var lastStatus string
	for {
		running, status, known := supervisorCityRunningHook(cityPath)
		if known {
			if running == wantRunning {
				return nil
			}
			if !wantRunning {
				return fmt.Errorf("city is still running under supervisor")
			}
			// If the supervisor reports an init failure, surface the
			// error immediately instead of polling until timeout.
			if wantRunning && status == "init_failed" {
				if errMsg := supervisorCityError(cityPath); errMsg != "" {
					return fmt.Errorf("city failed to start: %s", errMsg)
				}
				return fmt.Errorf("city failed to start under supervisor")
			}
		} else if !wantRunning {
			return nil
		}
		if stdout != nil && status != "" && status != lastStatus {
			fmt.Fprintf(stdout, "  %s\n", statusDisplayText(status)) //nolint:errcheck // best-effort stdout
			lastStatus = status
		}
		if time.Now().After(deadline) {
			if wantRunning {
				return fmt.Errorf("city did not become ready under supervisor")
			}
			return fmt.Errorf("city did not stop under supervisor")
		}
		time.Sleep(supervisorCityPollInterval)
	}
}

// supervisorCityError fetches the error message for a city from the supervisor API.
func supervisorCityError(cityPath string) string {
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return ""
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return ""
	}
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return ""
	}
	for _, city := range cities {
		path, pathErr := normalizeRegisteredCityPath(city.Path)
		if pathErr == nil && path == normalized {
			return city.Error
		}
	}
	return ""
}

// statusDisplayText maps an init status string to a human-readable display line.
func statusDisplayText(status string) string {
	switch status {
	case "loading_config":
		return "Loading configuration..."
	case "starting_bead_store":
		return "Starting bead store..."
	case "resolving_formulas":
		return "Resolving formulas..."
	case "adopting_sessions":
		return "Adopting sessions..."
	case "starting_agents":
		return "Starting agents..."
	default:
		return status + "..."
	}
}

func unregisterCityFromSupervisor(cityPath string, stdout, stderr io.Writer, commandName string) (bool, int) {
	cityPath = normalizePathForCompare(cityPath)
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return false, 1
	}
	if !registered {
		return false, 0
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Unregister(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return true, 1
	}

	fmt.Fprintf(stdout, "Unregistered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	if supervisorAliveHook() != 0 {
		if reloadSupervisorHook(stdout, stderr) != 0 {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: reconcile failed and restore failed for '%s': %v\n", commandName, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: reconcile failed; restored registration for '%s'\n", commandName, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
		if err := waitForSupervisorCity(cityPath, false, supervisorCityReadyTimeout, nil); err != nil {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: %v; restore failed for '%s': %v\n", commandName, err, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: %v; restored registration for '%s'\n", commandName, err, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
		if err := waitForSupervisorControllerStopHook(cityPath, supervisorCityStopTimeout(cityPath)); err != nil {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: %v; restore failed for '%s': %v\n", commandName, err, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: %v; restored registration for '%s'\n", commandName, err, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
	}
	return true, 0
}

var waitForSupervisorControllerStopHook = waitForStandaloneControllerStop

func supervisorAPIBaseURL() (string, error) {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return "", err
	}
	bind := cfg.Supervisor.BindOrDefault()
	switch bind {
	case "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.Supervisor.PortOrDefault()))), nil
}

var supervisorCityRunningHook = supervisorCityRunning

func supervisorCityAPIClient(cityPath string) *api.Client {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered || supervisorAliveHook() == 0 {
		return nil
	}
	if running, _, known := supervisorCityRunningHook(cityPath); !known || !running {
		return nil
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return nil
	}
	return api.NewCityScopedClient(baseURL, entry.EffectiveName())
}

func supervisorCityRunning(cityPath string) (running bool, status string, known bool) {
	if supervisorAliveHook() == 0 {
		return false, "", false
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return false, "", false
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return false, "", false
	}
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return false, "", false
	}
	for _, city := range cities {
		path, pathErr := normalizeRegisteredCityPath(city.Path)
		if pathErr == nil && path == normalized {
			return city.Running, city.Status, true
		}
	}
	return false, "", false
}
