package productmetrics

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	maxEventFileBytes      = 4 * 1024
	maxUploadRequestBytes  = 64 * 1024
	maxUploadResponseBytes = 4 * 1024
	uploadUserAgent        = "gascity-product-metrics/1"

	uploadConnectTimeout        = 2 * time.Second
	uploadTLSHandshakeTimeout   = 3 * time.Second
	uploadResponseHeaderTimeout = 3 * time.Second
	uploadTotalTimeout          = 5 * time.Second
)

var errUploadRedirect = errors.New("productmetrics: upload redirect rejected")

// claimedEventFile is the immutable input boundary between the S4 spool and
// the upload codec. name is the canonical event-ID filename; body is the exact
// queue-file content.
type claimedEventFile struct {
	name string
	body []byte
}

// uploadBatchIdentity pins queue events to the state permit which authorized
// their claim. Metrics epoch is deliberately not part of the event DTO and is
// supplied separately when a response is verified.
type uploadBatchIdentity struct {
	installationID string
	releaseVersion string
}

// preparedUploadBatch is safe to retain after the state lock is released. Its
// slices never alias caller-owned claim buffers.
type preparedUploadBatch struct {
	body           []byte
	eventIDs       []string
	installationID string
	releaseVersion string
}

type uploadResponseKind uint8

const (
	uploadResponseRetry uploadResponseKind = iota
	uploadResponseAccepted
	uploadResponseDuplicate
	uploadResponsePause
)

type uploadResponse struct {
	kind            uploadResponseKind
	statusCode      int
	pause           verifiedPause
	diagnosticError DiagnosticErrorClass
}

type uploadTransport struct {
	endpoint         *url.URL
	client           *http.Client
	pauseKeys        pausePublicKeyCatalog
	productionPolicy bool
	roundTripGate    *roundTripStartGate
}

type uploadRequestDependencies struct {
	endpoint  string
	client    *http.Client
	pauseKeys pausePublicKeySet
}

func buildUploadBatch(claims []claimedEventFile, identity uploadBatchIdentity) (preparedUploadBatch, error) {
	if !validCanonicalUUIDv4(identity.installationID) {
		return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload identity has an invalid installation ID")
	}
	if !validPauseReleaseVersion(identity.releaseVersion) {
		return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload identity has an invalid release version")
	}
	if len(claims) == 0 || len(claims) > MaxBatchEvents {
		return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim count must be between 1 and %d", MaxBatchEvents)
	}

	events := make([]Event, 0, len(claims))
	eventIDs := make([]string, 0, len(claims))
	seen := make(map[string]struct{}, len(claims))
	for i, claim := range claims {
		if !validCanonicalUUIDv4(claim.name) {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d has an invalid event-ID filename", i)
		}
		if len(claim.body) == 0 || len(claim.body) > maxEventFileBytes {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d exceeds the event-file boundary", i)
		}
		event, err := DecodeEvent(claim.body)
		if err != nil {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d has invalid event JSON", i)
		}
		canonical, err := EncodeEvent(event)
		if err != nil {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d cannot be encoded", i)
		}
		if !bytes.Equal(canonical, claim.body) {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d is not canonical", i)
		}
		if event.EventID != claim.name {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d filename does not match its event ID", i)
		}
		if event.InstallationID != identity.installationID || event.ReleaseVersion != identity.releaseVersion {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim %d does not match its state permit", i)
		}
		if _, exists := seen[event.EventID]; exists {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload claim contains duplicate event ID")
		}
		seen[event.EventID] = struct{}{}
		events = append(events, event)
		eventIDs = append(eventIDs, event.EventID)
	}

	body, err := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: events})
	if err != nil {
		return preparedUploadBatch{}, fmt.Errorf("productmetrics: encode upload batch: %w", err)
	}
	if len(body) > maxUploadRequestBytes {
		return preparedUploadBatch{}, fmt.Errorf("productmetrics: upload request exceeds %d bytes", maxUploadRequestBytes)
	}
	return preparedUploadBatch{
		body:           append([]byte(nil), body...),
		eventIDs:       append([]string(nil), eventIDs...),
		installationID: identity.installationID,
		releaseVersion: identity.releaseVersion,
	}, nil
}

