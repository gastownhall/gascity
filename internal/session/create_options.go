package session

import "github.com/gastownhall/gascity/internal/runtime"

// CreateOptions is the single, field-named description of a session to create
// through Manager.CreateSession. It replaces the telescoping family of
// positional Create* worker parameters: every optional knob is a named field,
// so a transposed template/title or provider/transport is unrepresentable at
// compile time.
//
// When BeadOnly is true the session bead is created in the "start-pending"
// state without starting a runtime process (the reconciler starts it later);
// Env and Hints are ignored on that path. Otherwise the runtime session is
// started immediately.
type CreateOptions struct {
	Alias        string
	ExplicitName string
	Template     string
	Title        string
	Command      string
	WorkDir      string
	Provider     string
	Transport    string
	Env          map[string]string
	Resume       ProviderResume
	Hints        runtime.Config
	ExtraMeta    map[string]string
	BeadOnly     bool
}

// defaultSessionOrigin returns the session_origin to record when ExtraMeta does
// not set one explicitly. Started sessions default to "manual"; bead-only
// (deferred) sessions default to "ephemeral". This reproduces the per-path
// defaulting that the retired Create* wrappers each applied.
func (o CreateOptions) defaultSessionOrigin() string {
	if o.BeadOnly {
		return "ephemeral"
	}
	return "manual"
}
