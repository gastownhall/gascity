// Package fake defines the scripted fake-worker profiles used by worker tests.
package fake

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Default worker runtime timings for the standalone fake helper.
const (
	DefaultStartupTimeout = 30 * time.Second
	DefaultPollInterval   = 100 * time.Millisecond
)

// Event is one structured record emitted by the standalone fake worker.
type Event struct {
	Time        time.Time         `json:"time"`
	Kind        string            `json:"kind"`
	Provider    string            `json:"provider"`
	Scenario    string            `json:"scenario"`
	Step        string            `json:"step,omitempty"`
	State       string            `json:"state,omitempty"`
	Message     string            `json:"message,omitempty"`
	Path        string            `json:"path,omitempty"`
	Sequence    int               `json:"sequence,omitempty"`
	Transcript  *TranscriptEvent  `json:"transcript,omitempty"`
	Interaction *InteractionEvent `json:"interaction,omitempty"`
	Input       *InputEvent       `json:"input,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Runner executes a scripted fake-worker scenario.
type Runner struct {
	Now func() time.Time
}

// Run executes the configured fake-worker scenario and writes event output.
func (r Runner) Run(ctx context.Context, cfg HelperConfig, stdout io.Writer) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}

	profile := cfg.resolvedProfile()
	output, err := openOutputs(cfg.Output)
	if err != nil {
		return err
	}
	defer output.close()
	eventSink := io.MultiWriter(stdout, output.eventLog)

	startFile := firstNonEmpty(os.Getenv("GC_FAKE_WORKER_START_FILE"), cfg.Control.StartFile)
	if profile.Launch.Startup.RequireControlFile || startFile != "" {
		if err := writeEvent(eventSink, Event{
			Time:     now().UTC(),
			Kind:     "control_waiting",
			Provider: profile.Provider,
			Scenario: cfg.Scenario.Name,
			Path:     startFile,
			Message:  "waiting for startup control",
		}); err != nil {
			return err
		}
		if err := waitForControl(ctx, startFile, "", cfg.Control.timeout(), cfg.Control.pollInterval()); err != nil {
			return err
		}
		if err := writeEvent(eventSink, Event{
			Time:     now().UTC(),
			Kind:     "control_observed",
			Provider: profile.Provider,
			Scenario: cfg.Scenario.Name,
			Path:     startFile,
			Message:  "startup control observed",
		}); err != nil {
			return err
		}
	}

	seq := 0
	for i, step := range cfg.Scenario.Steps {
		if err := sleepContext(ctx, step.Delay); err != nil {
			return err
		}

		stepID := step.ID
		if stepID == "" {
			stepID = fmt.Sprintf("step-%02d", i+1)
		}
		switch step.Action {
		case "startup":
			state := firstNonEmpty(step.State, profile.Launch.Startup.Outcome, "ready")
			if err := writeState(output.statePath, state); err != nil {
				return err
			}
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "state_transition",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				State:    state,
				Message:  step.Message,
				Metadata: step.Metadata,
			}); err != nil {
				return err
			}
		case "emit_state":
			if err := writeState(output.statePath, step.State); err != nil {
				return err
			}
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "state_transition",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				State:    step.State,
				Message:  step.Message,
				Metadata: step.Metadata,
			}); err != nil {
				return err
			}
		case "append_transcript":
			seq++
			if err := appendTranscript(output.transcriptPath, seq, now().UTC(), step.Transcript); err != nil {
				return err
			}
			transcript := step.Transcript
			if err := writeEvent(eventSink, Event{
				Time:       now().UTC(),
				Kind:       "transcript_append",
				Provider:   profile.Provider,
				Scenario:   cfg.Scenario.Name,
				Step:       stepID,
				Sequence:   seq,
				Transcript: &transcript,
				Message:    step.Message,
				Metadata:   step.Metadata,
			}); err != nil {
				return err
			}
		case "emit_interaction":
			interaction := step.Interaction
			state := firstNonEmpty(interaction.State, step.State)
			if state != "" {
				if err := writeState(output.statePath, state); err != nil {
					return err
				}
			}
			if err := writeEvent(eventSink, Event{
				Time:        now().UTC(),
				Kind:        "interaction",
				Provider:    profile.Provider,
				Scenario:    cfg.Scenario.Name,
				Step:        stepID,
				State:       state,
				Message:     step.Message,
				Interaction: &interaction,
				Metadata:    mergeMetadata(step.Metadata, interaction.Metadata),
			}); err != nil {
				return err
			}
		case "input":
			input := step.Input
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "input_waiting",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				Path:     input.Path,
				Message:  step.Message,
				Input:    &input,
				Metadata: mergeMetadata(step.Metadata, input.Metadata),
			}); err != nil {
				return err
			}
			observed, err := waitForInput(ctx, input.Path, input.Expect, cfg.Control.timeout(), cfg.Control.pollInterval())
			if err != nil {
				return err
			}
			input.Observed = observed
			if input.ReceiptPath != "" {
				if err := writeFile(input.ReceiptPath, observed, false); err != nil {
					return err
				}
			}
			if input.EchoPath != "" {
				if err := writeFile(input.EchoPath, observed, false); err != nil {
					return err
				}
			}
			if step.State != "" {
				if err := writeState(output.statePath, step.State); err != nil {
					return err
				}
			}
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "input_received",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				State:    step.State,
				Path:     input.Path,
				Message:  step.Message,
				Input:    &input,
				Metadata: mergeMetadata(step.Metadata, input.Metadata),
			}); err != nil {
				return err
			}
		case "write_file":
			if err := writeFile(step.Path, step.Content, step.Append); err != nil {
				return err
			}
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "file_write",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				Path:     step.Path,
				Message:  step.Message,
				Metadata: step.Metadata,
			}); err != nil {
				return err
			}
		case "wait_for_control":
			if err := waitForControl(ctx, step.Path, step.ExpectControl, cfg.Control.timeout(), cfg.Control.pollInterval()); err != nil {
				return err
			}
			if err := writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "control_observed",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				Path:     step.Path,
				Message:  step.Message,
				Metadata: step.Metadata,
			}); err != nil {
				return err
			}
		case "sleep":
			continue
		case "exit":
			if step.State != "" {
				if err := writeState(output.statePath, step.State); err != nil {
					return err
				}
			}
			return writeEvent(eventSink, Event{
				Time:     now().UTC(),
				Kind:     "exit",
				Provider: profile.Provider,
				Scenario: cfg.Scenario.Name,
				Step:     stepID,
				State:    step.State,
				Message:  step.Message,
				Metadata: step.Metadata,
			})
		}
	}
	return nil
}

type outputFiles struct {
	eventLog       io.Writer
	eventLogCloser io.Closer
	transcriptPath string
	statePath      string
}

func openOutputs(output OutputSpec) (*outputFiles, error) {
	out := &outputFiles{
		eventLog:       io.Discard,
		transcriptPath: firstNonEmpty(os.Getenv("GC_FAKE_WORKER_TRANSCRIPT_PATH"), output.TranscriptPath),
		statePath:      firstNonEmpty(os.Getenv("GC_FAKE_WORKER_STATE_PATH"), output.StatePath),
	}
	eventLogPath := firstNonEmpty(os.Getenv("GC_FAKE_WORKER_EVENT_LOG_PATH"), output.EventLogPath)
	if eventLogPath != "" {
		fh, err := openAppendFile(eventLogPath)
		if err != nil {
			return nil, err
		}
		out.eventLog = fh
		out.eventLogCloser = fh
	}
	return out, nil
}

func (o *outputFiles) close() {
	if o.eventLogCloser != nil {
		_ = o.eventLogCloser.Close()
	}
}

func (c HelperConfig) resolvedProfile() Profile {
	if c.Scenario.Profile != nil {
		return *c.Scenario.Profile
	}
	return *c.Profile
}

func (c ControlSpec) timeout() time.Duration {
	if c.StartupTimeout == "" {
		return DefaultStartupTimeout
	}
	d, _ := time.ParseDuration(c.StartupTimeout)
	if d <= 0 {
		return DefaultStartupTimeout
	}
	return d
}

func (c ControlSpec) pollInterval() time.Duration {
	if c.PollInterval == "" {
		return DefaultPollInterval
	}
	d, _ := time.ParseDuration(c.PollInterval)
	if d <= 0 {
		return DefaultPollInterval
	}
	return d
}

func writeEvent(dst io.Writer, event Event) error {
	if dst == nil {
		return nil
	}
	enc := json.NewEncoder(dst)
	return enc.Encode(event)
}

func appendTranscript(path string, sequence int, ts time.Time, transcript TranscriptEvent) (err error) {
	if path == "" {
		return nil
	}
	fh, err := openAppendFile(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := fh.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	record := struct {
		Time      time.Time         `json:"time"`
		Sequence  int               `json:"sequence"`
		Role      string            `json:"role,omitempty"`
		Type      string            `json:"type,omitempty"`
		Text      string            `json:"text,omitempty"`
		ToolUseID string            `json:"tool_use_id,omitempty"`
		ToolName  string            `json:"tool_name,omitempty"`
		Content   string            `json:"content,omitempty"`
		IsError   bool              `json:"is_error,omitempty"`
		Metadata  map[string]string `json:"metadata,omitempty"`
	}{
		Time:      ts,
		Sequence:  sequence,
		Role:      transcript.Role,
		Type:      transcript.Type,
		Text:      transcript.Text,
		ToolUseID: transcript.ToolUseID,
		ToolName:  transcript.ToolName,
		Content:   transcript.Content,
		IsError:   transcript.IsError,
		Metadata:  transcript.Metadata,
	}
	err = json.NewEncoder(fh).Encode(record)
	return err
}

func mergeMetadata(parts ...map[string]string) map[string]string {
	var merged map[string]string
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		if merged == nil {
			merged = make(map[string]string)
		}
		for key, value := range part {
			merged[key] = value
		}
	}
	return merged
}

func writeState(path, state string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return os.WriteFile(path, []byte(state+"\n"), 0o644)
}

func writeFile(path, content string, appendMode bool) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if appendMode {
		fh, err := openAppendFile(path)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := fh.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}()
		_, err = fh.WriteString(content)
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func openAppendFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create parent dir for %s: %w", path, err)
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return fh, nil
}

func waitForControl(ctx context.Context, path, expect string, timeout, poll time.Duration) error {
	_, err := waitForFileMatch(ctx, path, expect, timeout, poll, "control")
	return err
}

func waitForInput(ctx context.Context, path, expect string, timeout, poll time.Duration) (string, error) {
	if expect == "" {
		return "", fmt.Errorf("input expect is required")
	}
	return waitForFileMatch(ctx, path, expect, timeout, poll, "input")
}

func waitForFileMatch(ctx context.Context, path, expect string, timeout, poll time.Duration, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		data, ok, err := fileContentSatisfied(path, expect, label)
		if err != nil {
			return "", err
		}
		if ok {
			return data, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("timeout waiting for %s file %s", label, path)
		case <-ticker.C:
		}
	}
}

func fileContentSatisfied(path, expect, label string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s file %s: %w", label, path, err)
	}
	content := string(data)
	if expect == "" {
		return content, true, nil
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == expect {
			return content, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("scan %s file %s: %w", label, path, err)
	}
	return "", false, nil
}

func sleepContext(ctx context.Context, delay string) error {
	if delay == "" {
		return nil
	}
	d, err := time.ParseDuration(delay)
	if err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
