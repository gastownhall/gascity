package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestPreparedStartOversizedPromptHardFailsBeforeLaunch covers the
// integration path for gastownhall/gascity ga-q8wgom.1.1: an oversized
// startup prompt bound for a runtime with no usable post-start delivery
// (subprocess execs the rendered command as a single "sh -c" argv element,
// see internal/runtime/subprocess/subprocess.go) must hard-fail inside
// buildPreparedStart, BEFORE any runtime.Config carrying the giant prompt
// is ever constructed for Provider.Start/exec.Command to consume.
func TestPreparedStartOversizedPromptHardFailsBeforeLaunch(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "subprocess",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err == nil {
		t.Fatalf("buildPreparedStart() error = nil, want a hard-fail error for an oversized prompt on an unsupported runtime")
	}
	if !errors.Is(err, errOversizedPromptUnsupportedRuntime) {
		t.Errorf("buildPreparedStart() error = %v, want errors.Is(err, errOversizedPromptUnsupportedRuntime)", err)
	}
	if strings.Contains(err.Error(), oversizedPrompt) {
		t.Errorf("buildPreparedStart() error leaks prompt content")
	}
	if prepared != nil {
		if len(prepared.cfg.PromptSuffix) > 0 {
			t.Errorf("buildPreparedStart() must not construct a runtime.Config carrying the oversized prompt in argv, got PromptSuffix len=%d", len(prepared.cfg.PromptSuffix))
		}
		if strings.Contains(prepared.cfg.Command, oversizedPrompt) {
			t.Errorf("buildPreparedStart() must not fold the oversized prompt into cfg.Command")
		}
	}
}

// TestPreparedStartOversizedPromptFallsBackOnNudgeCapableRuntime covers the
// companion path: an oversized prompt bound for a runtime with a working
// post-start Nudge (tmux) must still succeed, routing the prompt through the
// nudge instead of argv, rather than hard-failing.
func TestPreparedStartOversizedPromptFallsBackOnNudgeCapableRuntime(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "tmux",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart() unexpected error for a nudge-capable runtime: %v", err)
	}
	if len(prepared.cfg.PromptSuffix) > 0 {
		t.Errorf("buildPreparedStart() must not place an oversized prompt in argv even on a nudge-capable runtime, got PromptSuffix len=%d", len(prepared.cfg.PromptSuffix))
	}
	if !strings.Contains(prepared.cfg.Nudge, oversizedPrompt) {
		t.Errorf("buildPreparedStart() must route the oversized prompt through cfg.Nudge on a nudge-capable runtime")
	}
	if !prepared.promptDelivered {
		t.Errorf("buildPreparedStart() promptDelivered = false, want true (delivery falls back to nudge, it does not become non-delivery)")
	}
}

// TestPreparedStartOversizedPromptByteExactWithEmbeddedQuotesAndNewlines
// covers the realistic Claude/Codex-family regression called for by
// ga-q8wgom.1.1: a 100-150KB prompt containing embedded single quotes, double
// quotes, and newlines (representative of real assistant output and code
// blocks) must still reach the nudge-fallback path byte-for-byte, with zero
// prompt bytes anywhere in argv/Command, regardless of the special
// characters it contains.
func TestPreparedStartOversizedPromptByteExactWithEmbeddedQuotesAndNewlines(t *testing.T) {
	unit := "it's \"quoted\" and\nmultiline\n"
	oversizedPrompt := repeatToBytes(unit, maxPromptSuffixRawBytes+25000) // ~125KB: inside the 100-150KB range

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "codex",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "tmux",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart() unexpected error: %v", err)
	}
	if len(prepared.cfg.PromptSuffix) > 0 || prepared.cfg.PromptFlag != "" {
		t.Errorf("buildPreparedStart() must not place any prompt bytes in argv, got PromptSuffix len=%d PromptFlag=%q", len(prepared.cfg.PromptSuffix), prepared.cfg.PromptFlag)
	}
	if strings.Contains(prepared.cfg.Command, unit) {
		t.Errorf("buildPreparedStart() must not fold the prompt into cfg.Command")
	}
	if prepared.cfg.Nudge != oversizedPrompt {
		t.Errorf("buildPreparedStart() must deliver the prompt byte-exact through the nudge (no escaping/mangling of embedded quotes or newlines); got len=%d, want len=%d", len(prepared.cfg.Nudge), len(oversizedPrompt))
	}
	if !prepared.promptDelivered {
		t.Errorf("buildPreparedStart() promptDelivered = false, want true")
	}
}

