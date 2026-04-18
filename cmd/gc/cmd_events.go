package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

type eventsAPIScope struct {
	apiURL   string
	cityName string
}

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
	cmd.Flags().StringArrayVar(&payloadMatch, "payload-match", nil, "Filter by payload field (key=value, repeatable)")
	return cmd
}

func cmdEvents(apiURLOverride, typeFilter, sinceFlag string, payloadMatchArgs []string, stdout, stderr io.Writer) int {
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
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
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	if err := validateEventsCursor(scope, afterSeq, afterCursor); err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	return doEventsFollow(scope, typeFilter, pm, afterSeq, afterCursor, stdout, stderr)
}

func cmdEventsWatch(apiURLOverride, typeFilter string, payloadMatchArgs []string, afterSeq uint64, afterCursor, timeoutFlag string, stdout, stderr io.Writer) int {
	scope, code := openEventsScope(apiURLOverride, stderr)
	if code != 0 {
		return code
	}
	if err := validateEventsCursor(scope, afterSeq, afterCursor); err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
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
		return eventsAPIScope{
			apiURL:   strings.TrimRight(override, "/"),
			cityName: cityName,
		}, nil
	}

	if supervisorAliveHook() != 0 {
		baseURL, err := supervisorAPIBaseURL()
		if err != nil {
			return eventsAPIScope{}, err
		}
		return eventsAPIScope{
			apiURL:   strings.TrimRight(baseURL, "/"),
			cityName: cityName,
		}, nil
	}

	if cityPath == "" {
		return eventsAPIScope{}, fmt.Errorf(
			"could not auto-discover the supervisor API; start the supervisor with %q or pass --api explicitly",
			"gc supervisor start",
		)
	}
	if _, ok := discoverStandaloneDashboardAPI(cfg); ok {
		return eventsAPIScope{}, fmt.Errorf(
			"gc events requires the supervisor API; standalone city APIs do not expose /v0/city/{cityName}/events. Start the supervisor with %q or pass --api to a supervisor endpoint explicitly",
			"gc supervisor start",
		)
	}
	return eventsAPIScope{}, fmt.Errorf(
		"could not auto-discover the supervisor API for %q; start the supervisor with %q or pass --api explicitly",
		cityPath,
		"gc supervisor start",
	)
}

func resolvedEventsCityName(cityPath string, cfg *config.City) string {
	if cfg != nil && strings.TrimSpace(cfg.Workspace.Name) != "" {
		return strings.TrimSpace(cfg.Workspace.Name)
	}
	if strings.TrimSpace(cityPath) == "" {
		return ""
	}
	return resolveCityName("", cityPath)
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

func doEvents(scope eventsAPIScope, typeFilter, sinceFlag string, payloadMatch map[string][]string, stdout, stderr io.Writer) int {
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

	items, _, err := fetchCityEvents(ctx, client, scope.cityName, typeFilter, sinceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	items = filterCityEvents(items, 0, typeFilter, payloadMatch)
	return printJSONLines(items, stdout, stderr)
}

func doEventsSeq(scope eventsAPIScope, stdout, stderr io.Writer) int {
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
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, index) //nolint:errcheck
	return 0
}

func doEventsFollow(scope eventsAPIScope, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, afterCursor string, stdout, stderr io.Writer) int {
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
		head, err := fetchCityHeadIndex(ctx, client, scope.cityName)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		resumeSeq, err = strconv.ParseUint(head, 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid X-GC-Index %q\n", head) //nolint:errcheck
			return 1
		}
	}
	return streamCityEvents(ctx, client, scope.cityName, resumeSeq, typeFilter, payloadMatch, false, stdout, stderr)
}

func doEventsWatch(scope eventsAPIScope, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, afterCursor string, timeout time.Duration, stdout, stderr io.Writer) int {
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
		items, _, err := fetchCityEvents(ctx, client, scope.cityName, "", "")
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		matches := filterCityEvents(items, resumeSeq, typeFilter, payloadMatch)
		if len(matches) > 0 {
			return printJSONLines(cityEnvelopesFor(matches), stdout, stderr)
		}
	} else {
		head, err := fetchCityHeadIndex(ctx, client, scope.cityName)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
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

func fetchCityEvents(ctx context.Context, client *genclient.ClientWithResponses, cityName, typeFilter, sinceFlag string) ([]genclient.Event, string, error) {
	limit := int64(500)
	var all []genclient.Event
	var cursor *string
	index := "0"

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
			return nil, "", fmt.Errorf("request failed: %w", err)
		}
		if err := eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
			return nil, "", err
		}
		if resp.HTTPResponse != nil {
			header := strings.TrimSpace(resp.HTTPResponse.Header.Get("X-GC-Index"))
			if header != "" {
				index = header
			}
		}
		if resp.JSON200 == nil || resp.JSON200.Items == nil {
			return all, index, nil
		}
		all = append(all, *resp.JSON200.Items...)
		if resp.JSON200.NextCursor == nil || strings.TrimSpace(*resp.JSON200.NextCursor) == "" {
			return all, index, nil
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
		return "", fmt.Errorf("request failed: %w", err)
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

func fetchSupervisorEvents(ctx context.Context, client *genclient.ClientWithResponses, typeFilter, sinceFlag string) ([]genclient.TaggedEvent, error) {
	params := &genclient.GetV0EventsParams{}
	if strings.TrimSpace(typeFilter) != "" {
		params.Type = &typeFilter
	}
	if strings.TrimSpace(sinceFlag) != "" {
		params.Since = &sinceFlag
	}
	resp, err := client.GetV0EventsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if err := eventsListError(resp.StatusCode(), resp.ApplicationproblemJSONDefault); err != nil {
		return nil, err
	}
	if resp.JSON200 == nil || resp.JSON200.Items == nil {
		return []genclient.TaggedEvent{}, nil
	}
	return *resp.JSON200.Items, nil
}

func fetchSupervisorHeadCursor(ctx context.Context, client *genclient.ClientWithResponses) (string, error) {
	items, err := fetchSupervisorEvents(ctx, client, "", "")
	if err != nil {
		return "", err
	}
	return supervisorCursorFor(items), nil
}

func eventsListError(statusCode int, problem *genclient.ErrorModel) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	if problem != nil && problem.Detail != nil && strings.TrimSpace(*problem.Detail) != "" {
		return errors.New(strings.TrimSpace(*problem.Detail))
	}
	if problem != nil && problem.Title != nil && strings.TrimSpace(*problem.Title) != "" {
		return errors.New(strings.TrimSpace(*problem.Title))
	}
	if statusCode == 0 {
		return fmt.Errorf("request failed")
	}
	return fmt.Errorf("API returned HTTP %d", statusCode)
}

