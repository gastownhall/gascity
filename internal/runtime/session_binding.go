package runtime

// SessionBinding identifies the objects a runtime hosts a session in when
// they live in the runtime's own store rather than gc's: the conversation
// thread the session runs in, and the project that scopes the thread.
type SessionBinding struct {
	ThreadID  string
	ProjectID string
}

// IsZero reports whether b names no thread.
func (b SessionBinding) IsZero() bool { return b.ThreadID == "" && b.ProjectID == "" }

// SessionBindingProvider is an optional extension for runtimes that host a
// session in a thread of their own. SessionBinding reports the thread bound
// to name, so gc can record it on the session row: [ErrSessionNotFound] when
// no thread is bound, [ErrRuntimeUnavailable] when the runtime cannot be read.
type SessionBindingProvider interface {
	SessionBinding(name string) (SessionBinding, error)
}