func newProductionUploadTransport(identity ReleaseIdentity) (*uploadTransport, error) {
	compiledIdentity := CurrentReleaseIdentity()
	if identity != compiledIdentity {
		return nil, fmt.Errorf("productmetrics: release identity does not match this artifact")
	}
	// S1 deliberately defines no official BuildKind. Even a same-package test
	// literal with plausible endpoint/version/epoch material remains inert. R2
	// must add an attested official kind and explicitly open BuildKind.String.
	if compiledIdentity.BuildKind() == BuildDevelopment {
		return nil, fmt.Errorf("productmetrics: development release identity cannot upload")
	}
	if compiledIdentity.BuildKind().String() == "unknown" {
		return nil, fmt.Errorf("productmetrics: unknown release identity cannot upload")
	}
	if !validPauseReleaseVersion(compiledIdentity.ReleaseVersion()) || !validMetricsEpoch(compiledIdentity.MetricsEpoch()) {
		return nil, fmt.Errorf("productmetrics: release identity cannot upload")
	}
	_, err := parseProductionUploadEndpoint(compiledIdentity.Endpoint())
	if err != nil {
		return nil, err
	}
	approvedPauseKeys, err := indexPausePublicKeyCatalog(productionPausePublicKeyCatalog)
	if err != nil || len(approvedPauseKeys) == 0 {
		return nil, fmt.Errorf("productmetrics: release identity has no approved signed-pause key")
	}
	return &uploadTransport{productionPolicy: true}, nil
}

func parseProductionUploadEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("productmetrics: invalid compiled upload endpoint")
	}
	if raw == "" || strings.Contains(raw, "#") || strings.HasSuffix(endpoint.Host, ":") ||
		endpoint.Scheme != "https" || endpoint.Opaque != "" || endpoint.User != nil ||
		endpoint.Host == "" || endpoint.Hostname() == "" || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawFragment != "" || (endpoint.Port() != "" && endpoint.Port() != "443") {
		return nil, fmt.Errorf("productmetrics: invalid compiled upload endpoint")
	}
	if endpoint.Path != "" && endpoint.Path[0] != '/' {
		return nil, fmt.Errorf("productmetrics: invalid compiled upload endpoint")
	}
	return endpoint, nil
}

func newProductionHTTPTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: uploadConnectTimeout, KeepAlive: -1}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   uploadTLSHandshakeTimeout,
		ResponseHeaderTimeout: uploadResponseHeaderTimeout,
		DisableCompression:    true,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		IdleConnTimeout:       0,
		ExpectContinueTimeout: 0,
	}
}

func newStrictUploadHTTPClient(roundTripper http.RoundTripper) *http.Client {
	// Clone concrete transports so test trust roots remain test-owned while the
	// product-metrics policy still removes proxies, decompression, and reuse.
	// Production always supplies newProductionHTTPTransport above.
	if concrete, ok := roundTripper.(*http.Transport); ok && concrete != nil {
		cloned := concrete.Clone()
		cloned.Proxy = nil
		cloned.DisableCompression = true
		cloned.DisableKeepAlives = true
		roundTripper = cloned
	}
	return &http.Client{
		Transport:     roundTripper,
		Jar:           nil,
		Timeout:       uploadTotalTimeout,
		CheckRedirect: rejectUploadRedirect,
	}
}

func rejectUploadRedirect(*http.Request, []*http.Request) error {
	return errUploadRedirect
}

func newProductionUploadHTTPClient() (*http.Client, error) {
	// Unlike the test-only client helper, production does not clone a
	// caller-owned transport. The entire graph is fresh and private to this
	// request, so validation observes the pre-use standard-library defaults.
	client := &http.Client{
		Transport:     newProductionHTTPTransport(),
		Jar:           nil,
		Timeout:       uploadTotalTimeout,
		CheckRedirect: rejectUploadRedirect,
	}
	if err := validateProductionUploadHTTPClient(client); err != nil {
		return nil, err
	}
	return client, nil
}

