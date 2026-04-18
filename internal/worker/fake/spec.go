package fake

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidProfile reports invalid fake-worker profile configuration.
	ErrInvalidProfile = errors.New("invalid fake worker profile")
	// ErrInvalidScenario reports invalid fake-worker scenario configuration.
	ErrInvalidScenario = errors.New("invalid fake worker scenario")
	// ErrInvalidConfig reports invalid standalone fake-worker helper config.
	ErrInvalidConfig = errors.New("invalid fake worker config")
)

// Profile describes a provider-flavored fake worker contract surface.
type Profile struct {
	Name         string            `json:"name" yaml:"name"`
	Provider     string            `json:"provider" yaml:"provider"`
	Description  string            `json:"description,omitempty" yaml:"description,omitempty"`
	Claims       Claims            `json:"claims,omitempty" yaml:"claims,omitempty"`
	Launch       LaunchSpec        `json:"launch,omitempty" yaml:"launch,omitempty"`
	Transcript   TranscriptSpec    `json:"transcript,omitempty" yaml:"transcript,omitempty"`
	Continuation ContinuationSpec  `json:"continuation,omitempty" yaml:"continuation,omitempty"`
	Interactions []InteractionSpec `json:"interactions,omitempty" yaml:"interactions,omitempty"`
}

// Claims describes the behavior a fake profile advertises to callers.
type Claims struct {
	ProfileFlavor        string   `json:"profile_flavor,omitempty" yaml:"profile_flavor,omitempty"`
	RequirementCodes     []string `json:"requirement_codes,omitempty" yaml:"requirement_codes,omitempty"`
	SupportsContinuation bool     `json:"supports_continuation,omitempty" yaml:"supports_continuation,omitempty"`
	SupportsTranscript   bool     `json:"supports_transcript,omitempty" yaml:"supports_transcript,omitempty"`
}

// LaunchSpec describes how the fake worker should appear at startup.
type LaunchSpec struct {
	Args    []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Startup StartupSpec       `json:"startup,omitempty" yaml:"startup,omitempty"`
}

// StartupSpec defines startup timing and control behavior for the fake worker.
type StartupSpec struct {
	Outcome            string `json:"outcome,omitempty" yaml:"outcome,omitempty"`
	ReadyAfter         string `json:"ready_after,omitempty" yaml:"ready_after,omitempty"`
	RequireControlFile bool   `json:"require_control_file,omitempty" yaml:"require_control_file,omitempty"`
}

// TranscriptSpec defines transcript output behavior for the fake worker.
type TranscriptSpec struct {
	Format   string `json:"format,omitempty" yaml:"format,omitempty"`
	Path     string `json:"path,omitempty" yaml:"path,omitempty"`
	StreamID string `json:"stream_id,omitempty" yaml:"stream_id,omitempty"`
}

// ContinuationSpec defines how the fake worker simulates continuation.
type ContinuationSpec struct {
	Mode              string `json:"mode,omitempty" yaml:"mode,omitempty"`
	HandleEnv         string `json:"handle_env,omitempty" yaml:"handle_env,omitempty"`
	SameConversation  bool   `json:"same_conversation,omitempty" yaml:"same_conversation,omitempty"`
	ConversationIDEnv string `json:"conversation_id_env,omitempty" yaml:"conversation_id_env,omitempty"`
}

