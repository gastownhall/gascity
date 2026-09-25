package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// jsonRPCMethodNotFound is the JSON-RPC 2.0 "method not found" error code.
const jsonRPCMethodNotFound = -32601

// agentRequest is an agent->client JSON-RPC request (it has both a method
// and a non-null id). ID keeps the id's raw bytes so the reply echoes it
// exactly, whether the agent used a number or a string.
type agentRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
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

// methodNotFoundReply is the wire shape of a JSON-RPC error reply whose id is
// echoed from the request verbatim.
type methodNotFoundReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   JSONRPCError    `json:"error"`
}

// encodeMethodNotFound encodes the -32601 reply to req, without a trailing
// newline. HTML escaping is disabled so a string id is echoed byte-for-byte.
func encodeMethodNotFound(req agentRequest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(methodNotFoundReply{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: JSONRPCError{
			Code:    jsonRPCMethodNotFound,
			Message: "method not found: " + req.Method,
		},
	}); err != nil {
		return nil, fmt.Errorf("encoding reply to %s: %w", req.Method, err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// answerUnsupported replies -32601 to an agent request gc does not serve.
// gc advertises no fs or terminal capability, so every agent->client request
// lands here. The write runs on its own goroutine so a backpressured agent
// stdin can never stall the read loop, which must keep routing responses.
func (sc *sessionConn) answerUnsupported(req agentRequest) {
	if sc.firstUnsupported(req.Method) {
		fmt.Fprintf(os.Stderr, "acp: agent requested unsupported method %q; answering method not found\n", req.Method)
	}
	data, err := encodeMethodNotFound(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp: %v\n", err)
		return
	}
	go func() {
		if err := sc.writeMessage(data); err != nil {
			fmt.Fprintf(os.Stderr, "acp: replying to %s: %v\n", req.Method, err)
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
