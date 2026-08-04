//go:build productmetrics_testhook

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
)

const (
	taggedProductMetricsEndpointEnvironment = "GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT"
	taggedProductMetricsCAFileEnvironment   = "GC_PRODUCT_METRICS_TESTHOOK_CA_FILE"
	taggedProductMetricsMaximumCABytes      = 64 * 1024
	taggedProductMetricsReleaseVersion      = "0.31.0"
)

func configuredPrivateProductMetricsRunner() privateProductMetricsRunFunc {
	return runProductMetricsTaggedChild
}

func runProductMetricsTaggedChild(ctx context.Context, invocation productmetrics.PrivateUploaderInvocation) error {
	if os.Getenv(taggedProductMetricsEndpointEnvironment) == "" {
		return runProductionProductMetricsChild(ctx, invocation)
	}
	return runProductMetricsTesthookChild(ctx, invocation)
}

func runProductMetricsTesthookChild(ctx context.Context, invocation productmetrics.PrivateUploaderInvocation) error {
	service, err := openProductMetricsTesthookService()
	if err != nil {
		return err
	}
	return service.RunPrivateUploader(ctx, invocation)
}

func configuredProductMetricsControlService() (*productmetrics.Service, error) {
	if os.Getenv(taggedProductMetricsEndpointEnvironment) == "" {
		return openProductionProductMetricsService()
	}
	return openProductMetricsTesthookService()
}

func openProductMetricsTesthookService() (*productmetrics.Service, error) {
	endpoint := os.Getenv(taggedProductMetricsEndpointEnvironment)
	if err := validateProductMetricsTesthookEndpoint(endpoint); err != nil {
		return nil, err
	}
	certificatePEM, err := readProductMetricsTesthookCA(os.Getenv(taggedProductMetricsCAFileEnvironment))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		return nil, errors.New("product metrics testhook CA file has no certificate")
	}
	return productmetrics.OpenTesthook(productmetrics.TesthookOptions{
		Home:           gchome.ResolveReadOnly(),
		ReleaseVersion: taggedProductMetricsReleaseVersion,
		MetricsEpoch:   1,
		NoticeVersion:  1,
		NoticeText:     []byte("Gas City product metrics test-only notice."),
		Endpoint:       endpoint,
		Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}}},
	})
}

func validateProductMetricsTesthookEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return errors.New("product metrics testhook endpoint is invalid")
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (address == nil || !address.IsLoopback()) {
		return errors.New("product metrics testhook endpoint is not loopback")
	}
	return nil
}

func readProductMetricsTesthookCA(path string) (contents []byte, returnErr error) {
	if path == "" {
		return nil, errors.New("product metrics testhook CA file is absent")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open product metrics testhook CA file: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	contents, err = io.ReadAll(io.LimitReader(file, taggedProductMetricsMaximumCABytes+1))
	if err != nil {
		return nil, fmt.Errorf("read product metrics testhook CA file: %w", err)
	}
	if len(contents) == 0 || len(contents) > taggedProductMetricsMaximumCABytes {
		return nil, errors.New("product metrics testhook CA file has invalid size")
	}
	return contents, nil
}