// InteractionSpec declares one interaction kind the fake worker can emit.
type InteractionSpec struct {
	Kind     string `json:"kind" yaml:"kind"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

// Scenario is a declarative scripted fake-worker run.
type Scenario struct {
	Name        string            `json:"name" yaml:"name"`
	Provider    string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Profile     *Profile          `json:"profile,omitempty" yaml:"profile,omitempty"`
	Steps       []Step            `json:"steps" yaml:"steps"`
}

// Step is one scripted action in a fake-worker scenario.
type Step struct {
	ID            string            `json:"id,omitempty" yaml:"id,omitempty"`
	Action        string            `json:"action" yaml:"action"`
	Delay         string            `json:"delay,omitempty" yaml:"delay,omitempty"`
	State         string            `json:"state,omitempty" yaml:"state,omitempty"`
	Message       string            `json:"message,omitempty" yaml:"message,omitempty"`
	Path          string            `json:"path,omitempty" yaml:"path,omitempty"`
	Content       string            `json:"content,omitempty" yaml:"content,omitempty"`
	Append        bool              `json:"append,omitempty" yaml:"append,omitempty"`
	ExpectControl string            `json:"expect_control,omitempty" yaml:"expect_control,omitempty"`
	Transcript    TranscriptEvent   `json:"transcript,omitempty" yaml:"transcript,omitempty"`
	Interaction   InteractionEvent  `json:"interaction,omitempty" yaml:"interaction,omitempty"`
	Input         InputEvent        `json:"input,omitempty" yaml:"input,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// TranscriptEvent describes one transcript append operation.
type TranscriptEvent struct {
	Role      string            `json:"role,omitempty" yaml:"role,omitempty"`
	Type      string            `json:"type,omitempty" yaml:"type,omitempty"`
	Text      string            `json:"text,omitempty" yaml:"text,omitempty"`
	ToolUseID string            `json:"tool_use_id,omitempty" yaml:"tool_use_id,omitempty"`
	ToolName  string            `json:"tool_name,omitempty" yaml:"tool_name,omitempty"`
	Content   string            `json:"content,omitempty" yaml:"content,omitempty"`
	IsError   bool              `json:"is_error,omitempty" yaml:"is_error,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// InteractionEvent describes one interactive prompt or response event.
type InteractionEvent struct {
	Kind      string            `json:"kind,omitempty" yaml:"kind,omitempty"`
	RequestID string            `json:"request_id,omitempty" yaml:"request_id,omitempty"`
	Prompt    string            `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Response  string            `json:"response,omitempty" yaml:"response,omitempty"`
	State     string            `json:"state,omitempty" yaml:"state,omitempty"`
	Options   []string          `json:"options,omitempty" yaml:"options,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// InputEvent describes one awaited input file exchange.
type InputEvent struct {
	Path        string            `json:"path,omitempty" yaml:"path,omitempty"`
	Expect      string            `json:"expect,omitempty" yaml:"expect,omitempty"`
	ReceiptPath string            `json:"receipt_path,omitempty" yaml:"receipt_path,omitempty"`
	EchoPath    string            `json:"echo_path,omitempty" yaml:"echo_path,omitempty"`
	Observed    string            `json:"observed,omitempty" yaml:"observed,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// HelperConfig is the top-level input consumed by the standalone fake worker.
type HelperConfig struct {
	Profile  *Profile    `json:"profile,omitempty" yaml:"profile,omitempty"`
	Scenario Scenario    `json:"scenario" yaml:"scenario"`
	Output   OutputSpec  `json:"output,omitempty" yaml:"output,omitempty"`
	Control  ControlSpec `json:"control,omitempty" yaml:"control,omitempty"`
}

// OutputSpec defines where the fake worker writes its outputs.
type OutputSpec struct {
	EventLogPath   string `json:"event_log_path,omitempty" yaml:"event_log_path,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty" yaml:"transcript_path,omitempty"`
	StatePath      string `json:"state_path,omitempty" yaml:"state_path,omitempty"`
}

// ControlSpec defines external control files and timing for the fake worker.
type ControlSpec struct {
	StartFile      string `json:"start_file,omitempty" yaml:"start_file,omitempty"`
	StartupTimeout string `json:"startup_timeout,omitempty" yaml:"startup_timeout,omitempty"`
	PollInterval   string `json:"poll_interval,omitempty" yaml:"poll_interval,omitempty"`
}

// Validate checks that the fake-worker profile is internally consistent.
func (p Profile) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidProfile)
	}
	if p.Provider == "" {
		return fmt.Errorf("%w: provider is required", ErrInvalidProfile)
	}
	switch p.Launch.Startup.Outcome {
	case "", "ready", "blocked", "failed":
	default:
		return fmt.Errorf("%w: unsupported startup outcome %q", ErrInvalidProfile, p.Launch.Startup.Outcome)
	}
	if err := validateDuration("ready_after", p.Launch.Startup.ReadyAfter, ErrInvalidProfile); err != nil {
		return err
	}
	switch p.Transcript.Format {
	case "", "jsonl", "text":
	default:
		return fmt.Errorf("%w: unsupported transcript format %q", ErrInvalidProfile, p.Transcript.Format)
	}
	switch p.Continuation.Mode {
	case "", "ephemeral", "handle", "same-session":
	default:
		return fmt.Errorf("%w: unsupported continuation mode %q", ErrInvalidProfile, p.Continuation.Mode)
	}
	for _, interaction := range p.Interactions {
		if interaction.Kind == "" {
			return fmt.Errorf("%w: interaction kind is required", ErrInvalidProfile)
		}
	}
	return nil
}

