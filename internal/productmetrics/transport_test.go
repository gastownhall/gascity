package productmetrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func preparedFixedUpload(t *testing.T) preparedUploadBatch {
	t.Helper()
	prepared, err := buildUploadBatch([]claimedEventFile{{
		name: fixedEvent().EventID,
		body: encodedEventForUpload(t, fixedEvent()),
	}}, uploadBatchIdentity{
		installationID: fixedEvent().InstallationID,
		releaseVersion: fixedEvent().ReleaseVersion,
	})
	if err != nil {
		t.Fatalf("buildUploadBatch: %v", err)
	}
	return prepared
}

func strictTestUploadTransport(t *testing.T, rawURL string, roundTripper http.RoundTripper, catalog pausePublicKeyCatalog) *uploadTransport {
	t.Helper()
	endpoint, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return &uploadTransport{
		endpoint:  endpoint,
		client:    newStrictUploadHTTPClient(roundTripper),
		pauseKeys: catalog,
	}
}

func acceptedBody(eventIDs []string, action string) string {
	quoted := make([]string, len(eventIDs))
	for i, eventID := range eventIDs {
		quoted[i] = `"` + eventID + `"`
	}
	return `{"schema_version":1,"app":"gascity","action":"` + action + `","event_ids":[` + strings.Join(quoted, ",") + `]}`
}

func TestUploadTransportSendsExactOneShotRequest(t *testing.T) {
	prepared := preparedFixedUpload(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL.Path != "/v1/command-usage" || request.URL.RawQuery != "" {
			t.Errorf("request URL = %s", request.URL.String())
		}
		for header, want := range map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"User-Agent":   uploadUserAgent,
		} {
			if got := request.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		for _, header := range []string{"Authorization", "Cookie", "Accept-Encoding", "Proxy-Authorization"} {
			if got := request.Header.Get(header); got != "" {
				t.Errorf("forbidden %s = %q", header, got)
			}
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if !bytes.Equal(body, prepared.body) {
			t.Errorf("request body mismatch\n got: %s\nwant: %s", body, prepared.body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, acceptedBody(prepared.eventIDs, "accepted"))
	}))
	t.Cleanup(server.Close)

	transport := strictTestUploadTransport(t, server.URL+"/v1/command-usage", server.Client().Transport, productionPausePublicKeyCatalog)
	result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if result.kind != uploadResponseAccepted || result.statusCode != http.StatusOK {
		t.Fatalf("result = %#v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want exactly one", calls.Load())
	}
}

func TestUploadTransportSnapshotsPreparedBatchAtEntry(t *testing.T) {
	prepared := preparedFixedUpload(t)
	originalBody := append([]byte(nil), prepared.body...)
	originalEventID := prepared.eventIDs[0]
	replacementEventID := secondFixedEvent().EventID
	mutatedBody := bytes.ReplaceAll(originalBody, []byte(originalEventID), []byte(replacementEventID))
	if bytes.Equal(mutatedBody, originalBody) || len(mutatedBody) != len(originalBody) {
		t.Fatal("test mutation did not preserve a distinct same-size canonical body")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	capturedBody := make(chan []byte, 1)
	transport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		capturedBody <- body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(acceptedBody([]string{originalEventID}, "accepted"))),
			Request:    request,
		}, nil
	}), productionPausePublicKeyCatalog)

	type uploadOutcome struct {
		response uploadResponse
		err      error
	}
	outcome := make(chan uploadOutcome, 1)
	go func() {
		response, err := transport.upload(context.Background(), prepared, testPauseEpoch)
		outcome <- uploadOutcome{response: response, err: err}
	}()

	<-entered
	copy(prepared.body, mutatedBody)
	prepared.eventIDs[0] = replacementEventID
	close(release)

	if got := <-capturedBody; !bytes.Equal(got, originalBody) {
		t.Fatalf("request body was not bound to the entry snapshot\n got: %s\nwant: %s", got, originalBody)
	}
	got := <-outcome
	if got.err != nil || got.response.kind != uploadResponseAccepted {
		t.Fatalf("entry-snapshot acknowledgement = %#v, %v; want accepted", got.response, got.err)
	}
}

