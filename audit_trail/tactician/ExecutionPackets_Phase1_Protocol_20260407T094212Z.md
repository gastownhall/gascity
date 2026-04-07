# Execution Packets: gc-pty-bridge -- Phase 1: Protocol Types & Dependencies

**Integration Branch:** `gc-pty-module`

---

## Batch Header

| Field | Value |
|-------|-------|
| Module | gc-pty-bridge |
| Phase | 1 -- Protocol Types & Dependencies |
| Milestone | `internal/pty/protocol.go` exists with all shared message types; `go get` for `creack/pty` done; `go vet ./internal/pty/...` passes |
| Packets in batch | 2 (1.1 sequential, then 1.2) |
| Integration Branch | `gc-pty-module` |

---

## Packet 1.1 -- Add `creack/pty` dependency

| Field | Value |
|-------|-------|
| **Packet ID** | 1.1 |
| **Depends On** | none |
| **Prerequisite State** | Branch `gc-pty-module` exists and is checked out. `go.mod` exists at repo root. `creack/pty` is NOT in go.mod. `gorilla/websocket` is already an indirect dep. `golang.org/x/term` is already an indirect dep. |
| **Objective** | Add `creack/pty` as a direct dependency. |
| **Allowed Files** | `go.mod`, `go.sum` |
| **Behavioral Intent** | **Positive:** After this packet, `go list -m github.com/creack/pty` succeeds and shows v1.1.24+. **Negative:** If `go get` fails (network, version constraint), the packet fails and escalates. **Edge:** No code files change -- this is purely dependency management. |
| **Checklist** | 1. Run `go get github.com/creack/pty@latest`. 2. Run `go mod tidy` to clean up. |
| **Commands** | `go vet ./...` |
| **Pass Condition** | `go list -m github.com/creack/pty` succeeds. `go vet ./...` clean. `go mod tidy` produces no diff. |
| **Commit Message** | `build: add creack/pty dependency for gc-pty-bridge` |
| **Stop / Escalate If** | `creack/pty` has breaking changes above v1.1.x that change the `pty.Start` API. Network failure prevents `go get`. |
| **Carry Forward** | Exact `creack/pty` version resolved. |

---

## Packet 1.2 -- Define protocol types

| Field | Value |
|-------|-------|
| **Packet ID** | 1.2 |
| **Depends On** | 1.1 |
| **Prerequisite State** | `go.mod` has `creack/pty` as a dependency. Directory `internal/pty/` does not yet exist. |
| **Objective** | Create `internal/pty/protocol.go` with the WebSocket message protocol types shared between server and client. |
| **Allowed Files** | `internal/pty/protocol.go`, `internal/pty/protocol_test.go` |
| **Behavioral Intent** | **Types to define:** (1) `ResizeMessage` struct with fields `Type string` (always `"resize"`), `Rows uint16`, `Cols uint16`. (2) `EncodeResize(rows, cols uint16) ([]byte, error)` -- serializes a ResizeMessage to JSON bytes. (3) `DecodeResize(data []byte) (ResizeMessage, error)` -- deserializes JSON bytes to ResizeMessage. **Positive cases:** `EncodeResize(24, 80)` produces `{"type":"resize","rows":24,"cols":80}`. `DecodeResize` of that output returns `ResizeMessage{Type:"resize", Rows:24, Cols:80}`. Round-trip: encode then decode preserves values. **Negative cases:** `DecodeResize([]byte("not json"))` returns an error wrapping the JSON parse failure. `DecodeResize([]byte({"type":"unknown"}))` returns an error indicating unrecognized message type. `DecodeResize([]byte({"type":"resize","rows":-1}))` -- negative values: uint16 cannot be negative in JSON with strict typing, but if the JSON contains a negative number, `json.Unmarshal` into uint16 will error -- verify this behavior. **Edge cases:** `EncodeResize(0, 0)` -- zero dimensions are valid (degenerate terminal). Maximum uint16 values (65535, 65535) round-trip correctly. Empty byte slice to DecodeResize returns error. **Build constraint:** File should have `//go:build !windows` since PTY is Unix-only. **Doc comments:** All exported types and functions must have doc comments. |
| **Checklist** | 1. Create `internal/pty/` directory. 2. Create `protocol.go` with package `pty`, build constraint `//go:build !windows`. 3. Define `ResizeMessage` struct with JSON tags. 4. Implement `EncodeResize` function. 5. Implement `DecodeResize` function with validation (type field must be `"resize"`). 6. Create `protocol_test.go` with build constraint `//go:build !windows`. |
| **Commands** | `go vet ./internal/pty/...`, `go test ./internal/pty/...` |
| **Pass Condition** | `go vet ./internal/pty/...` clean. `go test ./internal/pty/...` passes. All exported symbols have doc comments. |
| **Commit Message** | `feat(pty): add WebSocket protocol types for PTY resize messages` |
| **Stop / Escalate If** | Uncertainty about whether binary vs text WebSocket frame distinction should be encoded in Go types or left to the server/client usage. If the Contract Agent thinks a `DataMessage` wrapper type is needed, escalate for Tactician review. |

