package runtime

import "time"

// StartupNudge is a startup nudge a Start handed back
// ([StartupNudgeHandbackProvider]): its text, and the earliest time its
// caller's queue may send it. A zero NotBefore sends it as soon as the queue
// finds the session ready.
type StartupNudge struct {
	// Text is the nudge, delivered verbatim.
	Text string
	// NotBefore is the earliest send time: a runtime that cannot tell
	// when its agent takes input sets it from the start config's
	// ReadyDelayMs, counted from the launch.
	NotBefore time.Time
}

// StartupNudgeHandbackProvider is an optional extension: a leaf whose Start
// did not deliver its startup nudge ([Config.Nudge]) hands the nudge back to
// the Start's caller, which queues it for later delivery. One cause is a
// session whose program reported a dialog through the whole startup ladder
// (design §10.2 row A20, under the program-status switch on); another is an
// exec pack's un-welded launch, whose Start never types the first turn and
// always hands it back. The Start itself succeeded: an undelivered startup
// nudge never fails a start. Composites do not implement it
// ([TakeStartupNudgeHandback] resolves the leaf).
type StartupNudgeHandbackProvider interface {
	// TakeStartupNudgeHandback returns the startup nudge the last Start of
	// session name did not deliver, and forgets it: ok is true once per
	// such Start.
	TakeStartupNudgeHandback(name string) (nudge StartupNudge, ok bool)
}

// TakeStartupNudgeHandback takes the startup nudge the last Start of session
// name handed back on the backend serving it ([StartupNudgeHandbackProvider]).
// ok is false when the backend hands nothing back.
func TakeStartupNudgeHandback(p Provider, name string) (StartupNudge, bool) {
	if p == nil || name == "" {
		return StartupNudge{}, false
	}
	leaf, _, _ := ResolveBackend(p, name)
	h, ok := leaf.(StartupNudgeHandbackProvider)
	if !ok {
		return StartupNudge{}, false
	}
	return h.TakeStartupNudgeHandback(name)
}
