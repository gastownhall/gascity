package runtime

// SessionBinding identifies the objects a runtime hosts a session in when
// they live in the runtime's own store rather than gc's: the conversation
// thread the session runs in, and the project that scopes the thread.
type SessionBinding struct {
	ThreadID  string
	ProjectID string
}

// SessionBindingProvider is an optional extension for runtimes that host a
// session in a thread of their own. SessionBinding reports the thread bound
// to name, so gc can record it on the session row: [ErrSessionNotFound] when
// no thread is bound, [ErrRuntimeUnavailable] when the runtime cannot be read,
// and a binding with no ThreadID (and a nil error) when the runtime does not
// host name in a thread at all, as a composite routing name to a backend
// without threads.
type SessionBindingProvider interface {
	SessionBinding(name string) (SessionBinding, error)
}
