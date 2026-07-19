package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/gastownhall/gascity/internal/testutil"
)

const (
	productMetricsTesthookEndpointEnvironment  = "GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT"
	productMetricsTesthookCAFileEnvironment    = "GC_PRODUCT_METRICS_TESTHOOK_CA_FILE"
	productMetricsTestReleaseVersion           = "0.31.0"
	productMetricsTestInstallationID           = "3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21"
	productMetricsTestSpoolGeneration          = "22222222-2222-4222-8222-222222222222"
	productMetricsTestEventID                  = "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"
	productMetricsTestRecordHelpCommandFixture = "__testhook-record-help"
)

type capturedProductMetricsRequest struct {
	method             string
	path               string
	contentType        string
	accept             string
	userAgent          string
	acceptEncoding     string
	authorization      string
	cookie             string
	proxyAuthorization string
	batch              productmetrics.Batch
	err                error
}

func runProductMetricsPrivateUploaderProcessContract(t *testing.T, taggedBinary string) {
	t.Helper()
	requests := make(chan capturedProductMetricsRequest, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 65*1024))
		batch, decodeErr := productmetrics.DecodeBatch(body)
		requests <- capturedProductMetricsRequest{
			method:             request.Method,
			path:               request.URL.Path,
			contentType:        request.Header.Get("Content-Type"),
			accept:             request.Header.Get("Accept"),
			userAgent:          request.Header.Get("User-Agent"),
			acceptEncoding:     request.Header.Get("Accept-Encoding"),
			authorization:      request.Header.Get("Authorization"),
			cookie:             request.Header.Get("Cookie"),
			proxyAuthorization: request.Header.Get("Proxy-Authorization"),
			batch:              batch,
			err:                errors.Join(readErr, decodeErr),
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer,
			`{"schema_version":1,"app":"gascity","action":"accepted","event_ids":[%q]}`,
			productMetricsTestEventID,
		)
	}))
	t.Cleanup(server.Close)
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "loopback-ca.pem")
	if err := os.WriteFile(caFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}

	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "city.toml"), []byte("invalid = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	privateHome := t.TempDir()
	attemptToken := "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	queuedEvent := seedPrivateUploaderProcessFixture(t, privateHome, attemptToken, time.Now().UTC())
	baseEnvironment := []string{
		"GC_HOME=" + privateHome,
		"GC_OTEL_METRICS_URL=://invalid-private-uploader-test-url",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"SSL_CERT_FILE=/does/not/exist",
		productMetricsTesthookEndpointEnvironment + "=" + server.URL + "/v1/command-usage",
		productMetricsTesthookCAFileEnvironment + "=" + caFile,
		"HOME=" + t.TempDir(),
		"LANG=C",
	}
	t.Run("missing marker cannot read tagged CA", func(t *testing.T) {
		mkfifo, err := exec.LookPath("mkfifo")
		if err != nil {
			t.Skip("mkfifo is unavailable")
		}
		blockedCA := filepath.Join(t.TempDir(), "blocked-ca.pem")
		if output, err := exec.Command(mkfifo, blockedCA).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo: %v\n%s", err, output)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer cancel()
		missingMarker := exec.CommandContext(ctx, taggedBinary,
			productMetricsPrivateUploaderSentinelFixture,
			attemptToken,
		)
		missingMarker.Dir = workingDir
		missingMarker.Env = replaceProductMetricsProcessEnvironment(
			baseEnvironment,
			productMetricsTesthookCAFileEnvironment,
			blockedCA,
		)
		output, err := missingMarker.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatal("missing-marker child tried to open the blocking tagged CA path")
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
			t.Fatalf("missing-marker child error = %v, want nonzero exit", err)
		}
		if len(output) != 0 {
			t.Fatalf("missing-marker child wrote normal output: %q", output)
		}
		select {
		case request := <-requests:
			t.Fatalf("missing-marker child reached injected transport: %#v", request)
		default:
		}
	})

	valid := exec.Command(taggedBinary,
		productMetricsPrivateUploaderSentinelFixture,
		attemptToken,
	)
	valid.Dir = workingDir
	valid.Env = slices.Clone(baseEnvironment)
	valid.Env = append(valid.Env, "GC_PRODUCT_METRICS_PRIVATE_UPLOADER=1")
	if output, err := valid.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("valid private child = %v, output %q; want silent success", err, output)
	}
	var captured capturedProductMetricsRequest
	select {
	case captured = <-requests:
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("tagged private child made no injected upload request")
	}
	if captured.err != nil || captured.method != http.MethodPost || captured.path != "/v1/command-usage" ||
		captured.contentType != "application/json" || captured.accept != "application/json" ||
		captured.userAgent != "gascity-product-metrics/1" || captured.acceptEncoding != "" ||
		captured.authorization != "" || captured.cookie != "" || captured.proxyAuthorization != "" ||
		len(captured.batch.Events) != 1 || captured.batch.Events[0] != queuedEvent {
		t.Fatalf("captured upload = %#v", captured)
	}
	queuedPath := filepath.Join(privateHome, "product-usage", "queue", productMetricsTestSpoolGeneration, productMetricsTestEventID+".json")
	if _, err := os.Stat(queuedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted event still queued: %v", err)
	}
	replacementToken := "123e4567-e89b-42d3-a456-426614174000"
	if err := os.WriteFile(filepath.Join(privateHome, "product-usage", "spawn-throttle"), []byte(fmt.Sprintf(
		"throttle_schema = 1\nattempt_token = %q\nattempted_at = %q\n",
		replacementToken, time.Now().UTC().Format(time.RFC3339Nano),
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := exec.Command(taggedBinary, productMetricsPrivateUploaderSentinelFixture, attemptToken)
	stale.Dir = workingDir
	stale.Env = slices.Clone(baseEnvironment)
	stale.Env = append(stale.Env, "GC_PRODUCT_METRICS_PRIVATE_UPLOADER=1")
	if output, err := stale.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("stale private child = %v, output %q; want silent success", err, output)
	}
	select {
	case extra := <-requests:
		t.Fatalf("stale private child reached injected transport: %#v", extra)
	default:
	}

	malformed := exec.Command(taggedBinary, productMetricsPrivateUploaderSentinelFixture, "not-a-uuid", "version")
	malformed.Dir = workingDir
	malformed.Env = baseEnvironment
	output, err := malformed.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("malformed private child error = %v, want nonzero exit", err)
	}
	if len(output) != 0 {
		t.Fatalf("malformed private child reached normal output: %q", output)
	}
	select {
	case extra := <-requests:
		t.Fatalf("malformed private child reached injected transport: %#v", extra)
	default:
	}
}

