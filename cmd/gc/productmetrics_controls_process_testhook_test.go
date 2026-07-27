//go:build productmetrics_testhook && ((linux && !android) || (darwin && !ios))

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestProductMetricsTaggedBinaryProcessContracts(t *testing.T) {
	configureProductMetricsTrustedProcessTempRoot(t)
	taggedBinary := reexecGCTestBinaryForTests(t)

	t.Run("private uploader bypasses normal startup", func(t *testing.T) {
		runProductMetricsPrivateUploaderProcessContract(t, taggedBinary)
	})
	t.Run("control flow bypasses city and pack state", func(t *testing.T) {
		workingDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workingDir, "city.toml"), []byte("invalid = [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		holdProductMetricsPackCacheLock(t, home)
		runProductMetricsTaggedControlFlow(t, taggedBinary, workingDir, home)
	})
}

type productMetricsControlProcessStatus struct {
	State                  string `json:"state"`
	Reason                 string `json:"reason"`
	ConfigPath             string `json:"config_path"`
	EndpointHostname       string `json:"endpoint_hostname"`
	InstallationIDPresent  bool   `json:"installation_id_present"`
	SpoolGenerationPresent bool   `json:"spool_generation_present"`
	CleanupPending         bool   `json:"cleanup_pending"`
	Queue                  productMetricsControlProcessQueue
	Diagnostics            productMetricsControlProcessDiagnostics
}

type productMetricsControlProcessQueue struct {
	Events           uint64  `json:"events"`
	Bytes            uint64  `json:"bytes"`
	OldestAgeSeconds *uint64 `json:"oldest_age_seconds"`
}

type productMetricsControlProcessDiagnostics struct {
	Available                bool    `json:"available"`
	DroppedEvents            uint64  `json:"dropped_events"`
	LastUploadAttemptHourUTC *string `json:"last_upload_attempt_hour_utc"`
	LastUploadSuccessHourUTC *string `json:"last_upload_success_hour_utc"`
	LastErrorClass           *string `json:"last_error_class"`
	SpawnThrottleAgeSeconds  *uint64 `json:"spawn_throttle_age_seconds"`
}

type productMetricsControlProcessResult struct {
	stdout []byte
	stderr []byte
}

