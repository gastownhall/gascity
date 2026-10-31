package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Activity sources a session or agent response names for its activity field.
const (
	// activitySourceProgram: activity is the session program's own OSC 7501
	// report, taken under the city switch's on mode.
	activitySourceProgram = "program"
	// activitySourceTranscript: activity is derived from the transcript.
	activitySourceTranscript = "transcript"
)

// Turn-state values of the activity field shared by session and agent
// responses and the session stream's activity frame.
const (
	activityIdle   = "idle"
	activityInTurn = "in-turn"
)

// SessionProgramStatus is the session program's own OSC 7501 report of its
// state, judged valid against the live runtime. It is observation only: it
// appears while the city's program_status switch is shadow or on and the
// reading is valid, and its absence means the status is unknown, never idle.
type SessionProgramStatus struct {
	State           string     `json:"state" enum:"idle,working,done,blocked,error" doc:"The state the program reports. done and error are input boundaries like idle (error is a failed turn with the program alive); blocked is waiting on the user."`
	Kind            string     `json:"kind,omitempty" enum:"permission,question,auth" doc:"What a blocked program waits for, when it says. Only with state blocked."`
	App             string     `json:"app,omitempty" doc:"The program's self-reported name."`
	Progress        *int       `json:"progress,omitempty" minimum:"0" maximum:"100" doc:"Percent complete the program reports, only with working or blocked."`
	Since           time.Time  `json:"since" doc:"When the program entered state, by the capture host's clock."`
	QuietSince      *time.Time `json:"quiet_since,omitempty" doc:"When the program last left working or blocked for an input boundary; absent if it never has in this capture."`
	WorkingChildren int        `json:"working_children" doc:"Child tasks the program reports as working."`
}

// SessionProgramStatusEvent is the session stream's program_status frame,
// sent when the session's program status changes between the stream's polls.
// A frame without program_status means the status became unknown.
type SessionProgramStatusEvent struct {
	ProgramStatus *SessionProgramStatus `json:"program_status,omitempty" doc:"The session program's current status; absent when it is unknown (never read as idle)."`
}

// programStatusView projects a judged reading onto the wire: nil unless the
// reading is valid. Counters, the nonce and the probe and error annotations
// stay internal.
func programStatusView(ps runtime.ProgramStatus) *SessionProgramStatus {
	if !ps.Valid() {
		return nil
	}
	view := &SessionProgramStatus{
		State:           string(ps.State),
		App:             ps.App,
		Since:           ps.Since.UTC(),
		WorkingChildren: ps.WorkingChildren,
	}
	if ps.State == runtime.ProgramBlocked {
		view.Kind = string(ps.Kind)
	}
	if ps.Progress >= 0 && ps.Progress <= 100 && (ps.State == runtime.ProgramWorking || ps.State == runtime.ProgramBlocked) {
		progress := ps.Progress
		view.Progress = &progress
	}
	if !ps.QuietSince.IsZero() {
		quiet := ps.QuietSince.UTC()
		view.QuietSince = &quiet
	}
	return view
}

// programActivity is the activity a valid reading reports: in-turn while the
// program works or is blocked mid-turn, idle at an input boundary.
func programActivity(ps runtime.ProgramStatus) string {
	if ps.Running() || ps.Blocked() {
		return activityInTurn
	}
	return activityIdle
}

// programObservation is what a program-status reading adds to a session or
// agent response.
type programObservation struct {
	status         *SessionProgramStatus
	activity       string
	activitySource string
}

// observeProgram folds reading ps into a response whose transcript-derived
// activity is transcriptActivity. A reading that is not valid changes
// nothing. A valid one is shown, and names the activity's source; only a
// reading that Decides (switch on) replaces the activity.
func observeProgram(ps runtime.ProgramStatus, transcriptActivity string) programObservation {
	obs := programObservation{status: programStatusView(ps), activity: transcriptActivity}
	switch {
	case obs.status == nil:
	case ps.Decides():
		obs.activity = programActivity(ps)
		obs.activitySource = activitySourceProgram
	case transcriptActivity != "":
		obs.activitySource = activitySourceTranscript
	}
	return obs
}

// programStatusFrames tracks the program-status view a session stream last
// sent, so the stream's poll sends a program_status frame only when the view
// changes. The stream starts from unknown: a session whose status stays
// unknown sends no frame, and one whose status becomes unknown sends a frame
// without program_status.
type programStatusFrames struct {
	read func() *SessionProgramStatus
	last *SessionProgramStatus
}

// newProgramStatusFrames tracks the program-status view of info's runtime
// session for one stream. A server without a state has no runtime to read:
// its status stays unknown.
func (s *Server) newProgramStatusFrames(ctx context.Context, info session.Info) *programStatusFrames {
	var sp runtime.Provider
	if s.state != nil {
		sp = s.state.SessionProvider()
	}
	return &programStatusFrames{read: func() *SessionProgramStatus {
		return programStatusView(runtimeProgramStatus(ctx, sp, info.SessionName, info.Provider))
	}}
}

// next reads the current view and returns the frame to send, and false when
// the view is unchanged since the last frame.
func (f *programStatusFrames) next() (SessionProgramStatusEvent, bool) {
	view := f.read()
	if sameProgramStatusView(f.last, view) {
		return SessionProgramStatusEvent{}, false
	}
	f.last = view
	return SessionProgramStatusEvent{ProgramStatus: view}, true
}

// sameProgramStatusView reports whether a and b show the same status.
func sameProgramStatusView(a, b *SessionProgramStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.State == b.State && a.Kind == b.Kind && a.App == b.App &&
		sameIntPtr(a.Progress, b.Progress) && a.Since.Equal(b.Since) &&
		sameTimePtr(a.QuietSince, b.QuietSince) && a.WorkingChildren == b.WorkingChildren
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// runtimeProgramStatus reads runtime session sessionName's program status
// through the provider-backed worker state handle the session responses use,
// which tmux serves from its shared state cache. A stopped or unreadable
// session reads Unread.
func runtimeProgramStatus(ctx context.Context, sp runtime.Provider, sessionName, providerName string) runtime.ProgramStatus {
	handle := newProviderSessionResponseHandle(sp, sessionName, providerName)
	if handle == nil {
		return runtime.ProgramStatus{}
	}
	state, err := handle.State(ctx)
	if err != nil {
		return runtime.ProgramStatus{}
	}
	return state.Program
}
