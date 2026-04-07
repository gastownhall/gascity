# Module Brief: gc-pty-bridge

| Field | Value |
|-------|-------|
| **Module Name** | gc-pty-bridge |
| **Purpose** | Standalone PTY-over-WebSocket binary that enables interactive terminal sessions inside Docker containers, bypassing Docker's broken PTY multiplexing bridge that deadlocks TUI renderers (confirmed with Claude Code's Ink/React TUI). Ships in container images alongside `gc` and is called by the exec provider script. |
| **Boundary: Owns** | 1. `gc-pty-bridge serve <command> [args...]` -- creates a real PTY via `creack/pty` (forkpty), execs the command, serves PTY I/O over WebSocket with terminal resize support and output ring buffer for peek. 2. `gc-pty-bridge attach <host:port>` -- connects to a WebSocket PTY server, sets local terminal to raw mode, proxies stdin/stdout bidirectionally, forwards SIGWINCH as WebSocket control messages. 3. `internal/pty/` package -- reusable PTY-over-WebSocket server and client types (not CLI-specific). 4. WebSocket message protocol definition (data frames for I/O, control messages for resize). |
| **Boundary: Consumes** | 1. `golang.org/x/term` for raw mode (already an indirect dep). 2. `gorilla/websocket` (already in go.mod as indirect dep -- will become direct). 3. `creack/pty` (new direct dependency). 4. Does NOT consume `internal/runtime.Provider` -- gc-pty-bridge is a standalone tool. 5. Does NOT use cobra -- standalone binary with simple subcommand dispatch. The Docker exec provider script (`gc-session-docker`) will consume gc-pty-bridge as a subprocess. |
| **Public Surface** | **CLI surface (cmd/gc-pty-bridge/):** `gc-pty-bridge serve --port <port> [--buffer-lines <n>] <command> [args...]` -- starts PTY WebSocket server. `gc-pty-bridge attach [--raw] <host:port>` -- connects terminal to PTY server. **Go API (internal/pty/):** `type Server struct` -- `NewServer(cmd string, args []string, opts ServerOptions) *Server`, `Server.ListenAndServe(ctx context.Context, addr string) error`. `type Client struct` -- `NewClient(addr string, opts ClientOptions) *Client`, `Client.Connect(ctx context.Context) error`. `type ResizeMessage struct { Rows, Cols uint16 }`. Protocol: binary frames for PTY data, text frames with JSON `{"type":"resize","rows":N,"cols":N}`. |
| **External Dependencies** | `github.com/creack/pty` v1.1.24+ -- Go wrapper for forkpty(). `github.com/gorilla/websocket` -- already in go.mod (promote from indirect to direct). `golang.org/x/term` -- already available transitively. |
| **Inherited Constraints** | 1. ZERO hardcoded roles -- gc-pty-bridge is generic (runs any command, not Claude-specific). 2. `internal/` packages for library code. 3. TDD -- tests first. 4. No panics in library code -- return errors with context. 5. Unit tests next to code (`*_test.go`). 6. Tmux safety -- gc-pty-bridge does not interact with tmux at all. |
| **Repo Location** | `internal/pty/` -- server and client library code (`server.go`, `client.go`, `protocol.go`, tests). `cmd/gc-pty-bridge/main.go` -- standalone binary entry point with subcommand dispatch (serve/attach). `cmd/gc-pty-bridge/main_test.go` -- tests for CLI flag parsing. |
| **Parallelism Hints** | Three independent work streams: (A) `internal/pty/server.go` + `server_test.go` -- PTY creation, WebSocket serving, output buffer. (B) `internal/pty/client.go` + `client_test.go` -- WebSocket client, raw mode, resize forwarding. (C) `cmd/gc-pty-bridge/main.go` -- depends on A and B being at least stub-complete. Stream A and B can be built in parallel. Stream C is sequential after A+B. Protocol types (`protocol.go`) should be defined first as a shared dependency for A and B. |
| **Cross-File Coupling** | `internal/pty/protocol.go` defines shared message types used by both `server.go` and `client.go` -- must be defined before either. `cmd/gc-pty-bridge/main.go` imports `internal/pty`. No coupling to `cmd/gc/` at all. |
| **Execution Mode Preference** | `Tool-Integrated` -- the design is fully specified from the ttyd validation. No ambiguous design decisions remain. The WebSocket protocol, PTY creation mechanism, and CLI interface are all determined. |
| **Definition of Done** | 1. `gc-pty-bridge serve /bin/sh` starts a WebSocket server on a configurable port and creates a real PTY running `/bin/sh`. 2. `gc-pty-bridge attach localhost:<port>` connects to the server, enters raw mode, and provides full interactive terminal access. 3. Terminal resize (SIGWINCH) propagates from client to server and resizes the PTY. 4. Output ring buffer supports peek-style reads (configurable line count). 5. Clean shutdown: client disconnect does not crash server; server exit closes all clients; SIGINT/SIGTERM handled gracefully. 6. `go test ./internal/pty/...` passes with unit tests covering: server startup/shutdown, client connect/disconnect, resize message round-trip, output buffer overflow, concurrent client access. 7. `go test ./cmd/gc-pty-bridge/...` passes. 8. `go vet ./internal/pty/... ./cmd/gc-pty-bridge/...` clean. 9. All exported types and functions have doc comments. |