func TestUploadTransportSnapshotsPauseKeysBeforeNetwork(t *testing.T) {
	prepared := preparedFixedUpload(t)
	firstPublic, _ := deterministicPauseKey()
	secondSeed := bytes.Repeat([]byte{0xff}, ed25519.SeedSize)
	secondPrivate := ed25519.NewKeyFromSeed(secondSeed)
	secondPublic := secondPrivate.Public().(ed25519.PublicKey)

	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	useSecondKey := atomic.Bool{}
	catalog := func(yield func(pausePublicKeyEntry)) {
		key := firstPublic
		if useSecondKey.Load() {
			key = secondPublic
		}
		yield(pausePublicKeyEntry{id: testPauseKeyID, key: key})
	}
	transport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-releaseResponse
		return &http.Response{
			StatusCode: http.StatusGone,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(signedPauseEnvelope(
				prepared.releaseVersion,
				testPauseEpoch,
				testPauseKeyID,
				secondPrivate,
			))),
			Request: request,
		}, nil
	}), catalog)

	type uploadResult struct {
		response uploadResponse
		err      error
	}
	done := make(chan uploadResult, 1)
	go func() {
		response, err := transport.upload(context.Background(), prepared, testPauseEpoch)
		done <- uploadResult{response: response, err: err}
	}()
	<-requestStarted
	useSecondKey.Store(true)
	close(releaseResponse)

	result := <-done
	if result.response.kind != uploadResponseRetry || result.err == nil {
		t.Fatalf("post-snapshot pause-key substitution = %#v, %v; want retry error", result.response, result.err)
	}
}

func TestUploadTransportClassifiesDuplicateAndSignedPause(t *testing.T) {
	prepared := preparedFixedUpload(t)
	publicKey, privateKey := deterministicPauseKey()

	for _, test := range []struct {
		name       string
		status     int
		body       string
		catalog    pausePublicKeyCatalog
		wantKind   uploadResponseKind
		wantPaused bool
	}{
		{name: "duplicate", status: http.StatusConflict, body: acceptedBody(prepared.eventIDs, "duplicate"), catalog: testPauseCatalog(publicKey), wantKind: uploadResponseDuplicate},
		{name: "signed pause", status: http.StatusGone, body: signedPauseEnvelope(prepared.releaseVersion, testPauseEpoch, testPauseKeyID, privateKey), catalog: testPauseCatalog(publicKey), wantKind: uploadResponsePause, wantPaused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			transport := strictTestUploadTransport(t, server.URL, server.Client().Transport, test.catalog)

			result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			if result.kind != test.wantKind || result.statusCode != test.status {
				t.Fatalf("result = %#v", result)
			}
			if test.wantPaused && (result.pause.metricsEpoch != testPauseEpoch || result.pause.releaseVersion != prepared.releaseVersion) {
				t.Fatalf("pause result = %#v", result.pause)
			}
		})
	}
}

