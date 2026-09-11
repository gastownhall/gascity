# Worker / Runtime / Transport — de-conflated execution model (v0)

Status: **proposal** · Supersedes the implicit weld in `internal/runtime/runtime.go`

## 1. Why

Two words in this codebase each carry several meanings, and the confusion is
load-bearing — it shapes how we name products on top of the stack.

- **"provider"** means three different things: the model vendor (aimux
  `resolve_upstream`), the session backend (`runtime.Provider`), and the remote
  sandbox vendor (RPP `gc-runtime-<provider>` — Daytona/E2B/Morph/Runloop/Blaxel).
- **"runtime"** means two: the tmux/CLI-driving layer, and the *where-it-runs*
  layer (RPP "Runtime Provider Protocol", the k8s `runtime.Provider`).

The root cause in code is that `runtime.Provider` (27 methods,
`internal/runtime/runtime.go:106`) does **two jobs at once**:

1. it provisions *where* the process lives — `Start`/`Stop`/`CopyTo`/`ListRunning`
   (local tmux, k8s pod, RPP sandbox), and
2. it drives *how* you talk to the live harness — `Peek`/`SendKeys`/`Nudge`/
   `ClearScrollback` (pure tmux/acp mechanics).

This doc separates those two jobs and assigns every existing word exactly one
meaning. **No new vocabulary is invented** — `Worker`, `Transport`, `Runtime`
already exist in gascity; `Upstream`, `Harness` already exist in aimux. We are
de-conflating, not renaming for novelty.

## 2. Canonical vocabulary

A **Worker** is the one handle callers talk to
(`start/stop/peek/nudge/history`). It is fully specified by five orthogonal axes:

| Question it answers | Noun | Examples | Resolves the overload of… |
|---|---|---|---|
| What do I talk to? | **Worker** | — | (keep) |
| Which brain? | **Model** | `opus-4.8`, `gpt-5.5` | — |
| Who serves it? | **Upstream** | `anthropic`, `manifold`, `byok:acme` | "provider" (model sense) |
| Which agent CLI? | **Harness** | `claude-code`, `codex` | gascity "builtin provider/profile" |
| How is it driven? | **Transport** | `tmux`, `acp`, `subprocess` | "runtime" (the *how* half) |
| Where does it run? | **Runtime** | `local`, `daytona`, `e2b`, … | "runtime" (the *where* half) |
| Who supplies the remote runtime? | **Runtime Provider** (RPP) | `daytona`, `e2b`, `morph`, `runloop`, `blaxel`, `custom` | "provider" (compute sense) |

Read as one sentence:

> A **Worker** is a **Harness** running a **Model** (served by an **Upstream**),
> attached over a **Transport**, inside a **Runtime** — local, or a sandbox from
> a **Runtime Provider**.

Two rules keep it unambiguous forever:

1. **"provider" is never bare.** It is always **Upstream** (model plane) or
   **Runtime Provider** (compute plane). Those are the only two provider planes,
   and they sit at opposite ends of the stack.
2. **"runtime" = where, "transport" = how.** tmux/acp are Transports. local/
   sandbox are Runtimes. RPP keeps its name ("Runtime Provider Protocol"); the
   cost is we never again call tmux/acp a "runtime."

## 3. The structural cut

```
WorkerSpec ─resolve─► RuntimeRequest ──► Runtime.Provision ──► Place   (WHERE: local | RPP sandbox)
           └────────► LaunchSpec    ──► Transport.Launch(Place) ─► Attachment (HOW: tmux | acp)
                                          │
Worker ─── start/stop/peek/nudge/history ─┘   (composes Place + Attachment; the only thing callers touch)
```

- **Runtime** owns the *where*; a **Place** is one provisioned environment whose
  only jobs are running commands and staging files.
- **Transport** owns the *how*; it drives the harness **through** the Place it is
  handed, and returns an **Attachment** (the live driving surface).
- Because a Transport drives through `Place.Exec`, **one tmux Transport is
  correct for local and for every RPP sandbox** — only the Place underneath
  changes. This delivers "CLIs run in tmux for local and remote instantiations"
  with a single implementation.

