package runtimecontract

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

func pstatusDeclared(hs runtime.ProtocolInfo) bool {
	return hs.Has(runtime.ProtocolCapabilityProgramStatus)
}

func probePStatusAnswers(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !pstatusDeclared(hs) {
		return StatusSkip, "program-status not declared"
	}
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(context.WithoutCancel(ctx), name)
	_, err := readPStatus(ctx, r, name)
	if err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, "program-status answers an object gc accepts"
}

func readPStatus(ctx context.Context, r *runner, name string) (sessionexec.ProgramStatusAnswer, error) {
	res := r.op(ctx, "program-status", name)
	if res.unsupported {
		return sessionexec.ProgramStatusAnswer{}, fmt.Errorf("declared program-status returned exit 2")
	}
	if res.err != nil {
		return sessionexec.ProgramStatusAnswer{}, fmt.Errorf("program-status failed")
	}
	return sessionexec.ParseProgramStatus([]byte(res.stdout))
}

func probePStatusFollowsReport(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !pstatusDeclared(hs) {
		return StatusSkip, "program-status not declared"
	}
	var first string
	for _, word := range []runtime.ProgramState{runtime.ProgramWorking, runtime.ProgramIdle} {
		name := r.nextName()
		if first == "" {
			first = name
		}
		command := "printf '\\033]7501;state=" + string(word) + "\\033\\\\'; exec " + r.opts.Command
		res := r.startCommand(ctx, name, quoteArgv("sh", "-c", command))
		if res.unsupported || res.err != nil {
			return StatusFail, "start of status-reporting command failed"
		}
		defer r.stop(context.WithoutCancel(ctx), name)
		if status, detail := awaitPStatus(ctx, r, name, word); status != StatusPass {
			return status, detail
		}
	}
	if status, detail := awaitPStatus(ctx, r, first, runtime.ProgramWorking); status != StatusPass {
		return status, "first session after another session started: " + detail
	}
	return StatusPass, "each session reports its own state by name, including after another starts"
}

// pstatusAwaitLimit bounds each awaitPStatus wait; OpTimeout still caps it.
// Tests that wait out a failing check shorten it.
var pstatusAwaitLimit = 3 * time.Second

func awaitPStatus(ctx context.Context, r *runner, name string, word runtime.ProgramState) (Status, string) {
	limit, cancel := context.WithTimeout(ctx, min(pstatusAwaitLimit, r.opts.OpTimeout))
	defer cancel()
	for {
		answer, err := readPStatus(limit, r, name)
		if err == nil && !answer.UnknownState && answer.State == word && !answer.Exited {
			return StatusPass, ""
		}
		last := fmt.Sprintf("wanted %s, got %s (read error %v)", word, answer.State, err)
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-limit.Done():
			timer.Stop()
			return StatusFail, last
		case <-timer.C:
		}
	}
}

func probePStatusMissingSession(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !pstatusDeclared(hs) {
		return StatusSkip, "program-status not declared"
	}
	res := r.op(ctx, "program-status", r.nextName())
	if res.exitCode != runtime.ExitCodeNoScreen {
		return StatusFail, "program-status on a name no box holds must exit 66"
	}
	return StatusPass, "a missing session answers exit 66"
}
