package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

type eventsAPIScope struct {
	apiURL             string
	cityName           string
	cityPath           string
	explicitAPI        bool
	localOnly          bool
	localSupervisorAPI bool
}

type eventsAPIError struct {
	statusCode int
	title      string
	detail     string
}

type eventsAPITransportError struct {
	err error
}

type cliWireEvent struct {
	Actor   string          `json:"actor"`
	Message string          `json:"message,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Seq     int64           `json:"seq"`
	Subject string          `json:"subject,omitempty"`
	Ts      time.Time       `json:"ts"`
	Type    string          `json:"type"`
}

type cliWireTaggedEvent struct {
	Actor   string          `json:"actor"`
	City    string          `json:"city"`
	Message string          `json:"message,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Seq     int64           `json:"seq"`
	Subject string          `json:"subject,omitempty"`
	Ts      time.Time       `json:"ts"`
	Type    string          `json:"type"`
}

type cliEventEnvelope = cliWireEvent

type cliTaggedEventEnvelope = cliWireTaggedEvent

func (e *eventsAPIError) Error() string {
	if e == nil {
		return "request failed"
	}
	if e.detail != "" {
		return e.detail
	}
	if e.title != "" {
		return e.title
	}
	if e.statusCode == 0 {
		return "request failed"
	}
	return fmt.Sprintf("API returned HTTP %d", e.statusCode)
}

func (e *eventsAPITransportError) Error() string {
	if e == nil || e.err == nil {
		return "request failed"
	}
	return fmt.Sprintf("request failed: %v", e.err)
}

