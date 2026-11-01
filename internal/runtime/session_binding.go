package runtime

// SessionBinding identifies the objects a runtime hosts a session in when
// they live in the runtime's own store rather than gc's: the conversation
// thread the session runs in, the project that scopes the thread, and the
// terminal pane (its server-scoped id and its process id) the session was
// spawned into. The pane process id is what the host's journal names, so it
// joins a pane death to the session.
type SessionBinding struct {
	ThreadID  string
	ProjectID string
	PaneID    string
	PanePID   string
}

// SessionBindingProvider is an optional extension for runtimes that host a
// session in a thread of their own. SessionBinding reports the thread bound
// to name (and the pane it runs in), so gc can record it on the session row: [ErrSessionNotFound] when
// no thread is bound, [ErrRuntimeUnavailable] when the runtime cannot be read,
// and a binding with no ThreadID (and a nil error) when the runtime does not
// host name in a thread or pane at all, as a composite routing name to a
// backend without either.
type SessionBindingProvider interface {
	SessionBinding(name string) (SessionBinding, error)
}
