package session

import "github.com/gastownhall/gascity/internal/runtime"

// LaunchKind says which Manager launch seam is finalizing a runtime.Config.
type LaunchKind string

const (
	// LaunchCreate is a launch that mints a new session (createStarted).
	LaunchCreate LaunchKind = "create"
	// LaunchResume is a launch that resumes an existing session's runtime
	// (ensureRunning on its launch branch).
	LaunchResume LaunchKind = "resume"
)

// LaunchContext is what a LaunchFinalizer sees of one launch: the session
// being launched, the transport it runs on and the fully assembled
// runtime.Config (command, work dir and identity env included) that is about
// to be handed to the runtime provider.
type LaunchContext struct {
	Kind      LaunchKind
	Info      Info
	Transport string
	Config    runtime.Config
}

// LaunchFinalizer returns the runtime.Config a launch hands to the runtime
// provider, given the config the Manager assembled. A non-nil error means the
// launch must not proceed.
type LaunchFinalizer func(LaunchContext) (runtime.Config, error)

// WithLaunchFinalizer installs f at the Manager's two hint-based launch seams.
// A nil f leaves launches exactly as the Manager assembled them.
func WithLaunchFinalizer(f LaunchFinalizer) ManagerOption {
	return func(m *Manager) { m.launchFinalizer = f }
}