// captureHandler is an in-memory slog.Handler that records every emitted
// slog.Record verbatim (not funneled through a text/JSON encoder), so a test
// can assert on individual field values by key and scan every field's
// rendered value for prompt-content leakage without depending on any
// particular serialization format.
type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	// Clone: slog's Handler contract requires copying a Record before
	// retaining it past the Handle call, since the caller may reuse/mutate it.
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// oversizedPromptDeliveryLogRecords returns every captured record carrying
// an "effective_mode" field -- the key logOversizedPromptDelivery always sets
// -- so a test can find its record(s) regardless of what else (if anything)
// logs during the same call.
func oversizedPromptDeliveryLogRecords(records []slog.Record) []slog.Record {
	var matches []slog.Record
	for _, r := range records {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "effective_mode" {
				found = true
			}
			return true
		})
		if found {
			matches = append(matches, r)
		}
	}
	return matches
}

// wantOversizedPromptLogFields are the structured fields every
// oversized-prompt delivery decision (fallback or hard-fail) must log, per
// gastownhall/gascity ga-q8wgom.1.1 exit criterion 5.
var wantOversizedPromptLogFields = []string{
	"session", "agent", "configured_mode", "effective_mode", "runtime",
	"raw_bytes", "argv_bytes", "raw_threshold", "argv_threshold",
}

// assertNoPromptLeak fails the test if the oversized prompt appears anywhere
// in any captured record's message or field values -- scanning every record
// from the call, not just the matched oversized-prompt-delivery record, so a
// leak through unrelated logging during the same call is still caught.
func assertNoPromptLeak(t *testing.T, records []slog.Record, oversizedPrompt string) {
	t.Helper()
	for _, r := range records {
		if strings.Contains(r.Message, oversizedPrompt) {
			t.Errorf("log record message contains raw prompt content: %q", r.Message)
		}
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), oversizedPrompt) {
				t.Errorf("log record field %q contains raw prompt content", a.Key)
			}
			return true
		})
	}
}

// recordFields collects a slog.Record's attributes into a key->Value map for
// assertions.
func recordFields(rec slog.Record) map[string]slog.Value {
	fields := make(map[string]slog.Value, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value
		return true
	})
	return fields
}