func validateProductionUploadHTTPClient(client *http.Client) error {
	if client == nil || client == http.DefaultClient || client.Transport == nil ||
		client.Jar != nil || client.Timeout != uploadTotalTimeout || client.CheckRedirect == nil {
		return fmt.Errorf("productmetrics: production upload client policy is invalid")
	}
	redirectRequest := &http.Request{URL: &url.URL{Path: "/next"}}
	redirectHistory := []*http.Request{{URL: &url.URL{Path: "/v1"}}}
	if err := client.CheckRedirect(redirectRequest, redirectHistory); !errors.Is(err, errUploadRedirect) {
		return fmt.Errorf("productmetrics: production upload redirect policy is invalid")
	}

	httpTransport, ok := client.Transport.(*http.Transport)
	legacyDialConfigured := httpTransport != nil && httpTransport.Dial != nil       //nolint:staticcheck // Reject the deprecated direct-dial bypass.
	legacyTLSDialConfigured := httpTransport != nil && httpTransport.DialTLS != nil //nolint:staticcheck // Reject the deprecated verified-TLS bypass.
	if !ok || httpTransport == nil || httpTransport == http.DefaultTransport ||
		httpTransport.Proxy != nil || httpTransport.OnProxyConnectResponse != nil ||
		httpTransport.DialContext == nil || legacyDialConfigured ||
		legacyTLSDialConfigured || httpTransport.DialTLSContext != nil ||
		httpTransport.TLSHandshakeTimeout != uploadTLSHandshakeTimeout ||
		httpTransport.ResponseHeaderTimeout != uploadResponseHeaderTimeout ||
		!httpTransport.DisableCompression || !httpTransport.DisableKeepAlives ||
		httpTransport.MaxIdleConns != 0 || httpTransport.MaxIdleConnsPerHost != 0 ||
		httpTransport.MaxConnsPerHost != 0 || httpTransport.IdleConnTimeout != 0 ||
		httpTransport.ExpectContinueTimeout != 0 || httpTransport.TLSNextProto != nil ||
		httpTransport.ProxyConnectHeader != nil || httpTransport.GetProxyConnectHeader != nil ||
		httpTransport.MaxResponseHeaderBytes != 0 || httpTransport.WriteBufferSize != 0 ||
		httpTransport.ReadBufferSize != 0 || !httpTransport.ForceAttemptHTTP2 ||
		httpTransport.HTTP2 != nil || httpTransport.Protocols != nil {
		return fmt.Errorf("productmetrics: production upload transport policy is invalid")
	}
	if !isClosedProductionTLSConfig(httpTransport.TLSClientConfig) {
		return fmt.Errorf("productmetrics: production upload TLS policy is invalid")
	}
	return nil
}

func isClosedProductionTLSConfig(config *tls.Config) bool {
	if config == nil {
		return false
	}
	return config.Rand == nil && config.Time == nil &&
		config.Certificates == nil &&
		config.GetCertificate == nil && config.GetClientCertificate == nil && config.GetConfigForClient == nil &&
		config.VerifyPeerCertificate == nil && config.VerifyConnection == nil &&
		config.RootCAs == nil && len(config.NextProtos) == 0 && config.ServerName == "" &&
		config.ClientAuth == tls.NoClientCert && config.ClientCAs == nil && !config.InsecureSkipVerify &&
		config.CipherSuites == nil && !config.SessionTicketsDisabled && config.ClientSessionCache == nil &&
		config.UnwrapSession == nil && config.WrapSession == nil &&
		config.MinVersion == tls.VersionTLS12 && config.MaxVersion == 0 && config.CurvePreferences == nil &&
		!config.DynamicRecordSizingDisabled && config.Renegotiation == tls.RenegotiateNever && config.KeyLogWriter == nil &&
		config.EncryptedClientHelloConfigList == nil && config.EncryptedClientHelloRejectionVerify == nil &&
		config.GetEncryptedClientHelloKeys == nil && config.EncryptedClientHelloKeys == nil
}

