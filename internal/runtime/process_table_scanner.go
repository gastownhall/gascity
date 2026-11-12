package runtime

// ConditionalProcessTableScanner is implemented by providers that satisfy
// [ProcessTableScanner] but can scan only under a condition: composites that
// forward to their backends scan only when a backend can, and an exec
// provider only when its pack is v2-ready and its boxes share the host. A
// provider reporting false has no process-table capability, exactly as if it
// did not implement [ProcessTableScanner].
type ConditionalProcessTableScanner interface {
	// CanScanProcessTable reports whether the provider's scanner methods can
	// find anything.
	CanScanProcessTable() bool
}

// AsProcessTableScanner returns sp's [ProcessTableScanner] and true when sp can
// scan the process table: it implements the interface and, if it is a
// [ConditionalProcessTableScanner], reports that it can scan. Callers should
// use it instead of a bare type assertion so a composite whose backends cannot
// scan reads as lacking the capability.
func AsProcessTableScanner(sp Provider) (ProcessTableScanner, bool) {
	scanner, ok := sp.(ProcessTableScanner)
	if !ok {
		return nil, false
	}
	if c, ok := sp.(ConditionalProcessTableScanner); ok && !c.CanScanProcessTable() {
		return nil, false
	}
	return scanner, true
}