// Validate checks that the fake-worker scenario can be executed safely.
func (s Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidScenario)
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: at least one step is required", ErrInvalidScenario)
	}
	if s.Profile != nil {
		if err := s.Profile.Validate(); err != nil {
			return err
		}
	}
	for i, step := range s.Steps {
		if err := step.validate(i); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks that the standalone fake-worker helper config is usable.
func (c HelperConfig) Validate() error {
	if c.Profile == nil && c.Scenario.Profile == nil {
		return fmt.Errorf("%w: profile or scenario.profile is required", ErrInvalidConfig)
	}
	if c.Profile != nil {
		if err := c.Profile.Validate(); err != nil {
			return err
		}
	}
	if err := c.Scenario.Validate(); err != nil {
		return err
	}
	if err := validateDuration("startup_timeout", c.Control.StartupTimeout, ErrInvalidConfig); err != nil {
		return err
	}
	if err := validateDuration("poll_interval", c.Control.PollInterval, ErrInvalidConfig); err != nil {
		return err
	}
	return nil
}

func (s Step) validate(index int) error {
	if s.Action == "" {
		return fmt.Errorf("%w: step %d action is required", ErrInvalidScenario, index)
	}
	if err := validateDuration("step delay", s.Delay, ErrInvalidScenario); err != nil {
		return err
	}
	switch s.Action {
	case "startup", "emit_state", "append_transcript", "emit_interaction", "input", "write_file", "wait_for_control", "sleep", "exit":
	default:
		return fmt.Errorf("%w: step %d has unsupported action %q", ErrInvalidScenario, index, s.Action)
	}
	switch s.Action {
	case "append_transcript":
		if s.Transcript.Text == "" && s.Transcript.Content == "" && s.Transcript.ToolName == "" {
			return fmt.Errorf("%w: step %d append_transcript requires transcript text, content, or tool_name", ErrInvalidScenario, index)
		}
	case "emit_interaction":
		if s.Interaction.Kind == "" {
			return fmt.Errorf("%w: step %d emit_interaction requires interaction.kind", ErrInvalidScenario, index)
		}
	case "input":
		if s.Input.Path == "" {
			return fmt.Errorf("%w: step %d input requires input.path", ErrInvalidScenario, index)
		}
		if s.Input.Expect == "" {
			return fmt.Errorf("%w: step %d input requires input.expect", ErrInvalidScenario, index)
		}
	case "write_file":
		if s.Path == "" {
			return fmt.Errorf("%w: step %d write_file requires path", ErrInvalidScenario, index)
		}
	case "wait_for_control":
		if s.Path == "" {
			return fmt.Errorf("%w: step %d wait_for_control requires path", ErrInvalidScenario, index)
		}
	case "sleep":
		if s.Delay == "" {
			return fmt.Errorf("%w: step %d sleep requires delay", ErrInvalidScenario, index)
		}
	}
	return nil
}

func validateDuration(field, value string, root error) error {
	if value == "" {
		return nil
	}
	if _, err := time.ParseDuration(value); err != nil {
		return fmt.Errorf("%w: invalid %s %q: %w", root, field, value, err)
	}
	return nil
}
