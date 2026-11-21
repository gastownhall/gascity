package runtime

// InterruptNoteProvider is an optional extension: a leaf whose idle wait
// after an interrupt it sent is that interrupt's settle (the tmux leaf, under
// the program-status switch on, reads the program's record against its
// position before the interrupt, RUNTIME-CONTRACT-017) also keeps that
// position before an interrupt its caller sends as keys rather than through
// Interrupt, which notes its own: a soft-Escape family's Escape. Only the
// caller knows those keys interrupt the turn; an Escape sent for anything
// else (a dialog dismissed, an input cleared) is never noted, and the wait
// after it stays the scan's. Composites do not implement it ([NoteInterrupt]
// resolves the leaf).
type InterruptNoteProvider interface {
	// NoteInterrupt keeps session name's position as its caller is about
	// to send the keys that interrupt its turn, for the idle wait that
	// follows them.
	NoteInterrupt(name string)
}

// NoteInterrupt notes, on the backend serving session name, that the keys
// its caller sends next interrupt the session's turn
// ([InterruptNoteProvider]). A backend that does not note interrupts notes
// nothing.
func NoteInterrupt(p Provider, name string) {
	if p == nil || name == "" {
		return
	}
	leaf, _, _ := ResolveBackend(p, name)
	if n, ok := leaf.(InterruptNoteProvider); ok {
		n.NoteInterrupt(name)
	}
}