## 4. WorkerSpec

```go
// WorkerSpec is the complete, location-agnostic description of one Worker.
// Every field is an independent axis; nothing here knows where it will run.
type WorkerSpec struct {
	Model     string // "opus-4.8", "gpt-5.5"            — the checkpoint (request label)
	Upstream  string // "anthropic", "manifold", "byok:acme" — who serves+resolves the model
	Harness   string // "claude-code", "codex"           — the agent CLI
	Transport string // "tmux", "acp", "subprocess"      — how the harness is driven
	Runtime   string // "local", "daytona", "e2b", ...   — where it runs (Runtime Provider id)

	WorkDir string            // the workspace to materialize
	Prompt  string            // initial prompt, if any
	Env     map[string]string // caller extras (merged UNDER resolved Upstream env)
}
```

### 4.1 Model vs Upstream — they are not the same field

`Model` and `Upstream` are coupled but distinct, and the distinction is what
makes virtual models possible. The DNS analogy is exact:

| | Analogy | In the stack |
|---|---|---|
| **Model** | the hostname you look up (`coder.internal`) | the single string in the request body's `model` field |
| **Upstream** | which resolver you ask | base_url **+ credentials + alias/resolution authority** |
| **Virtual model** | a hostname only your resolver knows, re-pointable without the client changing the name | a `Model` name a proxy/gateway resolves to a backing model |

**There is exactly one model label — `Model` — and it is what the harness sends.**
aimux keys off the request body's `model` field (`serve.rs:562` →
`resolve_model`), rewrites it to the backing id **server-side** (`serve.rs:669`
`rewrite_model`), and **passes unknown names through unchanged**
(`manifold.rs:928`). So:

- `Model="coder"`, `Upstream="manifold"` → harness sends `coder`; the proxy
  maps `coder` → a backing model, invisibly. **Virtual model.**
- `Model="claude-opus-4-8"`, `Upstream="anthropic"` → sent as-is to
  `api.anthropic.com`. **Direct.**

The virtual model **cannot** overwrite the real model because `Upstream` carries
**no model name** — there is no second source of truth. The rewrite happens at
the Upstream, not in the WorkerSpec.

Why keep them as two axes rather than collapsing into `Model`:

1. **Credentials live on Upstream, not Model.** `opus-4.8` does not say whether
   to use a pooled account, a direct API key, or a self-hosted endpoint.
2. **Many-to-many.** One `Model` label may be served by several Upstreams
   (anthropic-direct / pool / self-hosted); one Upstream serves many models.
   Collapsing would make re-pointing the backing model without touching the
   client impossible.
3. **Swap supply without touching the client.** Re-pointing a `Model` name to a
   different backing model or account is an `Upstream`-side change; the harness
   command line is unchanged.

They are orthogonal in representation, constrained in valid combinations (you
cannot ask `anthropic`-direct for the `coder` alias, just as you cannot run the
`claude` model flag on the `codex` harness).

## 5. The Worker interface

Worker no longer holds a `runtime.Provider`; it holds a resolved
`(Runtime, Transport)` pair and the live `(Place, Attachment)`.

```go
// Worker is the one interface callers talk to.
type Worker interface {
	Start(ctx context.Context) error          // Provision (where) → Launch (how)
	Stop(ctx context.Context) error           // Close (how) → Teardown (where)
	Attach(ctx context.Context) error         // a human terminal joins the live session
	Peek(ctx context.Context, lines int) (string, error)
	Nudge(ctx context.Context, req NudgeRequest) (NudgeResult, error)
	Message(ctx context.Context, req MessageRequest) (MessageResult, error)
	Interrupt(ctx context.Context) error
	History(ctx context.Context, req HistoryRequest) (*HistorySnapshot, error)
	State(ctx context.Context) (State, error) // unchanged: Phase/SessionID/...
}
```

The richer sub-interface decomposition (`LifecycleHandle`, `MessagingHandle`,
`TranscriptHandle`, …) from `internal/worker/handle.go` carries over unchanged;
only the construction changes.

## 6. Runtime + Place — the WHERE (the RPP layer, typed)

