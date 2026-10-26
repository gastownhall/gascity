package legacyop

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// Surfaces a recording step is counted under.
const (
	SurfaceAPI        = "api"
	SurfaceCLI        = "cli"
	SurfaceController = "controller"
)

// Stages: the telemetry stages (admit, close, adopt, sweep, and open for a
// controller sweeper pass that could not open the op store) and, for a
// Degrade, the step that failed (open, propose, close, adopt, sweep).
const (
	StageAdmit   = "admit"
	StageClose   = "close"
	StageAdopt   = "adopt"
	StageSweep   = "sweep"
	StageOpen    = "open"
	StagePropose = "propose"
)

// Results of a counted recording step (telemetry.RecordOpRecord).
const (
	ResultRecorded   = "recorded"
	ResultUnrecorded = "unrecorded"
	ResultReplayed   = "replayed"
	ResultAdopted    = "adopted"
	ResultBusy       = "busy"
	ResultKeyReuse   = "key_reuse"
	ResultRefused    = "refused"
	ResultClosed     = "closed"
	ResultLeftOpen   = "left_open"
	ResultFailed     = "failed"
	ResultLate       = "late"
)

// Reasons a request ran unrecorded (or, in require, was refused).
const (
	ReasonStoreUnavailable     = "store_unavailable"
	ReasonTypeUnregistered     = "type_unregistered"
	ReasonNoNamespace          = "no_namespace"
	ReasonPayloadTooLarge      = "payload_too_large"
	ReasonInvalidKey           = "invalid_key"
	ReasonInvalidRequest       = "invalid_request"
	ReasonUnmaterializedTarget = "unmaterialized_target"
	ReasonNoGeneration         = "no_generation"
	ReasonUnreadableTarget     = "unreadable_target"
	ReasonProposeError         = "propose_error"
)

// Degrade reason classes, the ops.recording_degraded event's reason.
const (
	DegradeConditionalWritesUnsupported = "conditional_writes_unsupported"
	DegradeTypeUnregistered             = "type_unregistered"
	DegradeNoNamespace                  = "no_namespace"
	DegradeStoreError                   = "store_error"
	DegradeContention                   = "contention"
)

// Degrade is one store failure a recorder degraded past: in auto the request
// ran unrecorded or its op stayed open, in require the request was refused.
// The caller's Degraded hook turns it into its loud signal (a city event, a
// CLI warning).
type Degrade struct {
	Mode    gate.Mode
	Surface string
	Kind    opjournal.Kind
	Stage   string
	Reason  string
	Err     error
}

// CannotRecord reports whether a degrade of reason is a store that cannot
// record ops at all as it is set up: no conditional writes, or no namespace.
// A store error, contention or an unregistered op type is a fault, not that.
func CannotRecord(reason string) bool {
	return reason == DegradeConditionalWritesUnsupported || reason == DegradeNoNamespace
}

// StoreCannotRecord reports whether d is an op store that did not open
// because the store cannot record ops at all (CannotRecord). A gate that is
// auto only by its builtin default treats that store as off: its surfaces
// stay quiet (Recorder.QuietCannotRecord), as beads.conditional_writes does
// under its auto default.
func (d Degrade) StoreCannotRecord() bool {
	return d.Stage == StageOpen && CannotRecord(d.Reason)
}

// surface is the telemetry surface of an executor: api, cli, or the
// executor itself for anything else.
func surface(e opjournal.ExecutorKind) string {
	if s, ok := strings.CutPrefix(string(e), "legacy:"); ok {
		return s
	}
	return string(e)
}

// recordOpRecord is telemetry.RecordOpRecord; tests capture it.
var recordOpRecord = telemetry.RecordOpRecord

// count records one recording step.
func (r *Recorder) count(kind opjournal.Kind, stage, result, reason string) {
	recordOpRecord(context.Background(), string(kind), surface(r.Executor), stage, result, reason)
}

// DegradeLogInterval is how often a recorder logs one degrade line per
// (surface, stage, reason): a store outage logs once per interval, not once
// per request (DES-O0API D13). The Degraded hook still hears every degrade.
const DegradeLogInterval = 10 * time.Minute

type degradeLogKey struct {
	surface, stage, reason string
}

// degrade tells the Degraded hook about a store failure and logs it, at most
// once per DegradeLogInterval per (surface, stage, reason). Under
// QuietCannotRecord a store that cannot record ops at all is logged at debug
// and kept from the hook.
func (r *Recorder) degrade(kind opjournal.Kind, stage, reason string, err error) {
	d := Degrade{Mode: r.Mode, Surface: surface(r.Executor), Kind: kind, Stage: stage, Reason: reason, Err: err}
	quiet := r.QuietCannotRecord && d.StoreCannotRecord()
	if r.shouldLogDegrade(degradeLogKey{surface: d.Surface, stage: stage, reason: reason}) {
		level := slog.LevelWarn
		if quiet {
			level = slog.LevelDebug
		}
		r.logger().Log(context.Background(), level, "op journal: recording degraded", "mode", d.Mode, "surface", d.Surface,
			"kind", d.Kind, "stage", d.Stage, "reason", d.Reason, "err", err)
	}
	if quiet {
		return
	}
	if r.Degraded != nil {
		r.Degraded(d)
	}
}

// closeHealthy tells the Healthy hook that a close just worked, so its owner's
// next close failure is news again.
func (r *Recorder) closeHealthy() {
	if r.Healthy != nil {
		r.Healthy(StageClose)
	}
}

// shouldLogDegrade reports whether key's degrade line is due, and if so
// latches it for DegradeLogInterval.
func (r *Recorder) shouldLogDegrade(key degradeLogKey) bool {
	now := r.Clock.Now()
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if last, ok := r.logged[key]; ok && now.Before(last.Add(DegradeLogInterval)) {
		return false
	}
	if r.logged == nil {
		r.logged = map[degradeLogKey]time.Time{}
	}
	r.logged[key] = now
	return true
}

// logger is the recorder's logger.
func (r *Recorder) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// late logs and counts a result that arrived after the op was closed by
// someone else.
func (r *Recorder) late(op opjournal.Op, outcome opjournal.Outcome, err error) {
	r.logger().Warn("op journal: late result for a closed op", "op", op.ID, "surface", surface(r.Executor),
		"arm", outcome.Arm, "reason", outcome.Reason, "err", err)
	r.count(op.Kind, StageClose, ResultLate, "")
}