func TestUploadTransportRestoresOnEveryUntrustedResponse(t *testing.T) {
	prepared := preparedFixedUpload(t)
	publicKey, privateKey := deterministicPauseKey()
	validPause := signedPauseEnvelope(prepared.releaseVersion, testPauseEpoch, testPauseKeyID, privateKey)
	validAck := acceptedBody(prepared.eventIDs, "accepted")

	tests := map[string]struct {
		status               int
		contentType          string
		contentEncoding      string
		duplicateContentType bool
		body                 string
		catalog              pausePublicKeyCatalog
	}{
		"empty success":             {status: http.StatusOK, contentType: "application/json", catalog: testPauseCatalog(publicKey)},
		"malformed success":         {status: http.StatusOK, contentType: "application/json", body: `{`, catalog: testPauseCatalog(publicKey)},
		"wrong content type":        {status: http.StatusOK, contentType: "text/html", body: validAck, catalog: testPauseCatalog(publicKey)},
		"duplicate content type":    {status: http.StatusOK, contentType: "application/json", duplicateContentType: true, body: validAck, catalog: testPauseCatalog(publicKey)},
		"declared content encoding": {status: http.StatusOK, contentType: "application/json", contentEncoding: "identity", body: validAck, catalog: testPauseCatalog(publicKey)},
		"generic conflict":          {status: http.StatusConflict, contentType: "application/json", body: `{}`, catalog: testPauseCatalog(publicKey)},
		"unsigned gone":             {status: http.StatusGone, contentType: "application/json", body: `{}`, catalog: testPauseCatalog(publicKey)},
		"unknown pause key":         {status: http.StatusGone, contentType: "application/json", body: validPause, catalog: productionPausePublicKeyCatalog},
		"wrong pause epoch":         {status: http.StatusGone, contentType: "application/json", body: validPause, catalog: testPauseCatalog(publicKey)},
		"oversized response":        {status: http.StatusOK, contentType: "application/json", body: validAck + strings.Repeat(" ", maxUploadResponseBytes-len(validAck)+1), catalog: testPauseCatalog(publicKey)},
		"bad request":               {status: http.StatusBadRequest, contentType: "application/json", body: validAck, catalog: testPauseCatalog(publicKey)},
		"unauthorized":              {status: http.StatusUnauthorized, contentType: "application/json", body: validAck, catalog: testPauseCatalog(publicKey)},
		"rate limited":              {status: http.StatusTooManyRequests, contentType: "application/json", body: validAck, catalog: testPauseCatalog(publicKey)},
		"server error":              {status: http.StatusInternalServerError, contentType: "application/json", body: validAck, catalog: testPauseCatalog(publicKey)},
		"unexpected status":         {status: http.StatusTeapot, contentType: "application/json", body: validAck, catalog: testPauseCatalog(publicKey)},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.contentType != "" {
					writer.Header().Set("Content-Type", test.contentType)
				}
				if test.duplicateContentType {
					writer.Header().Add("Content-Type", "text/plain")
				}
				if test.contentEncoding != "" {
					writer.Header().Set("Content-Encoding", test.contentEncoding)
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			transport := strictTestUploadTransport(t, server.URL, server.Client().Transport, test.catalog)
			epoch := testPauseEpoch
			if name == "wrong pause epoch" {
				epoch++
			}
			result, _ := transport.upload(context.Background(), prepared, epoch)
			if result.kind != uploadResponseRetry {
				t.Fatalf("result = %#v, want retry", result)
			}
		})
	}
}

func TestStrictUploadClientRejectsRedirectsCookiesAndCompression(t *testing.T) {
	prepared := preparedFixedUpload(t)
	var redirected atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, acceptedBody(prepared.eventIDs, "accepted"))
	}))
	t.Cleanup(destination.Close)

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirected.Store(0)
			redirector := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, destination.URL, status)
			}))
			t.Cleanup(redirector.Close)
			transport := strictTestUploadTransport(t, redirector.URL, redirector.Client().Transport, productionPausePublicKeyCatalog)
			result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
			if result.kind != uploadResponseRetry || err == nil || !errors.Is(err, errUploadRedirect) {
				t.Fatalf("redirect result = %#v, err = %v", result, err)
			}
			if redirected.Load() != 0 {
				t.Fatalf("redirect destination received %d requests", redirected.Load())
			}
		})
	}

	var cookieCalls atomic.Int32
	cookieServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cookieCalls.Add(1)
		if request.Header.Get("Cookie") != "" {
			t.Errorf("request carried a server cookie: %q", request.Header.Get("Cookie"))
		}
		writer.Header().Set("Set-Cookie", "session=hostile; Secure")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, acceptedBody(prepared.eventIDs, "accepted"))
	}))
	t.Cleanup(cookieServer.Close)
	cookieTransport := strictTestUploadTransport(t, cookieServer.URL, cookieServer.Client().Transport, productionPausePublicKeyCatalog)
	for range 2 {
		if result, err := cookieTransport.upload(context.Background(), prepared, testPauseEpoch); err != nil || result.kind != uploadResponseAccepted {
			t.Fatalf("cookie isolation upload = %#v, %v", result, err)
		}
	}
	if cookieCalls.Load() != 2 {
		t.Fatalf("cookie server calls = %d, want 2", cookieCalls.Load())
	}

	gzipServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "" {
			t.Errorf("Accept-Encoding = %q, want empty", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Encoding", "gzip")
		compressor := gzip.NewWriter(writer)
		_, _ = io.WriteString(compressor, acceptedBody(prepared.eventIDs, "accepted"))
		_ = compressor.Close()
	}))
	t.Cleanup(gzipServer.Close)
	gzipTransport := strictTestUploadTransport(t, gzipServer.URL, gzipServer.Client().Transport, productionPausePublicKeyCatalog)
	if result, err := gzipTransport.upload(context.Background(), prepared, testPauseEpoch); result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("compressed response = %#v, %v; want retry", result, err)
	}
}

