//go:build productmetrics_testhook

package productmetrics

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
)

// TesthookPauseKey is a tagged-only signed-pause trust entry.
type TesthookPauseKey struct {
	ID        string
	PublicKey ed25519.PublicKey
}

// TesthookOptions is the tagged-only process-test construction surface. None
// of these endpoint, client, trust, clock, or entropy seams exist in a normal
// artifact.
type TesthookOptions struct {
	Home           gchome.ResolvedHome
	ReleaseVersion string
	MetricsEpoch   uint64
	NoticeVersion  uint64
	NoticeText     []byte
	Endpoint       string
	PrivacyURL     string
	Client         *http.Client
	PauseKeys      []TesthookPauseKey
	Now            func() time.Time
	NewUUID        func() (string, error)
}

// OpenTesthook constructs a synthetic official/default-on service for a
// separately built tagged process binary. Endpoint policy remains HTTPS-only
// and loopback-only; the caller's RoundTripper contributes test trust roots
// while productmetrics owns the strict client wrapper.
func OpenTesthook(options TesthookOptions) (*Service, error) {
	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || options.Endpoint == "" || endpoint.Scheme != "https" || endpoint.Opaque != "" ||
		endpoint.User != nil || endpoint.Host == "" || endpoint.Hostname() == "" || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" ||
		(endpoint.Path != "" && endpoint.Path[0] != '/') || !testhookLoopbackHost(endpoint.Hostname()) {
		return nil, errors.New("productmetrics: tagged endpoint must be loopback HTTPS")
	}
	if options.Client == nil || options.Client.Transport == nil {
		return nil, errors.New("productmetrics: tagged client transport is required")
	}
	if !validPauseReleaseVersion(options.ReleaseVersion) || !validMetricsEpoch(options.MetricsEpoch) {
		return nil, errors.New("productmetrics: tagged release identity is invalid")
	}
	if options.NoticeVersion == 0 || len(options.NoticeText) == 0 {
		return nil, errors.New("productmetrics: tagged notice is incomplete")
	}
	entries := make([]pausePublicKeyEntry, 0, len(options.PauseKeys))
	for _, entry := range options.PauseKeys {
		entries = append(entries, pausePublicKeyEntry{id: entry.ID, key: append(ed25519.PublicKey(nil), entry.PublicKey...)})
	}
	catalog := func(yield func(pausePublicKeyEntry)) {
		for _, entry := range entries {
			yield(entry)
		}
	}
	if _, err := indexPausePublicKeyCatalog(catalog); err != nil {
		return nil, err
	}
	transport := &uploadTransport{
		endpoint:  endpoint,
		client:    newStrictUploadHTTPClient(options.Client.Transport),
		pauseKeys: catalog,
	}
	if err := transport.validate(); err != nil {
		return nil, err
	}
	resolved := options.Home
	if resolved.Path() == "" {
		resolved = gchome.ResolveReadOnly()
	}
	home, homeErr := gchome.InspectProductUsageHome(resolved)
	now := options.Now
	if now == nil {
		now = time.Now
	}
	newUUID := options.NewUUID
	if newUUID == nil {
		newUUID = func() (string, error) { return randomUUIDv4(rand.Reader) }
	}
	return openWithDependencies(serviceDependencies{
		home:       home,
		homeErr:    homeErr,
		homeReason: ReasonHomeUnstable,
		release: serviceRelease{
			platformSupported:  runtime.GOOS == "linux" || runtime.GOOS == "darwin",
			official:           true,
			endpointConfigured: true,
			rollout:            RolloutDefaultOn,
			releaseVersion:     options.ReleaseVersion,
			metricsEpoch:       options.MetricsEpoch,
			endpointHostname:   endpoint.Hostname(),
			privacyURL:         options.PrivacyURL,
		},
		notice:               noticeDefinition{testOnly: true, version: options.NoticeVersion, text: append([]byte(nil), options.NoticeText...)},
		getenv:               os.Getenv,
		newUUID:              newUUID,
		now:                  now,
		verifyTTY:            productionNoticeWriterIsTTY,
		privateUploaderStart: asynchronousUploadStart(transport),
	})
}

func testhookLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