func (e *eventsAPITransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

var eventsControllerAliveHook = controllerAlive

func (s eventsAPIScope) isSupervisor() bool { return s.cityName == "" }

func (s eventsAPIScope) client() (*genclient.ClientWithResponses, error) {
	httpClient := &http.Client{}
	return genclient.NewClientWithResponses(
		s.apiURL,
		genclient.WithHTTPClient(httpClient),
	)
}

func newEventsCmd(stdout, stderr io.Writer) *cobra.Command {
	var apiURL string
	var typeFilter string
	var sinceFlag string
	var watchFlag bool
	var followFlag bool
	var seqFlag bool
	var timeoutFlag string
	var afterFlag uint64
	var afterCursor string
	var payloadMatch []string
	var jsonFlagDeprecated bool

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show events from the GC API",
		Long: `Show events from the GC API with optional filtering.

The API is the source of truth for both city-scoped and supervisor-scoped
events. In a city directory (or with --city), this command reflects the
city's /v0/city/{cityName}/events and /stream endpoints. Without a city in
scope, it reflects the supervisor's /v0/events and /stream endpoints.

List, watch, and follow output are always JSON Lines. Each line is one API
DTO or SSE envelope.`,
		Example: `  gc events
  gc events --type bead.created --since 1h
  gc events --watch --type convoy.closed --timeout 5m
  gc events --follow
  gc events --seq
  gc events --follow --after-cursor city-a:12,city-b:9`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if afterFlag > 0 && strings.TrimSpace(afterCursor) != "" {
				fmt.Fprintln(stderr, "gc events: --after and --after-cursor are mutually exclusive") //nolint:errcheck
				return errExit
			}
			if seqFlag {
				if cmdEventsSeq(apiURL, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if followFlag {
				if cmdEventsFollow(apiURL, typeFilter, payloadMatch, afterFlag, afterCursor, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if watchFlag {
				if cmdEventsWatch(apiURL, typeFilter, payloadMatch, afterFlag, afterCursor, timeoutFlag, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if cmdEvents(apiURL, typeFilter, sinceFlag, payloadMatch, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiURL, "api", "", "GC API server URL override (auto-discovered by default)")
	cmd.Flags().StringVar(&typeFilter, "type", "", "Filter by event type (e.g. bead.created)")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "Show events since duration ago (e.g. 1h, 30m)")
	cmd.Flags().BoolVar(&watchFlag, "watch", false, "Block until matching events arrive (exits after first match or buffered replay)")
	cmd.Flags().BoolVar(&followFlag, "follow", false, "Continuously stream events as they arrive")
	cmd.Flags().BoolVar(&seqFlag, "seq", false, "Print the current head cursor and exit")
	cmd.Flags().StringVar(&timeoutFlag, "timeout", "30s", "Max wait duration for --watch (e.g. 30s, 5m)")
	cmd.Flags().Uint64Var(&afterFlag, "after", 0, "Resume from this city event sequence number (city scope only)")
	cmd.Flags().StringVar(&afterCursor, "after-cursor", "", "Resume from this supervisor event cursor (supervisor scope only)")
	cmd.Flags().StringArrayVar(&payloadMatch, "payload-match", nil, "Filter by payload field (key=value or key.subkey=value, repeatable)")
	cmd.Flags().BoolVar(&jsonFlagDeprecated, "json", false, "Deprecated: output is always JSONL. Accepted for back-compat.")
	_ = cmd.Flags().MarkDeprecated("json", "output is always JSONL; the flag is now a no-op and will be removed in a future release")
	return cmd
}

func cmdEvents(apiURLOverride, typeFilter, sinceFlag string, payloadMatchArgs []string, stdout, stderr io.Writer) int {
	if err := validateEventsSince(sinceFlag); err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	return doEvents(scope, typeFilter, sinceFlag, pm, stdout, stderr)
}

func cmdEventsSeq(apiURLOverride string, stdout, stderr io.Writer) int {
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	return doEventsSeq(scope, stdout, stderr)
}

func cmdEventsFollow(apiURLOverride, typeFilter string, payloadMatchArgs []string, afterSeq uint64, afterCursor string, stdout, stderr io.Writer) int {
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	if err := validateEventsCursor(scope, afterSeq, afterCursor); err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	return doEventsFollow(scope, typeFilter, pm, afterSeq, afterCursor, stdout, stderr)
}

func cmdEventsWatch(apiURLOverride, typeFilter string, payloadMatchArgs []string, afterSeq uint64, afterCursor, timeoutFlag string, stdout, stderr io.Writer) int {
	timeout, err := time.ParseDuration(timeoutFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: invalid --timeout %q: %v\n", timeoutFlag, err) //nolint:errcheck
		return 1
	}
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	if err := validateEventsCursor(scope, afterSeq, afterCursor); err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	return doEventsWatch(scope, typeFilter, pm, afterSeq, afterCursor, timeout, stdout, stderr)
}

func openEventsScope(apiURLOverride string, stderr io.Writer) (eventsAPIScope, int) {
	scope, err := resolveEventsScope(apiURLOverride)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return eventsAPIScope{}, 1
	}
	return scope, 0
}

func resolveEventsScope(apiURLOverride string) (eventsAPIScope, error) {
	cityPath, cfg, err := resolveDashboardContext()
	if err != nil {
		return eventsAPIScope{}, err
	}

	cityName := resolvedEventsCityName(cityPath, cfg)
	if override := strings.TrimSpace(apiURLOverride); override != "" {
		localSupervisorAPI := matchesLocalSupervisorAPI(override)
		cityName = resolvedExplicitEventsCityName(override, cityPath, cityName)
		return eventsAPIScope{
			apiURL:             strings.TrimRight(override, "/"),
			cityName:           cityName,
			cityPath:           cityPath,
			explicitAPI:        true,
			localSupervisorAPI: localSupervisorAPI,
		}, nil
	}

	if supervisorAliveHook() != 0 {
		cityName = resolvedManagedEventsCityName(cityPath, cityName)
		baseURL, err := supervisorAPIBaseURL()
		if err != nil {
			return eventsAPIScope{}, err
		}
		return eventsAPIScope{
			apiURL:   strings.TrimRight(baseURL, "/"),
			cityName: cityName,
			cityPath: cityPath,
		}, nil
	}

	if cityPath == "" {
		return eventsAPIScope{}, fmt.Errorf(
			"could not auto-discover the supervisor API; start the supervisor with %q or pass --api explicitly",
			"gc supervisor start",
		)
	}
	// Standalone-controller mode: the controller's API now serves
	// supervisor-shaped /v0/city/{cityName}/... routes, so `gc events`
	// can target it directly. Fall through to auto-discovery instead
	// of rejecting.
	if hasStandaloneDashboardAPI(cfg) {
		if eventsControllerAliveHook(cityPath) == 0 {
			return eventsAPIScope{
				cityName:  cityName,
				cityPath:  cityPath,
				localOnly: true,
			}, nil
		}
		return eventsAPIScope{
			apiURL:   strings.TrimRight(standaloneAPIBaseURL(cfg), "/"),
			cityName: cityName,
			cityPath: cityPath,
		}, nil
	}
	return eventsAPIScope{}, fmt.Errorf(
		"could not auto-discover the supervisor API for %q; start the supervisor with %q or pass --api explicitly",
		cityPath,
		"gc supervisor start",
	)
}

func resolvedExplicitEventsCityName(apiURLOverride, cityPath, fallback string) string {
	if !matchesLocalSupervisorAPI(apiURLOverride) {
		return fallback
	}
	return resolvedManagedEventsCityName(cityPath, fallback)
}

func matchesLocalSupervisorAPI(apiURLOverride string) bool {
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return false
	}
	return sameEventsAPIEndpoint(baseURL, apiURLOverride)
}

func sameEventsAPIEndpoint(a, b string) bool {
	left, err := url.Parse(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	right, err := url.Parse(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	if !strings.EqualFold(left.Scheme, right.Scheme) {
		return false
	}
	if normalizedURLPort(left) != normalizedURLPort(right) {
		return false
	}
	if !sameEventsAPIHost(left.Hostname(), right.Hostname()) {
		return false
	}
	return strings.TrimRight(left.EscapedPath(), "/") == strings.TrimRight(right.EscapedPath(), "/")
}

func normalizedURLPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	default:
		return "80"
	}
}

func sameEventsAPIHost(a, b string) bool {
	a = strings.ToLower(strings.Trim(a, "[]"))
	b = strings.ToLower(strings.Trim(b, "[]"))
	if a == b {
		return true
	}
	return isLoopbackEventsHost(a) && isLoopbackEventsHost(b)
}

func isLoopbackEventsHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func resolvedManagedEventsCityName(cityPath, fallback string) string {
	if strings.TrimSpace(cityPath) == "" {
		return fallback
	}
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered {
		return fallback
	}
	if name := strings.TrimSpace(entry.EffectiveName()); name != "" {
		return name
	}
	return fallback
}

func resolvedEventsCityName(cityPath string, cfg *config.City) string {
	return loadedCityName(cfg, cityPath)
}

func validateEventsCursor(scope eventsAPIScope, afterSeq uint64, afterCursor string) error {
	if scope.isSupervisor() && afterSeq > 0 {
		return fmt.Errorf("--after is only valid when a city is in scope; use --after-cursor for supervisor events")
	}
	if !scope.isSupervisor() && strings.TrimSpace(afterCursor) != "" {
		return fmt.Errorf("--after-cursor is only valid in supervisor scope")
	}
	return nil
}

func validateEventsSince(sinceFlag string) error {
	if strings.TrimSpace(sinceFlag) == "" {
		return nil
	}
	if _, err := time.ParseDuration(sinceFlag); err != nil {
		return fmt.Errorf("invalid --since %q: %w", sinceFlag, err)
	}
	return nil
}

func doEvents(scope eventsAPIScope, typeFilter, sinceFlag string, payloadMatch map[string][]string, stdout, stderr io.Writer) int {
	if scope.localOnly {
		fallback, _, fallbackErr := readLocalCityEvents(scope, stoppedCityLocalFallbackError(scope), typeFilter, sinceFlag, stderr)
		if fallbackErr != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", fallbackErr) //nolint:errcheck
			return 1
		}
		fallback = filterCityEvents(fallback, 0, typeFilter, payloadMatch)
		return printJSONLines(fallback, stdout, stderr)
	}

	client, err := scope.client()
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if scope.isSupervisor() {
		items, err := fetchSupervisorEvents(ctx, client, typeFilter, sinceFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		items = filterSupervisorEvents(items, typeFilter, payloadMatch)
		return printJSONLines(items, stdout, stderr)
	}

	items, err := fetchCityEvents(ctx, client, scope.cityName, typeFilter, sinceFlag)
	if err != nil {
		if fallback, ok, fallbackErr := readLocalCityEvents(scope, err, typeFilter, sinceFlag, stderr); ok {
			if fallbackErr != nil {
				fmt.Fprintf(stderr, "gc events: %v\n", fallbackErr) //nolint:errcheck
				return 1
			}
			fallback = filterCityEvents(fallback, 0, typeFilter, payloadMatch)
			return printJSONLines(fallback, stdout, stderr)
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	items = filterCityEvents(items, 0, typeFilter, payloadMatch)
	return printJSONLines(items, stdout, stderr)
}

func doEventsSeq(scope eventsAPIScope, stdout, stderr io.Writer) int {
	if scope.localOnly {
		fallback, _, fallbackErr := readLocalCityHeadIndex(scope, stoppedCityLocalFallbackError(scope))
		if fallbackErr != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", fallbackErr) //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stdout, fallback) //nolint:errcheck
		return 0
	}

	client, err := scope.client()
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if scope.isSupervisor() {
		cursor, err := fetchSupervisorHeadCursor(ctx, client)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		if cursor == "" {
			cursor = "0"
		}
		fmt.Fprintln(stdout, cursor) //nolint:errcheck
		return 0
	}

	index, err := fetchCityHeadIndex(ctx, client, scope.cityName)
	if err != nil {
		if fallback, ok, fallbackErr := readLocalCityHeadIndex(scope, err); ok {
			if fallbackErr != nil {
				fmt.Fprintf(stderr, "gc events: %v\n", fallbackErr) //nolint:errcheck
				return 1
			}
			fmt.Fprintln(stdout, fallback) //nolint:errcheck
			return 0
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, index) //nolint:errcheck
	return 0
}

func readLocalCityEvents(scope eventsAPIScope, apiErr error, typeFilter, sinceFlag string, warningWriter io.Writer) ([]cliWireEvent, bool, error) {
	if !shouldUseLocalCityEventsFallback(scope, apiErr) {
		return nil, false, nil
	}
	filter := events.Filter{Type: strings.TrimSpace(typeFilter)}
	if cutoff, err := eventsSinceCutoff(sinceFlag); err != nil {
		return nil, true, err
	} else if !cutoff.IsZero() {
		filter.Since = cutoff
	}
	all, err := events.ReadFiltered(filepath.Join(scope.cityPath, ".gc", "events.jsonl"), filter)
	if err != nil {
		return nil, true, fmt.Errorf("reading local city events: %w", err)
	}
	items := make([]cliWireEvent, 0, len(all))
	for _, item := range all {
		items = append(items, localWireEvent(item, warningWriter))
	}
	return items, true, nil
}

func readLocalCityHeadIndex(scope eventsAPIScope, apiErr error) (string, bool, error) {
	if !shouldUseLocalCityEventsFallback(scope, apiErr) {
		return "", false, nil
	}
	seq, err := events.ReadLatestSeq(filepath.Join(scope.cityPath, ".gc", "events.jsonl"))
	if err != nil {
		return "", true, fmt.Errorf("reading local city event head: %w", err)
	}
	return strconv.FormatUint(seq, 10), true, nil
}

func shouldUseLocalCityEventsFallback(scope eventsAPIScope, apiErr error) bool {
	if strings.TrimSpace(scope.cityPath) == "" || apiErr == nil {
		return false
	}
	if scope.explicitAPI && !scope.localSupervisorAPI {
		return false
	}
	var problem *eventsAPIError
	if errors.As(apiErr, &problem) {
		if problem.statusCode != http.StatusNotFound {
			return false
		}
		return gcapi.IsCityNotFoundOrNotRunningDetail(problem.detail)
	}
	if scope.explicitAPI && scope.localSupervisorAPI {
		var transport *eventsAPITransportError
		return errors.As(apiErr, &transport)
	}
	return false
}

func printStreamingCityAPIRequirement(mode string, stderr io.Writer) {
	_, _ = fmt.Fprintf(
		stderr,
		"gc events: %s requires a running city API; local fallback only supports `gc events` and `gc events --seq` when the city is stopped\n",
		mode,
	)
}

func requireStreamingCityAPI(ctx context.Context, client *genclient.ClientWithResponses, scope eventsAPIScope, mode string, stderr io.Writer) (string, bool) {
	head, err := fetchCityHeadIndex(ctx, client, scope.cityName)
	if err == nil {
		return head, true
	}
	if shouldUseLocalCityEventsFallback(scope, err) {
		printStreamingCityAPIRequirement(mode, stderr)
		return "", false
	}
	fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
	return "", false
}

func requireStreamingCityEventsReachable(ctx context.Context, client *genclient.ClientWithResponses, scope eventsAPIScope, mode string, stderr io.Writer) bool {
	if err := probeCityEventsReachable(ctx, client, scope.cityName); err != nil {
		if shouldUseLocalCityEventsFallback(scope, err) {
			printStreamingCityAPIRequirement(mode, stderr)
			return false
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return false
	}
	return true
}

func stoppedCityLocalFallbackError(scope eventsAPIScope) error {
	return &eventsAPIError{
		statusCode: http.StatusNotFound,
		detail:     gcapi.CityNotFoundOrNotRunningDetail(scope.cityName),
	}
}

func eventsSinceCutoff(sinceFlag string) (time.Time, error) {
	sinceFlag = strings.TrimSpace(sinceFlag)
	if sinceFlag == "" {
		return time.Time{}, nil
	}
	d, err := time.ParseDuration(sinceFlag)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q: %w", sinceFlag, err)
	}
	return time.Now().Add(-d), nil
}

func localWireEvent(e events.Event, _ io.Writer) cliWireEvent {
	item := cliWireEvent{
		Actor: e.Actor,
		Seq:   int64(e.Seq),
		Ts:    e.Ts,
		Type:  e.Type,
	}
	if e.Subject != "" {
		item.Subject = e.Subject
	}
	if e.Message != "" {
		item.Message = e.Message
	}
	if len(e.Payload) > 0 && string(e.Payload) != "null" {
		item.Payload = append(json.RawMessage(nil), e.Payload...)
	}
	return item
}

func cityWireEventFromTyped(item genclient.TypedEventStreamEnvelope) (cliWireEvent, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return cliWireEvent{}, err
	}
	var out cliWireEvent
	if err := json.Unmarshal(data, &out); err != nil {
		return cliWireEvent{}, err
	}
	return out, nil
}

func supervisorWireEventFromTyped(item genclient.TypedTaggedEventStreamEnvelope) (cliWireTaggedEvent, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return cliWireTaggedEvent{}, err
	}
	var out cliWireTaggedEvent
	if err := json.Unmarshal(data, &out); err != nil {
		return cliWireTaggedEvent{}, err
	}
	return out, nil
}

func doEventsFollow(scope eventsAPIScope, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, afterCursor string, stdout, stderr io.Writer) int {
	if scope.localOnly {
		printStreamingCityAPIRequirement("--follow", stderr)
		return 1
	}

	client, err := scope.client()
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}

	ctx := context.Background()
	if scope.isSupervisor() {
		cursor := strings.TrimSpace(afterCursor)
		if cursor == "" {
			cursor, err = fetchSupervisorHeadCursor(ctx, client)
			if err != nil {
				fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		return streamSupervisorEvents(ctx, client, cursor, typeFilter, payloadMatch, false, stdout, stderr)
	}

	resumeSeq := afterSeq
	if resumeSeq == 0 {
		head, ok := requireStreamingCityAPI(ctx, client, scope, "--follow", stderr)
		if !ok {
			return 1
		}
		resumeSeq, err = strconv.ParseUint(head, 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid X-GC-Index %q\n", head) //nolint:errcheck
			return 1
		}
	} else if !requireStreamingCityEventsReachable(ctx, client, scope, "--follow", stderr) {
		return 1
	}
	return streamCityEvents(ctx, client, scope.cityName, resumeSeq, typeFilter, payloadMatch, false, stdout, stderr)
}

func doEventsWatch(scope eventsAPIScope, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, afterCursor string, timeout time.Duration, stdout, stderr io.Writer) int {
	if scope.localOnly {
		printStreamingCityAPIRequirement("--watch", stderr)
		return 1
	}

	client, err := scope.client()
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if scope.isSupervisor() {
		cursor := strings.TrimSpace(afterCursor)
		if cursor != "" {
			items, err := fetchSupervisorEvents(ctx, client, "", "")
			if err != nil {
				fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
				return 1
			}
			matches := filterSupervisorEventsAfterCursor(items, cursor, typeFilter, payloadMatch)
			if len(matches) > 0 {
				return printJSONLines(taggedEnvelopesFor(matches), stdout, stderr)
			}
		} else {
			cursor, err = fetchSupervisorHeadCursor(ctx, client)
			if err != nil {
				fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		return streamSupervisorEvents(ctx, client, cursor, typeFilter, payloadMatch, true, stdout, stderr)
	}

	resumeSeq := afterSeq
	if resumeSeq > 0 {
		items, err := fetchCityEvents(ctx, client, scope.cityName, "", "")
		if err != nil {
			if shouldUseLocalCityEventsFallback(scope, err) {
				printStreamingCityAPIRequirement("--watch", stderr)
				return 1
			}
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		matches := filterCityEvents(items, resumeSeq, typeFilter, payloadMatch)
		if len(matches) > 0 {
			return printJSONLines(cityEnvelopesFor(matches), stdout, stderr)
		}
	} else {
		head, ok := requireStreamingCityAPI(ctx, client, scope, "--watch", stderr)
		if !ok {
			return 1
		}
		resumeSeq, err = strconv.ParseUint(head, 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid X-GC-Index %q\n", head) //nolint:errcheck
			return 1
		}
	}

	return streamCityEvents(ctx, client, scope.cityName, resumeSeq, typeFilter, payloadMatch, true, stdout, stderr)
}

func probeCityEventsReachable(ctx context.Context, client *genclient.ClientWithResponses, cityName string) error {
	limit := int64(1)
	resp, err := client.GetV0CityByCityNameEventsWithResponse(ctx, cityName, &genclient.GetV0CityByCityNameEventsParams{
		Limit: &limit,
	})
	if err != nil {
		return &eventsAPITransportError{err: err}
	}
	return eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault)
}

func fetchCityEvents(ctx context.Context, client *genclient.ClientWithResponses, cityName, typeFilter, sinceFlag string) ([]cliWireEvent, error) {
	limit := int64(500)
	var all []cliWireEvent
	var cursor *string

	for {
		params := &genclient.GetV0CityByCityNameEventsParams{
			Cursor: cursor,
			Limit:  &limit,
		}
		if strings.TrimSpace(typeFilter) != "" {
			params.Type = &typeFilter
		}
		if strings.TrimSpace(sinceFlag) != "" {
			params.Since = &sinceFlag
		}
		resp, err := client.GetV0CityByCityNameEventsWithResponse(ctx, cityName, params)
		if err != nil {
			return nil, &eventsAPITransportError{err: err}
		}
		if err := eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
			return nil, err
		}
		if resp.JSON200 == nil || resp.JSON200.Items == nil {
			return all, nil
		}
		for _, item := range *resp.JSON200.Items {
			wire, err := cityWireEventFromTyped(item)
			if err != nil {
				return nil, fmt.Errorf("decoding city event list item: %w", err)
			}
			all = append(all, wire)
		}
		if resp.JSON200.NextCursor == nil || strings.TrimSpace(*resp.JSON200.NextCursor) == "" {
			return all, nil
		}
		cursor = resp.JSON200.NextCursor
	}
}

func fetchCityHeadIndex(ctx context.Context, client *genclient.ClientWithResponses, cityName string) (string, error) {
	limit := int64(1)
	resp, err := client.GetV0CityByCityNameEventsWithResponse(ctx, cityName, &genclient.GetV0CityByCityNameEventsParams{
		Limit: &limit,
	})
	if err != nil {
		return "", &eventsAPITransportError{err: err}
	}
	if err := eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
		return "", err
	}
	if resp.HTTPResponse == nil {
		return "0", nil
	}
	index := strings.TrimSpace(resp.HTTPResponse.Header.Get("X-GC-Index"))
	if index == "" {
		return "", fmt.Errorf("missing X-GC-Index header")
	}
	return index, nil
}

func fetchSupervisorEvents(ctx context.Context, client *genclient.ClientWithResponses, typeFilter, sinceFlag string) ([]cliWireTaggedEvent, error) {
	return fetchSupervisorEventsWithLimit(ctx, client, typeFilter, sinceFlag, 0)
}

// fetchSupervisorEventsWithLimit is like fetchSupervisorEvents but applies
// a server-side result cap when limit > 0. The supervisor returns the
// most recent `limit` events. Used by fetchSupervisorHeadCursor so
// computing the head cursor is a cheap round-trip instead of downloading
// every event in the supervisor's history.
func fetchSupervisorEventsWithLimit(ctx context.Context, client *genclient.ClientWithResponses, typeFilter, sinceFlag string, limit int64) ([]cliWireTaggedEvent, error) {
	params := &genclient.GetV0EventsParams{}
	if strings.TrimSpace(typeFilter) != "" {
		params.Type = &typeFilter
	}
	if strings.TrimSpace(sinceFlag) != "" {
		params.Since = &sinceFlag
	}
	if limit > 0 {
		params.Limit = &limit
	}
	resp, err := client.GetV0EventsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if err := eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Items == nil {
		return []cliWireTaggedEvent{}, nil
	}
	items := make([]cliWireTaggedEvent, 0, len(*resp.JSON200.Items))
	for _, item := range *resp.JSON200.Items {
		wire, err := supervisorWireEventFromTyped(item)
		if err != nil {
			return nil, fmt.Errorf("decoding supervisor event list item: %w", err)
		}
		items = append(items, wire)
	}
	return items, nil
}

// fetchSupervisorHeadCursor asks the supervisor for its current head
// cursor. The cursor is composite: `{city: max_seq, ...}` — one seq per
// city. To compute it correctly we need at least one event per city, so
// fetching with Limit=1 would be wrong (it would only yield the single
// most recent event, dropping every other city from the cursor).
//
// Until the supervisor exposes a dedicated head-cursor endpoint, we
// fetch events with a modest tail limit and let supervisorCursorFor
// extract per-city maxima. The tail bound keeps the bootstrap cheap on
// long-running supervisors without losing the per-city cursor coverage
// needed for reconnects. Callers that cannot tolerate missing a city
// that has been quiet for the tail window should rely on the composite
// cursor's forward-only semantics — the supervisor stream will replay
// that city's events from seq 0 on a reconnect.
const supervisorHeadCursorLimit = 256

func fetchSupervisorHeadCursor(ctx context.Context, client *genclient.ClientWithResponses) (string, error) {
	items, err := fetchSupervisorEventsWithLimit(ctx, client, "", "", supervisorHeadCursorLimit)
	if err != nil {
		return "", err
	}
	return supervisorCursorFor(items), nil
}

func eventsListError(statusCode int, problem *genclient.ErrorModel) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	err := &eventsAPIError{statusCode: statusCode}
	if problem != nil {
		if problem.Detail != nil {
			err.detail = strings.TrimSpace(*problem.Detail)
		}
		if problem.Title != nil {
			err.title = strings.TrimSpace(*problem.Title)
		}
	}
	return err
}

func printJSONLines(items any, stdout, stderr io.Writer) int {
	switch typed := items.(type) {
	case []cliWireEvent:
		for _, item := range typed {
			if err := writeJSONLValue(stdout, item); err != nil {
				fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
				return 1
			}
		}
	case []cliWireTaggedEvent:
		for _, item := range typed {
			if err := writeJSONLValue(stdout, item); err != nil {
				fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
				return 1
			}
		}
	case []genclient.EventStreamEnvelope:
		for _, item := range typed {
			if err := writeJSONLValue(stdout, item); err != nil {
				fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
				return 1
			}
		}
	case []genclient.TaggedEventStreamEnvelope:
		for _, item := range typed {
			if err := writeJSONLValue(stdout, item); err != nil {
				fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
				return 1
			}
		}
	default:
		if err := writeJSONLValue(stdout, typed); err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	return 0
}

func writeJSONLValue(stdout io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(data))
	return err
}

func filterCityEvents(items []cliWireEvent, afterSeq uint64, typeFilter string, payloadMatch map[string][]string) []cliWireEvent {
	if len(items) == 0 {
		return []cliWireEvent{}
	}
	out := make([]cliWireEvent, 0, len(items))
	for _, item := range items {
		if uint64(item.Seq) <= afterSeq {
			continue
		}
		if typeFilter != "" && item.Type != typeFilter {
			continue
		}
		if !matchPayload(item.Payload, payloadMatch) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func filterSupervisorEvents(items []cliWireTaggedEvent, typeFilter string, payloadMatch map[string][]string) []cliWireTaggedEvent {
	if len(items) == 0 {
		return []cliWireTaggedEvent{}
	}
	out := make([]cliWireTaggedEvent, 0, len(items))
	for _, item := range items {
		if typeFilter != "" && item.Type != typeFilter {
			continue
		}
		if !matchPayload(item.Payload, payloadMatch) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func filterSupervisorEventsAfterCursor(items []cliWireTaggedEvent, cursor, typeFilter string, payloadMatch map[string][]string) []cliWireTaggedEvent {
	cursors := events.ParseCursor(cursor)
	out := make([]cliWireTaggedEvent, 0, len(items))
	for _, item := range items {
		if uint64(item.Seq) <= cursors[item.City] {
			continue
		}
		if typeFilter != "" && item.Type != typeFilter {
			continue
		}
		if !matchPayload(item.Payload, payloadMatch) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// Reconnect backoff schedule for --follow streams. Short enough to
// resume quickly after a supervisor restart, capped so repeated
// failures do not DOS the server from many clients at once. The
// schedule resets after a stream session that delivered at least
// one frame.
const (
	streamReconnectInitial = 1 * time.Second
	streamReconnectMax     = 30 * time.Second
)

// streamReconnectBackoff returns the next delay given the current
// attempt count (0 = first retry). Doubles up to streamReconnectMax.
func streamReconnectBackoff(attempt int) time.Duration {
	d := streamReconnectInitial
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= streamReconnectMax {
			return streamReconnectMax
		}
	}
	return d
}

func streamCityEvents(ctx context.Context, client *genclient.ClientWithResponses, cityName string, afterSeq uint64, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) int {
	resumeSeq := afterSeq
	attempt := 0
	for {
		exitCode, newSeq, reconnect := streamCityEventsOnce(ctx, client, cityName, resumeSeq, typeFilter, payloadMatch, stopAfterMatch, stdout, stderr)
		if !reconnect {
			return exitCode
		}
		// Delivered a frame this session? Reset backoff so a long-lived
		// connection that finally drops retries quickly, not at max.
		if newSeq > resumeSeq {
			resumeSeq = newSeq
			attempt = 0
		}
		// Clean EOF in follow mode → reconnect with the latest seq,
		// backing off exponentially so we don't DOS a down supervisor.
		delay := streamReconnectBackoff(attempt)
		attempt++
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(delay):
		}
	}
}

// streamCityEventsOnce runs one connection lifetime of the city events
// stream. Returns (exitCode, lastSeenSeq, reconnect). When reconnect is
// true, the caller should retry with lastSeenSeq. reconnect is true only
// when stopAfterMatch is false and the stream ended cleanly (EOF).
func streamCityEventsOnce(ctx context.Context, client *genclient.ClientWithResponses, cityName string, afterSeq uint64, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) (int, uint64, bool) {
	after := strconv.FormatUint(afterSeq, 10)
	resp, err := client.StreamEvents(ctx, cityName, &genclient.StreamEventsParams{AfterSeq: &after})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, afterSeq, false
		}
		// In follow mode, a transient setup failure (supervisor restart,
		// brief network blip) should loop through the outer backoff
		// rather than exiting status=1. --watch is bounded by its own
		// timeout so stopAfterMatch=true still exits on setup failure.
		if !stopAfterMatch {
			fmt.Fprintf(stderr, "gc events: connect failed, retrying: %v\n", err) //nolint:errcheck
			return 0, afterSeq, true
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1, afterSeq, false
	}
	if resp.StatusCode != http.StatusOK {
		return printStreamError(resp, stderr), afterSeq, false
	}
	defer resp.Body.Close() //nolint:errcheck

	lastSeq := afterSeq
	decoder := newSSEDecoder(resp.Body)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return 0, lastSeq, false
			}
			if errors.Is(err, io.EOF) {
				if stopAfterMatch {
					fmt.Fprintln(stderr, "gc events: stream ended before a matching event arrived") //nolint:errcheck
					return 1, lastSeq, false
				}
				// Follow mode: reconnect with lastSeq.
				return 0, lastSeq, true
			}
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1, lastSeq, false
		}
		if frame.Event == "heartbeat" || strings.TrimSpace(frame.Data) == "" {
			continue
		}
		if frame.Event != "" && frame.Event != "event" {
			continue
		}

		var envelope genclient.EventStreamEnvelope
		if err := json.Unmarshal([]byte(frame.Data), &envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: decode: %v\n", err) //nolint:errcheck
			return 1, lastSeq, false
		}
		if envelope.Seq > 0 && uint64(envelope.Seq) > lastSeq {
			lastSeq = uint64(envelope.Seq)
		}
		if typeFilter != "" && envelope.Type != typeFilter {
			continue
		}
		if !matchPayload(envelope.Payload, payloadMatch) {
			continue
		}
		if err := writeJSONLValue(stdout, envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
			return 1, lastSeq, false
		}
		if stopAfterMatch {
			return 0, lastSeq, false
		}
	}
}

func streamSupervisorEvents(ctx context.Context, client *genclient.ClientWithResponses, afterCursor, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) int {
	cursor := afterCursor
	attempt := 0
	for {
		exitCode, newCursor, reconnect := streamSupervisorEventsOnce(ctx, client, cursor, typeFilter, payloadMatch, stopAfterMatch, stdout, stderr)
		if !reconnect {
			return exitCode
		}
		// Reset backoff when we advanced the cursor this session.
		if newCursor != "" && newCursor != cursor {
			cursor = newCursor
			attempt = 0
		}
		delay := streamReconnectBackoff(attempt)
		attempt++
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(delay):
		}
	}
}

func streamSupervisorEventsOnce(ctx context.Context, client *genclient.ClientWithResponses, afterCursor, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) (int, string, bool) {
	params := &genclient.StreamSupervisorEventsParams{}
	if strings.TrimSpace(afterCursor) != "" {
		params.AfterCursor = &afterCursor
	}
	resp, err := client.StreamSupervisorEvents(ctx, params)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, afterCursor, false
		}
		// Follow mode: transient connect failures loop through the
		// outer backoff. --watch (stopAfterMatch=true) is bounded by
		// its own timeout and still exits on setup failure.
		if !stopAfterMatch {
			fmt.Fprintf(stderr, "gc events: connect failed, retrying: %v\n", err) //nolint:errcheck
			return 0, afterCursor, true
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1, afterCursor, false
	}
	if resp.StatusCode != http.StatusOK {
		return printStreamError(resp, stderr), afterCursor, false
	}
	defer resp.Body.Close() //nolint:errcheck

	lastCursor := afterCursor
	cursors := events.ParseCursor(lastCursor)
	decoder := newSSEDecoder(resp.Body)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return 0, lastCursor, false
			}
			if errors.Is(err, io.EOF) {
				if stopAfterMatch {
					fmt.Fprintln(stderr, "gc events: stream ended before a matching event arrived") //nolint:errcheck
					return 1, lastCursor, false
				}
				return 0, lastCursor, true
			}
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1, lastCursor, false
		}
		if frame.Event == "heartbeat" || strings.TrimSpace(frame.Data) == "" {
			// Reconnect SSE ID carries composite cursor updates, preserved via frame.ID.
			if strings.TrimSpace(frame.ID) != "" {
				lastCursor = frame.ID
				cursors = events.ParseCursor(lastCursor)
			}
			continue
		}
		if frame.Event != "" && frame.Event != "tagged_event" {
			continue
		}

		var envelope genclient.TaggedEventStreamEnvelope
		if err := json.Unmarshal([]byte(frame.Data), &envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: decode: %v\n", err) //nolint:errcheck
			return 1, lastCursor, false
		}
		// Track per-city seq in the composite cursor so reconnects resume
		// exactly where we left off.
		if envelope.City != "" && envelope.Seq > 0 {
			if cursors == nil {
				cursors = map[string]uint64{}
			}
			if uint64(envelope.Seq) > cursors[envelope.City] {
				cursors[envelope.City] = uint64(envelope.Seq)
			}
			lastCursor = events.FormatCursor(cursors)
		}
		if typeFilter != "" && envelope.Type != typeFilter {
			continue
		}
		if !matchPayload(envelope.Payload, payloadMatch) {
			continue
		}
		if err := writeJSONLValue(stdout, envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
			return 1, lastCursor, false
		}
		if stopAfterMatch {
			return 0, lastCursor, false
		}
	}
}