---

# Phase 2: Server (outline)

**Milestone:** `internal/pty/server.go` compiles, unit tests pass for: PTY creation, WebSocket upgrade, I/O proxying, resize handling, output ring buffer, graceful shutdown.
**Estimated packets:** 5
- 2.1: `ServerOptions` struct + `NewServer` constructor
- 2.2: `RingBuffer` output buffer (standalone, testable in isolation)
- 2.3: `Server.start` -- PTY creation via `creack/pty`, command exec
- 2.4: `Server.handleConn` -- WebSocket upgrade, bidirectional I/O proxy, resize dispatch
- 2.5: `Server.ListenAndServe` -- HTTP listener, context cancellation, graceful shutdown

**Key risks / unknowns:** `creack/pty` API surface needs confirmation (Packet 1.1 will surface this). Goroutine lifecycle for bidirectional copy needs careful design to avoid leaks.
**Depends on discoveries from:** Phase 1 (protocol types shape).

---

# Phase 3: Client (outline)

**Milestone:** `internal/pty/client.go` compiles, unit tests pass for: WebSocket dial, raw mode toggle, bidirectional I/O proxy, SIGWINCH forwarding, clean disconnect.
**Estimated packets:** 4
- 3.1: `ClientOptions` struct + `NewClient` constructor
- 3.2: `Client.Connect` -- WebSocket dial, raw mode entry/exit
- 3.3: `Client.proxy` -- bidirectional stdin/stdout copy with WebSocket
- 3.4: `Client.watchResize` -- SIGWINCH signal handler, resize message send

**Key risks / unknowns:** Raw mode restoration on panic/crash. SIGWINCH availability (Unix-only, confirmed acceptable). Testing raw mode without a real terminal.
**Depends on discoveries from:** Phase 1 (protocol types), Phase 2 may run in parallel.

---

# Phase 4: Binary Wiring (outline)

**Milestone:** `cmd/gc-pty-bridge/main.go` with subcommand dispatch (serve/attach), flag parsing, tested.
**Estimated packets:** 3
- 4.1: `main.go` scaffolding with `serve` subcommand -- flag parsing, calls `pty.NewServer` + `ListenAndServe`
- 4.2: `attach` subcommand -- flag parsing, calls `pty.NewClient` + `Connect`
- 4.3: `main_test.go` -- flag parsing tests, help output, error cases

**Key differences from prior plan:**
- Standalone binary, NOT a cobra subcommand of gc
- Simple `os.Args` dispatch with `flag` package (no cobra dependency)
- No changes to `cmd/gc/main.go`

**Key risks / unknowns:** None -- simple main.go pattern.
**Depends on discoveries from:** Phase 2 (Server API), Phase 3 (Client API).

---

# Phase 5: Integration & Hardening (outline)

**Milestone:** End-to-end `gc-pty-bridge serve` + `gc-pty-bridge attach` round-trip works. All DoD items verified.
**Estimated packets:** 2
- 5.1: Integration test (may need `//go:build integration` tag) -- serve + attach round-trip with `/bin/echo`
- 5.2: Hardening -- signal handling (SIGINT/SIGTERM), concurrent client disconnect, doc comment audit

**Key risks / unknowns:** Integration tests require a PTY-capable environment (CI may not have one). May need to skip in CI via build tag.
**Depends on discoveries from:** Phase 4 (binary wiring complete).