func configureProductMetricsTrustedProcessTempRoot(t *testing.T) {
	t.Helper()
	trustedTempRoot := "/tmp"
	if runtime.GOOS == "darwin" {
		trustedTempRoot = "/private/tmp"
	}
	// Go 1.26's testing.T.TempDir prefers GOTMPDIR over TMPDIR. Product
	// metrics deliberately reject a user-owned writable ancestor, so keep
	// these process trust-boundary fixtures below the root-owned sticky
	// directory even when repository build scratch lives below /data.
	t.Setenv("GOTMPDIR", trustedTempRoot)
	t.Setenv("TMPDIR", trustedTempRoot)
}

func TestProductMetricsNormalBinaryContainsNoTesthookSymbols(t *testing.T) {
	skipSlowCmdGCTest(t, "builds and scans a normal gc binary")
	buildDir := t.TempDir()
	normalBinary := filepath.Join(buildDir, "gc-productmetrics-normal")
	buildGCBinaryForProductMetricsTest(t, normalBinary, "")

	command := exec.Command("go", "tool", "nm", normalBinary)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm normal gc: %v\n%s", err, output)
	}
	for _, forbidden := range []string{
		"main.runProductMetricsTesthookChild",
		"main.newProductMetricsTesthookRecordHelpCommand",
		"internal/productmetrics.OpenTesthook",
		"internal/productmetrics.testhookLoopbackHost",
	} {
		if strings.Contains(string(output), forbidden) {
			t.Fatalf("normal gc binary contains product-metrics testhook symbol %q", forbidden)
		}
	}
	binary, err := os.ReadFile(normalBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		productMetricsTesthookEndpointEnvironment,
		productMetricsTesthookCAFileEnvironment,
		productMetricsTestRecordHelpCommandFixture,
	} {
		if bytes.Contains(binary, []byte(forbidden)) {
			t.Fatalf("normal gc binary contains tag-only literal %q", forbidden)
		}
	}
	help := exec.Command(normalBinary, "metrics", "--help")
	helpOutput, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("normal gc metrics --help: %v\n%s", err, helpOutput)
	}
	if bytes.Contains(helpOutput, []byte(productMetricsTestRecordHelpCommandFixture)) {
		t.Fatalf("normal gc metrics help exposes tagged command:\n%s", helpOutput)
	}

	normalFiles := goListProductMetricsFiles(t, "")
	taggedFiles := goListProductMetricsFiles(t, "productmetrics_testhook")
	if strings.Contains(normalFiles, "productmetrics_testhook.go") {
		t.Fatalf("normal go file set contains tagged adapter:\n%s", normalFiles)
	}
	if strings.Contains(normalFiles, "productmetrics_controls_testhook.go") {
		t.Fatalf("normal go file set contains tagged control registrar:\n%s", normalFiles)
	}
	if !strings.Contains(normalFiles, "productmetrics_controls_production.go") {
		t.Fatalf("normal go file set omits production control registrar:\n%s", normalFiles)
	}
	if !strings.Contains(taggedFiles, "productmetrics_controls_testhook.go") || strings.Contains(taggedFiles, "productmetrics_controls_production.go") {
		t.Fatalf("tagged go file set selected the wrong control registrar:\n%s", taggedFiles)
	}
	if count := strings.Count(taggedFiles, "productmetrics_testhook.go"); count != 2 {
		t.Fatalf("tagged go file set contains %d testhook adapters, want cmd and internal:\n%s", count, taggedFiles)
	}
}

