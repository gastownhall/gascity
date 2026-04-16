package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory [Provider] for testing. It records all calls
// (spy) and simulates session state (fake). Safe for concurrent use.
//
// When broken is true (via [NewFailFake]), all mutating operations return
// an error and IsRunning always returns false. Calls are still recorded.
type Fake struct {
	mu                   sync.Mutex
	sessions             map[string]Config            // live sessions
	meta                 map[string]map[string]string // session → key → value
	Calls                []Call                       // recorded calls in order
	broken               bool                         // when true, all ops fail
	Zombies              map[string]bool              // sessions with dead agent processes
	Attached             map[string]bool              // sessions with attached terminals
	PeekOutput           map[string]string            // session → canned peek output
	Activity             map[string]time.Time         // session → last activity time
	StartErrors          map[string]error             // per-session Start errors for testing
	StopErrors           map[string]error             // per-session Stop errors for testing
	PendingInteractions  map[string]*PendingInteraction
	Responses            map[string][]InteractionResponse
	SleepCapabilityValue SessionSleepCapability
	WaitForIdleErrors    map[string]error
	// WaitForIdleGates blocks WaitForIdle on a per-name channel until the
	// caller closes it. A nil or absent entry returns the configured
	// WaitForIdleErrors value immediately. The gate is read under f.mu
	// and the lock is released before the block, so other Fake methods
	// remain callable while a probe is gated.
	WaitForIdleGates map[string]chan struct{}
}

// Call records a single method invocation on [Fake].
type Call struct {
	Method    string         // method name (e.g. "Start", "Stop", "SetMeta")
	Name      string         // session name argument
	Config    Config         // only set for Start calls
	Message   string         // only set for Nudge/SendKeys calls (flattened text)
	Content   []ContentBlock // only set for Nudge calls (structured content)
	Key       string         // only set for meta calls
	Value     string         // only set for SetMeta calls
	Src       string         // only set for CopyTo calls
	Dst       string         // only set for CopyTo calls
	RequestID string         // only set for Respond calls
	Action    string         // only set for Respond calls
}

// NewFake returns a ready-to-use [Fake].
func NewFake() *Fake {
	return &Fake{
		sessions:             make(map[string]Config),
		meta:                 make(map[string]map[string]string),
		Zombies:              make(map[string]bool),
		Attached:             make(map[string]bool),
		StartErrors:          make(map[string]error),
		StopErrors:           make(map[string]error),
		PendingInteractions:  make(map[string]*PendingInteraction),
		Responses:            make(map[string][]InteractionResponse),
		SleepCapabilityValue: SessionSleepCapabilityFull,
		WaitForIdleErrors:    make(map[string]error),
		WaitForIdleGates:     make(map[string]chan struct{}),
	}
}

// NewFailFake returns a [Fake] where Start, Stop, and Attach always fail
// and IsRunning always returns false. Useful for testing error paths in
// session-dependent commands.
func NewFailFake() *Fake {
	return &Fake{
		sessions:             make(map[string]Config),
		meta:                 make(map[string]map[string]string),
		Zombies:              make(map[string]bool),
		Attached:             make(map[string]bool),
		StartErrors:          make(map[string]error),
		StopErrors:           make(map[string]error),
		PendingInteractions:  make(map[string]*PendingInteraction),
		Responses:            make(map[string][]InteractionResponse),
		SleepCapabilityValue: SessionSleepCapabilityFull,
		WaitForIdleErrors:    make(map[string]error),
		WaitForIdleGates:     make(map[string]chan struct{}),
		broken:               true,
	}
}

// Start creates a fake session. Returns an error if the name is taken.
// When broken, always returns an error.
func (f *Fake) Start(_ context.Context, name string, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Start", Name: name, Config: cfg})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	if err, ok := f.StartErrors[name]; ok {
		return err
	}
	if _, exists := f.sessions[name]; exists {
		return fmt.Errorf("%w: session %q", ErrSessionExists, name)
	}
	f.sessions[name] = cfg
	return nil
}