func TestProductionUploadTransportIsClosedAndHardened(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("SSL_CERT_FILE", "/definitely/not/a/custom/ca.pem")
	t.Setenv("SSL_CERT_DIR", "/definitely/not/a/custom/ca-directory")

	if got, err := newProductionUploadTransport(CurrentReleaseIdentity()); err == nil || got != nil {
		t.Fatalf("endpoint-empty development identity constructed uploader %#v with error %v", got, err)
	}

	for name, endpoint := range map[string]string{
		"empty":           "",
		"HTTP":            "http://metrics.invalid/v1",
		"relative":        "/v1",
		"userinfo":        "https://user:pass@metrics.invalid/v1",
		"query":           "https://metrics.invalid/v1?token=x",
		"fragment":        "https://metrics.invalid/v1#fragment",
		"unexpected port": "https://metrics.invalid:8443/v1",
		"empty port":      "https://metrics.invalid:/v1",
		"missing host":    "https:///v1",
		"opaque":          "https:metrics.invalid/v1",
		"empty fragment":  "https://metrics.invalid/v1#",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProductionUploadEndpoint(endpoint); err == nil {
				t.Fatalf("accepted endpoint %q", endpoint)
			}
		})
	}
	for _, endpoint := range []string{"https://metrics.invalid/v1", "https://metrics.invalid:443/v1"} {
		if _, err := parseProductionUploadEndpoint(endpoint); err != nil {
			t.Fatalf("parseProductionUploadEndpoint(%q): %v", endpoint, err)
		}
	}

	identity := ReleaseIdentity{
		releaseVersion: testPauseRelease,
		endpoint:       "https://metrics.invalid/v1",
		metricsEpoch:   testPauseEpoch,
	}
	if got, err := newProductionUploadTransport(identity); err == nil || got != nil {
		t.Fatalf("forged development identity constructed uploader %#v with error %v", got, err)
	}
	identity.buildKind = BuildKind(255)
	if got, err := newProductionUploadTransport(identity); err == nil || got != nil {
		t.Fatalf("unknown build kind constructed uploader %#v with error %v", got, err)
	}
	identity.buildKind = BuildDevelopment
	endpoint, err := parseProductionUploadEndpoint(identity.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, _ := deterministicPauseKey()
	transport := &uploadTransport{
		endpoint:         endpoint,
		pauseKeys:        testPauseCatalog(publicKey),
		productionPolicy: true,
	}
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	if err := transport.validate(); err == nil {
		t.Fatal("production transport accepted a substituted endpoint and pause-key catalog")
	}
	client, err := newProductionUploadHTTPClient()
	if err != nil {
		t.Fatalf("newProductionUploadHTTPClient: %v", err)
	}
	if client == http.DefaultClient || client.Jar != nil || client.Timeout != uploadTotalTimeout {
		t.Fatalf("client is not isolated: %#v", client)
	}
	httpTransport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if httpTransport == http.DefaultTransport || httpTransport.Proxy != nil || !httpTransport.DisableCompression || !httpTransport.DisableKeepAlives {
		t.Fatalf("HTTP transport is not direct/isolated: %#v", httpTransport)
	}
	if httpTransport.TLSHandshakeTimeout != uploadTLSHandshakeTimeout || httpTransport.ResponseHeaderTimeout != uploadResponseHeaderTimeout {
		t.Fatalf("transport deadlines = TLS %v, headers %v", httpTransport.TLSHandshakeTimeout, httpTransport.ResponseHeaderTimeout)
	}
	if httpTransport.TLSClientConfig == nil || httpTransport.TLSClientConfig.InsecureSkipVerify || httpTransport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("TLS config is unsafe: %#v", httpTransport.TLSClientConfig)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://metrics.invalid/v1", nil)
	if err := client.CheckRedirect(request, nil); !errors.Is(err, errUploadRedirect) {
		t.Fatalf("redirect policy returned %v", err)
	}
}

