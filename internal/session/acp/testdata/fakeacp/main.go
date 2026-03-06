// Command fakeacp is a minimal ACP server for integration tests.
// It reads JSON-RPC from stdin and responds to the ACP handshake.
// On session/prompt it echoes the text as a session/update notification
// then sends the response. Stays alive until SIGTERM.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *int64           `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
}

type promptParams struct {
	SessionID string           `json:"sessionId"`
	Messages  []promptMessage  `json:"messages"`
}

type promptMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func respond(id *int64, result any) {
	data, _ := json.Marshal(result)
	msg := message{JSONRPC: "2.0", ID: id, Result: data}
	out, _ := json.Marshal(msg)
	fmt.Fprintln(os.Stdout, string(out))
}

func notify(method string, params any) {
	data, _ := json.Marshal(params)
	msg := message{JSONRPC: "2.0", Method: method, Params: data}
	out, _ := json.Marshal(msg)
	fmt.Fprintln(os.Stdout, string(out))
}

func main() {
	const sessionID = "fakeacp-session-1"

	// Handle SIGTERM gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		switch msg.Method {
		case "initialize":
			respond(msg.ID, map[string]any{
				"serverInfo": map[string]string{"name": "fakeacp", "version": "1.0"},
			})
		case "initialized":
			// Notification — no response.
		case "session/new":
			respond(msg.ID, map[string]string{"sessionId": sessionID})
		case "session/prompt":
			var params promptParams
			if err := json.Unmarshal(msg.Params, &params); err == nil {
				var text string
				for _, m := range params.Messages {
					for _, c := range m.Content {
						text += c.Text
					}
				}
				notify("session/update", map[string]any{
					"sessionId": sessionID,
					"content":   []contentBlock{{Type: "text", Text: "echo: " + text}},
				})
			}
			respond(msg.ID, map[string]any{})
		}
	}
}