func (transport *uploadTransport) upload(ctx context.Context, prepared preparedUploadBatch, metricsEpoch uint64) (uploadResponse, error) {
	prepared = clonePreparedUploadBatch(prepared)
	retry := uploadResponse{kind: uploadResponseRetry}
	if ctx == nil {
		return retry, fmt.Errorf("productmetrics: upload context is nil")
	}
	dependencies, err := transport.requestDependencies()
	if err != nil {
		return retry, err
	}
	if transport.roundTripGate != nil {
		client := *dependencies.client
		client.Transport = &roundTripEntryTransport{
			base: dependencies.client.Transport,
			gate: transport.roundTripGate,
		}
		dependencies.client = &client
	}
	if !validMetricsEpoch(metricsEpoch) {
		return retry, fmt.Errorf("productmetrics: upload metrics epoch is invalid")
	}
	if err := validatePreparedUploadBatch(prepared); err != nil {
		return retry, err
	}

	requestContext, cancel := context.WithTimeout(ctx, uploadTotalTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, dependencies.endpoint, bytes.NewReader(prepared.body))
	if err != nil {
		return retry, fmt.Errorf("productmetrics: construct upload request")
	}
	// A POST is not idempotent. Removing GetBody and closing the connection
	// makes automatic transport replay impossible even if retry behavior changes.
	request.GetBody = nil
	request.Close = true
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", uploadUserAgent)

	response, err := dependencies.client.Do(request)
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		retry.statusCode = statusCode
		if errors.Is(err, errUploadRedirect) {
			retry.diagnosticError = DiagnosticErrorInvalidResponse
			return retry, errUploadRedirect
		}
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
			retry.diagnosticError = DiagnosticErrorNetworkTimeout
		} else {
			retry.diagnosticError = DiagnosticErrorNetworkFailure
		}
		return retry, fmt.Errorf("productmetrics: upload request failed")
	}
	retry.statusCode = response.StatusCode
	body, err := readBoundedUploadResponse(response)
	if err != nil {
		retry.diagnosticError = DiagnosticErrorInvalidResponse
		return retry, err
	}

	switch {
	case response.StatusCode >= 200 && response.StatusCode <= 299, response.StatusCode == http.StatusConflict:
		if !exactJSONResponseHeaders(response.Header) {
			retry.diagnosticError = DiagnosticErrorInvalidResponse
			return retry, fmt.Errorf("productmetrics: acknowledgement response headers are invalid")
		}
		kind, err := decodeUploadAcknowledgement(response.StatusCode, response.Header.Get("Content-Type"), body, prepared.eventIDs)
		if err != nil {
			retry.diagnosticError = DiagnosticErrorInvalidResponse
			return retry, err
		}
		return uploadResponse{kind: kind, statusCode: response.StatusCode}, nil
	case response.StatusCode == http.StatusGone:
		if !exactJSONResponseHeaders(response.Header) {
			retry.diagnosticError = DiagnosticErrorInvalidResponse
			return retry, fmt.Errorf("productmetrics: signed pause response headers are invalid")
		}
		pause, err := verifySignedPauseWithKeySet(body, pauseExpectation{
			releaseVersion: prepared.releaseVersion,
			metricsEpoch:   metricsEpoch,
		}, dependencies.pauseKeys)
		if err != nil {
			retry.diagnosticError = DiagnosticErrorInvalidResponse
			return retry, err
		}
		return uploadResponse{kind: uploadResponsePause, statusCode: response.StatusCode, pause: pause}, nil
	case response.StatusCode >= 300 && response.StatusCode <= 399:
		retry.diagnosticError = DiagnosticErrorInvalidResponse
		return retry, errUploadRedirect
	default:
		return retry, nil
	}
}

type roundTripEntryTransport struct {
	base http.RoundTripper
	gate *roundTripStartGate
}

func (transport *roundTripEntryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.gate == nil || !transport.gate.enter() {
		return nil, errors.New("productmetrics: upload start was canceled before RoundTrip entry")
	}
	return transport.base.RoundTrip(request)
}

type roundTripStartGate struct {
	mu      sync.Mutex
	entered chan struct{}
	state   uint8
}

const (
	roundTripStartPending uint8 = iota
	roundTripStartEntered
	roundTripStartAborted
)

func newRoundTripStartGate() *roundTripStartGate {
	return &roundTripStartGate{entered: make(chan struct{})}
}

func (gate *roundTripStartGate) enter() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.state == roundTripStartAborted {
		return false
	}
	if gate.state == roundTripStartPending {
		gate.state = roundTripStartEntered
		close(gate.entered)
	}
	return true
}

func (gate *roundTripStartGate) abort() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.state != roundTripStartPending {
		return false
	}
	gate.state = roundTripStartAborted
	return true
}

func (gate *roundTripStartGate) didEnter() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.state == roundTripStartEntered
}

func clonePreparedUploadBatch(prepared preparedUploadBatch) preparedUploadBatch {
	return preparedUploadBatch{
		body:           append([]byte(nil), prepared.body...),
		eventIDs:       append([]string(nil), prepared.eventIDs...),
		installationID: strings.Clone(prepared.installationID),
		releaseVersion: strings.Clone(prepared.releaseVersion),
	}
}

func (transport *uploadTransport) validate() error {
	_, err := transport.requestDependencies()
	return err
}

