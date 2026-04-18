package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

func newEventsCmd(stdout, stderr io.Writer) *cobra.Command {
	var typeFilter string
	var sinceFlag string
	var watchFlag bool
	var followFlag bool
	var seqFlag bool
	var jsonFlag bool
	var timeoutFlag string
	var debounceFlag string
	var afterFlag uint64
	var payloadMatch []string

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the event log",
		Long: `Show the city event log with optional filtering.

Events are recorded to .gc/events.jsonl by the controller, agent
lifecycle operations, and bead mutations. Use --type and --since to
filter. Use --watch to block until matching events arrive (useful for
scripting and automation).`,
		Example: `  gc events
  gc events --type bead.created --since 1h
  gc events --watch --type convoy.closed --timeout 5m
  gc events --watch --type bead.updated --timeout 5m --debounce 60s
  gc events --follow
  gc events --seq`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if seqFlag {
				if cmdEventsSeq(stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if followFlag {
				if cmdEventsFollow(typeFilter, payloadMatch, afterFlag, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if watchFlag {
				if cmdEventsWatch(typeFilter, payloadMatch, afterFlag, timeoutFlag, debounceFlag, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if cmdEvents(typeFilter, sinceFlag, payloadMatch, jsonFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&typeFilter, "type", "", "Filter by event type (e.g. bead.created)")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "Show events since duration ago (e.g. 1h, 30m)")
	cmd.Flags().BoolVar(&watchFlag, "watch", false, "Block until matching events arrive (exits after first match)")
	cmd.Flags().BoolVar(&followFlag, "follow", false, "Continuously stream events as they arrive")
	cmd.Flags().BoolVar(&seqFlag, "seq", false, "Print the current head sequence number and exit")
	cmd.Flags().StringVar(&timeoutFlag, "timeout", "30s", "Max wait duration for --watch (e.g. 30s, 5m)")
	cmd.Flags().Uint64Var(&afterFlag, "after", 0, "Resume watching from this sequence number (0 = current head)")
	cmd.Flags().StringArrayVar(&payloadMatch, "payload-match", nil, "Filter by payload field (key=value, repeatable)")
	cmd.Flags().StringVar(&debounceFlag, "debounce", "", "Minimum collection window before returning matches in --watch mode (e.g. 60s). Prevents rapid-fire wakes from high-frequency event streams")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format (list mode only)")
	return cmd
}

// cmdEvents is the CLI entry point for viewing the event log.
func cmdEvents(typeFilter, sinceFlag string, payloadMatchArgs []string, jsonOutput bool, stdout, stderr io.Writer) int {
	pm, err := parsePayloadMatch(payloadMatchArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	ep, code := openCityEventsProvider(stderr, "gc events")
	if ep == nil {
		return code
	}
	defer ep.Close() //nolint:errcheck // best-effort
	if jsonOutput {
		return doEventsJSON(ep, typeFilter, sinceFlag, pm, stdout, stderr)
	}
	return doEvents(ep, typeFilter, sinceFlag, pm, stdout, stderr)
}

// cmdEventsSeq prints the current head sequence number.
func cmdEventsSeq(stdout, stderr io.Writer) int {
	ep, code := openCityEventsProvider(stderr, "gc events")
	if ep == nil {
		return code
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doEventsSeq(ep, stdout, stderr)
}

// doEventsSeq prints the current head sequence number. Returns 0 on
// success. Prints "0" if the event log is missing or empty.
func doEventsSeq(ep events.Provider, stdout, stderr io.Writer) int {
	seq, err := ep.LatestSeq()
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, seq) //nolint:errcheck // best-effort stdout
	return 0
}

// doEvents reads and displays events from the provider.
func doEvents(ep events.Provider, typeFilter, sinceFlag string, payloadMatch map[string][]string, stdout, stderr io.Writer) int {
	var filter events.Filter
	filter.Type = typeFilter

	if sinceFlag != "" {
		d, err := time.ParseDuration(sinceFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid --since %q: %v\n", sinceFlag, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		filter.Since = time.Now().Add(-d)
	}

	evts, err := ep.List(filter)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Apply payload-match filter if specified.
	if len(payloadMatch) > 0 {
		evts = filterEventsByPayload(evts, payloadMatch)
	}

	if len(evts) == 0 {
		fmt.Fprintln(stdout, "No events.") //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tTYPE\tACTOR\tSUBJECT\tMESSAGE\tTIME") //nolint:errcheck // best-effort stdout
	for _, e := range evts {
		msg := e.Message
		if len(msg) > 40 {
			msg = msg[:37] + "..."
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck // best-effort stdout
			e.Seq, e.Type, e.Actor, e.Subject, msg,
			e.Ts.Format("2006-01-02 15:04:05"),
		)
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// doEventsJSON reads events and outputs them as a JSON array.
func doEventsJSON(ep events.Provider, typeFilter, sinceFlag string, payloadMatch map[string][]string, stdout, stderr io.Writer) int {
	var filter events.Filter
	filter.Type = typeFilter

	if sinceFlag != "" {
		d, err := time.ParseDuration(sinceFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid --since %q: %v\n", sinceFlag, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		filter.Since = time.Now().Add(-d)
	}

	evts, err := ep.List(filter)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Apply payload-match filter if specified.
	if len(payloadMatch) > 0 {
		evts = filterEventsByPayload(evts, payloadMatch)
	}

	data, err := json.MarshalIndent(evts, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
	return 0
}

// filterEventsByPayload returns events that match all payload criteria.
func filterEventsByPayload(evts []events.Event, pm map[string][]string) []events.Event {
	var out []events.Event
	for _, e := range evts {
		if matchPayload(e.Payload, pm) {
			out = append(out, e)
		}
	}
	return out
}

// cmdEventsFollow is the CLI entry point for follow mode — continuous streaming.
func cmdEventsFollow(typeFilter string, payloadMatch []string, afterSeq uint64, stdout, stderr io.Writer) int {
	pm, err := parsePayloadMatch(payloadMatch)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	ep, code := openCityEventsProvider(stderr, "gc events")
	if ep == nil {
		return code
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doEventsFollow(ep, typeFilter, pm, afterSeq, 500*time.Millisecond, stdout, stderr)
}

// doEventsFollow continuously polls for new events and prints them as they
// arrive. Unlike doEventsWatch, it never exits on its own — it streams
// until interrupted. Events are printed as JSON lines.
func doEventsFollow(ep events.Provider, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, pollInterval time.Duration, stdout, stderr io.Writer) int { //nolint:unparam // returns 1 on error, 0 unreachable due to infinite loop
	if afterSeq == 0 {
		seq, err := ep.LatestSeq()
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		afterSeq = seq
	}

	lastSeq := afterSeq
	for {
		evts, err := ep.List(events.Filter{AfterSeq: lastSeq})
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}

		if s := lastSeqOf(evts); s > lastSeq {
			lastSeq = s
		}

		if matches := filterEvents(evts, afterSeq, typeFilter, payloadMatch); len(matches) > 0 {
			printEventsJSON(matches, stdout, stderr)
			afterSeq = lastSeq
		}

		time.Sleep(pollInterval)
	}
}

// cmdEventsWatch is the CLI entry point for watch mode.
func cmdEventsWatch(typeFilter string, payloadMatch []string, afterSeq uint64, timeoutFlag, debounceFlag string, stdout, stderr io.Writer) int {
	timeout, err := time.ParseDuration(timeoutFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: invalid --timeout %q: %v\n", timeoutFlag, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	var debounce time.Duration
	if debounceFlag != "" {
		debounce, err = time.ParseDuration(debounceFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: invalid --debounce %q: %v\n", debounceFlag, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if debounce < 0 {
			fmt.Fprintf(stderr, "gc events: --debounce must not be negative\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		if debounce >= timeout {
			fmt.Fprintf(stderr, "gc events: --debounce (%s) must be less than --timeout (%s)\n", debounce, timeout) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	pm, err := parsePayloadMatch(payloadMatch)
	if err != nil {
		fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	ep, code := openCityEventsProvider(stderr, "gc events")
	if ep == nil {
		return code
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doEventsWatch(ep, typeFilter, pm, afterSeq, timeout, debounce, 250*time.Millisecond, stdout, stderr)
}

// doEventsWatch polls the event provider for new events matching the filter.
// It blocks until matching events arrive or the timeout expires. Outputs
// matching events as JSON lines (one per line). Returns 0 always — empty
// stdout means timeout, non-empty means events found.
//
// When debounce > 0, the first matching event starts a debounce window.
// Events continue accumulating until the window closes, then all matches
// are returned at once. This prevents high-frequency event streams from
// causing rapid-fire wakes in patrol loops (see issue #458).
func doEventsWatch(ep events.Provider, typeFilter string, payloadMatch map[string][]string, afterSeq uint64, timeout, debounce, pollInterval time.Duration, stdout, stderr io.Writer) int {
	explicitAfterSeq := afterSeq > 0

	// Determine starting point.
	if afterSeq == 0 {
		seq, err := ep.LatestSeq()
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		afterSeq = seq
	}

	// When afterSeq was explicitly provided, check existing events first.
	// Some may already be past the requested sequence number.
	if explicitAfterSeq {
		evts, err := ep.List(events.Filter{AfterSeq: afterSeq})
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if matches := filterEvents(evts, afterSeq, typeFilter, payloadMatch); len(matches) > 0 {
			if debounce <= 0 {
				return printEventsJSON(matches, stdout, stderr)
			}
			// Debounce: accumulate more events before returning.
			// Full timeout as remaining — no deadline created yet in this early path.
			return doEventsWatchDebounce(ep, typeFilter, payloadMatch, matches, lastSeqOf(evts), timeout, debounce, pollInterval, stdout, stderr)
		}
	}

	deadline := time.Now().Add(timeout)
	lastSeq := afterSeq

	for {
		evts, err := ep.List(events.Filter{AfterSeq: lastSeq})
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}

		if s := lastSeqOf(evts); s > lastSeq {
			lastSeq = s
		}

		if matches := filterEvents(evts, afterSeq, typeFilter, payloadMatch); len(matches) > 0 {
			if debounce <= 0 {
				return printEventsJSON(matches, stdout, stderr)
			}
			// Debounce: accumulate more events before returning.
			remaining := time.Until(deadline)
			return doEventsWatchDebounce(ep, typeFilter, payloadMatch, matches, lastSeq, remaining, debounce, pollInterval, stdout, stderr)
		}

		if time.Now().After(deadline) {
			return 0
		}

		time.Sleep(pollInterval)
	}
}

// doEventsWatchDebounce accumulates events within a debounce window after the
// first match. Returns all accumulated matches when the window closes. If the
// overall timeout expires first, returns whatever has been accumulated so far.
func doEventsWatchDebounce(ep events.Provider, typeFilter string, payloadMatch map[string][]string, initial []events.Event, lastSeq uint64, remaining, debounce, pollInterval time.Duration, stdout, stderr io.Writer) int {
	accumulated := append([]events.Event(nil), initial...)
	// highWater tracks the debounce-internal filter cursor. filterEvents
	// returns events with Seq > highWater, so initializing to the caller's
	// lastSeq excludes the initial batch already in accumulated.
	highWater := lastSeq

	debounceDeadline := time.Now().Add(debounce)
	overallDeadline := time.Now().Add(remaining)

	// Cap debounce at overall deadline.
	if debounceDeadline.After(overallDeadline) {
		debounceDeadline = overallDeadline
	}

	for {
		untilDebounce := time.Until(debounceDeadline)
		if untilDebounce <= 0 {
			return printEventsJSON(accumulated, stdout, stderr)
		}

		sleep := pollInterval
		if untilDebounce < sleep {
			sleep = untilDebounce
		}
		time.Sleep(sleep)

		evts, err := ep.List(events.Filter{AfterSeq: lastSeq})
		if err != nil {
			fmt.Fprintf(stderr, "gc events: %v\n", err) //nolint:errcheck // best-effort stderr
			return printEventsJSON(accumulated, stdout, stderr)
		}

		if s := lastSeqOf(evts); s > lastSeq {
			lastSeq = s
		}

		if matches := filterEvents(evts, highWater, typeFilter, payloadMatch); len(matches) > 0 {
			accumulated = append(accumulated, matches...)
			highWater = lastSeq
		}
	}
}

// lastSeqOf returns the highest sequence number in a slice of events.
func lastSeqOf(evts []events.Event) uint64 {
	var highest uint64
	for _, e := range evts {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	return highest
}

// filterEvents returns events with Seq > afterSeq that match typeFilter
// and all payloadMatch criteria.
func filterEvents(evts []events.Event, afterSeq uint64, typeFilter string, payloadMatch map[string][]string) []events.Event {
	var matches []events.Event
	for _, e := range evts {
		if e.Seq <= afterSeq {
			continue
		}
		if typeFilter != "" && e.Type != typeFilter {
			continue
		}
		if !matchPayload(e.Payload, payloadMatch) {
			continue
		}
		matches = append(matches, e)
	}
	return matches
}

// matchPayload checks whether the event payload satisfies all key constraints.
// Same key with multiple values = OR (any value matches).
// Different keys = AND (all keys must match).
// Returns true if payloadMatch is empty.
func matchPayload(payload json.RawMessage, payloadMatch map[string][]string) bool {
	if len(payloadMatch) == 0 {
		return true
	}
	if len(payload) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	for k, wants := range payloadMatch {
		raw, ok := obj[k]
		if !ok {
			return false
		}
		// Resolve the actual value (try string unquote, fall back to raw).
		var got string
		if json.Unmarshal(raw, &got) != nil {
			got = string(raw)
		}
		// OR: any value in the list matches.
		matched := false
		for _, w := range wants {
			if got == w {
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

// parsePayloadMatch parses "key=value" strings into a multi-map.
// Same key repeated = OR (match any value for that key).
// Different keys = AND (all keys must match).
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
		k, v := arg[:i], arg[i+1:]
		m[k] = append(m[k], v)
	}
	return m, nil
}

// printEventsJSON writes events as JSON lines to stdout. Returns 0.
func printEventsJSON(evts []events.Event, stdout, stderr io.Writer) int {
	for _, e := range evts {
		data, err := json.Marshal(e)
		if err != nil {
			fmt.Fprintf(stderr, "gc events: marshal: %v\n", err) //nolint:errcheck // best-effort stderr
			continue
		}
		fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
	}
	return 0
}