// TestPreparedStartOversizedPromptHardFailLogsStructuredRecordWithoutPromptContent
// covers gastownhall/gascity ga-q8wgom.1.1 exit criterion 5 for the hard-fail
// path: the decision to hard-fail an oversized prompt on a runtime with no
// fallback delivery must produce one structured log record with the
// documented fields, and the raw prompt content must never appear in any
// record emitted during the call.
func TestPreparedStartOversizedPromptHardFailLogsStructuredRecordWithoutPromptContent(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	handler := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			InstanceName:             "worker-1",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "subprocess",
		},
	}

	if _, _, err := buildPreparedStart(candidate, &config.City{}, store); err == nil {
		t.Fatalf("buildPreparedStart() error = nil, want a hard-fail error for an oversized prompt on an unsupported runtime")
	}

	assertNoPromptLeak(t, handler.records, oversizedPrompt)

	matches := oversizedPromptDeliveryLogRecords(handler.records)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 oversized-prompt-delivery log record for the hard-fail path, got %d (of %d records total)", len(matches), len(handler.records))
	}
	rec := matches[0]
	if rec.Level != slog.LevelError {
		t.Errorf("hard-fail log level = %v, want %v", rec.Level, slog.LevelError)
	}

	fields := recordFields(rec)
	for _, key := range wantOversizedPromptLogFields {
		if _, ok := fields[key]; !ok {
			t.Errorf("hard-fail log record missing expected field %q", key)
		}
	}
	if v := fields["effective_mode"].String(); v != "hard-fail" {
		t.Errorf("effective_mode = %q, want %q", v, "hard-fail")
	}
	if v := fields["session"].String(); v != "worker" {
		t.Errorf("session = %q, want %q", v, "worker")
	}
	if v := fields["agent"].String(); v != "worker-1" {
		t.Errorf("agent = %q, want %q", v, "worker-1")
	}
	if v := fields["runtime"].String(); v != "subprocess" {
		t.Errorf("runtime = %q, want %q", v, "subprocess")
	}
	if v := fields["raw_bytes"].Int64(); v != int64(len(oversizedPrompt)) {
		t.Errorf("raw_bytes = %d, want %d", v, len(oversizedPrompt))
	}
	if v := fields["raw_threshold"].Int64(); v != int64(maxPromptSuffixRawBytes) {
		t.Errorf("raw_threshold = %d, want %d", v, maxPromptSuffixRawBytes)
	}
	if v := fields["argv_threshold"].Int64(); v != int64(maxPromptSuffixQuotedBytes) {
		t.Errorf("argv_threshold = %d, want %d", v, maxPromptSuffixQuotedBytes)
	}
}

// TestPreparedStartOversizedPromptNudgeFallbackLogsStructuredRecordWithoutPromptContent
// covers gastownhall/gascity ga-q8wgom.1.1 exit criterion 5 for the
// nudge-fallback path: the decision to reroute an oversized prompt through a
// nudge-capable runtime must produce one structured log record with the
// documented fields, and the raw prompt content must never appear in any
// record emitted during the call.
func TestPreparedStartOversizedPromptNudgeFallbackLogsStructuredRecordWithoutPromptContent(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	handler := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			InstanceName:             "worker-1",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "tmux",
		},
	}

	if _, _, err := buildPreparedStart(candidate, &config.City{}, store); err != nil {
		t.Fatalf("buildPreparedStart() unexpected error for a nudge-capable runtime: %v", err)
	}

	assertNoPromptLeak(t, handler.records, oversizedPrompt)

	matches := oversizedPromptDeliveryLogRecords(handler.records)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 oversized-prompt-delivery log record for the nudge-fallback path, got %d (of %d records total)", len(matches), len(handler.records))
	}
	rec := matches[0]
	if rec.Level != slog.LevelWarn {
		t.Errorf("nudge-fallback log level = %v, want %v", rec.Level, slog.LevelWarn)
	}

	fields := recordFields(rec)
	for _, key := range wantOversizedPromptLogFields {
		if _, ok := fields[key]; !ok {
			t.Errorf("nudge-fallback log record missing expected field %q", key)
		}
	}
	if v := fields["effective_mode"].String(); v != "nudge-fallback" {
		t.Errorf("effective_mode = %q, want %q", v, "nudge-fallback")
	}
	if v := fields["session"].String(); v != "worker" {
		t.Errorf("session = %q, want %q", v, "worker")
	}
	if v := fields["agent"].String(); v != "worker-1" {
		t.Errorf("agent = %q, want %q", v, "worker-1")
	}
	if v := fields["runtime"].String(); v != "tmux" {
		t.Errorf("runtime = %q, want %q", v, "tmux")
	}
	if v := fields["raw_bytes"].Int64(); v != int64(len(oversizedPrompt)) {
		t.Errorf("raw_bytes = %d, want %d", v, len(oversizedPrompt))
	}
}