func printStreamError(resp *http.Response, stderr io.Writer) int {
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: HTTP %d\n", resp.StatusCode) //nolint:errcheck
		return 1
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var problem genclient.ErrorModel
		if err := json.Unmarshal(body, &problem); err == nil {
			if problem.Detail != nil && strings.TrimSpace(*problem.Detail) != "" {
				fmt.Fprintf(stderr, "gc events: %s\n", strings.TrimSpace(*problem.Detail)) //nolint:errcheck
				return 1
			}
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	fmt.Fprintf(stderr, "gc events: %s\n", msg) //nolint:errcheck
	return 1
}

type sseFrame struct {
	Data  string
	Event string
	ID    string
}

type sseDecoder struct {
	scanner *bufio.Scanner
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &sseDecoder{scanner: scanner}
}

func (d *sseDecoder) Next() (sseFrame, error) {
	var frame sseFrame
	var sawField bool

	for d.scanner.Scan() {
		line := d.scanner.Text()
		if line == "" {
			if sawField {
				return frame, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			frame.Event = value
			sawField = true
		case "id":
			frame.ID = value
			sawField = true
		case "data":
			if frame.Data != "" {
				frame.Data += "\n"
			}
			frame.Data += value
			sawField = true
		}
	}

	if err := d.scanner.Err(); err != nil {
		return sseFrame{}, err
	}
	if sawField {
		return frame, nil
	}
	return sseFrame{}, io.EOF
}

func supervisorCursorFor(items []cliWireTaggedEvent) string {
	if len(items) == 0 {
		return ""
	}
	cursors := make(map[string]uint64, len(items))
	for _, item := range items {
		if uint64(item.Seq) > cursors[item.City] {
			cursors[item.City] = uint64(item.Seq)
		}
	}
	return events.FormatCursor(cursors)
}

// cityEnvelopesFor wraps list-endpoint WireEvents into stream-shape
// envelopes so `gc events --list` and `gc events --follow` produce
// identical JSONL output. The only structural difference between the
// two shapes is the optional Workflow projection that the stream
// attaches to bead events; list results omit it.
func cityEnvelopesFor(items []cliWireEvent) []cliEventEnvelope {
	out := make([]cliEventEnvelope, 0, len(items))
	return append(out, items...)
}

// taggedEnvelopesFor is the supervisor-scope analog of cityEnvelopesFor,
// preserving the City tag for the aggregated events stream.
func taggedEnvelopesFor(items []cliWireTaggedEvent) []cliTaggedEventEnvelope {
	out := make([]cliTaggedEventEnvelope, 0, len(items))
	return append(out, items...)
}

func matchPayload(payload any, payloadMatch map[string][]string) bool {
	if len(payloadMatch) == 0 {
		return true
	}
	if payload == nil {
		return false
	}

	switch typed := payload.(type) {
	case json.RawMessage:
		var obj map[string]any
		if err := json.Unmarshal(typed, &obj); err != nil {
			return false
		}
		return matchPayloadObject(obj, payloadMatch)
	case map[string]any:
		return matchPayloadObject(typed, payloadMatch)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return false
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			return false
		}
		return matchPayloadObject(obj, payloadMatch)
	}
}

func matchPayloadObject(obj map[string]any, payloadMatch map[string][]string) bool {
	for key, wants := range payloadMatch {
		value, ok := lookupPayloadKey(obj, key)
		if !ok {
			return false
		}
		got := payloadValueString(value)
		matched := false
		for _, want := range wants {
			if got == want {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// lookupPayloadKey resolves a key against a payload object, supporting
// dotted paths into nested map[string]any values. A flat key like "type"
// looks up at the top level; "bead.issue_type" walks obj["bead"]["issue_type"].
//
// This allows --payload-match to filter nested event payloads such as
// bead.closed (where the actually-filterable fields live under
// payload.bead.*). At each object level, an exact match for the remaining
// key wins before walking another segment, so literal dotted keys such as
// "gc.root_bead_id" under bead.metadata remain filterable.
//
// Returns (value, true) if the path resolves; (nil, false) if any segment
// is missing or an intermediate value is not an object.
func lookupPayloadKey(obj map[string]any, key string) (any, bool) {
	if value, ok := obj[key]; ok {
		return value, true
	}
	if !strings.Contains(key, ".") {
		return nil, false
	}
	parts := strings.Split(key, ".")
	current := obj
	for i, part := range parts {
		remaining := strings.Join(parts[i:], ".")
		if value, ok := current[remaining]; ok {
			return value, true
		}
		value, ok := current[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return value, true
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

func payloadValueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "null"
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(data)
	}
}

func parsePayloadMatch(args []string) (map[string][]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	m := make(map[string][]string, len(args))
	for _, arg := range args {
		i := strings.IndexByte(arg, '=')
		if i < 1 {
			return nil, fmt.Errorf("invalid --payload-match %q: expected key=value", arg)
		}
		key, value := arg[:i], arg[i+1:]
		m[key] = append(m[key], value)
	}
	return m, nil
}
