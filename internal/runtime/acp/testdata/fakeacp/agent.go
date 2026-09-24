package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Exit statuses.
const (
	exitSIGINT       = 130
	exitDuringPrompt = 3
)

// agent is the fake ACP agent state shared by the reader and prompt turns.
type agent struct {
	opts options
	w    *writer
	logs *recorder
	out  *outbound

	mu    sync.Mutex
	turns map[string]*turn // in-flight prompts by request id
}

func newAgent(opts options, w *writer, logs *recorder) *agent {
	return &agent{
		opts:  opts,
		w:     w,
		logs:  logs,
		out:   newOutbound(w, opts.requestIDString),
		turns: make(map[string]*turn),
	}
}

// handleSignals installs SIGINT handling per --sigint and exits on SIGTERM.
func (a *agent) handleSignals() {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGTERM {
				a.logs.signal(sig, "exit")
				os.Exit(0)
			}
			switch a.opts.sigint {
			case sigintExit:
				a.logs.signal(sig, "exit")
				os.Exit(exitSIGINT)
			case sigintCancel:
				a.logs.signal(sig, "cancel")
				a.cancelTurns(0)
			default:
				a.logs.signal(sig, "ignore")
			}
		}
	}()
}

// serve reads JSON-RPC lines until stdin closes. Prompts run on their own
// goroutines so the reader keeps handling cancels and replies meanwhile.
func (a *agent) serve(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "fakeacp: skipping non-JSON line: %v\n", err)
			continue
		}
		if msg.Method == "" {
			if len(msg.ID) > 0 {
				a.logs.response(append([]byte(nil), line...))
				a.out.resolve(msg)
			}
			continue
		}
		a.logs.method(msg)
		a.dispatch(msg)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: reading stdin: %v\n", err)
	}
}

func (a *agent) dispatch(msg message) {
	switch msg.Method {
	case "initialize":
		a.logs.initialize(msg.Params)
		a.w.respond(msg.ID, map[string]any{
			"protocolVersion":   1,
			"serverInfo":        map[string]string{"name": "fakeacp", "version": "1.0"},
			"agentInfo":         map[string]string{"name": "fakeacp", "version": "1.0"},
			"agentCapabilities": map[string]any{"loadSession": false},
		})
	case "initialized", "notifications/initialized":
	case "session/new":
		a.w.respond(msg.ID, map[string]string{"sessionId": a.opts.sessionID})
	case "session/prompt":
		t := a.beginTurn(msg.ID)
		go a.runTurn(t, msg.Params)
	case "session/cancel":
		if a.opts.onCancel == onCancelReply {
			a.cancelTurns(a.opts.cancelLatency)
		}
	default:
		if len(msg.ID) > 0 {
			a.w.respondError(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)
		}
	}
}

// turn is one in-flight session/prompt. Exactly one answer is sent.
type turn struct {
	id     json.RawMessage
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (a *agent) beginTurn(id json.RawMessage) *turn {
	ctx, cancel := context.WithCancel(context.Background())
	t := &turn{id: id, ctx: ctx, cancel: cancel}
	a.mu.Lock()
	a.turns[idKey(id)] = t
	a.mu.Unlock()
	return t
}

// finish sends the turn's single answer and forgets the turn.
func (a *agent) finish(t *turn, send func()) {
	t.once.Do(func() {
		a.mu.Lock()
		delete(a.turns, idKey(t.id))
		a.mu.Unlock()
		t.cancel()
		send()
	})
}

func (a *agent) answer(t *turn, stopReason string, u *usage) {
	a.finish(t, func() {
		result := map[string]any{"stopReason": stopReason}
		if u != nil {
			result["usage"] = u
		}
		a.w.respond(t.id, result)
	})
}

func (a *agent) answerError(t *turn, text string) {
	a.finish(t, func() { a.w.respondError(t.id, codeInternalError, text) })
}

// cancelTurns stops every in-flight turn and answers each with stopReason
// canceled after latency.
func (a *agent) cancelTurns(latency time.Duration) {
	a.mu.Lock()
	turns := make([]*turn, 0, len(a.turns))
	for _, t := range a.turns {
		turns = append(turns, t)
	}
	a.mu.Unlock()
	for _, t := range turns {
		t.cancel()
		go func(t *turn) {
			waitFor(context.Background(), latency)
			a.answer(t, "canceled", nil)
		}(t)
	}
}