func printJSONLines(items any, stdout, stderr io.Writer) int {
	switch typed := items.(type) {
	case []genclient.Event:
		for _, item := range typed {
			if err := writeJSONLValue(stdout, item); err != nil {
				fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
				return 1
			}
		}
	case []genclient.TaggedEvent:
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

func filterCityEvents(items []genclient.Event, afterSeq uint64, typeFilter string, payloadMatch map[string][]string) []genclient.Event {
	if len(items) == 0 {
		return []genclient.Event{}
	}
	out := make([]genclient.Event, 0, len(items))
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

func filterSupervisorEvents(items []genclient.TaggedEvent, typeFilter string, payloadMatch map[string][]string) []genclient.TaggedEvent {
	if len(items) == 0 {
		return []genclient.TaggedEvent{}
	}
	out := make([]genclient.TaggedEvent, 0, len(items))
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

func filterSupervisorEventsAfterCursor(items []genclient.TaggedEvent, cursor, typeFilter string, payloadMatch map[string][]string) []genclient.TaggedEvent {
	cursors := events.ParseCursor(cursor)
	out := make([]genclient.TaggedEvent, 0, len(items))
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

func streamCityEvents(ctx context.Context, client *genclient.ClientWithResponses, cityName string, afterSeq uint64, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) int {
	after := strconv.FormatUint(afterSeq, 10)
	resp, err := client.StreamEvents(ctx, cityName, &genclient.StreamEventsParams{AfterSeq: &after})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		return printStreamError(resp, stderr)
	}
	defer resp.Body.Close() //nolint:errcheck

	decoder := newSSEDecoder(resp.Body)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return 0
			}
			if err == io.EOF {
				fmt.Fprintln(stderr, "gc events: stream ended before a matching event arrived") //nolint:errcheck
				return 1
			}
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
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
			return 1
		}
		if typeFilter != "" && envelope.Type != typeFilter {
			continue
		}
		if !matchPayload(envelope.Payload, payloadMatch) {
			continue
		}
		if err := writeJSONLValue(stdout, envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
			return 1
		}
		if stopAfterMatch {
			return 0
		}
	}
}

func streamSupervisorEvents(ctx context.Context, client *genclient.ClientWithResponses, afterCursor, typeFilter string, payloadMatch map[string][]string, stopAfterMatch bool, stdout, stderr io.Writer) int {
	params := &genclient.StreamSupervisorEventsParams{}
	if strings.TrimSpace(afterCursor) != "" {
		params.AfterCursor = &afterCursor
	}
	resp, err := client.StreamSupervisorEvents(ctx, params)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0
		}
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		return printStreamError(resp, stderr)
	}
	defer resp.Body.Close() //nolint:errcheck

	decoder := newSSEDecoder(resp.Body)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return 0
			}
			if err == io.EOF {
				fmt.Fprintln(stderr, "gc events: stream ended before a matching event arrived") //nolint:errcheck
				return 1
			}
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck
			return 1
		}
		if frame.Event == "heartbeat" || strings.TrimSpace(frame.Data) == "" {
			continue
		}
		if frame.Event != "" && frame.Event != "tagged_event" {
			continue
		}

		var envelope genclient.TaggedEventStreamEnvelope
		if err := json.Unmarshal([]byte(frame.Data), &envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: decode: %v\n", err) //nolint:errcheck
			return 1
		}
		if typeFilter != "" && envelope.Type != typeFilter {
			continue
		}
		if !matchPayload(envelope.Payload, payloadMatch) {
			continue
		}
		if err := writeJSONLValue(stdout, envelope); err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck
			return 1
		}
		if stopAfterMatch {
			return 0
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

func supervisorCursorFor(items []genclient.TaggedEvent) string {
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

func cityEnvelopesFor(items []genclient.Event) []genclient.EventStreamEnvelope {
	out := make([]genclient.EventStreamEnvelope, 0, len(items))
	for _, item := range items {
		out = append(out, genclient.EventStreamEnvelope{
			Actor:   item.Actor,
			Message: item.Message,
			Payload: item.Payload,
			Seq:     item.Seq,
			Subject: item.Subject,
			Ts:      item.Ts,
			Type:    item.Type,
		})
	}
	return out
}

func taggedEnvelopesFor(items []genclient.TaggedEvent) []genclient.TaggedEventStreamEnvelope {
	out := make([]genclient.TaggedEventStreamEnvelope, 0, len(items))
	for _, item := range items {
		out = append(out, genclient.TaggedEventStreamEnvelope{
			Actor:   item.Actor,
			City:    item.City,
			Message: item.Message,
			Payload: item.Payload,
			Seq:     item.Seq,
			Subject: item.Subject,
			Ts:      item.Ts,
			Type:    item.Type,
		})
	}
	return out
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
		value, ok := obj[key]
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