func TestProductionUploadCustomCAEnvironmentPredicate(t *testing.T) {
	tests := map[string]struct {
		certFile string
		certDir  string
		want     bool
	}{
		"empty":     {},
		"cert file": {certFile: "/tmp/test-ca.pem", want: true},
		"cert dir":  {certDir: "/tmp/test-ca-dir", want: true},
		"both":      {certFile: "/tmp/test-ca.pem", certDir: "/tmp/test-ca-dir", want: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SSL_CERT_FILE", test.certFile)
			t.Setenv("SSL_CERT_DIR", test.certDir)
			if got := productionUploadCustomCAEnvironmentConfigured(); got != test.want {
				t.Fatalf("productionUploadCustomCAEnvironmentConfigured() = %v, want %v", got, test.want)
			}
			_, err := (&uploadTransport{productionPolicy: true}).requestDependencies()
			if test.want {
				if err == nil || !strings.Contains(err.Error(), "custom CA environment") {
					t.Fatalf("production request dependencies with custom CA = %v, want custom-CA rejection", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "custom CA environment") {
				t.Fatalf("endpoint-empty production request dependencies = %v, want non-CA fail-closed error", err)
			}
		})
	}
}

func TestProductionUploadHTTPClientValidatorRejectsPolicyDrift(t *testing.T) {
	mutations := map[string]func(*http.Client){
		"default transport": func(client *http.Client) {
			client.Transport = http.DefaultTransport
		},
		"proxy function": func(client *http.Client) {
			client.Transport.(*http.Transport).Proxy = http.ProxyFromEnvironment
		},
		"compression enabled": func(client *http.Client) {
			client.Transport.(*http.Transport).DisableCompression = false
		},
		"keepalives enabled": func(client *http.Client) {
			client.Transport.(*http.Transport).DisableKeepAlives = false
		},
		"insecure TLS": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
		},
		"short TLS floor": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS10
		},
		"permissive redirect policy": func(client *http.Client) {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
		},
		"missing redirect policy": func(client *http.Client) {
			client.CheckRedirect = nil
		},
		"short total deadline": func(client *http.Client) {
			client.Timeout = time.Second
		},
		"custom system roots": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
		},
		"TLS server name override": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.ServerName = "redirect.invalid"
		},
		"TLS clock override": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.Time = func() time.Time { return time.Unix(1, 0) }
		},
		"TLS verification hook": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSClientConfig.EncryptedClientHelloRejectionVerify = func(tls.ConnectionState) error { return nil }
		},
		"legacy direct dialer": func(client *http.Client) {
			httpTransport := client.Transport.(*http.Transport)
			httpTransport.DialContext = nil
			//nolint:staticcheck // The production guard must cover the deprecated bypass.
			httpTransport.Dial = func(string, string) (net.Conn, error) {
				return nil, errors.New("legacy dialer must not run")
			}
		},
		"TLS dial hook": func(client *http.Client) {
			//nolint:staticcheck // The production guard must cover the deprecated bypass.
			client.Transport.(*http.Transport).DialTLS = func(string, string) (net.Conn, error) {
				return nil, errors.New("TLS dial hook must not run")
			}
		},
		"TLS context dial hook": func(client *http.Client) {
			client.Transport.(*http.Transport).DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("TLS context dial hook must not run")
			}
		},
		"alternate TLS protocol": func(client *http.Client) {
			client.Transport.(*http.Transport).TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
				"bypass": func(string, *tls.Conn) http.RoundTripper { return http.DefaultTransport },
			}
		},
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			client, err := newProductionUploadHTTPClient()
			if err != nil {
				t.Fatalf("newProductionUploadHTTPClient: %v", err)
			}
			mutate(client)
			if err := validateProductionUploadHTTPClient(client); err == nil {
				t.Fatal("tampered production HTTP client passed structural validation")
			}
		})
	}
}