// Stop removes a fake session. Returns nil if it doesn't exist.
// When broken, always returns an error.
func (f *Fake) Stop(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Stop", Name: name})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	if err, ok := f.StopErrors[name]; ok {
		return err
	}
	delete(f.sessions, name)
	return nil
}

// Interrupt records the call. Best-effort: returns nil normally,
// or an error if the fake is broken.
func (f *Fake) Interrupt(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Interrupt", Name: name})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// IsRunning reports whether the fake session exists.
// When broken, always returns false.
func (f *Fake) IsRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "IsRunning", Name: name})
	if f.broken {
		return false
	}
	_, exists := f.sessions[name]
	return exists
}

// SetAttached sets the canned attached state for the named session.
// Used in test setup.
func (f *Fake) SetAttached(name string, val bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Attached == nil {
		f.Attached = make(map[string]bool)
	}
	f.Attached[name] = val
}

// IsAttached reports whether the fake session has an attached terminal.
// When broken, always returns false.
func (f *Fake) IsAttached(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "IsAttached", Name: name})
	if f.broken {
		return false
	}
	return f.Attached[name]
}

// Attach records the call but returns immediately (no terminal to attach).
// When broken, always returns an error.
func (f *Fake) Attach(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Attach", Name: name})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	if _, exists := f.sessions[name]; !exists {
		return fmt.Errorf("session %q not found", name)
	}
	return nil
}

// ProcessAlive reports whether the named session has a live agent process.
// Returns true if processNames is empty (no check possible).
// Returns false if the session does not exist, is in the Zombies set, or
// the fake is broken.
func (f *Fake) ProcessAlive(name string, processNames []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "ProcessAlive", Name: name})
	if f.broken {
		return false
	}
	if len(processNames) == 0 {
		return true
	}
	if _, exists := f.sessions[name]; !exists {
		return false
	}
	return !f.Zombies[name]
}

// Nudge records the call and returns nil (or an error if broken).
func (f *Fake) Nudge(name string, content []ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{
		Method:  "Nudge",
		Name:    name,
		Message: FlattenText(content),
		Content: content,
	})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// NudgeNow records the call and returns nil (or an error if broken).
func (f *Fake) NudgeNow(name string, content []ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{
		Method:  "NudgeNow",
		Name:    name,
		Message: FlattenText(content),
		Content: content,
	})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// SetPendingInteraction configures a structured pending interaction for the
// named session. A nil value clears any pending interaction.
func (f *Fake) SetPendingInteraction(name string, pending *PendingInteraction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PendingInteractions == nil {
		f.PendingInteractions = make(map[string]*PendingInteraction)
	}
	if pending == nil {
		delete(f.PendingInteractions, name)
		return
	}
	copyPending := *pending
	f.PendingInteractions[name] = &copyPending
}

// Pending returns the configured pending interaction for the named session.
func (f *Fake) Pending(name string) (*PendingInteraction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Pending", Name: name})
	if f.broken {
		return nil, fmt.Errorf("session unavailable")
	}
	pending := f.PendingInteractions[name]
	if pending == nil {
		return nil, nil
	}
	copyPending := *pending
	return &copyPending, nil
}

// Respond records the response and clears the matching pending interaction.
func (f *Fake) Respond(name string, response InteractionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{
		Method:    "Respond",
		Name:      name,
		RequestID: response.RequestID,
		Action:    response.Action,
		Message:   response.Text,
	})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	pending := f.PendingInteractions[name]
	if pending == nil {
		return fmt.Errorf("no pending interaction")
	}
	if pending.RequestID != "" && response.RequestID != "" && pending.RequestID != response.RequestID {
		return fmt.Errorf("pending interaction %q does not match request %q", pending.RequestID, response.RequestID)
	}
	if response.RequestID == "" {
		response.RequestID = pending.RequestID
	}
	if f.Responses == nil {
		f.Responses = make(map[string][]InteractionResponse)
	}
	f.Responses[name] = append(f.Responses[name], response)
	delete(f.PendingInteractions, name)
	return nil
}

// SetMeta stores a key-value pair for the named session.
func (f *Fake) SetMeta(name, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "SetMeta", Name: name, Key: key, Value: value})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	if f.meta[name] == nil {
		f.meta[name] = make(map[string]string)
	}
	f.meta[name][key] = value
	return nil
}