```go
// Runtime provisions Places. A "Runtime Provider" (RPP pack: daytona/e2b/morph/
// runloop/blaxel/custom) is a Runtime whose Places are remote sandboxes.
// "local" is the Runtime whose Places are this box.
type Runtime interface {
	Provision(ctx context.Context, name string, req RuntimeRequest) (Place, error) // RPP: start
	Open(ctx context.Context, name string) (Place, bool, error)                    // RPP: stateless re-resolve by name
	List(ctx context.Context, prefix string) ([]string, error)
	Capabilities() RuntimeCapabilities // the RPP `protocol` handshake, typed
}

// Place is one execution environment. Note what is NOT here: no peek, no
// sendkeys, no nudge — driving the harness is the Transport's job, done THROUGH
// Place.Exec. A Place only runs commands and stages files.
type Place interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error) // RPP: exec (one-shot, wait)
	Stage(ctx context.Context, files []CopyEntry) error            // RPP: env.workspace
	IsRunning(ctx context.Context) (bool, error)                   // RPP: is-running
	Teardown(ctx context.Context) error                            // RPP: stop (idempotent)
	Env() map[string]string                                        // GC_*/upstream env (env.identity)
}

// StreamPlace is the OPTIONAL Place extension for Runtimes that can host a
// long-lived attached process (duplex stdio). tmux does NOT need it; acp and
// subprocess do. Probe with a type assertion, exactly like today's optional
// runtime extensions (InteractionProvider, IdleWaitProvider, …).
type StreamPlace interface {
	Dial(ctx context.Context, req DialRequest) (Stream, error)
}

// TTYPlace is the OPTIONAL Place extension for Runtimes that can attach an
// interactive human terminal (duplex PTY). Distinct from StreamPlace: a stream
// is raw process stdio for acp; a TTY adds resize, signals, and raw mode for a
// human at a keyboard. A Runtime may offer one, both, or neither beyond Exec.
// The tmux Transport's Attach needs this on a REMOTE runtime; local implements
// it trivially.
type TTYPlace interface {
	AttachTTY(ctx context.Context, req AttachRequest) (TTY, error)
}

type ExecRequest struct {
	Command string
	Dir     string // cwd; defaults to the Place work_dir
	Env     map[string]string
	Stdin   []byte
}
type ExecResult struct {
	Output   []byte // combined stdout+stderr (RPP semantics)
	ExitCode int    // == the command's exit code
}

type DialRequest struct {
	Command string
	Dir     string
	Env     map[string]string
}
// Stream is the harness child's stdio: Read = stdout, Write = stdin.
type Stream interface {
	io.ReadWriteCloser
	Stderr() io.Reader
	Wait(ctx context.Context) (exitCode int, err error)
}

type AttachRequest struct {
	Session string // the live session a human joins (e.g. tmux session name)
}

// TTY is a human terminal channel: stdio plus terminal control.
type TTY interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// RuntimeCapabilities is the RPP handshake, typed instead of a string list.
type RuntimeCapabilities struct {
	Env       []string // "env.workspace", "env.tooling", "env.identity", "env.ledger"
	Stream    bool     // RPP "proc.stream": raw process stdio stream (for acp's JSON-RPC)
	AttachTTY bool     // RPP "tty.attach": interactive human terminal (PTY resize/signals)
}
```

`exec.Provider` + a `gc-runtime-daytona` script collapses into a `Runtime` whose
`Place.Exec` shells the RPP `exec` op. `local` and `k8s` are just two more
Runtimes.

## 7. Transport + Attachment — the HOW (tmux/acp), driven over a Place