func TestProductionUploadPolicyRejectsTampering(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	publicKey, _ := deterministicPauseKey()
	endpoint, err := url.Parse("https://metrics.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]*uploadTransport{
		"endpoint override": {
			endpoint:         endpoint,
			productionPolicy: true,
		},
		"client override": {
			client:           newStrictUploadHTTPClient(newProductionHTTPTransport()),
			productionPolicy: true,
		},
		"pause-key override": {
			pauseKeys:        testPauseCatalog(publicKey),
			productionPolicy: true,
		},
		"combined overrides": {
			endpoint:         endpoint,
			client:           newStrictUploadHTTPClient(newProductionHTTPTransport()),
			pauseKeys:        testPauseCatalog(publicKey),
			productionPolicy: true,
		},
	}

	for name, transport := range tests {
		t.Run(name, func(t *testing.T) {
			if err := transport.validate(); err == nil {
				t.Fatal("production transport accepted an injected dependency")
			}
		})
	}
	if err := (&uploadTransport{productionPolicy: true}).validate(); err == nil {
		t.Fatal("endpoint-empty Stage 1a artifact constructed production request dependencies")
	}
}

func TestProductionUploadPolicyCannotBeReplacedToFollowRedirect(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	prepared := preparedFixedUpload(t)
	publicKey, _ := deterministicPauseKey()
	var redirected atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/accepted", http.StatusFound)
			return
		}
		redirected.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, acceptedBody(prepared.eventIDs, "accepted"))
	}))
	t.Cleanup(server.Close)

	testTLSConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	httpTransport := newProductionHTTPTransport()
	httpTransport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: uploadConnectTimeout},
			Config:    testTLSConfig.Clone(),
		}
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	client := newStrictUploadHTTPClient(httpTransport)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	endpoint, err := url.Parse("https://127.0.0.1/start")
	if err != nil {
		t.Fatal(err)
	}
	transport := &uploadTransport{
		endpoint:         endpoint,
		client:           client,
		pauseKeys:        testPauseCatalog(publicKey),
		productionPolicy: true,
	}

	result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
	if result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("tampered redirect upload = %#v, %v; want retry error", result, err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("tampered redirect policy reached destination %d times", redirected.Load())
	}
}

func TestProductionUploadPolicyDoesNotUseRegisteredHTTPSProtocol(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	prepared := preparedFixedUpload(t)
	publicKey, _ := deterministicPauseKey()
	var protocolCalls atomic.Int32
	httpTransport := newProductionHTTPTransport()
	client := newStrictUploadHTTPClient(httpTransport)
	clientTransport := client.Transport.(*http.Transport)
	clientTransport.ForceAttemptHTTP2 = false
	clientTransport.RegisterProtocol("https", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		protocolCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(acceptedBody(prepared.eventIDs, "accepted"))),
			Request:    request,
		}, nil
	}))
	endpoint, err := url.Parse("https://127.0.0.1/v1")
	if err != nil {
		t.Fatal(err)
	}
	transport := &uploadTransport{
		endpoint:         endpoint,
		client:           client,
		pauseKeys:        testPauseCatalog(publicKey),
		productionPolicy: true,
	}

	result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
	if result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("registered-protocol upload = %#v, %v; want retry error", result, err)
	}
	if protocolCalls.Load() != 0 {
		t.Fatalf("registered https protocol ran %d times", protocolCalls.Load())
	}
}

