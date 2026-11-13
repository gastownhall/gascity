package runtime

// StartupNudgeHandbackProvider is an optional extension: a leaf whose Start
// could not type its startup nudge ([Config.Nudge]) because the session's
// program reported a dialog through the whole startup ladder (design §10.2
// row A20, under the program-status switch on) hands the nudge back to the
// Start's caller, which queues it for after the dialog is answered. The
// Start itself succeeded: a dialog never fails a start. Composites do not
// implement it ([TakeStartupNudgeHandback] resolves the leaf).
type StartupNudgeHandbackProvider interface {
	// TakeStartupNudgeHandback returns the startup nudge the last Start of
	// session name left undelivered behind a program-reported dialog, and
	// forgets it: ok is true once per such Start.
	TakeStartupNudgeHandback(name string) (nudge string, ok bool)
}

// TakeStartupNudgeHandback takes the startup nudge the last Start of session
// name handed back on the backend serving it ([StartupNudgeHandbackProvider]).
// ok is false when the backend hands nothing back.
func TakeStartupNudgeHandback(p Provider, name string) (string, bool) {
	if p == nil || name == "" {
		return "", false
	}
	leaf, _, _ := ResolveBackend(p, name)
	h, ok := leaf.(StartupNudgeHandbackProvider)
	if !ok {
		return "", false
	}
	return h.TakeStartupNudgeHandback(name)
}
