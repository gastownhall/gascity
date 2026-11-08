package runtime

// HeldFileProvider is an optional extension for runtimes that can name the
// files a session's agent process holds open on this host: the session's own
// process tree, read from the live process table. It lets a reader find the
// transcript the agent is writing wherever its provider home lies, rather than
// only under the configured transcript search roots.
//
// HeldFiles returns the absolute paths of the regular files the process tree
// of session name holds open that are owned by the current user; files
// another user owns are never named, so a caller never reads them. A process
// in the tree whose open files the current user may not read, such as a
// root-owned sandbox wrapper above the agent or a zombie, is skipped. A
// non-nil err means the tree could not be read; the caller must not take an
// empty answer then. Composites do not implement it: it is read on the
// session's routed leaf ([HeldFilesFor]).
type HeldFileProvider interface {
	HeldFiles(name string) ([]string, error)
}

// HeldFilesFor returns the [HeldFileProvider] of the backend serving session
// name, the leaf [ResolveBackend] names, and false when that backend cannot
// name the files its session holds open.
func HeldFilesFor(p Provider, name string) (HeldFileProvider, bool) {
	leaf, _, _ := ResolveBackend(p, name)
	hf, ok := leaf.(HeldFileProvider)
	return hf, ok
}
