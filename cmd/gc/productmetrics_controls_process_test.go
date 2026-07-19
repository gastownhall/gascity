//go:build (linux && !android) || (darwin && !ios)

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
	"github.com/santhosh-tekuri/jsonschema/v6"
)

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

func TestProductMetricsJSONSchemasEnforceClosedDomains(t *testing.T) {
	statusPayload, err := json.Marshal(productMetricsStatusForJSON(productMetricsStatus{
		State:                      productmetrics.StateEnabled,
		Reason:                     productmetrics.ReasonEnabled,
		HomeStable:                 true,
		StateSchema:                1,
		RequiredNoticeVersion:      1,
		AcceptedNoticeVersion:      1,
		QueueDiagnosticsAvailable:  true,
		StatusDiagnosticsAvailable: true,
		LastUploadAttemptHourUTC:   "2026-07-12T20:00:00Z",
		LastUploadSuccessHourUTC:   "2026-07-12T19:00:00Z",
		LastErrorClass:             productmetrics.DiagnosticErrorServer5xx,
	}, productMetricsPolicyMetadata{
		EndpointHostname:         "metrics.gascity.example",
		PrivacyURL:               "https://gascity.example/privacy/command-usage",
		EdgeLogRetentionDays:     7,
		RawEventRetentionDays:    90,
		AggregateRetentionMonths: 13,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductMetricsJSONSchemaE([]string{"metrics", "status"}, statusPayload); err != nil {
		t.Fatalf("valid metrics status rejected: %v\n%s", err, statusPayload)
	}
	statusMutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "reason", old: `"reason":"enabled"`, new: `"reason":"private-path"`},
		{name: "upload hour", old: `"last_upload_attempt_hour_utc":"2026-07-12T20:00:00Z"`, new: `"last_upload_attempt_hour_utc":"2026-07-12T20:15:00Z"`},
		{name: "error class", old: `"last_error_class":"server-5xx"`, new: `"last_error_class":"private-path"`},
		{name: "retention", old: `"edge_log_days":7`, new: `"edge_log_days":8`},
		{name: "privacy URL", old: `"privacy_url":"https://gascity.example/privacy/command-usage"`, new: `"privacy_url":"not a URI"`},
		{name: "independence", old: `"independence":"` + productMetricsIndependenceText + `"`, new: `"independence":"coupled"`},
	}
	for _, mutation := range statusMutations {
		t.Run("status "+mutation.name, func(t *testing.T) {
			assertProductMetricsSchemaRejectsMutation(t, []string{"metrics", "status"}, statusPayload, mutation.old, mutation.new)
		})
	}

	examplePayload, err := productmetrics.EncodeBatch(productmetrics.ExampleBatch())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductMetricsJSONSchemaE([]string{"metrics", "example"}, examplePayload); err != nil {
		t.Fatalf("valid metrics example rejected: %v\n%s", err, examplePayload)
	}
	exampleMutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "UUID version", old: `"event_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"`, new: `"event_id":"8c4f4128-a6e8-5f66-bd1b-1fcf1298b124"`},
		{name: "release version", old: `"release_version":"0.31.0"`, new: `"release_version":"v0.31.0"`},
		{name: "occurred hour", old: `"occurred_hour_utc":"2026-07-11T00:00:00Z"`, new: `"occurred_hour_utc":"2026-07-11T00:30:00Z"`},
		{name: "command ID", old: `"command_id":"help"`, new: `"command_id":"private-command"`},
	}
	for _, mutation := range exampleMutations {
		t.Run("example "+mutation.name, func(t *testing.T) {
			assertProductMetricsSchemaRejectsMutation(t, []string{"metrics", "example"}, examplePayload, mutation.old, mutation.new)
		})
	}
}

func assertProductMetricsSchemaRejectsMutation(t *testing.T, command []string, valid []byte, old, replacement string) {
	t.Helper()
	mutated := bytes.Replace(valid, []byte(old), []byte(replacement), 1)
	if bytes.Equal(mutated, valid) {
		t.Fatalf("schema mutation did not match %q in %s", old, valid)
	}
	if err := validateProductMetricsJSONSchemaE(command, mutated); err == nil {
		t.Fatalf("schema for %v accepted mutation %q -> %q:\n%s", command, old, replacement, mutated)
	}
}

func validateProductMetricsJSONSchemaE(command []string, data []byte) error {
	rawSchema, err := readBuiltinSchema(command, jsonSchemaResultRole)
	if err != nil {
		return err
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schemaURL := strings.Join(command, "/") + "/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return err
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return err
	}
	return compiled.Validate(instance)
}

func TestProductMetricsTaggedBinaryProcessContracts(t *testing.T) {
	skipSlowCmdGCTest(t, "builds and executes a tagged gc binary")
	configureProductMetricsTrustedProcessTempRoot(t)

	taggedBinary := filepath.Join(t.TempDir(), "gc-productmetrics-tagged")
	buildGCBinaryForProductMetricsTest(t, taggedBinary, "productmetrics_testhook")

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