// GetMeta retrieves a metadata value. Returns ("", nil) if not set.
func (f *Fake) GetMeta(name, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "GetMeta", Name: name, Key: key})
	if f.broken {
		return "", fmt.Errorf("session unavailable")
	}
	return f.meta[name][key], nil
}

// RemoveMeta removes a metadata key from the named session.
func (f *Fake) RemoveMeta(name, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "RemoveMeta", Name: name, Key: key})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	delete(f.meta[name], key)
	return nil
}

// SetPeekOutput sets the canned output returned by [Fake.Peek] for the
// named session. Used in test setup.
func (f *Fake) SetPeekOutput(name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PeekOutput == nil {
		f.PeekOutput = make(map[string]string)
	}
	f.PeekOutput[name] = content
}

// Peek returns canned output for the named session. Records the call.
// Returns ("", error) if broken.
func (f *Fake) Peek(name string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "Peek", Name: name})
	if f.broken {
		return "", fmt.Errorf("session unavailable")
	}
	return f.PeekOutput[name], nil
}

// ListRunning returns session names matching the given prefix.
func (f *Fake) ListRunning(prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "ListRunning"})
	if f.broken {
		return nil, fmt.Errorf("session unavailable")
	}
	var names []string
	for name := range f.sessions {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// SetActivity sets the canned last activity time for the named session.
// Used in test setup.
func (f *Fake) SetActivity(name string, t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Activity == nil {
		f.Activity = make(map[string]time.Time)
	}
	f.Activity[name] = t
}

// GetLastActivity returns the configured activity time for the named session.
// Returns zero time if not set.
func (f *Fake) GetLastActivity(name string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "GetLastActivity", Name: name})
	if f.broken {
		return time.Time{}, fmt.Errorf("session unavailable")
	}
	return f.Activity[name], nil
}

// ClearScrollback records the call and returns nil (or error if broken).
func (f *Fake) ClearScrollback(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "ClearScrollback", Name: name})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// WaitForIdle records the call and returns the configured result. When
// WaitForIdleGates[name] is set, the method releases f.mu and blocks on
// the gate (or ctx cancellation) before returning, giving tests
// deterministic control over when the call completes.
func (f *Fake) WaitForIdle(ctx context.Context, name string, _ time.Duration) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, Call{Method: "WaitForIdle", Name: name})
	if f.broken {
		f.mu.Unlock()
		return fmt.Errorf("session unavailable")
	}
	err, ok := f.WaitForIdleErrors[name]
	if !ok {
		f.mu.Unlock()
		return ErrInteractionUnsupported
	}
	gate := f.WaitForIdleGates[name]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// CopyTo records the call and returns nil (or error if broken).
func (f *Fake) CopyTo(name, src, relDst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "CopyTo", Name: name, Src: src, Dst: relDst})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// SendKeys records the call and returns nil (or error if broken).
func (f *Fake) SendKeys(name string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "SendKeys", Name: name, Message: strings.Join(keys, " ")})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}

// Capabilities returns the fake provider's capabilities.
// By default, reports both attachment and activity as available.
func (f *Fake) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		CanReportAttachment: true,
		CanReportActivity:   true,
	}
}

// SleepCapability returns the configured idle sleep capability.
func (f *Fake) SleepCapability(string) SessionSleepCapability {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SleepCapabilityValue == "" {
		return SessionSleepCapabilityFull
	}
	return f.SleepCapabilityValue
}

// LastStartConfig returns the Config used in the most recent Start call for
// the named session, or nil if no Start was recorded for that name.
func (f *Fake) LastStartConfig(name string) *Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Calls) - 1; i >= 0; i-- {
		if f.Calls[i].Method == "Start" && f.Calls[i].Name == name {
			cfg := f.Calls[i].Config
			return &cfg
		}
	}
	return nil
}

// RunLive records the call and returns nil (or error if broken).
func (f *Fake) RunLive(name string, _ Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "RunLive", Name: name})
	if f.broken {
		return fmt.Errorf("session unavailable")
	}
	return nil
}