func (transport *uploadTransport) requestDependencies() (uploadRequestDependencies, error) {
	if transport == nil {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload transport is incomplete")
	}
	if transport.productionPolicy {
		if transport.endpoint != nil || transport.client != nil || transport.pauseKeys != nil {
			return uploadRequestDependencies{}, fmt.Errorf("productmetrics: production upload forbids runtime dependency overrides")
		}
		// Go's Unix system-root loader honors these variables. The detached S7
		// child omits them; this earlier check also gives endpoint-empty builds a
		// direct, testable fail-closed production-policy boundary.
		if productionUploadCustomCAEnvironmentConfigured() {
			return uploadRequestDependencies{}, fmt.Errorf("productmetrics: custom CA environment disables upload")
		}
		identity := CurrentReleaseIdentity()
		if identity.BuildKind() == BuildDevelopment || identity.BuildKind().String() == "unknown" ||
			!validPauseReleaseVersion(identity.ReleaseVersion()) || !validMetricsEpoch(identity.MetricsEpoch()) {
			return uploadRequestDependencies{}, fmt.Errorf("productmetrics: production upload release identity is invalid")
		}
		parsedEndpoint, err := parseProductionUploadEndpoint(identity.Endpoint())
		if err != nil {
			return uploadRequestDependencies{}, fmt.Errorf("productmetrics: production upload endpoint policy is invalid")
		}
		approvedPauseKeys, err := indexPausePublicKeyCatalog(productionPausePublicKeyCatalog)
		if err != nil || len(approvedPauseKeys) == 0 {
			return uploadRequestDependencies{}, fmt.Errorf("productmetrics: production upload has no approved signed-pause key")
		}
		// Production request dependencies are rebuilt after validating the
		// immutable endpoint/key snapshot. No caller-owned Client, Transport,
		// redirect callback, dial hook, TLS config, or registered protocol can
		// survive into the request or be mutated between validation and Do.
		client, err := newProductionUploadHTTPClient()
		if err != nil {
			return uploadRequestDependencies{}, err
		}
		return uploadRequestDependencies{
			endpoint:  parsedEndpoint.String(),
			client:    client,
			pauseKeys: approvedPauseKeys,
		}, nil
	}

	if transport.endpoint == nil {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload transport is incomplete")
	}
	rawEndpoint := transport.endpoint.String()
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload endpoint is invalid")
	}
	if transport.client == nil || transport.client.Transport == nil {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload transport is incomplete")
	}
	if endpoint.Scheme != "https" || endpoint.Opaque != "" || endpoint.User != nil || endpoint.Host == "" || endpoint.Hostname() == "" ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload endpoint is invalid")
	}
	if transport.client.Jar != nil || transport.client.CheckRedirect == nil || transport.client.Timeout <= 0 || transport.client.Timeout > uploadTotalTimeout {
		return uploadRequestDependencies{}, fmt.Errorf("productmetrics: upload client policy is invalid")
	}
	pauseKeys, err := indexPausePublicKeyCatalog(transport.pauseKeys)
	if err != nil {
		return uploadRequestDependencies{}, err
	}
	return uploadRequestDependencies{
		endpoint:  endpoint.String(),
		client:    transport.client,
		pauseKeys: pauseKeys,
	}, nil
}

func productionUploadCustomCAEnvironmentConfigured() bool {
	return os.Getenv("SSL_CERT_FILE") != "" || os.Getenv("SSL_CERT_DIR") != ""
}

func validatePreparedUploadBatch(prepared preparedUploadBatch) error {
	if len(prepared.body) == 0 || len(prepared.body) > maxUploadRequestBytes {
		return fmt.Errorf("productmetrics: prepared upload body is empty or oversized")
	}
	if !validCanonicalUUIDv4(prepared.installationID) || !validPauseReleaseVersion(prepared.releaseVersion) {
		return fmt.Errorf("productmetrics: prepared upload identity is invalid")
	}
	if err := validateEventIDSet(prepared.eventIDs); err != nil {
		return fmt.Errorf("productmetrics: prepared upload event IDs are invalid: %w", err)
	}
	batch, err := DecodeBatch(prepared.body)
	if err != nil {
		return fmt.Errorf("productmetrics: prepared upload body is invalid")
	}
	canonical, err := EncodeBatch(batch)
	if err != nil || !bytes.Equal(canonical, prepared.body) {
		return fmt.Errorf("productmetrics: prepared upload body is not canonical")
	}
	if len(batch.Events) != len(prepared.eventIDs) {
		return fmt.Errorf("productmetrics: prepared upload event count mismatch")
	}
	for i, event := range batch.Events {
		if event.EventID != prepared.eventIDs[i] || event.InstallationID != prepared.installationID || event.ReleaseVersion != prepared.releaseVersion {
			return fmt.Errorf("productmetrics: prepared upload event %d does not match its permit", i)
		}
	}
	return nil
}