func runProductMetricsTaggedControlFlow(t *testing.T, binary, workingDir, home string) {
	t.Helper()
	const privacySentinel = "s10-private-ordinary-help-sentinel"
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatalf("make product-metrics control home private: %v", err)
	}

	var requests atomic.Uint64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "loopback-ca.pem")
	if err := os.WriteFile(caFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{
		"GC_HOME=" + home,
		"HOME=" + t.TempDir(),
		"LANG=C",
		productMetricsTesthookEndpointEnvironment + "=" + server.URL + "/v1/command-usage",
		productMetricsTesthookCAFileEnvironment + "=" + caFile,
		"S10_PRIVATE_SENTINEL=" + privacySentinel,
	}
	productUsageRoot := filepath.Join(home, "product-usage")

	initial := runProductMetricsControlProcess(t, binary, workingDir, environment, "metrics", "status", "--json")
	assertProductMetricsProcessOmits(t, privacySentinel, initial)
	if len(initial.stderr) != 0 {
		t.Fatalf("initial metrics status wrote stderr: %q", initial.stderr)
	}
	initialStatus := decodeProductMetricsControlProcessStatus(t, initial.stdout)
	if initialStatus.State != string(productmetrics.StatePendingNotice) || initialStatus.Reason != string(productmetrics.ReasonPreferenceUnset) ||
		initialStatus.InstallationIDPresent || initialStatus.Queue.Events != 0 || initialStatus.Queue.Bytes != 0 {
		t.Fatalf("initial metrics status = %#v", initialStatus)
	}
	if initialStatus.ConfigPath != filepath.Join(productUsageRoot, "config.toml") {
		t.Fatalf("initial config path = %q, want product-usage config", initialStatus.ConfigPath)
	}
	if _, err := os.Stat(productUsageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only status created product state: %v", err)
	}
	assertProductMetricsExampleProcess(t, binary, workingDir, environment, privacySentinel)
	if _, err := os.Stat(productUsageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("example created product state: %v", err)
	}

	rejected := runProductMetricsControlProcessExpectFailure(t, binary, workingDir, environment, "metrics", "on")
	assertProductMetricsProcessOmits(t, privacySentinel, rejected)
	if len(rejected.stdout) != 0 || bytes.Contains(rejected.stderr, []byte("Gas City product metrics test-only notice.")) ||
		!bytes.Contains(rejected.stderr, []byte("cannot enable while state is pending-notice (preference-unset)")) {
		t.Fatalf("non-TTY metrics on = stdout %q stderr %q, want bounded rejection without notice", rejected.stdout, rejected.stderr)
	}
	if _, err := os.Stat(productUsageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-TTY metrics on created product state: %v", err)
	}
	ordinaryWorkingDir := filepath.Join(t.TempDir(), privacySentinel)
	if err := os.MkdirAll(ordinaryWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	helpBaseline := runProductMetricsControlProcess(t, binary, ordinaryWorkingDir, environment, "help")
	assertProductMetricsProcessOmits(t, privacySentinel, helpBaseline)
	if len(helpBaseline.stdout) == 0 || len(helpBaseline.stderr) != 0 {
		t.Fatalf("pending ordinary help baseline = stdout %q stderr %q, want help and no metrics notice", helpBaseline.stdout, helpBaseline.stderr)
	}

	on := runProductMetricsControlProcessTTY(t, binary, workingDir, environment, "metrics", "on")
	assertProductMetricsProcessOmits(t, privacySentinel, on)
	if !bytes.Contains(on.stderr, []byte("Gas City product metrics test-only notice.")) {
		t.Fatalf("metrics on stderr = %q, want complete tagged notice", on.stderr)
	}
	installationID := readProductMetricsControlInstallationID(t, filepath.Join(productUsageRoot, "config.toml"))
	if installationID == "" {
		t.Fatal("metrics on created no installation ID")
	}
	assertProductMetricsProcessRedacted(t, on, installationID)

	recorded := runProductMetricsControlProcess(t, binary, ordinaryWorkingDir, environment, "help")
	assertProductMetricsProcessOmits(t, privacySentinel, recorded)
	if !bytes.Equal(recorded.stdout, helpBaseline.stdout) || !bytes.Equal(recorded.stderr, helpBaseline.stderr) {
		t.Fatalf("enabled ordinary help changed output:\nrecorded stdout=%q stderr=%q\nbaseline stdout=%q stderr=%q", recorded.stdout, recorded.stderr, helpBaseline.stdout, helpBaseline.stderr)
	}
	for _, stream := range [][]byte{helpBaseline.stdout, helpBaseline.stderr, recorded.stdout, recorded.stderr} {
		if bytes.Contains(stream, []byte(privacySentinel)) {
			t.Fatalf("ordinary help process stream leaked privacy sentinel %q: %q", privacySentinel, stream)
		}
	}

	enabled := runProductMetricsControlProcess(t, binary, workingDir, environment, "metrics", "status", "--json")
	assertProductMetricsProcessOmits(t, privacySentinel, enabled)
	assertProductMetricsProcessRedacted(t, enabled, installationID)
	if len(enabled.stderr) != 0 {
		t.Fatalf("enabled metrics status wrote stderr: %q", enabled.stderr)
	}
	enabledStatus := decodeProductMetricsControlProcessStatus(t, enabled.stdout)
	if enabledStatus.State != string(productmetrics.StateEnabled) || enabledStatus.Reason != string(productmetrics.ReasonEnabled) ||
		!enabledStatus.InstallationIDPresent || !enabledStatus.SpoolGenerationPresent || enabledStatus.CleanupPending ||
		enabledStatus.Queue.Events != 1 || enabledStatus.Queue.Bytes == 0 || enabledStatus.Queue.OldestAgeSeconds == nil {
		t.Fatalf("enabled metrics status = %#v", enabledStatus)
	}
	queuedEvents, rawQueuedEvents := readProductMetricsControlQueuedEvents(t, productUsageRoot, privacySentinel)
	if len(queuedEvents) != 1 || queuedEvents[0].CommandID != productmetrics.CommandHelp {
		t.Fatalf("ordinary help queued events = %+v, want exactly one help event", queuedEvents)
	}
	for _, raw := range rawQueuedEvents {
		if bytes.Contains(raw, []byte(privacySentinel)) {
			t.Fatalf("raw queued help event leaked privacy sentinel %q: %s", privacySentinel, raw)
		}
	}
	assertProductMetricsExampleProcess(t, binary, workingDir, environment, privacySentinel)

	off := runProductMetricsControlProcess(t, binary, workingDir, environment, "metrics", "off")
	assertProductMetricsProcessOmits(t, privacySentinel, off)
	assertProductMetricsProcessRedacted(t, off, installationID)
	if len(off.stderr) != 0 {
		t.Fatalf("successful metrics off wrote stderr: %q", off.stderr)
	}
	if !bytes.Contains(bytes.ToLower(off.stdout), []byte("disabled")) {
		t.Fatalf("metrics off stdout = %q, want disabled summary", off.stdout)
	}
	if !bytes.Contains(off.stdout, []byte("Removed 1 queued events")) {
		t.Fatalf("metrics off stdout = %q, want one purged ordinary-help event", off.stdout)
	}
	if queuedAfterOff, rawAfterOff := readProductMetricsControlQueuedEvents(t, productUsageRoot, privacySentinel); len(queuedAfterOff) != 0 || len(rawAfterOff) != 0 {
		t.Fatalf("metrics off retained queued events: decoded=%+v raw=%q", queuedAfterOff, rawAfterOff)
	}
	if got := readProductMetricsControlInstallationID(t, filepath.Join(productUsageRoot, "config.toml")); got != "" {
		t.Fatalf("metrics off retained installation ID %q", got)
	}

	disabled := runProductMetricsControlProcess(t, binary, workingDir, environment, "metrics", "status", "--json")
	assertProductMetricsProcessOmits(t, privacySentinel, disabled)
	assertProductMetricsProcessRedacted(t, disabled, installationID)
	if len(disabled.stderr) != 0 {
		t.Fatalf("disabled metrics status wrote stderr: %q", disabled.stderr)
	}
	disabledStatus := decodeProductMetricsControlProcessStatus(t, disabled.stdout)
	if disabledStatus.State != string(productmetrics.StateDisabled) || disabledStatus.Reason != string(productmetrics.ReasonPersistedDisabled) ||
		disabledStatus.InstallationIDPresent || disabledStatus.SpoolGenerationPresent || disabledStatus.CleanupPending ||
		disabledStatus.Queue.Events != 0 || disabledStatus.Queue.Bytes != 0 {
		t.Fatalf("disabled metrics status = %#v", disabledStatus)
	}
	assertProductMetricsExampleProcess(t, binary, workingDir, environment, privacySentinel)

	if got := requests.Load(); got != 0 {
		t.Fatalf("metrics control flow made %d HTTP requests, want zero", got)
	}
}

func runProductMetricsControlProcess(t *testing.T, binary, workingDir string, environment []string, args ...string) productMetricsControlProcessResult {
	t.Helper()
	result, err := runProductMetricsControlProcessRaw(t, binary, workingDir, environment, args...)
	if err != nil {
		t.Fatalf("gc %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, result.stdout, result.stderr)
	}
	return result
}

func runProductMetricsControlProcessExpectFailure(t *testing.T, binary, workingDir string, environment []string, args ...string) productMetricsControlProcessResult {
	t.Helper()
	result, err := runProductMetricsControlProcessRaw(t, binary, workingDir, environment, args...)
	if err == nil {
		t.Fatalf("gc %s succeeded, want nonzero exit\nstdout: %s\nstderr: %s", strings.Join(args, " "), result.stdout, result.stderr)
	}
	return result
}

func runProductMetricsControlProcessTTY(t *testing.T, binary, workingDir string, environment []string, args ...string) productMetricsControlProcessResult {
	t.Helper()
	terminalOutput, terminalWriter, err := openProductMetricsControlPTY()
	if err != nil {
		t.Fatalf("open product-metrics control PTY: %v", err)
	}
	defer func() { _ = terminalOutput.Close() }()
	defer func() { _ = terminalWriter.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workingDir
	command.Env = append([]string(nil), environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = terminalWriter
	if err := command.Start(); err != nil {
		t.Fatalf("start gc %s with PTY stderr: %v", strings.Join(args, " "), err)
	}
	if err := terminalWriter.Close(); err != nil {
		t.Fatalf("close parent PTY writer: %v", err)
	}
	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&stderr, terminalOutput)
		stderrDone <- copyErr
	}()
	err = command.Wait()
	copyErr := <-stderrDone
	if copyErr != nil && !errors.Is(copyErr, syscall.EIO) {
		t.Fatalf("read gc %s PTY stderr: %v", strings.Join(args, " "), copyErr)
	}
	if ctx.Err() != nil {
		t.Fatalf("gc %s exceeded process deadline with PTY stderr: %v", strings.Join(args, " "), ctx.Err())
	}
	result := productMetricsControlProcessResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if err != nil {
		t.Fatalf("gc %s with PTY stderr: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, result.stdout, result.stderr)
	}
	return result
}

func runProductMetricsControlProcessRaw(t *testing.T, binary, workingDir string, environment []string, args ...string) (productMetricsControlProcessResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workingDir
	command.Env = append([]string(nil), environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("gc %s exceeded process deadline (pack discovery may have blocked): %v", strings.Join(args, " "), ctx.Err())
	}
	return productMetricsControlProcessResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, err
}

func decodeProductMetricsControlProcessStatus(t *testing.T, data []byte) productMetricsControlProcessStatus {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode metrics status keys: %v\n%s", err, data)
	}
	if _, present := raw["installation_id"]; present {
		t.Fatalf("default metrics status contains raw installation_id field: %s", data)
	}
	var status productMetricsControlProcessStatus
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("decode metrics status: %v\n%s", err, data)
	}
	return status
}

func assertProductMetricsProcessRedacted(t *testing.T, result productMetricsControlProcessResult, installationID string) {
	t.Helper()
	if installationID != "" && (bytes.Contains(result.stdout, []byte(installationID)) || bytes.Contains(result.stderr, []byte(installationID))) {
		t.Fatalf("metrics command exposed raw installation ID %q: stdout=%q stderr=%q", installationID, result.stdout, result.stderr)
	}
}

func assertProductMetricsExampleProcess(t *testing.T, binary, workingDir string, environment []string, privacySentinel string) {
	t.Helper()
	want, err := productmetrics.EncodeBatch(productmetrics.ExampleBatch())
	if err != nil {
		t.Fatal(err)
	}
	result := runProductMetricsControlProcess(t, binary, workingDir, environment, "metrics", "example", "--json")
	assertProductMetricsProcessOmits(t, privacySentinel, result)
	if !bytes.Equal(result.stdout, want) || len(result.stderr) != 0 {
		t.Fatalf("metrics example --json = stdout %q stderr %q, want exact encoder bytes %q and empty stderr", result.stdout, result.stderr, want)
	}
}

func readProductMetricsControlInstallationID(t *testing.T, path string) string {
	t.Helper()
	var config struct {
		InstallationID string `toml:"installation_id"`
	}
	if _, err := toml.DecodeFile(path, &config); err != nil {
		t.Fatalf("decode product metrics config: %v", err)
	}
	return config.InstallationID
}

func readProductMetricsControlQueuedEvents(t *testing.T, productUsageRoot, privacySentinel string) ([]productmetrics.Event, [][]byte) {
	t.Helper()
	var events []productmetrics.Event
	var rawEvents [][]byte
	err := filepath.WalkDir(productUsageRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(privacySentinel)) {
			return fmt.Errorf("queued event contains privacy sentinel")
		}
		event, err := productmetrics.DecodeEvent(data)
		if err != nil {
			return err
		}
		events = append(events, event)
		rawEvents = append(rawEvents, append([]byte(nil), data...))
		return nil
	})
	if err != nil {
		t.Fatalf("read queued product-metrics events: %v", err)
	}
	return events, rawEvents
}

func assertProductMetricsProcessOmits(t *testing.T, privacySentinel string, results ...productMetricsControlProcessResult) {
	t.Helper()
	for _, result := range results {
		if bytes.Contains(result.stdout, []byte(privacySentinel)) || bytes.Contains(result.stderr, []byte(privacySentinel)) {
			t.Fatalf("product-metrics process stream leaked privacy sentinel %q: stdout=%q stderr=%q", privacySentinel, result.stdout, result.stderr)
		}
	}
}

func holdProductMetricsPackCacheLock(t *testing.T, home string) {
	t.Helper()
	cacheRoot := filepath.Join(home, "cache", "repos")
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(cacheRoot, ".packman-cache.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	})
}