```go
type Transport interface {
	// Launch starts the harness inside an already-provisioned Place and returns a
	// live Attachment. Every control op runs through place.Exec (tmux) or a
	// place.(StreamPlace).Dial stream (acp) — which is why one tmux Transport is
	// correct for local AND every RPP sandbox.
	Launch(ctx context.Context, place Place, spec LaunchSpec) (Attachment, error)
	Open(ctx context.Context, place Place, name string) (Attachment, bool, error)
	Attach(ctx context.Context, place Place, name string) error // interactive human join; needs TTYPlace when remote
	Name() string      // "tmux" | "acp" | "subprocess"
	NeedsStream() bool // tmux:false  acp/subprocess:true — gates against RuntimeCapabilities.Stream
	Capabilities() TransportCapabilities
}

// Attachment is the live driving surface — the half of today's runtime.Provider
// that is pure tmux/acp mechanics.
type Attachment interface {
	Peek(ctx context.Context, lines int) (string, error)
	Nudge(ctx context.Context, content []ContentBlock, d NudgeDelivery) error
	SendKeys(ctx context.Context, keys ...string) error
	Interrupt(ctx context.Context) error
	Observe(ctx context.Context) (LiveObservation, error) // liveness, last-activity, attached?
	History(ctx context.Context) (TranscriptRef, error)
	ClearScrollback(ctx context.Context) error
	Close(ctx context.Context) error
}

type LaunchSpec struct {
	Command           string // harness binary + resolved flags (model, prompt) — see §9
	WorkDir           string
	Lifecycle         Lifecycle
	ReadyPromptPrefix string
	ProcessNames      []string
	Env               map[string]string // harness-only extras; the bulk lives on the Place
}

type TransportCapabilities struct {
	SendKeys, ClearScrollback bool // tmux: yes; acp: no
	StructuredInteraction     bool // acp: yes; tmux: no
	IdleWait                  bool
}
```

tmux is identical local and remote — it never knows which it is on:

```go
func (t *tmuxTransport) Launch(ctx context.Context, place Place, spec LaunchSpec) (Attachment, error) {
	if _, err := place.Exec(ctx, ExecRequest{
		Command: fmt.Sprintf("tmux new-session -d -s %s %q", t.session, spec.Command),
	}); err != nil {
		return nil, err
	}
	return &tmuxAttachment{place: place, session: t.session}, nil
}
func (a *tmuxAttachment) Peek(ctx context.Context, lines int) (string, error) {
	r, err := a.place.Exec(ctx, ExecRequest{
		Command: fmt.Sprintf("tmux capture-pane -p -S -%d -t %s", lines, a.session),
	})
	return string(r.Output), err
}
func (t *tmuxTransport) NeedsStream() bool { return false }
```

acp type-asserts the optional stream extension and fails fast otherwise:

```go
func (t *acpTransport) Launch(ctx context.Context, place Place, spec LaunchSpec) (Attachment, error) {
	sp, ok := place.(StreamPlace)
	if !ok {
		return nil, ErrStreamUnsupported
	}
	stream, err := sp.Dial(ctx, DialRequest{Command: spec.Command, Dir: spec.WorkDir, Env: spec.Env})
	if err != nil {
		return nil, err
	}
	return &acpAttachment{conn: jsonrpc.NewConn(stream), stream: stream}, nil
}
func (t *acpTransport) NeedsStream() bool { return true }

// tmux Attach: local always implements TTYPlace; a remote runtime does so only if
// it declared tty.attach. Otherwise attach degrades gracefully — Peek still works
// for read-only observation.
func (t *tmuxTransport) Attach(ctx context.Context, place Place, name string) error {
	tp, ok := place.(TTYPlace)
	if !ok {
		return ErrAttachUnsupported // remote runtime can't host an interactive terminal
	}
	tty, err := tp.AttachTTY(ctx, AttachRequest{Session: name})
	if err != nil {
		return err
	}
	defer tty.Close()
	return proxyTerminal(ctx, tty) // pipe caller stdio ⇄ tty; forward SIGWINCH → tty.Resize
}
```

### 7.1 Why tmux is the universal transport and acp is premium

- **tmux externalizes persistence to a daemon.** Each control op is a fresh
  one-shot `Exec` that reconnects to the tmux-managed session, so tmux rides the
  **minimal RPP `Exec` floor** and works on *every* Runtime Provider.
- **acp's process IS the session** — a persistent JSON-RPC duplex over the
  child's stdio — so it requires a `StreamPlace` (`proc.stream`).

