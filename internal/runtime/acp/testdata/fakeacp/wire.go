package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// JSON-RPC error codes used by the fake.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// wireCancelled is the ACP wire spelling of the cancel stop reason and of
// the cancel permission outcome. The US-locale misspell autofix would
// rewrite it into a value no ACP client recognizes.
const wireCancelled = "cancelled" //nolint:misspell // ACP wire value

// message is one JSON-RPC 2.0 frame. IDs stay raw so numeric and string ids
// round-trip unchanged.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// idKey canonicalizes a raw JSON-RPC id for map lookups.
func idKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}

// writer serializes whole JSON-RPC lines onto stdout.
type writer struct {
	mu  sync.Mutex
	out io.Writer
}

func newWriter(out io.Writer) *writer {
	return &writer{out: out}
}

func (w *writer) send(msg message) {
	msg.JSONRPC = "2.0"
	data := mustJSON(msg)
	w.mu.Lock()
	defer w.mu.Unlock()
	// A write failure means the client is gone; there is nobody to tell.
	_, _ = w.out.Write(append(data, '\n'))
}

func (w *writer) respond(id json.RawMessage, result any) {
	w.send(message{ID: id, Result: mustJSON(result)})
}

func (w *writer) respondError(id json.RawMessage, code int, text string) {
	w.send(message{ID: id, Error: &rpcError{Code: code, Message: text}})
}

func (w *writer) notify(method string, params any) {
	w.send(message{Method: method, Params: mustJSON(params)})
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		// Only the fake's own fixed shapes are marshaled; failure is a bug
		// in the fake, and a half-written protocol frame would be worse.
		fmt.Fprintf(os.Stderr, "fakeacp: marshal %T: %v\n", v, err)
		os.Exit(1)
	}
	return data
}

// outbound tracks requests the fake sends to the client and routes replies.
type outbound struct {
	w         *writer
	stringIDs bool

	mu      sync.Mutex
	next    int
	pending map[string]chan message
}

func newOutbound(w *writer, stringIDs bool) *outbound {
	return &outbound{w: w, stringIDs: stringIDs, pending: make(map[string]chan message)}
}

// request sends method to the client and returns its id and a channel that
// receives the reply. prefix names string ids (for example "perm").
func (o *outbound) request(prefix, method string, params any) (json.RawMessage, <-chan message) {
	o.mu.Lock()
	o.next++
	var id json.RawMessage
	if o.stringIDs {
		id = mustJSON(fmt.Sprintf("%s-%d", prefix, o.next))
	} else {
		id = mustJSON(o.next)
	}
	ch := make(chan message, 1)
	o.pending[idKey(id)] = ch
	o.mu.Unlock()
	o.w.send(message{ID: id, Method: method, Params: mustJSON(params)})
	return id, ch
}

// forget drops a request that will no longer be awaited.
func (o *outbound) forget(id json.RawMessage) {
	o.mu.Lock()
	delete(o.pending, idKey(id))
	o.mu.Unlock()
}

// resolve delivers a client reply to its waiter, if any.
func (o *outbound) resolve(reply message) {
	o.mu.Lock()
	ch, ok := o.pending[idKey(reply.ID)]
	delete(o.pending, idKey(reply.ID))
	o.mu.Unlock()
	if ok {
		ch <- reply
	}
}