func exactJSONResponseHeaders(header http.Header) bool {
	contentTypes := header.Values("Content-Type")
	return len(contentTypes) == 1 && contentTypes[0] == "application/json" && len(header.Values("Content-Encoding")) == 0
}

func readBoundedUploadResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("productmetrics: upload response has no body")
	}
	if response.ContentLength > maxUploadResponseBytes {
		_ = response.Body.Close()
		return nil, fmt.Errorf("productmetrics: upload response exceeds %d bytes", maxUploadResponseBytes)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxUploadResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("productmetrics: read upload response failed")
	}
	if len(body) > maxUploadResponseBytes {
		return nil, fmt.Errorf("productmetrics: upload response exceeds %d bytes", maxUploadResponseBytes)
	}
	return body, nil
}

type acknowledgementWire struct {
	SchemaVersion int      `json:"schema_version"`
	App           string   `json:"app"`
	Action        string   `json:"action"`
	EventIDs      []string `json:"event_ids"`
}

func decodeUploadAcknowledgement(statusCode int, contentType string, body []byte, submitted []string) (uploadResponseKind, error) {
	if contentType != "application/json" {
		return uploadResponseRetry, fmt.Errorf("productmetrics: acknowledgement has an invalid content type")
	}
	if len(body) == 0 || len(body) > maxUploadResponseBytes {
		return uploadResponseRetry, fmt.Errorf("productmetrics: acknowledgement body is empty or oversized")
	}
	if err := validateEventIDSet(submitted); err != nil {
		return uploadResponseRetry, fmt.Errorf("productmetrics: invalid submitted event IDs: %w", err)
	}

	wantAction := ""
	var wantKind uploadResponseKind
	switch {
	case statusCode >= 200 && statusCode <= 299:
		wantAction = "accepted"
		wantKind = uploadResponseAccepted
	case statusCode == 409:
		wantAction = "duplicate"
		wantKind = uploadResponseDuplicate
	default:
		return uploadResponseRetry, fmt.Errorf("productmetrics: status %d cannot acknowledge an upload", statusCode)
	}

	var acknowledgement acknowledgementWire
	if err := strictUnmarshalObject(body, &acknowledgement, exactAcknowledgementField); err != nil {
		return uploadResponseRetry, fmt.Errorf("productmetrics: acknowledgement JSON is invalid")
	}
	if acknowledgement.SchemaVersion != SchemaVersionV1 || acknowledgement.App != AppGasCity || acknowledgement.Action != wantAction {
		return uploadResponseRetry, fmt.Errorf("productmetrics: acknowledgement contract mismatch")
	}
	if err := validateEventIDSet(acknowledgement.EventIDs); err != nil {
		return uploadResponseRetry, fmt.Errorf("productmetrics: invalid acknowledged event IDs: %w", err)
	}
	if !sameEventIDSet(acknowledgement.EventIDs, submitted) {
		return uploadResponseRetry, fmt.Errorf("productmetrics: acknowledgement event IDs do not match the submitted batch")
	}
	return wantKind, nil
}

func exactAcknowledgementField(field string) bool {
	switch field {
	case "schema_version", "app", "action", "event_ids":
		return true
	default:
		return false
	}
}

func validateEventIDSet(eventIDs []string) error {
	if len(eventIDs) == 0 || len(eventIDs) > MaxBatchEvents {
		return fmt.Errorf("event ID count must be between 1 and %d", MaxBatchEvents)
	}
	seen := make(map[string]struct{}, len(eventIDs))
	for _, eventID := range eventIDs {
		if !validCanonicalUUIDv4(eventID) {
			return fmt.Errorf("event ID is not a canonical UUIDv4")
		}
		if _, exists := seen[eventID]; exists {
			return fmt.Errorf("duplicate event ID")
		}
		seen[eventID] = struct{}{}
	}
	return nil
}

func sameEventIDSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[string]struct{}, len(right))
	for _, value := range right {
		want[value] = struct{}{}
	}
	for _, value := range left {
		if _, exists := want[value]; !exists {
			return false
		}
	}
	return true
}