Three distinct Place I/O surfaces fall out, and they are **independent
capabilities** — a Runtime may offer any subset beyond the floor:

- **`Exec`** (floor, every Runtime) — one-shot, output-captured. tmux rides this.
- **`proc.stream`** (`StreamPlace.Dial`) — raw process stdio for acp's JSON-RPC.
- **`tty.attach`** (`TTYPlace.AttachTTY`) — an interactive human terminal. The
  tmux Transport's `Attach` needs this on a REMOTE runtime (driving tmux
  headlessly is one-shot `Exec`, but a human joining a remote session is a duplex
  PTY). acp has no human-attach surface.

Durability follows from this and should be leaned into, not hidden:

- tmux sessions survive a controller restart — `Open` reconnects via `Exec`.
- acp streams do **not** survive unless the Runtime brokers/daemonizes the
  process; `acp.Open` returns `(nil, false, nil)` after a drop and forces a
  restart.

**Posture:** tmux is the default for long-lived/background workers and the only
transport guaranteed on every Runtime Provider; acp is the premium transport for
interactive/structured-interaction workers on streaming-capable runtimes.

## 8. Composition — the whole Worker is two lines each way

```go
func (w *DefaultWorker) Start(ctx context.Context) error {
	rr, ls, err := w.resolver.Resolve(ctx, w.spec) // axes → (where-req, how-cmd)
	if err != nil {
		return err
	}
	if w.transport.NeedsStream() && !w.runtime.Capabilities().Stream {
		return fmt.Errorf("transport %q needs a streaming runtime; %q is exec-only",
			w.transport.Name(), w.spec.Runtime)
	}
	if w.place, err = w.runtime.Provision(ctx, w.name, rr); err != nil { // WHERE
		return err
	}
	w.att, err = w.transport.Launch(ctx, w.place, ls) // HOW, over the Place
	return err
}

func (w *DefaultWorker) Stop(ctx context.Context) error {
	if w.att != nil {
		_ = w.att.Close(ctx) // stop driving first
	}
	return w.place.Teardown(ctx) // then tear down the box
}

func (w *DefaultWorker) Peek(ctx context.Context, n int) (string, error) { return w.att.Peek(ctx, n) }
```

## 9. Upstream resolution

An Upstream resolves to the environment that points the harness at a model API.
A virtual model is just an Upstream that injects `ANTHROPIC_BASE_URL` pointing at
a proxy/gateway (e.g. aimux) plus an API key; alias resolution happens
proxy-side. The harness and Transport never know the difference.

```go
type Upstream interface {
	// Env points the harness at this upstream. The Worker does not care which.
	Env(ctx context.Context) (map[string]string, error)
}

// proxyUpstream injects a base_url + key; the model label rides on
// WorkerSpec.Model and any aliasing resolves proxy-side (it carries NO model name).
func (u proxyUpstream) Env(ctx context.Context) (map[string]string, error) {
	return map[string]string{
		"ANTHROPIC_BASE_URL": u.baseURL, // a proxy/gateway that pools accounts + fails over
		"ANTHROPIC_API_KEY":  u.key,
	}, nil
}
// directUpstream → returns nil (harness uses its built-in default).
// keyUpstream    → injects a self-supplied provider key.

func (r *Resolver) Resolve(ctx context.Context, s WorkerSpec) (RuntimeRequest, LaunchSpec, error) {
	h := r.harnesses.Lookup(s.Harness) // binary, args, model-flag rule, ACP support
	up, _ := r.upstreams.Resolve(s.Upstream)
	upEnv, err := up.Env(ctx)
	if err != nil {
		return RuntimeRequest{}, LaunchSpec{}, err
	}
	return RuntimeRequest{
			WorkDir: s.WorkDir,
			Env:     merge(s.Env, upEnv),         // upstream env rides on the Place (env.identity)
			Stage:   r.overlaysFor(s),
		},
		LaunchSpec{
			Command:      h.Command(s.Model, s.Prompt), // e.g. `claude --model opus-4.8 -p "…"`
			WorkDir:      s.WorkDir,
			Lifecycle:    LifecycleOneShot,
			ProcessNames: h.ProcessNames,
		}, nil
}
```

