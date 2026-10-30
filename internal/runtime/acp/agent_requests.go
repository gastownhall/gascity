package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// JSON-RPC 2.0 error codes gc answers agent requests with.
const (
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
)

// agentRequest is an agent->client JSON-RPC request (it has both a method
// and a non-null id). ID keeps the id's raw bytes so the reply echoes it
// exactly, whether the agent used a number or a string.
type agentRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// parseAgentRequest probes one inbound line and reports whether it is an
// agent->client request. It runs before the typed JSONRPCMessage decode,
// which cannot represent string ids.
func parseAgentRequest(line []byte) (agentRequest, bool) {
	var req agentRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return agentRequest{}, false
	}
	if req.Method == "" || len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null")) {
		return agentRequest{}, false
	}
	return req, true
}

// errorReply is the wire shape of a JSON-RPC error reply whose id is echoed
// from the request verbatim.
type errorReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   JSONRPCError    `json:"error"`
}

// encodeReply encodes one reply to an agent request, without a trailing
// newline. HTML escaping is disabled so a string id is echoed byte-for-byte.
func encodeReply(method string, reply any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(reply); err != nil {
		return nil, fmt.Errorf("encoding reply to %s: %w", method, err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// encodeError encodes a JSON-RPC error reply to req.
func encodeError(req agentRequest, code int, message string) ([]byte, error) {
	return encodeReply(req.Method, errorReply{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error:   JSONRPCError{Code: code, Message: message},
	})
}

// encodeMethodNotFound encodes the -32601 reply to req.
func encodeMethodNotFound(req agentRequest) ([]byte, error) {
	return encodeError(req, jsonRPCMethodNotFound, "method not found: "+req.Method)
}

// handleAgentRequest routes one agent->client request. It runs on the read
// loop, so every handler must return without waiting on the agent.
func (sc *sessionConn) handleAgentRequest(req agentRequest) {
	switch req.Method {
	case methodRequestPermission:
		sc.holdPermission(req)
	default:
		sc.answerUnsupported(req)
	}
}

// answerUnsupported replies -32601 to an agent request gc does not serve.
// gc advertises no fs or terminal capability, so those requests land here.
func (sc *sessionConn) answerUnsupported(req agentRequest) {
	if sc.firstUnsupported(req.Method) {
		fmt.Fprintf(os.Stderr, "acp: agent requested unsupported method %q; answering method not found\n", req.Method)
	}
	data, err := encodeMethodNotFound(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp: %v\n", err)
		return
	}
	sc.writeReplyAsync(req.Method, data)
}

// writeReplyAsync writes an encoded reply on its own goroutine so a
// backpressured agent stdin can never stall the read loop, which must keep
// routing responses.
func (sc *sessionConn) writeReplyAsync(method string, data []byte) {
	go func() {
		if err := sc.writeMessage(data); err != nil {
			fmt.Fprintf(os.Stderr, "acp: replying to %s: %v\n", method, err)
		}
	}()
}

// firstUnsupported reports whether method is being refused for the first
// time on this connection, so each distinct method is logged once.
func (sc *sessionConn) firstUnsupported(method string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.unsupportedSeen == nil {
		sc.unsupportedSeen = make(map[string]struct{})
	}
	if _, seen := sc.unsupportedSeen[method]; seen {
		return false
	}
	sc.unsupportedSeen[method] = struct{}{}
	return true
}