func TestUploadTransportDoesNotEchoHostileNetworkMaterial(t *testing.T) {
	prepared := preparedFixedUpload(t)
	const secret = "response-secret-must-not-escape"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"`+secret+`":true}`)
	}))
	t.Cleanup(server.Close)
	transport := strictTestUploadTransport(t, server.URL, server.Client().Transport, productionPausePublicKeyCatalog)
	result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
	if result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("hostile response = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error echoed response material: %v", err)
	}

	networkTransport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	}), productionPausePublicKeyCatalog)
	result, err = networkTransport.upload(context.Background(), prepared, testPauseEpoch)
	if result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("hostile network error = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error echoed network material: %v", err)
	}
}

func TestUploadTransportHonorsEarlierCancellationWithoutRetry(t *testing.T) {
	prepared := preparedFixedUpload(t)
	var calls atomic.Int32
	transport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}), productionPausePublicKeyCatalog)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := transport.upload(ctx, prepared, testPauseEpoch)
	if result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("canceled upload = %#v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled upload took %v", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("round trips = %d, want one", calls.Load())
	}
}

func TestUploadTransportBoundsAndClosesEveryResponseBody(t *testing.T) {
	prepared := preparedFixedUpload(t)
	const secret = "hostile-body-error-material"
	tests := map[string]struct {
		contentLength int64
		body          *controlledResponseBody
	}{
		"declared oversized": {
			contentLength: maxUploadResponseBytes + 1,
			body:          &controlledResponseBody{reader: strings.NewReader(acceptedBody(prepared.eventIDs, "accepted"))},
		},
		"read failure": {
			contentLength: -1,
			body:          &controlledResponseBody{reader: errorReader{err: errors.New(secret)}},
		},
		"close failure": {
			contentLength: -1,
			body: &controlledResponseBody{
				reader:   strings.NewReader(acceptedBody(prepared.eventIDs, "accepted")),
				closeErr: errors.New(secret),
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			transport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": []string{"application/json"}},
					Body:          test.body,
					ContentLength: test.contentLength,
					Request:       request,
				}, nil
			}), productionPausePublicKeyCatalog)
			result, err := transport.upload(context.Background(), prepared, testPauseEpoch)
			if result.kind != uploadResponseRetry || err == nil {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
			if !test.body.closed {
				t.Fatal("response body was not closed")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error echoed hostile body failure: %v", err)
			}
		})
	}
}

func TestUploadTransportAppliesTotalDeadlineAndRejectsInvalidPreparedValues(t *testing.T) {
	prepared := preparedFixedUpload(t)
	var calls atomic.Int32
	roundTripper := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Error("request context has no deadline")
		} else if remaining := time.Until(deadline); remaining <= 0 || remaining > uploadTotalTimeout {
			t.Errorf("request deadline remaining = %v", remaining)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(acceptedBody(prepared.eventIDs, "accepted"))),
			Request:    request,
		}, nil
	})
	transport := strictTestUploadTransport(t, "https://metrics.invalid/v1", roundTripper, productionPausePublicKeyCatalog)
	if result, err := transport.upload(context.Background(), prepared, testPauseEpoch); err != nil || result.kind != uploadResponseAccepted {
		t.Fatalf("upload = %#v, %v", result, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("round trips = %d, want one", calls.Load())
	}

	invalid := []preparedUploadBatch{
		{},
		{body: bytes.Repeat([]byte{'x'}, maxUploadRequestBytes+1), eventIDs: prepared.eventIDs, installationID: prepared.installationID, releaseVersion: prepared.releaseVersion},
		{body: prepared.body, eventIDs: nil, installationID: prepared.installationID, releaseVersion: prepared.releaseVersion},
		{body: prepared.body, eventIDs: []string{strings.ToUpper(prepared.eventIDs[0])}, installationID: prepared.installationID, releaseVersion: prepared.releaseVersion},
		{body: prepared.body, eventIDs: prepared.eventIDs, installationID: "invalid", releaseVersion: prepared.releaseVersion},
		{body: prepared.body, eventIDs: prepared.eventIDs, installationID: prepared.installationID, releaseVersion: "v0.31.0"},
	}
	for i, value := range invalid {
		before := calls.Load()
		if result, err := transport.upload(context.Background(), value, testPauseEpoch); result.kind != uploadResponseRetry || err == nil {
			t.Errorf("invalid %d = %#v, %v; want retry error", i, result, err)
		}
		if calls.Load() != before {
			t.Errorf("invalid %d performed network work", i)
		}
	}
	if result, err := transport.upload(context.Background(), prepared, 0); result.kind != uploadResponseRetry || err == nil {
		t.Fatalf("zero epoch = %#v, %v; want retry error", result, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type controlledResponseBody struct {
	reader   io.Reader
	closeErr error
	closed   bool
}

func (body *controlledResponseBody) Read(destination []byte) (int, error) {
	return body.reader.Read(destination)
}

func (body *controlledResponseBody) Close() error {
	body.closed = true
	return body.closeErr
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}
