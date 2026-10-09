package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderOutcomeFile is the per-run file an exec order may write its
// skipped/partial declaration to (see orders.ExecOutcomeFileEnv). It is
// created before the command runs and removed after its result is read.
type orderOutcomeFile struct {
	path string
}

// newOrderOutcomeFile creates an empty outcome file for one exec run.
func newOrderOutcomeFile() (*orderOutcomeFile, error) {
	f, err := os.CreateTemp("", "gc-order-outcome-*.json")
	if err != nil {
		return nil, fmt.Errorf("creating exec order outcome file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("closing exec order outcome file: %w", err)
	}
	return &orderOutcomeFile{path: path}, nil
}

// envEntry is the exec env entry that hands the file to the order.
func (f *orderOutcomeFile) envEntry() string {
	return orders.ExecOutcomeFileEnv + "=" + f.path
}

// read decodes the order's declaration. ok is false when the order wrote
// nothing.
func (f *orderOutcomeFile) read() (orders.ExecOutcome, bool, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return orders.ExecOutcome{}, false, fmt.Errorf("reading exec order outcome file: %w", err)
	}
	return orders.DecodeExecOutcome(data)
}

// remove deletes the file; a file the order already removed is not an error.
func (f *orderOutcomeFile) remove() error {
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing exec order outcome file: %w", err)
	}
	return nil
}

// orderSkippedEventFor builds the order.skipped event for a declaration.
func orderSkippedEventFor(scoped string, out orders.ExecOutcome) events.Event {
	payload := events.OrderSkippedPayload{
		OrderName: scoped,
		Outcome:   string(out.Outcome),
		Reason:    out.Reason,
		Scopes:    make([]events.OrderSkippedScope, 0, len(out.Scopes)),
	}
	names := make([]string, 0, len(out.Scopes))
	for _, scope := range out.Scopes {
		payload.Scopes = append(payload.Scopes, events.OrderSkippedScope{Scope: scope.Scope, Reason: scope.Reason})
		names = append(names, scope.Scope+" ("+scope.Reason+")")
	}
	message := fmt.Sprintf("%s: %s", out.Outcome, out.Reason)
	if len(names) > 0 {
		message += "; not run: " + strings.Join(names, ", ")
	}
	return events.Event{
		Type:    events.OrderSkipped,
		Actor:   "controller",
		Subject: scoped,
		Message: message,
		Payload: events.OrderSkippedPayloadJSON(payload),
	}
}

// orderSkippedEventForUnreadable reports a declaration the controller could
// not decode, so a broken declaration is visible rather than dropped.
func orderSkippedEventForUnreadable(scoped string, decodeErr error) events.Event {
	payload := events.OrderSkippedPayload{
		OrderName: scoped,
		Outcome:   "unknown",
		Reason:    decodeErr.Error(),
		Scopes:    []events.OrderSkippedScope{},
	}
	return events.Event{
		Type:    events.OrderSkipped,
		Actor:   "controller",
		Subject: scoped,
		Message: "unreadable outcome declaration: " + decodeErr.Error(),
		Payload: events.OrderSkippedPayloadJSON(payload),
	}
}

// recordOrderOutcome reads a successful run's declaration and records the
// matching order.skipped event, if any. Decode failures are logged to stderr
// and recorded with outcome "unknown".
func recordOrderOutcome(rec events.Recorder, stderr io.Writer, scoped string, f *orderOutcomeFile) {
	out, ok, err := f.read()
	if err != nil {
		logDispatchError(stderr, "gc: order %s: unreadable outcome declaration: %v", scoped, err)
		rec.Record(orderSkippedEventForUnreadable(scoped, err))
		return
	}
	if !ok {
		return
	}
	rec.Record(orderSkippedEventFor(scoped, out))
}

// printOrderRunOutcome prints a manual `gc order run`'s skipped/partial
// declaration so the operator sees what did not run.
func printOrderRunOutcome(f *orderOutcomeFile, name string, stdout, stderr io.Writer) {
	out, ok, err := f.read()
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: order %q wrote an unreadable outcome declaration: %v\n", name, err) //nolint:errcheck // best-effort stderr
		return
	}
	if !ok {
		return
	}
	fmt.Fprintf(stdout, "Order %q %s: %s\n", name, out.Outcome, out.Reason) //nolint:errcheck // best-effort stdout
	for _, scope := range out.Scopes {
		fmt.Fprintf(stdout, "  not run: %s (%s)\n", scope.Scope, scope.Reason) //nolint:errcheck // best-effort stdout
	}
}