func buildGCBinaryForProductMetricsTest(t *testing.T, destination, tags string) {
	t.Helper()
	args := []string{"build", "-o", destination}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")
	command := exec.Command("go", args...)
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func goListProductMetricsFiles(t *testing.T, tags string) string {
	t.Helper()
	args := []string{"list", "-f", `{{.ImportPath}} {{join .GoFiles ","}}`}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".", "../../internal/productmetrics")
	command := exec.Command("go", args...)
	command.Dir = "."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func replaceProductMetricsProcessEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			replaced = append(replaced, entry)
		}
	}
	return append(replaced, prefix+value)
}

func seedPrivateUploaderProcessFixture(t *testing.T, home, attemptToken string, now time.Time) productmetrics.Event {
	t.Helper()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatalf("make product-metrics test home private: %v", err)
	}
	root := filepath.Join(home, "product-usage")
	queue := filepath.Join(root, "queue", productMetricsTestSpoolGeneration)
	if err := os.MkdirAll(queue, 0o700); err != nil {
		t.Fatal(err)
	}
	event := productmetrics.Event{
		EventID:         productMetricsTestEventID,
		InstallationID:  productMetricsTestInstallationID,
		App:             productmetrics.AppGasCity,
		ReleaseVersion:  productMetricsTestReleaseVersion,
		OS:              productmetrics.OperatingSystem(runtime.GOOS),
		OccurredHourUTC: now.UTC().Truncate(time.Hour).Format(time.RFC3339),
		CommandID:       productmetrics.CommandHelp,
	}
	eventBytes, err := productmetrics.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		filepath.Join(root, "config.toml"): []byte(fmt.Sprintf(
			"state_schema = 1\ncounter_namespace = 1\nstate_generation = 1\npreference = \"enabled\"\n"+
				"required_notice_version = 1\naccepted_notice_version = 1\ninstallation_id = %q\n"+
				"spool_generation = %q\ncleanup_kind = \"none\"\ncleanup_epoch = 0\npaused_through_metrics_epoch = 0\n",
			productMetricsTestInstallationID, productMetricsTestSpoolGeneration,
		)),
		filepath.Join(root, "quota.toml"): []byte(fmt.Sprintf(
			"quota_schema = 1\nreserved_events = 1\nreserved_bytes = %d\n", len(eventBytes),
		)),
		filepath.Join(root, "spawn-throttle"): []byte(fmt.Sprintf(
			"throttle_schema = 1\nattempt_token = %q\nattempted_at = %q\n",
			attemptToken, now.UTC().Format(time.RFC3339Nano),
		)),
		filepath.Join(queue, productMetricsTestEventID+".json"): eventBytes,
	}
	for path, contents := range files {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write product-metrics process fixture %s: %v", path, err)
		}
	}
	return event
}