## 10. Optional capabilities — keep the type-assert pattern, just split it

The optional extension interfaces in `runtime.go:222-279` survive in spirit;
they attach to the correct half. Interaction / idle-wait are **Transport**
concerns; `env.*` guarantees and `proc.stream` are **Runtime** concerns.

```go
// Only acp Attachments implement this; callers type-assert like today.
type InteractiveAttachment interface {
	Pending(ctx context.Context) (*PendingInteraction, error)
	Respond(ctx context.Context, resp InteractionResponse) error
}
// Only Runtimes that proxy the host work store (beads) into the sandbox
// implement this (RPP env.ledger capability — the agent's bd reaches the host).
type LedgerRuntime interface {
	BridgeLedger(ctx context.Context, place Place, controllerAPI string) error
}
```

## 11. Migration map — nothing is thrown away

| Today (`runtime.Provider`) | Goes to | Axis |
|---|---|---|
| `Start` | `Runtime.Provision` + `Transport.Launch` | both (the weld) |
| `Stop` | `Attachment.Close` + `Place.Teardown` | both |
| `CopyTo`, overlay staging | `Place.Stage` | where (env.workspace) |
| `ListRunning` | `Runtime.List` | where |
| `IsRunning` | `Place.IsRunning` | where |
| `Peek`, `SendKeys`, `ClearScrollback`, `Nudge` | `Attachment.*` | how |
| `Interrupt`, `ProcessAlive`, `IsAttached`, `GetLastActivity` | `Attachment.Interrupt` / `Attachment.Observe` | how |
| `Capabilities` | split: `RuntimeCapabilities` + `TransportCapabilities` | both |
| **`auto.Provider`** (tmux-vs-ACP) | a **Transport selector** | how |
| **`hybrid.Provider`** (local-vs-remote) | a **Runtime selector** | where |

The last pair is the proof the cut is real: the two composite "providers" that
exist today turn out to be selectors on the two now-separated axes — `auto`
picks a Transport, `hybrid` picks a Runtime.

## 12. From a formula step to a WorkerSpec

`WorkerSpec` is the shared atom, so a formula step resolves cleanly into one:

- A formula instantiates a wisp; **each step's metadata resolves into a
  `WorkerSpec`**; the target pool hands out a `Worker` to execute it.
- A step `{gc.provider="claude-code", gc.model="opus-4.8", gc.run_target="reviewer"}`
  becomes `WorkerSpec{Harness:"claude-code", Model:"opus-4.8", Upstream:<pool
  default>, Transport:<pool default>, Runtime:<pool default>}`.

Same five axes, top to bottom.

## 13. Decisions locked

- A **Run** is one execution of a formula/order/chat.
- **Runtime Provider** (RPP) keeps its name; tmux/acp are always **Transport**.
- `Upstream` carries **no model name**; `Model` is the single request label.
- `Dial` is an **optional `StreamPlace` extension**, gated by
  `Transport.NeedsStream()` vs `RuntimeCapabilities.Stream`.
- RPP handshake gains a **`proc.stream`** capability + a streaming op; default
  per provider is exec-only.
- Interactive **`Attach` is its own capability (`tty.attach`)**, distinct from
  `proc.stream`: a process-stdio stream (acp) and a human PTY (attach) are
  different guarantees, tracked as separate columns per Runtime Provider.

## 14. Open questions

1. **Metadata store.** `SetMeta`/`GetMeta`/`RemoveMeta` are orthogonal to both
   axes (drain signaling, config fingerprints). Land them on a small per-Worker
   `MetaStore` rather than on Place or Attachment?
2. **`RunLive`** (re-apply `session_live` on config change without restart) is a
   Place-level `Exec` replay — confirm it belongs to Runtime, not Transport.
3. **Per-provider capability truth table** — for each of daytona/e2b/morph/
   runloop/blaxel, two independent columns: does it support `proc.stream` (acp)
   and/or `tty.attach` (interactive attach), and via what (PTY, websocket exec,
   ssh)? A provider may have one, both, or neither beyond the `Exec` floor.