---

## Supplementary Analysis

### Why a standalone binary, not a gc subcommand

gc-pty-bridge runs inside Docker containers as infrastructure for the exec provider. The `gc` binary inside containers exists for hooks (`gc prime`, `gc nudge`, `gc hook`) — lightweight CLI commands. Adding PTY/WebSocket serving to `gc` would:
- Bloat every `gc` binary with `creack/pty` (cgo, Unix-only) and `gorilla/websocket`
- Mix infrastructure concerns (PTY bridging) with orchestration concerns (hooks, session management)
- Require `//go:build !windows` constraints that complicate the `gc` build matrix

A standalone binary keeps concerns separated. The exec provider script (`gc-session-docker`) calls `gc-pty-bridge serve` inside the container and `gc-pty-bridge attach` from the host — matching the existing pattern where the script calls external tools (`docker`, `tmux`) as subprocesses.

### Why not cobra

The `gc` binary uses cobra for its complex command tree (40+ subcommands). gc-pty-bridge has exactly two subcommands (`serve` and `attach`) with a handful of flags each. Simple `os.Args` dispatch with `flag` package is sufficient and avoids pulling cobra as a dependency into the standalone binary. This keeps the binary small (~3MB static).

### WebSocket protocol design

Keeping the protocol simple and ttyd-compatible where possible:

- **Binary frames**: raw PTY output (server-to-client) and raw terminal input (client-to-server)
- **Text frames**: JSON control messages -- currently only `{"type":"resize","rows":N,"cols":N}`
- No authentication in v1 (container-internal communication only; the WebSocket listener binds to the container's loopback or a container-internal port)
- No TLS in v1 (same reason -- container-local traffic)

### Integration with gc-session-docker (out of scope for this module)

The exec provider script will be updated separately to:
- `start`: launch `gc-pty-bridge serve` inside the container instead of `tmux new-session -d`
- `attach`: run `gc-pty-bridge attach` instead of `docker exec -it tmux attach`
- `nudge`: send text over WebSocket to the running gc-pty-bridge server
- `peek`: read from the output ring buffer via a WebSocket query or HTTP endpoint

That integration is a separate module brief.

### Dependency on gorilla/websocket

Already present as an indirect dependency (pulled in by k8s client-go). Promoting to direct is safe and avoids adding a second WebSocket library. `nhooyr.io/websocket` was considered but gorilla is already in the dependency tree and is the more established library for this use case.

### What is explicitly NOT in scope

1. **Modifications to `gc-session-docker`** -- the exec provider script will be updated separately to use `gc-pty-bridge serve`/`attach` instead of tmux-in-Docker. That is a separate module.
2. **Modifications to `cmd/gc/`** -- gc-pty-bridge is standalone, no changes to the gc binary.
3. **Authentication or TLS** -- gc-pty-bridge is for container-internal or trusted-network use. Security hardening is future work if the scope expands.
4. **Multiple concurrent PTY sessions on one server** -- v1 is one PTY per server instance (matching ttyd's model). The exec provider runs one `gc-pty-bridge serve` per container.
5. **ACP (Agent Communication Protocol) integration** -- the ACP provider in `internal/runtime/acp/` is headless JSON-RPC and orthogonal to interactive PTY access.
6. **Any changes to `internal/runtime.Provider` interface** -- gc-pty-bridge operates below the provider abstraction; the Docker exec provider script calls it as a subprocess.

### Risk: Platform constraints

`creack/pty` uses forkpty() which is Unix-only. gc-pty-bridge will not work on Windows. This is acceptable because:
- Docker containers run Linux
- The `serve` subcommand only runs inside containers
- The `attach` subcommand runs on macOS/Linux hosts (the development targets)
- A `//go:build !windows` constraint on `internal/pty/` prevents compilation errors on Windows
