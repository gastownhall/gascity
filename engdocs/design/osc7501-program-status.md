---
title: "OSC 7501 program status as the agent-state source"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-10-31 |
| Author(s) | Claude (Opus 5.5) |
| Issue | `ga-f4ovjm.7` (decision) in epic `ga-f4ovjm` |
| Supersedes | The synthesized, adversarially reviewed draft that preceded this document. Where the two disagree, this document wins. |

All `path:line` citations are at origin/main `6f488617b2`. The protocol is the
Program Status Protocol, OSC 7501, spec revision 0.4 (2026-10-10).

## Summary

OSC 7501 lets an agent program report its own state to its terminal: `idle`,
`working`, `done`, `blocked` (optionally `:permission`, `:question`, `:auth`),
`error`, plus child records and `clear`. tmux 3.4 drops the sequence from the
screen and never answers the support probe (`ESC ] 7501 ; ?`). Today gc infers
the same facts by scraping the pane: prompt prefixes, busy strings, dialog
regexes, blind readiness delays.

This design captures each declared agent's reports, publishes them as live
tmux pane options, and exposes a judged reading through a new runtime
capability. Consumers then use the program's own word where gc scrapes today,
one consumer at a time, behind a city switch, after a shadow run.

Five decisions carry the design:

1. **One capture path.** Every declared agent pane gets `pipe-pane -O` into
   `gc-osc7501 tap`. Probe-gated families (Claude) additionally run under
   `gc-osc7501 relay`, which only answers the probe. It parses nothing and
   publishes nothing.
2. **One companion binary.** `gc-osc7501` has three subcommands, `tap`,
   `relay` and `version`. It is pure Go and ships next to `gc`.
3. **A stdlib runtime contract.** `internal/runtime` gains `ProgramStatus`,
   `ProgramStatusProvider`/`ProgramStatusFor`, and the controller-only
   `ProgramCaptureHealer`. RUNTIME-CONTRACT-017 governs them. A record is
   positive-only, bound to the pane by a nonce, and every hold it creates is
   bounded. Age never turns a record into `idle`.
4. **Provider data plus a city switch.**
   - `[providers.<p>.program_status]` declares capture. Forced env and the
     relay wrap are both applied outside the launch fingerprint.
   - `[session] program_status = "off" | "shadow" | "on"` gates everything,
     readers included. The default is `off`.
5. **Shadow before cutover.**
   - With the switch at `shadow`, every decision point emits a typed event,
     `session.program_status_shadow`, holding closed tokens only.
   - Cutover slices then switch decisions consumer by consumer under `on`.

## Decisions

| # | Item | Decision | Why |
|---|---|---|---|
| a | Capture path | `pipe-pane -O` into `gc-osc7501 tap` for every declared agent. The relay is added only where the provider declares `relay = true`. It answers the probe and nothing else. | Keeps the one component that can kill an agent minimal. Gives one reader-liveness, heal and doctor model. Passive emitters get no wrapper. Local tmux has no other pipe user. See [§3](#3-capture-path). |
| b | Binary layout | One binary, `gc-osc7501`, with `tap`, `relay` and `version` subcommands. Shipped by goreleaser, the Homebrew formula, Bazel and `make install`. | One artifact, one lookup, one version handshake. Its 10-character name survives 15/16-byte `comm` truncation. |
| c | Contract | `ProgramStatusProvider` plus `ProgramStatusFor`, following the `TurnRunningFor` precedent. `ProgramCaptureHealer` handles controller heal. The validity enum's zero value is `Unread`. `HoldLive` bounds holds at 10 minutes. `AdvancedPast` provides positive evidence. | The zero value is safe: an unread reading carries no state. Reads go to the routed leaf. Readings are positive-only and bounded. See [§6](#6-runtime-contract). |
| d | Provider data | `[providers.<p>.program_status]` with `protocol`, `app`, `relay` and `env`. The env merges into the session env after `OperatorEnv`. The arm travels in `runtime.Config.ProgramStatus`, which is excluded from the fingerprint. | Follows the `UpstreamEnv` precedent, so nothing relaunches fleet-wide. One derivation serves every `runtime.Config` construction site. See [§8](#8-configuration). |
| e | City switch | `off`, `shadow` or `on`, default `off`. `off` behaves exactly as today, readers included. `shadow` arms, exposes the new fields and records comparisons. `on` makes decisions record-first. Every reading carries the mode it was taken under. | One knob and a read-side kill switch. Rollback relaunches nothing; relay-wrapped seats stay wrapped until their next restart. See [§8.5](#85-city-switch). |
| f | Conformance | A standalone `runtimetest.RunProgramStatusContract` driven by fixtures, kept out of the ledger-proved `TestXxxConformance` functions. `Fake`/`FullFake` implement the capability. A Go fake emitter (`go_binary`) drives the tmux integration lane. An inline `ProgramStatusCatalog` holds `WC-PSTAT-*` rows using the declare / drift / unsupported pattern. | Avoids the provider-ledger proof check and the RPP lockstep scan. Per RUNTIME-CONTRACT-005, a profile declaration is a certification decision. See [§12](#12-conformance). |
| g | Blocked refusal | The leaf refuses typing with an error wrapping `ErrPendingInteraction`, the sentinel parser-detected dialogs already use. Wait-idle and queue delivery queue; explicit `immediate` is refused. Bounded attempt counters refund. Holds are bounded. | Reuses `refuseOrQueueNudgeBehindPendingInteraction` semantics, so callers need no new vocabulary. See [§10](#10-blocked-refusal-queue-dont-fail). |
| h | Shadow sink | A typed event, `session.program_status_shadow`, written through the city events recorder from a bounded asynchronous queue in each process. The leaf reaches it through a stdlib sink function in `tmux.Config`. | The events log is the only facility that is multi-process, rotated and queryable. Precedent: `session.demand_claim_divergence`. The trace store is controller-only, OTel is a no-op without a collector, and the `.log` diagnostics overwrite. See [§9](#9-shadow-mode). |
| i | Relay wrap | Applied in `tmuxStartOps.createSession`/`respawnAgent`. Order: slice(`env -u`(relay(cmd))). `gc-osc7501` joins `wrapperCommands`. The wrap is applied only when the companion passes the capability gate. It never moves the fingerprint. Live seats keep their wrap until restart. | Follows the `wrapPaneCommand` precedent. Withheld env stays withheld from the relay and the agent. See [§5.3](#53-relay-wrap). |
| j | Deploy lineage | Slices land upstream-first, and either lineage works. **Port:** cherry-pick the merged slices plus an Opus-max port review. **Refresh:** run the origin/main build with `off` for a baseline window before shadow. | Edits to high-churn files stay thin. Shadow agreement compares two readings from the same build. See [§14](#14-rollout). |
| k | PTY library | The relay uses `golang.org/x/sys/unix` (pty open, winsize) and `golang.org/x/term` (raw mode). Both are already direct dependencies (`go.mod:39-40`). No creack/pty, no cgo. | No new module. Linux and darwin. |
| l | Typing callers | The inventory in [§10.2](#102-typing-caller-inventory) gives every production typing path and its required outcome under a live `blocked` record. The record joins the parser results at one point, `dialogInPane` (`internal/runtime/tmux/interaction.go:871`). The leaf rows go to `.22` and the startup row to `.23`. Seven caller paths already violate queue-not-fail for parser dialogs today; they move to a new, independent `.22b`. | Completes the queue-not-fail contract with one test per row. Fixing the seven paths first shrinks the cutover. |
| m | Registration | This PR reserves the id as Planned row RUNTIME-PLAN-006. The full RUNTIME-CONTRACT-017 row ([§6.3](#63-runtime-contract-017)) moves into the ledger with the conformance suite in `ga-f4ovjm.11`; `.14` adds the tmux evidence. 012 is reserved by the turn-observation plan; do not reuse it. `WC-PSTAT-*` rows ([§12.3](#123-workertest-rows)) land with the slices that prove them. | RUNTIME-CONTRACT-001: contracts land as conformance, not prose. A Planned row prevents another id collision like 012's. |
| U6 | Readiness widening | Report-based readiness applies to prefixless declared families only. Claude, Codex and Antigravity keep today's readiness. | Codex reports `idle` while its hooks-review modal is open (`internal/runtime/dialog.go:383-387`). The orchestrator decided U6. |

## Primitive Test

This is new SDK surface, so it must pass
`engdocs/contributors/primitive-test.md`. It is an optional capability behind
the existing runtime provider interface (RUNTIME-CONTRACT-004), not a new
primitive.

| Capability | Atomicity? | More useful as models improve? | Transport, not cognition? | Verdict |
|---|---|---|---|---|
| Capture and publish the program's own OSC 7501 record per pane | Yes. One writer per pane under a nonce guard; tmux serializes option writes. | Yes. A better agent reports its state more precisely, while screen heuristics degrade with every TUI redesign. | Yes. The program decides its state; gc moves the claim and judges only validity (nonce, pane, pipe, reader alive, bound). | SDK, as a runtime capability |
| Using the claim at gates | n/a | Yes. The scrape it replaces is model-version-specific. | The rules are positive-only vetoes and bounded holds, the same category as today's trust policy. No `if stuck then restart` is added; destructive backstops are unchanged. | SDK (leaf policy) |

## 1. Scope

**In v1 (local tmux runtime):**
- **Codex** through the tap. Verified against 0.165.
- **Claude** through the relay plus the tap. Requires Claude 2.1.295 or later; 2.1.311 is verified.
- **Later, each after verification and as provider data only:** pi, Antigravity and Kimi Code 2.4 or later (env-forced), and opencode, `cerebras` and `groq` (tap only).

**Follow-ups, not v1:**
- k8s, ssh and exec carrier taps (`ga-f4ovjm.28`)
- an herdr-core `ProgramStatusProvider` (`.29`)
- a push doorbell (`.30`)
- an exit annotation (`.31`)
- verified-submit enrollment (`.43`)
- a durable status-change event

**Never changed by a record:**
- startup dialog acceptance
- `Respond` keys
- crash accounting (`ExitRapidCrash`)
- `max_session_age`
- drain-ack SIGKILL
- `GetLastActivity`
- transcript turn ids and turn ends
- verified-submit eligibility

## 2. Architecture

```
 agent ──pty──▶ [gc-osc7501 relay]? ──pty──▶ tmux pane ──▶ screen (tmux drops OSC 7501)
   │  relay only when provider declares relay=true: answers ESC]7501;? into the
   │  child BEFORE forwarding the chunk that holds DA1; forwards every byte verbatim
   │
   │  pipe-pane -O (armed in the SAME tmux command list as new-session/respawn-pane)
   ▼
 gc-osc7501 tap -x <tmux> -S <socket_path> -t <pane_id> -n <rand> [-a <app>]
   reader goroutine: Scanner → Store (never blocks; tmux buffers without bound otherwise)
   writer goroutine: coalesce (25 ms → 1 s under churn), latest wins, guarded publish:
     tmux -S <sock> if-shell -F -t %N '#{==:#{@gc_ps_tap},<rand>.%N}' \
       'set-option -p -t %N @gc_ps v1|… ; set-option -p -t %N @gc_ps_text v1|…'
   │
 pane options: @gc_ps_tap=<rand>.%N (gc writes)  @gc_ps, @gc_ps_text (tap writes)
   │
 READ (polled, any gc process, no files, no controller memory)
   tmux state cache FetchState (2 s) + its ps snapshot (is the tap alive?)
   runtime inventory batch (controller lanes)
   one-look read at leaf decision points (same tmux invocation as the scan)
     agent-pane row only: pane_id pane_dead pane_pipe @gc_ps_tap @gc_ps (sanitized)
   ▼
 runtime.ProgramStatus (judged: mode, own nonce, pane, pipe, dead, tap alive, root)
   ├─ tmux leaf gates (shadow: compare + emit; on: vetoes, evidence, bounded holds)
   ├─ ProgramStatusFor → worker.Handle.State.Program → API, SSE, CLI
   └─ InventoryEntry.Program → cmd/gc lanes (backstop, ladder, sleep proof, queue)
 HEAL (controller, inventory-lane cadence): ProgramCaptureHealer re-arms/disarms
```

**Where state lives:**
- `@gc_ps_tap` is written by gc, in the arm list, with `set-option -p -F` binding it to `#{pane_id}`.
- `@gc_ps` and `@gc_ps_text` are written only by the tap, through the nonce guard.
- Everything else is judged at read time from live tmux facts and the process table.
- There are no status files. A controller restart rebuilds nothing.

## 3. Capture path

**Decision.** `pipe-pane -O` into the tap is the only capture mechanism, and it
is used for every declared agent. The relay is inserted only for providers
that declare `relay = true`. It answers the probe and forwards bytes. It never
parses reports, publishes, writes files or forks.

**Why:**

- **Data-path minimalism.** The relay is the only component whose failure kills the agent: closing the PTY master hangs up the child (SIGHUP). So it holds the least code: a probe matcher of about 30 lines, two copy loops and signal forwarding. Everything likely to have bugs lives in the tap, where a failure only loses status: limits, base64, store semantics, coalescing, retries, tmux forks. The scrape then takes over, and heal re-arms.
- **One validity model.** Reader liveness is `pane_pipe` plus the process table for every family. Heal, doctor and the conformance suite each have one path.
- **No wrapper for passive emitters.** Codex, opencode, and pi and Antigravity with env, never get a pane-root wrapper, so their `pane_current_command`, `WaitForCommand` and liveness paths are untouched.
- **Live heal.** A tap can be armed onto, and removed from, a running pane. A relay can only be inserted at create or respawn. So a relay-everywhere design would make the kill switch unable to undo anything without restarts.
- **The pipe conflict is not a v1 problem.**
  - Local tmux uses no `pipe-pane` today. The only user is in k8s pods (`internal/runtime/k8s/provider.go:324,388`), and k8s is out of v1.
  - A user's own `pipe-pane` displaces the tap. Readers see that as `reader_lost`, and heal never takes a foreign pipe over.
  - The k8s pane log is folded in later through `tap -tee` (`.28`).

**Rejected: one relay that answers and captures for every declared agent.**
- It puts the parser and publisher in the data path of every declared agent, so a tap bug becomes agent death for every family.
- It changes `pane_current_command` everywhere.
- It cannot be armed live, so the switch cannot disarm it.
- Its two advantages are both already covered. Avoiding the pipe conflict only matters for k8s, which is a follow-up. Catching the first report needs the relay to sit on the data path, but same-list arming already catches a report written at the program's first instant (verified 3/3 on create and 10/10 on respawn).

**Cost:**
- One tap per armed pane: about 4 MB RSS each for a stdlib reader, versus about 106 MB for `gc`.
- Each coalesced publish costs one `tmux` client fork, 4-6 ms.
- Relay panes add a relay of the same size. Measured with the prototype: DA1 round-trip 0.047 → 0.125 ms at the median, keystroke latency unchanged at about 9.5 ms, and nothing leaked to the screen.

## 4. Companion binary

### 4.1 Layout

| Path | Contents | Imports |
|---|---|---|
| `internal/runtime/programstatus` | `Scanner`, `ProbeMatcher`, `ParseBody`, `Store`, and the pane-option codec (`Encode`, `EncodeText`, `Decode`, `DecodeText`) | stdlib and `internal/runtime` only |
| `internal/runtime/programstatus/tap` | Tap engine: reader and writer goroutines, coalescing, and a guarded publish through an injected `Publisher` | stdlib and `programstatus` |
| `internal/runtime/programstatus/relay` | Relay engine: pty pair, raw mode, copy loops, probe answer, signals, exit passthrough | adds `golang.org/x/sys/unix` and `golang.org/x/term` |
| `cmd/gc-osc7501` | `main`: subcommand dispatch, flags, and `version` | the three packages above; never `//cmd/gc:gc_lib`, which is private (`scripts/cmd_gc_testhooks_test.go:91-97`) |

`gc` itself imports `programstatus` to decode records, and nothing else from
this list.

### 4.2 `gc-osc7501 tap`

```
gc-osc7501 tap -x <tmux-bin> -S <socket-path> -t <pane-id> -n <rand> [-a <app>]
```

**Startup:**
- Unsets `TMUX` and `TMUX_PANE`. The pipe child inherits the tmux server's global env, which can name another server (verified). Every write therefore passes `-S`.
- The nonce is `<rand>.<pane-id>`.

**Reader goroutine:**
- Reads stdin in 32 KiB chunks.
- `Scanner.Feed` reassembles reports split across reads. Verified: a split between `ESC` and `\`, a split mid-prefix, and a 6 KiB report spanning two 4095-byte reads.
- Accepts both BEL and ST terminators and unwraps DCS tmux passthrough.
- Recognizes the probe (sets `Probed`) and OSC 133 `A`.
- `Store.Apply` follows rev 0.4 ([§6.2](#62-record-semantics-and-codec)).
- It never blocks. It hands the latest snapshot to a one-slot mailbox.

**Writer goroutine:**
- **Coalescing:** at least 25 ms between publishes. The interval doubles up to 1 s while publishes stay above 10/s for 5 s, and resets after 5 s of calm.
- **Publish:** each publish is one guarded command (see [§2](#2-architecture)). `@gc_ps_text` is included only when the title, msg or last-error text changed.
- **Failure handling:** each write has a 2 s timeout. A failure retries latest-wins with backoff from 100 ms to 1 s. After 5 consecutive failures the tap exits 1, which readers see as `reader_lost` and heal repairs.

**App filter:**
- With `-a`, the tap drops a report whose effective `app` names another program. Effective means the record's own `app`, else one inherited from its nearest ancestor (spec).
- Reports with no `app` anywhere are accepted.

**Shutdown:** on EOF it flushes the pending publish once, which the nonce guard makes safe, then exits 0.

**Bounds:**
- An in-progress sequence is capped at 4096 bytes (the spec limit) and discarded past that.
- The store holds at most 64 child records, evicting the least recently updated.
- No timers other than the coalescer. No files.

### 4.3 `gc-osc7501 relay`

```
gc-osc7501 relay -- <argv...>
```

1. **Not a terminal.** If stdin is not a TTY, the relay `exec`s argv directly (transparent no-op).
2. **Child setup.**
   - Opens a pty pair: `/dev/ptmx`, then unlock and name via `TIOCSPTLCK`/`TIOCGPTN` on Linux and `TIOCPTYGRANT`/`TIOCPTYUNLK`/`TIOCPTYGNAME` on darwin.
   - Copies stdin's window size to the master before starting the child.
   - Puts stdin in raw mode (`term.MakeRaw`) and restores it on exit.
   - Starts the child with `SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}` and the slave on fds 0-2, then closes the slave in the parent.
3. **Output loop** (master to stdout).
   - `ProbeMatcher` sees each chunk. It is a strict state machine for `ESC ] 7 5 0 1 ; ?` followed by BEL or `ESC \`, and matches across reads.
   - When a chunk completes a probe, the relay first writes `ESC ] 7501 ; ? ESC \` into the master, then forwards the chunk. The program therefore sees the 7501 reply before tmux's DA1 reply.
   - Bytes are forwarded verbatim and in order. Probes are forwarded too, which tmux drops and the tap counts.
4. **Input loop** (stdin to master). Bytes are forwarded verbatim. Writes to the master are serialized with the reply under one mutex, so the 12-byte reply is never interleaved inside an input write.
5. **Signals.**
   - `SIGWINCH` copies the window size to the master; the kernel then signals the child's foreground group.
   - `SIGTERM`, `SIGHUP`, `SIGINT` and `SIGQUIT` are forwarded to the child's process group.
6. **Exit.**
   - When the child exits, the relay drains the master until `EIO`, for at most 250 ms (background grandchildren may hold the slave open).
   - It then exits with the child's code. If the child died by signal S, it resets S and re-raises it, so `#{pane_dead_status}` and `#{pane_dead_signal}` keep their fidelity (`internal/runtime/tmux/tmux.go:4381`).
7. **Pane gone.** Stdin EOF, or `EIO` on stdout, closes the master (hanging up the child), waits for it, and exits as in step 6. The relay never kills on a timer of its own; gc's Stop already kills the whole tree.
8. **Errors.** The relay never writes to stdout or stderr, because both are the pane. The one exception is a pre-start failure (pty, raw mode or exec): it prints a single line to stderr and exits 127, so start-crash diagnostics capture it.

**Budgets** (enforced by tests in `.35`):
- RSS of 8 MB or less.
- Added forward latency of 1 ms or less at p99 per chunk.
- DA1 round-trip of 0.25 ms or less at p50.
- No screen leakage.

### 4.4 Failure semantics

| Failure | Effect | Detected by | Recovery |
|---|---|---|---|
| Tap crashes or exits | Status lost for that pane | `ps` snapshot shows no tap with this nonce (`reader_lost`); `pane_pipe` goes to 0 at the next pane output | Heal re-arms once `pane_pipe=0` ([§5.4](#54-heal-and-disarm)) |
| Tap publish keeps failing | Record stale, then the tap exits | As above | Heal |
| Tap stops reading | tmux would buffer without bound (4.6 → 91 MB seen) | Prevented by construction: the reader never blocks. `TestTapKeepsDrainingWhileWriterBlocked` feeds 80 MB. | n/a |
| Relay crashes | The agent is hung up, and in practice dies | Pane dead or session gone | The existing crash accounting and restart path, unchanged. Doctor attributes relay-wrapped exits. |
| Companion missing or wrong version | No arming and no relay wrap | Capability gate ([§5.5](#55-capability-gate)) | Doctor names the fix |
| tmux too old or socket path unsafe | No arming | Capability gate | Doctor |

### 4.5 Packaging and lookup

**Goreleaser** (`.goreleaser.yml:3-22`):
- Add a second `builds` entry: `id: gc-osc7501`, `main: ./cmd/gc-osc7501`, `binary: gc-osc7501`, `CGO_ENABLED=0`, and the same goos/goarch.
- Leave the `gc` build's id at its default, because `release.yml:64` hardcodes `dist/gascity_linux_amd64_v1/gc`.
- The unfiltered archive then carries both binaries. Archive names, checksums and attestations are unchanged.

**Homebrew** (`release.yml`):
- Change line 222 to `bin.install "gc", "gc-osc7501"` (`.github/workflows/release.yml:221-223`). Keep the heredoc's ten-space indentation (`:245`).
- The formula test (`:240-242`) adds `system "#{bin}/gc-osc7501", "version"`.
- `homebrew-tap-smoke.yml:43` adds the same check.

**Bazel:**
- Add `cmd/gc-osc7501/BUILD.bazel` with a `go_binary` set to `pure = "on"`, modeled on `cmd/gc-write-mint/BUILD.bazel`.
- `make bazel-sync` regenerates the filegroups.

**Make:** `build` and `install` (`Makefile:136-141,171-191`) build and install both binaries. `clean` removes both.

**Lookup.** Resolved at every arm, never cached across arms:
1. A sibling of `os.Executable()` as returned. On darwin this keeps Homebrew's stable `bin/` symlink; on Linux `/proc/self/exe` is already resolved.
2. Otherwise, `exec.LookPath("gc-osc7501")`.
3. Tests set `tmux.Config.ProgramStatusBin` directly.

A Homebrew upgrade can remove an old Cellar path (`cmd/gc/cmd_bd.go:235-237`), which is why the path is never cached.

**Version handshake.** `gc-osc7501 version` prints
`gc-osc7501 <version> codec=1 relay=1`. gc arms only if the codec matches.
The result is cached per process for the companion path it saw. A `go install
./cmd/gc` user without the companion degrades to today's behavior, and `gc
doctor` prints the exact install command.

## 5. Arming

### 5.1 Create

An unexported variant of `NewSessionWithCommandAndEnv`
(`internal/runtime/tmux/tmux.go:855`) takes the arm. The exported method keeps
its signature and delegates with no arm. When armed, `runNewSession`
(`internal/runtime/tmux/session_env_file.go:52`) runs one command list:

```
new-session -d -P -F '#{pane_id}' -s NAME [-c DIR] [-e K=V …] CMD ;
set-option -p -F -t =NAME: @gc_ps_tap 'RAND.#{pane_id}' ;
pipe-pane -O -t =NAME: 'exec BIN tap -x TMUXBIN -S #{socket_path} -t #{pane_id} -n RAND [-a APP]'
```

**Two execution paths:**
- **Command file (the primary path).** Every managed session carries values that are not argv-safe, so it takes the command-file path (`runtime.EnvHasArgvSecrets`). A new `tmuxCommandLines([][]string)` quotes each argument with `tmuxQuote` and joins commands with a bare ` ; `.
- **Argv path.** The separator is a bare `";"` element.

With one command, both renderings are byte-identical to today's
`tmuxCommandLine` (`session_env_file.go:157`). An unarmed create, which is
every create when the switch is `off`, therefore changes nothing.

**Partial-list failure.**
- tmux exits 1 when a later command in the list fails, but the session still exists (verified), and the executor drops stdout on error.
- For a list, `runNewSession` returns `*tmuxCommandListError{Stdout string; Err error}`.
- If the stdout carries a well-formed `%N`, the session was created. The code then runs `markAgentPane`, `publishInputFence` and `markSessionEnvRemoved` as today (`tmux.go:897-904`), logs the arm failure once, and returns nil. Heal re-arms.
- Any other failure is returned unchanged. `ErrSessionExists` classification is unaffected, because tmux stops at the failing `new-session`.

### 5.2 Respawn

`respawnAgent` (`internal/runtime/tmux/adapter.go:1547`) calls
`RespawnPaneWithWorkDir` (`tmux.go:6005`). When armed, it runs one list, which
is the list verified 10/10:

```
pipe-pane -t P ;
respawn-pane -k -t P [-c DIR] CMD ;
display-message -p -t P '#{pane_id}' ;
set-option -p -F -t P @gc_ps_tap 'RAND2.#{pane_id}' ;
set-option -pu -t P @gc_ps ; set-option -pu -t P @gc_ps_text ;
pipe-pane -O -t P 'exec BIN tap … -n RAND2 …'
```

- The old tap gets EOF. Its flush write fails the guard, because the nonce has already changed.
- The new process's first report is caught: tmux reads pane output only after the list completes.
- The `display-message` line is the success marker for partial-list handling. Only `respawn-pane` printing nothing would be ambiguous.
- Never use `pipe-pane -o`: it toggles the pipe (the k8s bug, `ga-f4ovjm.1`).
- Build the respawn-list helper once. An identity-changing relaunch needs the same helper to republish the input fence in the same list (`internal/runtime/tmux/fence.go:212-217`).

### 5.3 Relay wrap

Applied in `tmuxStartOps.createSession` and `respawnAgent`
(`adapter.go:1526,1547`) when `cfg.ProgramStatus.Relay` is set and the
capability gate passes. The resulting pane command, from outside in:

```
systemd-run … -- sh -c '                       ← GC_AGENT_SLICE (wrapPaneCommand), outermost
  env -u K1 -u K2 …                             ← withheld env (withEnvUnsetPrefix, tmux.go:887)
    /path/gc-osc7501 relay -- /bin/sh -c '<cmd>'   ← relay wrap
'
```

**Why this order:**
- **`env -u` sits outside the relay.** The relay and the agent it spawns both start without withheld credentials. If the relay were outside `env -u`, it would hold the controller-only values in its environment, and the agent, its child, could read them (`/proc/<ppid>/environ`, same uid).
- **The slice stays outermost**, as `GC_AGENT_SLICE`'s contract requires: it wraps the pane's whole initial command (`internal/runtime/tmux/agent_slice.go:17-29`). The relay then sits in the agent's slice. That is correct: it is part of the agent's tree and its CPU cost scales with the agent's output.
- **The relay runs `/bin/sh -c <cmd>`**, exactly as tmux would have run the pane command.

**Other rules:**
- `wrapperCommands` (`agent_slice.go:40`) gains `"gc-osc7501"`. Without the slice, `pane_current_command` reports the relay for the pane's whole life, and `WaitForCommand` (`tmux.go:5088`) must keep waiting for the agent descendant. `IsRuntimeRunning` (`tmux.go:5013`) and the state cache's `processAlive` already walk descendants.
- **Fingerprint.** The wrap is applied after the fingerprint is computed, to the leaf's copy of the command. `cfg.Command`, which is hashed (`internal/runtime/fingerprint.go:362`), never changes. That is the `wrapPaneCommand` precedent (`tmux.go:715,892,6014`).
- **The kill switch cannot unwrap a live seat.** Turning the switch to `off` leaves wrapped seats wrapped until their next start. That is harmless: the relay keeps answering probes, and tmux drops the reports.
- **Accepted differences under a relay.**
  - `#{pane_current_path}` is the relay's working directory, which equals the start directory.
  - A probe that arrives after user input could, in principle, place its 12-byte reply between two input reads. Probes happen at program start, before gc types anything.

### 5.4 Heal and disarm

`ProgramCaptureHealer.HealProgramCapture(name, want)` runs on the tmux leaf.
Only the controller calls it, on the runtime inventory lane's cadence, never in
CLI or API processes. It acts only on the agent-pane row
(`runtime.PickTmuxAgentPaneRow`) and skips dead panes, because `pipe-pane`
fails on them.

| Situation | Action |
|---|---|
| `want` declared, mode not `off`; own nonce missing, or `reader_lost` with `pane_pipe=0` | Re-arm with `if-shell -F -t P '#{pane_pipe}' '' '<nonce ; unset @gc_ps* ; pipe-pane -O …>'`. An existing pipe is never taken over. |
| `reader_lost` with `pane_pipe=1` (tap died; tmux has not noticed yet) | Defer. The next pane output closes the pipe, and a later pass re-arms. Heal cannot tell a dead tap's pipe from a foreign one, and does not guess. |
| Not declared (mode not `off`), and own nonce present | If the `ps` snapshot shows our tap alive: `pipe-pane -t P` (close), then unset the three options. Otherwise unset the options only, never closing a pipe that may be foreign. |
| Mode `off` | The steady state adds no reads. The controller runs one disarm sweep per config load: one `list-panes -a` of `pane_id`, `pane_pipe` and `@gc_ps_tap`, then the disarm rule above for each pane bearing our nonce. |
| Our tap is alive (the `ps` snapshot shows our nonce) but writes a codec version this gc does not know (an upgrade with no restart) | Replace it: one list setting a new nonce, unsetting `@gc_ps*` and running `pipe-pane -O` with the current companion. The pipe is provably ours. |
| Re-arm keeps failing | Per-pane backoff in controller memory, from 30 s up to 10 min. This is soft state: losing it costs one extra attempt. |

Heal returns a `ProgramCaptureAction`, one of `unchanged`, `armed`,
`disarmed` or `deferred`. The lane records it in its existing trace.

### 5.5 Capability gate

Evaluated once per `Tmux` instance and cached for the process. If any check
fails, nothing is armed or wrapped, every reading is `Unread`, one line is
logged, and `gc doctor` reports the cause.

1. **The companion resolves** ([§4.5](#45-packaging-and-lookup)) and its `version` reports a matching codec.
2. **`tmux display-message -p '#{version}|#{socket_path}'`** succeeds against the city server.
   - The version must be 3.2 or later. gc documents but does not enforce that floor (`tmux.go:841`); pane options (`-p`), `#{s/…}` and `set-option -F` all predate it.
   - The socket path must match `[A-Za-z0-9._/-]+`, so the `pipe-pane` command needs no shell quoting of the expanded path.

## 6. Runtime contract

### 6.1 Go signatures

New file `internal/runtime/program_status.go`. It is stdlib-only, as
`TestRuntimeContractPackageStaysStdlibOnly`
(`internal/runtime/import_boundary_test.go:25-50`) requires.

```go
// ProgramState is the root word of an OSC 7501 report (spec rev 0.4).
type ProgramState string

const (
	ProgramIdle    ProgramState = "idle"
	ProgramWorking ProgramState = "working"
	ProgramDone    ProgramState = "done"
	ProgramBlocked ProgramState = "blocked"
	ProgramError   ProgramState = "error"
)

// ProgramBlockKind qualifies ProgramBlocked. Unknown kinds read as unspecified.
type ProgramBlockKind string

const (
	ProgramBlockUnspecified ProgramBlockKind = ""
	ProgramBlockPermission  ProgramBlockKind = "permission"
	ProgramBlockQuestion    ProgramBlockKind = "question"
	ProgramBlockAuth        ProgramBlockKind = "auth"
)

// ProgramStatusMode is the city switch a reading was taken under.
type ProgramStatusMode string

const (
	ProgramStatusOff    ProgramStatusMode = "off"
	ProgramStatusShadow ProgramStatusMode = "shadow" // advisory: observe and compare, never decide
	ProgramStatusOn     ProgramStatusMode = "on"     // a Valid reading may change a decision
)

// ProgramStatusValidity says whether a reading may be used and, if not, why.
// The zero value is Unread, so a zero ProgramStatus carries no state.
type ProgramStatusValidity string

const (
	ProgramStatusUnread     ProgramStatusValidity = ""            // switch off, or capability gate failed
	ProgramStatusValid      ProgramStatusValidity = "valid"
	ProgramStatusNotArmed   ProgramStatusValidity = "not_armed"   // no nonce of ours bound to this pane (incl. inherited values)
	ProgramStatusSilent     ProgramStatusValidity = "silent"      // armed; nothing reported this incarnation
	ProgramStatusCleared    ProgramStatusValidity = "cleared"     // root cleared (state=clear or OSC 133;A)
	ProgramStatusReaderLost ProgramStatusValidity = "reader_lost" // pipe closed, or no live tap with our nonce
	ProgramStatusExited     ProgramStatusValidity = "exited"      // pane dead, or the caller's liveness says the agent is gone
)

// ProgramStatus is the agent program's own OSC 7501 root record, judged
// against the live pane in the same read. It is the program's claim:
// positive-only evidence with no heartbeat. Counters compare only within
// one Nonce (one capture incarnation). Text (title, msg) is never carried here.
type ProgramStatus struct {
	Mode            ProgramStatusMode // stamped on every reading; "" reads as off
	Validity        ProgramStatusValidity
	State           ProgramState     // "" unless Validity == Valid
	Kind            ProgramBlockKind // only with ProgramBlocked
	App             string
	Progress        int       // -1 when absent
	Since           time.Time // root entered State (capture host clock)
	QuietSince      time.Time // root last left working/blocked for a boundary; zero if never
	Nonce           string    // "<rand>.<pane_id>"
	Seq             uint64
	TurnStarts      uint32 // entries into working from a boundary or no root; blocked→working is not a start
	TurnEnds        uint32 // exits from working/blocked to a boundary
	Children        int
	WorkingChildren int
	Probed          bool // the program sent the support probe this incarnation (doctor)
	LastError       bool // an error report was seen; sticky across clear (annotation only)
	ReaderVerified  bool // the process table confirmed the tap; false when the snapshot was unavailable
}

func (s ProgramStatus) Valid() bool         // Validity == Valid && State != ""
func (s ProgramStatus) Decides() bool       // Valid() && Mode == ProgramStatusOn
func (s ProgramStatus) Running() bool       // Valid() && State == ProgramWorking
func (s ProgramStatus) Blocked() bool       // Valid() && State == ProgramBlocked
func (s ProgramStatus) InputBoundary() bool // Valid() && State in {idle, done, error}; root only
func (s ProgramStatus) Quiescent() bool     // InputBoundary() && WorkingChildren == 0

// ProgramStatusHoldLimit bounds every hold derived from a record
// (the idleClaimNudgeBusyHoldLimit value).
const ProgramStatusHoldLimit = 10 * time.Minute

// HoldLive reports whether a Running/Blocked/WorkingChildren hold may still
// count: now - max(Since, lastActivity) < ProgramStatusHoldLimit. Past the
// bound a caller must act as if no record existed. It never reads idle.
func (s ProgramStatus) HoldLive(lastActivity, now time.Time) bool

// ProgramBaseline is taken before gc types; AdvancedPast is positive delivery evidence.
type ProgramBaseline struct {
	Nonce      string
	Seq        uint64
	TurnStarts uint32
	State      ProgramState
}

func (s ProgramStatus) Baseline() ProgramBaseline

// AdvancedPast: same Nonce, b.State is a boundary (idle/done/error), and
// (TurnStarts > b.TurnStarts || (Seq > b.Seq && State in {working, blocked})).
// A busy, silent or other-nonce baseline credits nothing.
func (s ProgramStatus) AdvancedPast(b ProgramBaseline) bool

// ProgramStatusProvider is an optional extension: the leaf's judged reading of
// the session's agent pane. Composites do not implement it; it is read on the
// routed leaf (ProgramStatusFor), as TurnRunningProvider is (runtime.go:741-752).
// With the switch off it returns an Unread reading and nil error; a pane it has
// not armed reads not_armed; a missing session returns an error wrapping
// ErrSessionNotFound; an unobservable runtime returns one wrapping
// ErrRuntimeUnavailable.
type ProgramStatusProvider interface {
	ProgramStatus(name string) (ProgramStatus, error)
}

func ProgramStatusFor(p Provider, name string) (ProgramStatusProvider, bool) // ResolveBackend + assert

// ProgramStatusArm is a session's capture declaration, resolved from provider
// data, the city switch and the transport. The zero value declares nothing.
type ProgramStatusArm struct {
	Capture bool   // arm the pane-output reader
	App     string // reader drops reports naming another program
	Relay   bool   // wrap the agent in the probe-answering relay (only with Capture)
}

// ProgramCaptureAction is what one heal call did.
type ProgramCaptureAction string

const (
	ProgramCaptureUnchanged ProgramCaptureAction = "unchanged"
	ProgramCaptureArmed     ProgramCaptureAction = "armed"
	ProgramCaptureDisarmed  ProgramCaptureAction = "disarmed"
	ProgramCaptureDeferred  ProgramCaptureAction = "deferred" // pipe busy, pane dead, or backoff
)

// ProgramCaptureHealer is an optional extension the controller calls on the
// runtime inventory lane to converge a session's capture to want. Composites
// do not implement it (ProgramCaptureHealerFor resolves the leaf).
type ProgramCaptureHealer interface {
	HealProgramCapture(name string, want ProgramStatusArm) (ProgramCaptureAction, error)
}

func ProgramCaptureHealerFor(p Provider, name string) (ProgramCaptureHealer, bool)

// ProgramStatusComparison is one shadow-mode decision: today's reading beside
// what the record would decide. Closed tokens only.
type ProgramStatusComparison struct {
	Session   string
	Family    string // provider family (resolved built-in ancestor); config data, not a role
	Point     string // closed token, validated by the event payload enum (§9.2)
	Scrape    string // closed token
	Program   string // closed token
	Status    ProgramStatus
	LatencyMs int64 // program signal minus scrape signal; meaningful only when Timed
	Timed     bool
}

// ProgramStatusComparisonSink receives shadow comparisons. Implementations
// must not block the caller.
type ProgramStatusComparisonSink func(ProgramStatusComparison)

// InventoryEntry (runtime.go:828-857) gains:
//	Program ProgramStatus // Unread unless the runtime reads program status for this session
```

**Why a new interface rather than overloading `IdleSnapshotProvider`.**
- The API must read through `worker.Handle` from the 2 s state cache, not with a `list-panes` per request (the ga-jnavd lesson, `internal/runtime/tmux/state_cache.go:832`).
- `internal/worker` cannot import `tmux`.
- The shape follows `TurnRunningProvider` and `TurnRunningFor` exactly, which have one leaf implementation plus `FullFake`.

**Bookkeeping the new interfaces force:**
- `composite_forwarding_test.go:17-35` lists both interfaces as `deliberate` for `auto` and `hybrid`.
- `fake_profile_test.go:31-77` lists them as optional interfaces.
- `ProfileBackend`, `tmuxProfile` and `FullFake` (`internal/runtime/fake_profile.go:296-300`) implement them.

### 6.2 Record semantics and codec

These are owned by `programstatus` (`ga-f4ovjm.9`).

**`ParseBody` follows rev 0.4 strictly:**
- **Pairs.** Pairs are `key=value`, joined by `:`. Values use the charset `[A-Za-z0-9_.,+/=-]`. Whitespace around keys and values is stripped. A malformed pair is skipped. Unknown keys are ignored. A repeated key keeps its last value.
- **Required and validated fields:**
  - `state` is required and must be known; otherwise the whole report is dropped, never guessed.
  - `id` must match the path grammar: at most 128 bytes total, 32 per segment, and 8 levels. Otherwise the report is dropped.
  - An unknown `kind` reads as absent.
  - `progress` counts only as 0-100, with `working` or `blocked`.
  - `app` must match `[A-Za-z0-9_.+-]{1,32}`; otherwise it reads as absent.
- **Text fields.**
  - `msg` and `title` are std base64, with padding optional.
  - Encoded limits (2732 and 256 bytes) are checked before decoding, and decoded limits are 2048 and 192 bytes.
  - Invalid base64 drops the whole report. So does decoded text containing C0, DEL or C1 characters. The spec says discard, not strip.
  - Bidi and invisible formatting characters are stripped for display (U+061C, U+200B-200F, U+202A-202E, U+2060-2069, U+FEFF).
- **Size.** A whole sequence over 4096 bytes is dropped.
- **Probe.** A body of `?` is `ErrProbe`.

**`Store`:**
- **Replace and clear.** A report replaces its record. `clear` with an id drops that id and its descendants; `clear` with no id drops everything except the sticky `LastError`.
- **OSC 133;A** drops `working` and `blocked` records without counting a turn end.
- **Turns.** `blocked` pauses a turn and does not end it. Children never start or end the root's turn.
- **Turn counters.** `TurnStarts` increments when the root enters `working` from a boundary state or from having no root. `TurnEnds` increments when the root goes from `working` or `blocked` to a boundary state.
- **Timestamps.** `Since` changes only when the root state changes. `QuietSince` is stamped at each turn end.
- **Seq.** Increments with every change to the encoded `@gc_ps` value. Coalescing may skip intermediate states, but the counters still record that a turn happened, which is why coalescing is safe for `AdvancedPast`.

**Codec.** Values must survive the read sanitizer
`#{s/[^-+A-Za-z0-9=_.|%]//:@opt}` unchanged. `+` was added for `app`, and
`TestEncodeSurvivesServerSanitizerUnchanged` pins this.

```
@gc_ps      v1|<rand>.%N|seq|state|kind|since_ms|quiet_ms|starts|ends|progress|wchildren|children|flags|app
@gc_ps_text v1|<rand>.%N|b64url(title)|b64url(msg)|b64url(last_error_msg)
```

- `flags` is a subset of `P` (probed) and `E` (last error).
- An absent `progress` is the empty string.
- Text is truncated to 512 bytes at a rune boundary before encoding.
- `Decode` rejects wrong field counts and anything outside the alphabet.

### 6.3 RUNTIME-CONTRACT-017

This PR reserves the id as a Planned row, RUNTIME-PLAN-006, in
`internal/runtime/REQUIREMENTS.md`. The file's reconcile rule 4 says a row
moves into the ledger only together with the tests that prove it. So
`ga-f4ovjm.11` moves it into the Provider Contract table (after 016 at L126)
with `RunProgramStatusContract` and `TestFakeProgramStatusContract`, and
`ga-f4ovjm.14` adds the tmux evidence. 012 is reserved for the t3bridge
turn-observation row and must not be reused. The row:

> | RUNTIME-CONTRACT-017 | A program-status reading is the pane program's own bounded claim | `ProgramStatusProvider.ProgramStatus` returns the session's agent-pane OSC 7501 root record judged at read time. `Validity` is `valid` only when (a) the reading's mode is not off and the capture gate passed; (b) the record's nonce equals the nonce this runtime armed for that exact pane, bound to its pane id, so a window-, session- or global-scope value never qualifies; (c) the pane is alive and, when the caller holds a liveness fact, the agent is; (d) the capture reader is attached: the pipe is open, and the reader process is alive whenever the process table is observable; and (e) a root record exists. A respawn arms a new nonce, after which nothing published under an earlier nonce is ever valid. A switch-off reading, or one taken when the capture gate failed, is `Unread` whatever pane options exist, and `Unread` carries no state. A pane the runtime has not armed reads `not_armed`. The reading has no heartbeat: every hold a caller derives from `working`, `blocked` or working children is bounded by `HoldLive` (`ProgramStatusHoldLimit`, 10 minutes past the later of the record's `Since` and the pane's last output). Past it the caller must act as if no record existed, never as if the session were idle. A reading is positive-only: it never clears a dialog check, changes `GetLastActivity`, ends a transcript turn, enrolls a family in verified submit, exempts an exit from crash accounting, or extends `max_session_age` or drain-ack escalation. Only a `Decides()` reading (mode `on`) may change a decision; a `shadow` reading is advisory. Composites do not implement it; `ProgramStatusFor` resolves the routed leaf. A missing session wraps `ErrSessionNotFound`; an unobservable runtime wraps `ErrRuntimeUnavailable`. | `internal/runtime/program_status.go`; `internal/runtime/runtimetest/program_status.go` (`RunProgramStatusContract`, PS-001…PS-014); `internal/runtime/fake_program_status_test.go` (`TestFakeProgramStatusContract`); `internal/runtime/tmux/program_status_integration_test.go` (`TestTmuxProgramStatusContract`); `internal/runtime/programstatus` (`TestEncodeSurvivesServerSanitizerUnchanged`, `TestDecodeRejectsForgedFields`); `engdocs/design/osc7501-program-status.md` |

### 6.4 Amendments to existing contracts

These ship with the cutover slices that make them true, not before:

- **`IdleSnapshotProvider`** (`runtime.go:682-709`; `ga-f4ovjm.22`): "an error wrapping both `ErrIdleIndeterminate` and `ErrPendingInteraction` means a program-reported dialog owns input." Callers that test only `ErrIdleIndeterminate` read it exactly as they read a parser-detected dialog today, as indeterminate.
- **`IdleWaitProvider` / RUNTIME-CONTRACT-016** (`ga-f4ovjm.22`): "a timeout whose last reading was a program-reported `blocked` also wraps `ErrPendingInteraction`."
- **`TurnRunningProvider`** (`ga-f4ovjm.8`): "an error wrapping `ErrIdleIndeterminate` means the runtime cannot read this family's turn."
- **`PendingInteraction.Kind`** gains `auth` and `dialog`. `Respond` refuses `osc7501-*` request ids with `ErrInteractionActionUnavailable` (`ga-f4ovjm.22`).

## 7. Validity, staleness and precedence

### 7.1 Validity derivation

The input is one agent-pane row: `pane_id`, `pane_dead`, `pane_pipe`, the
sanitized `@gc_ps_tap`, and the decoded `@gc_ps`. Add the tap-liveness
tristate from the `ps` snapshot (`yes`, `no`, `unknown`), the mode, and an
optional caller liveness fact. The first matching rule wins:

1. Mode `off`, or the capability gate failed → `Unread`.
2. `@gc_ps_tap` is empty, or does not end in `.<pane_id>` of this row → `not_armed`. A reader cannot see a session's declaration, so this covers undeclared sessions too. Heal and doctor know the declaration from config and tell the two apart. Undeclaring a family reaches live panes at the next heal pass; the city switch is the immediate kill switch.
3. `pane_dead=1`, or the caller says the agent is gone → `exited`. `LastError` is kept when the nonce matches.
4. `pane_pipe=0`, or the tap is `no` → `reader_lost`.
5. `@gc_ps` carries a codec version this gc does not know, while our tap is alive → `reader_lost`, which heal replaces. Otherwise, `@gc_ps` is undecodable or its nonce does not equal `@gc_ps_tap` exactly (compared in Go) → `silent`. A mismatched record is treated as absent.
6. No root: `silent` if `Seq` is 0, otherwise `cleared`.
7. Otherwise `valid`, with `ReaderVerified = (tap == yes)`.

**Where it lives.** The derivation and the read-format constant are pure
stdlib code in `internal/runtime`, beside `runtime.PickTmuxAgentPaneRow`.
Box carriers (`.28`) reuse them, the way the input fence is shared. That is
why `TestNonceMustNameReadPane` belongs to `.10`.

**Tap liveness.** A process in the cache's `ps` snapshot
(`state_cache.go:1051-1056`, pid/ppid/comm/args) whose args carry
`-n <rand>` and `-t <pane_id>`.

**Folds.** Rows are taken only from the agent pane: the marked pane, or else
the only pane (`runtime.PickTmuxAgentPaneRow`). Never the lowest index.

**When fields are read.** Both `FetchState`
(`internal/runtime/tmux/state_cache.go:819`) and `runtimeInventoryFormat`
(`tmux.go:1588`) append the fields only when the mode is not `off`.

### 7.2 Staleness

| Situation | Observable | Reading |
|---|---|---|
| Login screen, pre-REPL dialog, legacy build | Own nonce, no `@gc_ps` | `silent`, so callers fall back. `Probed` with no reports means the program supports 7501 but the probe went unanswered (doctor). |
| Program stuck at `working` (no heartbeat) | Record unchanged | Holds count only while `HoldLive`. After that, act as if there were no record. Never idle. |
| Tap died, pane silent | `pane_pipe` stays 1 (verified) | `ps` shows no tap, so `reader_lost`; heal re-arms after the pipe closes. If the snapshot is unavailable, `ReaderVerified=false`, and the hold bound limits the damage. |
| Tap cannot publish | Record stale | Retries; after 5 failures it exits, giving `reader_lost`. |
| Process exited (remain-on-exit) | `pane_dead=1`; the record may still say `working` (verified) | `exited` |
| Agent died under a wrapper that keeps the pane alive | `pane_dead=0` | Callers holding liveness mark it `exited`. Leaf gates rely on the hold bound. Under the relay this does not occur: the relay exits with its child. |
| Respawn | New nonce set in the same list | A late write from the old tap is refused by tmux. A leftover record reads `silent`. |
| Inherited window/session/global `@gc_ps_tap` or `@gc_ps`, including after `-pu` | Inherited value | Matches at most the one pane it names, and also needs a live tap with that nonce. Otherwise `not_armed` or `silent`. |
| User or k8s `pipe-pane` on the pane | The tap gets EOF and exits | `reader_lost`; heal never takes over the foreign pipe. |
| Controller restart | — | Nothing to rebuild; state lives in tmux. |
| Switch flipped to `off` | — | Every reading is `Unread` at once; heal disarms. |
| Codex startup flicker (working→idle about 20 ms apart) | Starts=1, Ends=1 | Baselines are taken only at a boundary, so the flicker credits nothing. |
| `done`, `idle` or `error` | — | All three are input boundaries. `error` is a failed turn with the process alive, never `PhaseFailed`. |
| `blocked` | — | Neither a boundary nor running. Pending-only, positive-only, bounded. |
| Child records | `Children`, `WorkingChildren` | Typing and idle gates read the root only. The sleep proof requires `Quiescent()`. |

### 7.3 Precedence

1. **Protocol runtimes are untouched.** ACP, t3bridge and herdr do not implement the interface.
2. **Observation under `on`.** API activity, agent state, worker phase, SSE and CLI take a valid record and label it `activity_source: program`. Otherwise they keep today's source, and an unobserved session reads `unknown`, never idle.
3. **Gates under `on`** (`Decides()`):
   - **Vetoes:** a live `working` record means not idle and a turn running. A live `blocked` record means pending, which refuses typing and queues the nudge.
   - **Positive evidence:** `AdvancedPast(baseline)` confirms delivery and stops an Escape re-send. A boundary report past the pre-interrupt baseline settles an interrupt.
   - **Boundaries need agreement where a trusted scan exists.** For claude and codex (`submitVerifyEligible`, `tmux.go:3108`), the scan must also read idle. Families with no trusted scan need two boundary reads with the same nonce and `Seq`.
   - No valid record, or an expired hold: today's path.
4. **Under `shadow`** every gate computes both readings, decides with today's, and emits the comparison ([§9](#9-shadow-mode)).
5. **Transcript conflicts** (Claude, `ga-f4ovjm.24`):
   - A `working` record with no open transcript turn means the turn is starting.
   - Only an edge newer than the open turn's start may end that turn.
   - Transcript growth reopens a turn.

**Threat model.** The tmux socket is already a full-trust boundary: any pane
process inherits `TMUX` and can set options on any pane. So the nonce guards
only against stale taps, and sanitization guards only the wire. The defence
against spoofing is the policy itself:
- capture is opt-in by provider data
- an app filter drops reports from other programs
- use is positive-only
- holds are bounded
- no destructive consumer ([§1](#1-scope))

## 8. Configuration

### 8.1 Provider data

```toml
[providers.codex.program_status]
protocol = "osc7501"   # "" inherit; "osc7501" declare; "none" explicitly undeclare
app = "codex"          # optional: the tap drops reports naming another program
relay = false          # true: wrap the agent in `gc-osc7501 relay` (probe-gated families)
env = { }              # forced env, e.g. { PI_PROGRAM_STATUS = "1" }; injected outside the fingerprint
```

```go
// internal/config
type ProgramStatusBinding struct {
	Protocol string            `toml:"protocol,omitempty" jsonschema:"enum=osc7501,enum=none"`
	App      string            `toml:"app,omitempty"`
	Relay    *bool             `toml:"relay,omitempty"`
	Env      map[string]string `toml:"env,omitempty"`
}
// ProviderSpec.ProgramStatus  ProgramStatusBinding `toml:"program_status,omitempty"`
// ResolvedProvider.ProgramStatus ProgramStatusBinding
```

**Wiring, following the `UpstreamEnv` footprint:**
- `internal/config/provider.go:175,259,526`
- `resolve.go:319-326,649`
- `compose.go:1504-1515`
- `undecoded.go:269`

**Merge rules:**
- A non-empty `protocol` or `app` overrides.
- A non-nil `relay` overrides.
- `env` merges per key, with the later layer winning. An empty value is copied verbatim, which withholds the variable: the repo-wide meaning of an empty env value (`tmux.go:727-735`).

**Exclusions and generated files:**
- `program_status` is excluded from `ProviderPatch`, with its reason recorded in `TestProviderFieldSync` (`internal/config/field_sync_test.go:496`, the `UpstreamEnv` precedent).
- `internal/worker/builtin/profiles.go:40-80` gains flat fields `ProgramStatusProtocol`, `ProgramStatusApp`, `ProgramStatusRelay` and `ProgramStatusEnv`, because that package cannot import `config`.
- Regenerate `docs/reference/schema/{city,pack}-schema.*` and `config.md` (`TestSchemaFreshness`).

### 8.2 Built-in declarations

Each family is declared only after it is verified under gc's tmux against
local mocks. Every declaration adds `WC-PSTAT` rows ([§12.3](#123-workertest-rows)).

| Profile | Declaration | Slice |
|---|---|---|
| codex | `protocol="osc7501", app="codex"` | `ga-f4ovjm.18` |
| claude | `protocol="osc7501", app="claude-code", relay=true` | `ga-f4ovjm.18` |
| pi | `protocol="osc7501", app="pi", env={PI_PROGRAM_STATUS="1"}` (the env also skips the probe) | `ga-f4ovjm.38` |
| antigravity | `protocol="osc7501", env={AGY_CLI_OSC7501="on"}` | `.38` |
| opencode, cerebras, groq | `protocol="osc7501"` once the installed major (2.x) is confirmed | `.38` |
| kimi (≥ 2.4), grok, omp, amp, droid | After verification. amp and droid need `relay=true`. | `.38` |

**Overrides in a city.** A city overrides with plain data:
- `[providers.claude.program_status] protocol = "none"` undeclares a family, for example while a Claude pin predates 7501.
- A custom provider with `base = "builtin:claude"` and its own binding opts a cohort in.

### 8.3 Env injection and the fingerprint

Profile `Env` would relaunch the fleet, because it flows into `OperatorEnv`,
which is hashed (`cmd/gc/template_resolve.go:539`;
`internal/runtime/fingerprint.go:438-442`). The program-status env is instead
merged into the session `env` only.

**Where:**
- In `cmd/gc/template_resolve.go`, after the Step 10b upstream block (`:542`) and before the controller-only re-pin (`:609`). The re-pin stays the last writer.
- Gated: mode not `off`, transport not ACP, and the binding declared.

**Why that is safe:** `Config.Env` is hashed only through the allow-list
(`fingerprint.go:313-351`), and these keys are not on it.

**Tests (`ga-f4ovjm.13`):**
- `TestProgramStatusEnvDoesNotMoveLaunchFingerprint`
- `TestProgramStatusEnvSkippedOnACPTransport`
- `TestProgramStatusOffInjectsNothing`

### 8.4 `runtime.Config` plumbing

`runtime.Config` (`internal/runtime/runtime.go:1266`) gains
`ProgramStatus ProgramStatusArm`:
- It is a struct, not a map. `configWithOperatorAuthoredEnv` requires exactly one extra `map[string]string` field (`internal/runtime/fingerprint_config_env_test.go:13-33`).
- It is listed in `excludedFromCore` (`fingerprint_partition_test.go:116`) with the reason "leaf-applied capture declaration; arming and heal never restart the agent". `TestFingerprintPartitionAccountsForEveryConfigField` (`:139`) forces that classification.

**One derivation.** It is a method on the config binding:

```go
func (b ProgramStatusBinding) Arm(mode string, acp bool) (runtime.ProgramStatusArm, map[string]string)
```

It returns the arm and the env to merge, both empty when the mode is `off`,
the transport is ACP, or the binding does not declare. It cannot live beside
`agent.StartupHints`, because `internal/config` already imports
`internal/agent` (`internal/config/named_sessions.go`), so that would be a
cycle. `internal/config` reaches `internal/runtime` through `internal/agent`
already, so a direct import adds no new edge.

`agent.StartupHints` (`internal/agent/hints.go:9`) gains a
`ProgramStatus runtime.ProgramStatusArm` field, so `ToRuntimeConfig` carries
it. Every `runtime.Config` construction site fills it from
`resolved.ProgramStatus.Arm(…)`:
- `cmd/gc/template_resolve.go:992`, the reconciler and CLI create path
- `cmd/gc/worker_handle.go:150-169`, the unmanaged `gc session new`
- `cmd/gc/worker_handle.go:766-774`, resume
- `internal/api/session_runtime.go:123`, `sessionCreateHints`

`TestEveryRuntimeConfigSiteAppliesProgramStatusArm` pins this.

**The switch reaches the leaf** through `tmuxConfigFromSession`
(`cmd/gc/providers.go:86`), as `tmux.Config.ProgramStatus` (the mode),
`ProgramStatusBin`, and `ComparisonSink`. Every process that builds a tmux
provider, whether controller, CLI or API, therefore judges readings under the
same switch.

### 8.5 City switch

`[session] program_status` on `SessionConfig` (`internal/config/config.go:1697`),
with `jsonschema:"enum=off,enum=shadow,enum=on,default=off"`.

| | `off` (default) | `shadow` | `on` |
|---|---|---|---|
| Env injection, relay wrap and tap arm on create/respawn | no | yes | yes |
| Heal | one disarm sweep per config load (closes our pipe only if our tap is alive) | arms or re-arms | arms or re-arms |
| `ProgramStatus()` | `Unread` | judged, `Mode=shadow` | judged, `Mode=on` |
| New observation fields (`program_status`, `activity_source`, SSE frame, CLI STATUS column) | absent | present | present |
| Existing observation fields (activity, agent state, worker phase) | today | today | record-first |
| Decisions | today | today, plus comparison events | record-first per §7.3, in the slices that have landed |
| tmux command lines and read formats | byte-identical to today | extended | extended |

**When a changed switch takes effect.** At the next provider construction:
controller config reload, or the next CLI/API invocation. Taps are disarmed
within one inventory cadence. Relay-wrapped seats stay wrapped until their
next start.

**The default.** It flips to `on`, for declared providers, only in
`ga-f4ovjm.27`. That comes after the shadow analysis (`.20`), review R2
(`.25`), the canary (`.41`) and final review R3 (`.42`).

## 9. Shadow mode

### 9.1 Decision points

With the switch at `shadow`, each point computes today's reading and the
record's reading, decides with today's, and emits one comparison. The
record-side rules are pure functions in `internal/runtime`, written in
`ga-f4ovjm.17`. The cutover slices switch those same functions on, so the
shadow measures exactly the code that will decide.

| Point token | Where | Scrape vs program |
|---|---|---|
| `wait_for_idle` | tmux `WaitForIdle` (`tmux.go:5458`) | Return or timeout at T1, versus the record rule's first boundary at T2 (timed) |
| `typing_gate` | pre-type checks (`checkNoApprovalPromptIn`, `pendingPromptRecheck` `tmux.go:3254`) | Parser pending versus live `blocked` |
| `submit_confirm` | `submitAndConfirm` / best-effort branch (`tmux.go:3610-3640`) | Busy transition or none, versus `AdvancedPast` (timed) |
| `escape_resend` | Codex `mayResend` (`tmux.go:3529-3537`) | `submitStillWaiting`, versus "advanced past baseline" |
| `readiness` | `WaitForRuntimeReady` (`tmux.go:5216`) | Prefix or delay time, versus first valid report plus 800 ms settle (timed) |
| `startup_ladder` | `paneBusy` (`adapter.go:1766`) | Scan busy versus `AdvancedPast` |
| `interrupt_settle` | interrupt boundary wait | Today's settle versus a boundary past the pre-interrupt baseline (timed) |
| `pending` | `Pending` (`interaction.go:695`) | Parser kind versus record kind |
| `wait_idle_preread` | worker/session wait-idle pre-read (`ga-f4ovjm.8`) | Snapshot versus record |
| `execution_backstop` | `cmd/gc/execution_backstop.go:444-503` | Scan veto versus live `working` |
| `claim_nudge_hold` | `turnHoldFor` (`cmd/gc/idle_nudge.go:651-671`) | Hold versus record hold |
| `work_ladder` | `observeRuntime` (`cmd/gc/work_ladder_lane.go:451-486`) | Same |
| `proved_idle` | `provedIdle` (`cmd/gc/reconcile_effect_fence.go:153-166`) | Proof versus `Quiescent()` plus two reads |
| `queued_delivery` | nudge dispatcher quiescence (`cmd/gc/cmd_nudge.go:2605`) | Deliver versus record veto |
| `drain_reminder` | `cmd/gc/drain_reminder.go:485-500` | Pending hold versus `blocked` |
| `observation` | API activity / worker phase | Transcript-derived versus record |

**Reads in shadow:**
- **Fresh at leaf points.** Where the point polls (`WaitForIdle`, the busy-transition loop, the readiness loop), the record read rides on the scan's own tmux invocation: `capture-pane … ; display-message -p -t P '<fields>'`, with the fields parsed from the last line. Elsewhere it is one `display-message` per decision, never one per poll.
- **cmd/gc points** use `InventoryEntry.Program` from the batch that already runs.

### 9.2 Event

There is one new typed event, `session.program_status_shadow`, with Worker
authorship, because CLI processes write it. It is registered beside the
precedent `session.demand_claim_divergence` (`internal/events/events.go:291`,
`payloads.go:243-267`, `authorship.go:144`).

```go
// SessionProgramStatusShadowPayload: closed tokens and numbers only; never msg/title.
type SessionProgramStatusShadowPayload struct {
	SessionID string `json:"session_id"`
	Family    string `json:"family"`                     // resolved built-in ancestor, e.g. "codex"
	Point     string `json:"point" enum:"wait_for_idle,typing_gate,submit_confirm,escape_resend,readiness,startup_ladder,interrupt_settle,pending,wait_idle_preread,execution_backstop,claim_nudge_hold,work_ladder,proved_idle,queued_delivery,drain_reminder,observation"`
	Scrape    string `json:"scrape" enum:"idle,busy,indeterminate,pending,confirmed,unconfirmed,ready,not_ready,settled,unsettled,hold,no_hold,resend,no_resend,none"`
	Program   string `json:"program" enum:"idle,busy,indeterminate,pending,confirmed,unconfirmed,ready,not_ready,settled,unsettled,hold,no_hold,resend,no_resend,none"`
	Agree     bool   `json:"agree"`
	Validity  string `json:"validity" enum:"valid,not_armed,silent,cleared,reader_lost,exited"`
	State     string `json:"state,omitempty" enum:"idle,working,done,blocked,error"`
	Kind      string `json:"kind,omitempty" enum:"permission,question,auth"`
	HoldLive  bool   `json:"hold_live"`
	RecordAgeMs   int64 `json:"record_age_ms"`
	LatencyMs     int64 `json:"latency_ms,omitempty"` // program minus scrape, timed points only
	ReaderVerified bool `json:"reader_verified"`
	Dropped   int    `json:"dropped,omitempty"` // comparisons this process dropped since its last emit
}
```

Cost of adding it:
- `KnownEventTypes`, `RegisterPayload` and `builtinAuthorship`
- `TestEveryKnownEventTypeHasRegisteredPayload`
- the OpenAPI `oneOf` variant, plus regenerated TS types and `make dashboard-check`

### 9.3 Emission rules and cost

**When to emit:**
- **Change points.** These are evaluated continuously (backstop, ladder, claim hold, observation, queued delivery). They emit when the (scrape, program) pair for (session, point) changes, using per-process memory that is soft state.
- **On-demand points.** Wait, gate, confirm, readiness and settle emit once per decision.

**How:**
- **The path.** The leaf calls `tmux.Config.ComparisonSink`, a `runtime.ProgramStatusComparisonSink`. cmd/gc points call the same adapter.
- **The adapter is asynchronous.** A queue of 256 entries per process is drained by one goroutine into the city recorder (`openCityRecorder`, `cmd/gc/main.go:1486-1503`). The recorder is opened lazily on the first comparison and kept for the process lifetime. The recorder's flock waits up to 250 ms (`internal/events/recorder.go:27-50`), and the shadow must not change the decision path's timing.
- **Overflow drops** the newest entry and counts it in the next payload's `Dropped`.
- **CLI processes** flush the queue at exit, bounded to 1 s.
- **Off and on** record nothing. The sink is nil unless the mode is `shadow`.

### 9.4 Volume, retention, privacy

- **Volume.** Each turn produces about 15 comparisons: four continuous points times about three changes, plus about three on-demand decisions. On maintainer-city that is roughly 30 seats × 50 turns × 15 ≈ 22,500 events, or about 9 MB a day, against the events log's 256 MiB rotation (`internal/config/config.go:2066`). Every comparison also costs one bounded queue push and one flock append, with no extra tmux fork per poll.
- **Retention.** Retention follows `[events]`. Archives are kept unless `archive_retain_age` is set (`internal/events/rotation.go:315-318`). The append-only log cannot delete individual records. That is acceptable only because the payload holds closed tokens and numbers. If the operator still wants the records gone after the analysis (`ga-f4ovjm.19` note), set `archive_retain_age` for the shadow window. The user must confirm this trade.
- **Privacy.** No `msg`, `title` or prompt text, and no `Message` envelope text. `app` is a 1-32 character program id.

### 9.5 Summarizer

`gc internal program-status-shadow-report [--since D] [--json]` reads the
events log through `internal/events` and prints a per-point by per-family
table, with these columns:
- the count
- the agreement percentage, both by count and time-weighted between consecutive change records
- the disagreement split by validity
- latency p50 and p95 at timed points
- the sum of `Dropped`

The export for `.19` is `gc events --type session.program_status_shadow
--since 24h --json`, already supported (`cmd/gc/cmd_events.go:228`).

### 9.6 No behavior change

**Shadow reads.** Every shadow computation is read-only:
- It runs beside today's decision, and today's decision is the one returned.
- Its tmux reads are extra format fields, or one `display-message` per decision.
- The sink never blocks.

**Tests (`ga-f4ovjm.17`):**
- **Per point:** a table-driven test asserts an identical outcome with the switch at `shadow` and at `off`.
- **Worker level:** `WC-PSTAT-007` asserts identical worker decisions.

**One known side effect.** Reports from a relay-enabled program are pane
output, so they advance `#{window_activity}` (verified, check 4 of the draft).
Codex already emits today, so for Codex nothing changes. For relay-wrapped
Claude seats the output clock now also sees report bytes. Those arrive at
state changes, which coincide with repaints. The shadow analysis (`.20`) must
still check idle-sleep and drain timing on those seats, against the `.34`
baseline.

## 10. Blocked refusal: queue, don't fail

### 10.1 Semantics

This is cutover behavior, under `on`, in `ga-f4ovjm.22`. A record that is
`Decides()`, `Blocked()` and `HoldLive` makes the tmux leaf refuse to type.

**The refusal error:**
- Typing paths (`Provider.nudge` `adapter.go:812`, submit, `SendKeys` with text) refuse before any capture, with an error wrapping `runtime.ErrPendingInteraction`. That is the sentinel the parser-detected pending path already uses.
- `SnapshotIdle` returns an error wrapping both `ErrIdleIndeterminate` and `ErrPendingInteraction`, so callers that test only indeterminate are unchanged.

**What each delivery mode does:**
- Wait-idle and queue delivery queue, through `refuseOrQueueNudgeBehindPendingInteraction` (`cmd/gc/cmd_nudge.go:1610`).
- Explicit `--delivery=immediate` is refused, as today.

**Attempts and backoff:**
- A path with a bounded attempt counter refunds the attempt for this refusal, as it already does for `ErrSessionBusy`.
- The queued dispatcher releases without spending an attempt (`queuedNudgeReleaseAfter`).

**Bounds:**
- A `blocked` record past `HoldLive` is ignored: the parsers decide, as today. A stale `blocked` therefore cannot hold delivery for ever.
- `Respond` refuses `osc7501-*` pending ids (kind-only pending has no option keys), so a human answers in the pane.

### 10.2 Typing-caller inventory

**One integration point.** A live `blocked` record joins the parser results
in `dialogInPane` (`internal/runtime/tmux/interaction.go:871`):
- `paneDialog` gains a third, record-backed member. Parsers win when they match, because only they carry option keys for `Respond`.
- The record is read in the same tmux invocation as the capture.
- `Pending`, `approvalInPane` and `checkNoApprovalPromptIn` all read `dialogInPane`, so every pre-check and runtime refusal sees the blocked state from that one place. The pre-checks include `cmd_nudge.go:1569`, `nudge_backstop.go:101`, `chat.go:1465`, `lanesubmit/pending.go:45`, `conversation_lane_exec.go:370`, `work_ladder_notice.go:401` and `pending_interaction_lane.go:203`.
- The refusals then reach callers wrapping `runtime.ErrPendingInteraction`, defined at `runtime.go:40`.

**What `dialogInPane` does not reach**, so each needs its own rule:
- the startup look (`tmux.go:3276`)
- raw keys (B3, B4)
- the dismissers (B6)
- `Interrupt`

Every row below gets one test under `on`, driven by the `Fake` (unit) or the
emitter (tmux). Tests are named `Test<Row>…BehindProgramBlocked`.

**Rule.** Each row's required outcome is what the approved rule demands for a
parser-detected dialog too: the same sentinel gives the same answer. Rows
marked **gap** violate that rule today, for parser dialogs as well.

| Row | Path | Today, with a parser-detected dialog | Required under a live `blocked` record | Bead |
|---|---|---|---|---|
| A1 | `gc session nudge` wait-idle (`cmd/gc/cmd_nudge.go:1476`) | Pre-check (`:1439-1445`) queues | Queued, **before** any 30 s wait | `.22` |
| A2 | `gc session nudge --delivery=immediate` | Refused, `pending_interaction` (`:1617-1624`) | Refused | `.22` |
| A3 | `--delivery=queue` (`:1419-1420`) | Queued without a look | Unchanged | — |
| A4 | Nudge under lane authority (`conversation_lane_cli.go:236-280`) | **Gap:** immediate also queues, exit 0 (`opjournal/lane_plan.go:773-774`) | Wait-idle and queue wait; immediate is **refused** at the front door, before Propose | new `.22b` |
| A5 | Queued dispatcher / `gc nudge poll` (`cmd_nudge.go:2548`) | Released for 15 s, no attempt (`queuedNudgeReleaseAfter` `:3458-3477`) | Unchanged; requires the `ErrPendingInteraction` wrap | `.22` |
| A6 | Lane `Deliver` (`conversation_lane_exec.go:529`) | Held by `PendingStatus` (`:370`); a refusal at delivery is transient (`:606-608`) | Held. The record needs a **stable RequestID** (`osc7501-` + hash(nonce, since_ms, kind, msg)), otherwise a respond waits on "unknown" (`opjournal/lane_respond.go:249-250`) | `.22` |
| A7 | Nudge backstops (`nudge_backstop.go:365`) | **Gap:** a refusal after the write-ahead reserve spends one of 3 attempts (`:378-379`); only `ErrSessionBusy` refunds | Pre-check holds; a late refusal **refunds** the attempt, as busy does | `.22b` |
| A8 | Drain reminder (`drain_reminder.go:599`) | **Gap:** a late refusal spends one of 3 attempts (`:577-586`) | Restore the marker and hold, as busy does (`:571-575`) | `.22b` |
| A9 | Work ladder (`work_ladder_lane.go:791,811`) | Refusal goes to Paused, attempt refunded (`workladder/decide.go:388-395`) | Unchanged. (Separate gap: `ErrSessionBusy` is counted; out of scope.) | — |
| A10 | Warm-bind claim nudge (`warm_bind_nudge.go:356-359`) | 30 s `WaitForIdle`, then logs; retries next tick | **Check pending before the wait** and skip the tick | `.22b` |
| A11–A13 | `gc sling --nudge` (`cmd_sling.go:1884`), mail notify (`cmd_nudge.go:2142`), GitHub PR repair (`cmd_github.go:453`, 15 s timeout) | Falls through to enqueue after a 30 s wait. A13 can be killed by its 15 s timeout first and is then neither delivered nor queued. | Pre-check first, then enqueue with no wait (fixes A13) | `.22b` |
| A14 | API extmsg notify (`internal/api/session_resolution.go:1041`) | **Gap:** logged and dropped (`handler_extmsg.go:179`) | **Queued.** `sendLocked` (`chat.go:1531-1555`) enqueues a deferred submit for non-immediate sends, as it does for busy | `.22b` |
| A15, A16, A18, A19 | API `POST /submit` and `/messages` (`huma_handlers_sessions_command.go:959`, `rejectPendingInteraction` `session_resolution.go:843-867`); lane submit (`session_lane_ops.go:1488-1506`, `conversation_lane_submit.go:284`); legacy `gc session submit` (`cmd_session.go:2974`) | 409 `pending_interaction` at the front door | Per user decision U5 (recorded on `.22`), unchanged: **409** for every intent, nothing written. A refusal racing after acceptance closes the op `refused:pending_interaction`, not `failed{other}` (`legacyop/classify.go:93`). | `.22` |
| A17 | API create with an initial message (`huma_handlers_sessions_command.go:405`) | **Gap:** the new session is **rolled back** (`:406-412`) | Never roll back. Queue the initial message (a fresh session showing an in-REPL notice reports `blocked`). | `.22b` |
| A20 | Startup nudge/prompt (`adapter.go:2336`, `nudgeStartupSession` `tmux.go:3266`) | **Gap:** no look at all; types into any dialog (`tmux.go:3276`) | A live `blocked` record counts as **busy** in the ladder: retry with backoff. On exhaustion, return a typed error wrapping `ErrPendingInteraction`, which the start classifies like today's unconfirmed nudge, recording and never failing Start or Relaunch. The start effect then enqueues `cfg.Nudge`. | `.23` |
| A21, A22 | `interrupt_now` final send (`internal/session/submit.go:333`); hard-restart send (`:356`) | **Gap:** a dialog appearing after the interrupt drops the message after the turn was already interrupted | Keep the up-front refusal (`submit.go:188-192`). After the interrupt, **queue** the message rather than drop it. | `.22b` |
| N3 | Refusal after the first Enter (`tmux.go:3327-3332,3542-3545`) | **Gap:** reported `ErrNudgeSubmitUnconfirmed`; the dispatcher spends an attempt and re-delivers (duplicate) | `blocked` seen after the first Enter is **delivered-unobserved** (`ErrNudgeSubmitDeliveredUnobserved`, `tmux.go:289`): the agent took the text and raised a dialog | `.22` |
| N4 | `awaitIdleComposerBeforeTyping` (`submit_turn_guard.go:37-73`) | Waits up to 10 s | Refuses at once on `blocked` | `.22` |
| B1, B2 | Interrupt / `/stop` / run cancel / city stop (`adapter.go:333`, `session_control_ops.go:847,864`) | No pending check | **Unchanged:** stop intent stays allowed | — |
| B3, B4 | Raw keys after an interrupt (`submit.go:404,448,660`; Gemini `/rewind` follow-up `adapter.go:752-776`); StopTurn follow-ons (`ClearInput`, Codex `/stop`, Claude task view) | No look | Skip the non-interrupt keys under `blocked`. These paths are already log-only. | `.22b` |
| B5 | `Respond` (`interaction.go:959`) | A dialog seen only by OSC is a silent no-op (`:983-989`) | Refuse `osc7501-*` ids with `ErrInteractionActionUnavailable`, as for a question (`:978-981`) | `.22` |
| B6 | Known-dialog dismissers (startup accept `tmux.go:3788`, mid-session `:3858`, model switch `:3891`, survey `:4027`, Codex verification `chat.go:1540`) | Type into recognized dialogs by design | **Unchanged.** A record never vetoes or clears a parser-recognized dialog. | — |

**Wording.** "Blocked" wording reuses the question/approval branches:
- `Kind` maps `permission` to `approval`, `question` to `question`, and `auth` or unspecified to `dialog`.
- The message says "answer it in the session's terminal" (`cmd_nudge.go:1586-1604`; `chat.go:1482-1499`).

**Why `.22b` should land early.** `.22b` is a working label for a new child
bead of `ga-f4ovjm`; its id is assigned when the bead is created. Its rows fix
today's behavior for parser-detected dialogs, because the sentinel is the same. So they can land
before the shadow, independent of 7501 and not gated by the switch, which
shrinks `.22`.

## 11. Exposure constraints

`ga-f4ovjm.16` is observation only and ships before the shadow. It must
respect the switch:
- `program_status` (`SessionProgramStatus`: state, kind, app, progress, since, quiet_since, working_children), `activity_source`, the SSE `program_status` frame, and the CLI STATUS column are populated whenever the reading is `Valid()`, which is in `shadow` and `on`. An absent `program_status` means unknown, never idle.
- Existing fields change only when the reading `Decides()`, that is under `on`: `activity`, agent `state` gaining `blocked`/`error`, and worker `Phase`. The mode is stamped on the reading, so the API never re-reads config. Those enum widenings reach T3 Code consumers and must be checked there.
- Counters, nonce, `Probed` and `LastError` stay internal. `Probed` appears only in `gc doctor`.
- Kind-only `Pending` from a `blocked` record is not observation. It drives refuse-or-queue, so it belongs to `.22`.

## 12. Conformance

### 12.1 Fake emitter

`internal/runtime/runtimetest/osc7501emitter` is a `go_binary` (package
`main`), modeled on `internal/runtime/acp/testdata/fakeacp`.

**How tests get it:**
- It is passed to tests by `$(rootpath)` in `GC_TEST_OSC7501_EMITTER_BIN`, only under `select({"//:gotags_integration": …})`.
- Tests read it with `bazeltest.DataPath` (`internal/bazeltest/datapath.go:10-32`; `TESTING.md:134-139`).

**The script.** A line script is passed by path. Each line is one step:
- `report <body>`
- `raw <hex>`
- `split <n>` (write the next sequence in n chunks, including mid-`ESC\`)
- `term bel|st`
- `sleep <d>`
- `probe-gated` (send the probe and DA1; stay silent unless a 7501 reply arrives before the DA1 reply)
- `force-env <NAME>` (report without probing when NAME is set)
- `on-line <script>` (run a sub-script per typed input line, to emulate turns)
- `exit <code>` and `signal <sig>`
- `clear`
- `hang` (stop reading and writing)

**Modes covered:**
- a passive ST emitter, Codex-like
- a probe-gated BEL emitter, Claude-like
- an env-forced emitter, pi-like
- invalid reports (control characters, over-limit, bad base64)
- child records
- every `blocked` kind
- `error`
- clear at exit
- stale `working` with no heartbeat

**Golden bytes.** The captured bytes from the experiments (Claude BEL, Codex
ST, app-before-kind ordering, 6 KiB split msg) are fixtures for
`programstatus` unit tests. They need no binary.

### 12.2 runtimetest suite

**The file.** `internal/runtime/runtimetest/program_status.go` holds
`RunProgramStatusContract(t, ProgramStatusFixtures)`. Fixtures are factories
for an armed session in a given scripted state. A nil fixture skips its case.

**Callers.** It is called from standalone test functions:
- `TestFakeProgramStatusContract`: unit lane, `Fake` with scripted readings
- `TestTmuxProgramStatusContract`: integration lane, the real tmux and the emitter

**Where it must not go.** It is not called from `TestFakeConformance` or
`TestTmuxConformance`:
- The provider-ledger proof check requires those to end in exactly `runtimetest.RunProviderTests(t, <factory>)` (`internal/testutil/providerledger/proof.go:109-134`, `ledger.go:155,278`).
- The RPP lockstep scan classifies only `conformance.go` (`runtimecontract/lockstep_test.go`).

Branch `fix/herdr-stop-waitforidle-contracts` (`ga-f4ovjm.5`) currently
changes the ledger-proved functions and needs the same treatment.

| Case | Asserts |
|---|---|
| PS-001 | Switch off reads `Unread` with no state, even with armed pane options present |
| PS-002 | First report gives `valid`, with state, app and kind exact |
| PS-003 | Armed with nothing reported reads `silent` |
| PS-004 | A probe-gated emitter is `silent` with `Probed` set and no relay, and `valid` with the relay (tmux only) |
| PS-005 | Respawn gives a new nonce; nothing from the old incarnation is ever `valid` |
| PS-006 | Reader death gives `reader_lost` (pipe closed, or tap missing from `ps`) |
| PS-007 | Pane death gives `exited`; `LastError` is kept |
| PS-008 | An inherited global or session `@gc_ps_tap`/`@gc_ps` gives `not_armed` or `silent`, never `valid` |
| PS-009 | `HoldLive` expires at the bound measured from max(Since, last activity), with an injected clock |
| PS-010 | `blocked` with each kind gives `Blocked()` and `Kind` exact; `Running()` is false |
| PS-011 | Children counts; `Quiescent()` is false while a child works; the root turn is unaffected |
| PS-012 | `AdvancedPast`: a boundary baseline credits a new turn; busy, silent and other-nonce baselines credit nothing |
| PS-013 | A foreign `pipe-pane` displaces the tap, giving `reader_lost`; heal defers and never takes over |
| PS-014 | Heal re-arms `not_armed`, and `reader_lost` after the pipe closes, back to `valid`; disarms on switch off |

**Fake implementation.** `Fake` implements `ProgramStatusProvider` and
`ProgramCaptureHealer` through scripted maps, as it does for `WaitForIdle`
(`internal/runtime/fake.go:61-84`), so the suite's own logic runs in the unit
lane.

### 12.3 workertest rows

The phase-2 catalog cannot express a capability only some profiles declare:
every scenario must list all ten profiles (`catalog.go:27-38,287-294`). So
the rows go in an inline `ProgramStatusCatalog`, using the
declare / drift / unsupported pattern from `TranscriptUsageResult`
(`internal/worker/workertest/phase1.go:93-103`):
- A profile that declares `program_status` must pass.
- A profile whose declaration drifts from the implementation fails.
- An undeclared profile is `Unsupported`.

Row wording stays transport-neutral (`engdocs/design/worker-conformance.md`
§2.2).

| Code | Requirement | Lands in |
|---|---|---|
| WC-PSTAT-001 | A declared profile's own status reports surface through the runtime program-status seam: first report, turn start, turn end. | `.18` (codex, claude), `.38` (others) |
| WC-PSTAT-002 | Worker phase follows a valid record under `on`: working → Busy, blocked → Blocked, idle/done/error → Ready. Under `shadow`, phase is unchanged. | `.16` |
| WC-PSTAT-003 | A program-reported dialog yields a kind-only pending interaction that cannot be answered by key. | `.22` |
| WC-PSTAT-004 | Under a live `blocked` record, wait-idle and queue delivery queue the message and spend no attempt. | `.22` |
| WC-PSTAT-005 | Under a live `blocked` record, explicit immediate delivery is refused with the pending-interaction error. | `.22` |
| WC-PSTAT-006 | A prefixless declared profile is ready at its first valid report plus settle. A prefixed profile's readiness ignores reports. | `.23` |
| WC-PSTAT-007 | With the switch at `shadow`, every worker decision is identical to `off`. | `.17` |
| WC-PSTAT-008 | A record never ends a transcript turn whose start is newer than the record's edge. | `.24` |

### 12.4 Lanes and manifests

**Unit lane** (`bazel test --config=ci`):
- `programstatus` golden-byte and fuzz tests, including the `ProbeMatcher` / `Scanner` agreement property
- the tap engine with a fake publisher (80 MB drain, coalescing, retry, EOF flush)
- relay pty-pair tests (`/dev/ptmx` works in this repo's Bazel sandbox; see `cmd/gc/productmetrics_controls_pty_linux_test.go`)
- `TestFakeProgramStatusContract`
- the workertest rows

**Integration lane** (`--config=integration`):
- `TestTmuxProgramStatusContract`
- tmux arming tests
- `TestTapCapturesFirstInstantReport`

**Bookkeeping:**
- Every new tmux test gets a line in `scripts/runtime-tmux-tests.manifest`, and new integration-only files go in `scripts/runtime-tmux-integration-files.manifest`.
- New packages get generated `testenv_import_test.go` files and `test/test-resource-counts.toml` rows.

## 13. Slices and acceptance tests

Run every test through Bazel. Each slice falls back to today's path when no
valid record exists, and each changes nothing while the switch is `off`.

| Bead | Slice | Acceptance tests (first failing) |
|---|---|---|
| `.10` | Vocabulary, `ProgramStatus`, mode/validity, `HoldLive`, `Baseline`/`AdvancedPast`, both interfaces and their `For` helpers, `InventoryEntry.Program`, `ProgramStatusComparison`; composite and fake-profile registries | `TestProgramStatusZeroValueIsUnread`, `TestNonceMustNameReadPane`, `TestAdvancedPastIgnoresOtherNonceAndBusyBaseline`, `TestHoldLiveUsesLaterOfSinceAndActivity`, `TestCompositesForwardEveryLeafCapability` (updated), `TestOptionalInterfacesAreListed` (updated) |
| `.9` | `Scanner`, `ProbeMatcher`, `ParseBody`, `Store`, codec | `TestScannerReassemblesReportSplitBetweenEscAndBackslash` (golden), `TestParseBodyDropsReportWithControlCharsInText`, `TestParseBodyStripsBidiFromText`, `TestStoreBlockedPausesTurnWithoutEndingIt`, `TestPromptStartDropsWorkingWithoutCountingEnd`, `TestClearIDRemovesDescendants`, `TestErrorThenClearKeepsStickyLastError`, `TestEncodeSurvivesServerSanitizerUnchanged`, `TestDecodeRejectsForgedFields`, `FuzzProbeMatcherAgreesWithScanner` |
| `.11` | Fake emitter `go_binary`, `RunProgramStatusContract` PS-001…014, `Fake` capability; RUNTIME-PLAN-006 moves into the ledger as RUNTIME-CONTRACT-017 | `TestFakeProgramStatusContract`, `TestEmitterProbeGatedStaysSilentWithoutReply` |
| `.12` | `cmd/gc-osc7501` skeleton, `version`, `tap`, packaging (goreleaser, formula, tap smoke, Bazel, Makefile) | `TestTapKeepsDrainingWhileWriterBlocked` (80 MB), `TestTapWriteIsNonceGuarded`, `TestTapIgnoresInheritedTMUXEnv`, `TestTapRetriesFailedWriteLatestWins`, `TestTapExitsAfterFiveFailures`, `TestTapFlushesPendingWriteOnEOF`, `TestTapDropsForeignApp`, `TestTapBacksOffUnderChurn`, `TestTapRSSBudget`, `//:goreleaser_check_test` |
| `.35` | `relay` subcommand | `TestRelayAnswersProbeBeforeForwardingDA1` (pty pair), `TestRelayProbeSplitAcrossReads`, `TestRelayForwardsBytesVerbatim`, `TestRelayPropagatesWindowSize`, `TestRelayForwardsTermAndHup`, `TestRelayReraisesChildSignal`, `TestRelayExitCodePassthrough`, `TestRelayNoTTYExecsDirectly`, `TestRelayLatencyBudget`, `TestRelayRSSBudget` |
| `.13` | Binding, switch, env injection, `runtime.Config.ProgramStatus`, `StartupHints` field, one derivation at every Config site, schema regen | `TestProgramStatusEnvDoesNotMoveLaunchFingerprint`, `TestProgramStatusArmExcludedFromFingerprint`, `TestProgramStatusOffInjectsNothing`, `TestProgramStatusEnvSkippedOnACPTransport`, `TestEveryRuntimeConfigSiteAppliesProgramStatusArm`, `TestProviderFieldSync` (exclusion), `TestSchemaFreshness` |
| `.36` | Arm on create and respawn, `tmuxCommandLines`, partial-list error, capability gate, companion lookup and handshake, relay wrap order, `wrapperCommands` | `TestTmuxCommandLineUnchangedForSingleCommand`, `TestTmuxCommandLinesKeepsSeparatorsBare`, `TestNewSessionArmsOnCommandFilePath`, `TestNewSessionArmFailureStillMarksSessionAndWithholdsCredentials`, `TestRespawnRearmsTapAndClearsRecordInOneList`, `TestUndeclaredProviderIsNeverArmed`, `TestRelayWrapInsideEnvUnsetInsideSlice`, `TestRelayWrapDoesNotMoveFingerprint`, `TestWaitForCommandTreatsRelayAsWrapper`, `TestCapabilityGateDisablesArmingWhenCompanionMissing`; integration `TestTapCapturesFirstInstantReport`, `TestLiteralTextByteExactThroughRelay` (RUNTIME-CONTRACT-011), `TestFencedInputThroughRelay` (RUNTIME-CONTRACT-009); full `TestTmuxConformance` with the switch off |
| `.14` | Read path: format fields when enabled, agent-pane fold, validity table, tap liveness from `ps`, `ProgramStatus()`, `InventoryEntry.Program` | `TestParseRuntimeInventoryReadsAgentPaneRecordNotSplit`, `TestInheritedGlobalTapOptionIsNotArmed`, `TestDeadTapProcessReadsReaderLost`, `TestSwitchOffIgnoresArmedRecords`, `TestProgramStatusValidityTable`, `TestReadFormatsUnchangedWhenOff`; integration `TestTmuxProgramStatusContract` |
| `.37` | Heal (`ProgramCaptureHealer`) on the inventory lane; `gc doctor` checks | `TestHealRearmsReaderLostAgentPaneOnly`, `TestHealNeverTakesOverForeignPipe`, `TestHealDefersWhilePipeOpen`, `TestHealDisarmsWhenSwitchOff`, `TestHealBacksOffRepeatedFailure`, `TestDoctorReportsDeclaredButSilent`, `TestDoctorReportsProbedButUnanswered`, `TestDoctorReportsCompanionMissing` |
| `.16` | Exposure under shadow and on ([§11](#11-exposure-constraints)) | `TestSessionResponseProgramStatusFromWorkerState`, `TestShadowLeavesExistingObservationFieldsUnchanged`, `TestComputeAgentStateBlockedAndErrorOnlyUnderOn`, `TestSessionStreamEmitsProgramStatusOnChange`, `TestOpenAPISpecInSync`, `make dashboard-check`, WC-PSTAT-002 |
| `.17` | Pure record-side rules; comparison sink; event type; async adapter; summarizer | `TestShadowDecisionIdenticalToOff` (per point), `TestShadowEmitsOnPairChangeOnly`, `TestShadowSinkNeverBlocks`, `TestShadowPayloadHasNoFreeText`, `TestEveryKnownEventTypeHasRegisteredPayload`, `TestShadowReportAgreementTable`, WC-PSTAT-007 |
| `.18` / `.38` | Declarations, as data only, verified per family under gc tmux with mocks | WC-PSTAT-001 per declared profile; `TestBuiltinProgramStatusEnvOnlyOnTerminalTransports` |
| `.22b` (new) | Pending-interaction callers queue, refund and never drop: rows A4, A7, A8, A10–A14, A17, A21/A22, B3/B4. Independent of 7501, same sentinel, not switch-gated. | `TestLaneImmediateNudgeRefusedBehindPending`, `TestBackstopRefundsAttemptOnPendingRefusal`, `TestDrainReminderRestoresMarkerOnPendingRefusal`, `TestWarmBindSkipsWaitBehindPending`, `TestSlingNudgeEnqueuesWithoutWaitBehindPending`, `TestExtmsgNotifyQueuesBehindPending`, `TestCreateWithInitialMessageQueuesBehindPending`, `TestInterruptNowQueuesWhenDialogAppearsAfterInterrupt`, `TestStopTurnSkipsKeysBehindPending` |
| `.22` | Leaf merge: `dialogInPane` record member; `SnapshotIdle`, `WaitForIdle`, `Pending` (kind-only, stable RequestID), typing refusal, hold bound; rows A1, A2, A5, A6, A15–A19, N3, N4, B5 | The tests on the bead, plus `TestDialogInPaneParsersWinOverRecord`, `TestBlockedAfterFirstEnterIsDeliveredUnobserved`, `TestAwaitIdleComposerRefusesAtOnceOnBlocked`, `TestPendingRequestIDStableAcrossReads`, WC-PSTAT-003…005 |
| `.23` | Readiness (prefixless only), startup ladder including row A20, interrupt settle | The tests on the bead, plus `TestStartupNudgeTreatsBlockedAsBusyAndNeverFailsStart`, WC-PSTAT-006 |
| `.8`, `.21`, `.24`, `.26` | Other cutovers, unchanged in scope | The tests listed on each bead, plus WC-PSTAT-008 for `.24` |

## 14. Rollout

**Order:**
1. Foundations: `.10`, `.9`, `.11`, `.12`, `.35`, `.13`, `.36`, `.14`, `.37`.
2. Review R1 (`.15`).
3. Pre-shadow slices: `.16`, `.17`, `.18`.
4. Review R1b (`.39`).
5. Stage the build (`.40`).
6. A 24-hour shadow (`.19`), judged against the targets set in `.34`.
7. Analysis (`.20`).
8. Cutovers.
9. Review R2 (`.25`).
10. Canary under `on` (`.41`).
11. Review R3 (`.42`).
12. Flip the default (`.27`).

**Deploy lineage (`.33`). Both outcomes work:**

- **Port onto `next/enterprise-8d6r5`.**
  - That branch is local and unpushed (head `8e75d2fabe`, the running release `mc-enterprise8d6r5-8e75d2fabe`).
  - Cherry-pick the merged foundation and pre-shadow commits.
  - Expect conflicts in `tmux.go`, `adapter.go`, `state_cache.go`, `runtime.go` and `template_resolve.go`. New logic lives in new files, so the edits in those files stay thin hooks.
  - The port gets its own Opus-max review.
  - Run `TestShadowDecisionIdenticalToOff` and the tmux suites on the port.
- **Refresh to an origin/main build.**
  - First run the refreshed build with the switch at `off` for a baseline window of at least 4 hours.
  - That separates main's drift from shadow effects for the abort criterion "any decision delta versus off".
  - The agreement metric itself is immune to drift, because both readings in each event come from the same build.
- **Either way:**
  - `gc` and `gc-osc7501` are built from the same commit into the same release directory, and the sibling lookup finds the companion.
  - `manifest.json` lists both, with their sha256 values.
  - The switch is set in maintainer-city's `city.toml` only. Other cities run the same binary with `off`, which is byte-identical.

**Claude pin (`.32`).** All three outcomes are expressible as data:
- **Pin bumped to 2.1.295 or later (2.1.311 verified):** claude stays declared with `relay=true`.
- **Pin stays at 2.1.283:** set `[providers.claude.program_status] protocol = "none"` in maintainer-city. A relay around a pre-7501 Claude is pure risk with no data. The shadow then covers Codex only.
- **Cohort:** a custom provider based on `builtin:claude`, with a bumped pin and its own binding.

**Rollback.**
- Set the switch to `off`: readers ignore records at once, and heal disarms taps.
- Restart relay-wrapped seats to remove the relay.
- The binary rollback is the repoint of `/opt/gascity/current` that already exists.

## 15. Risks

1. **Coverage.** Most scraping exists for Claude, and Claude needs 2.1.295 or later and the relay. If maintainer-city's pin stays at 2.1.283, the shadow measures Codex only, and the Claude cutovers (`.24`) wait for a pinned cohort.
2. **The relay is in the data path.** Its death hangs up the agent. Mitigations:
   - its minimal code ([§4.3](#43-gc-osc7501-relay))
   - the pty-pair and latency tests
   - the shadow's abort criterion: any agent death attributable to the relay
   - existing crash accounting, unchanged
3. **No heartbeat.** A hung program at `working` adds at most `ProgramStatusHoldLimit` of hold to non-destructive gates. A 401 retry storm reads `working` for minutes, so stall timers stay. Destructive backstops are untouched.
4. **Process-snapshot gating.** When the `ps` scan is skipped or degraded (`state_cache.go:959`), tap liveness is unverified. The hold bound limits the impact, and doctor and the shadow payload count `ReaderVerified=false`.
5. **Second binary.** Upstream has shipped only `gc`. A missing companion degrades silently to the scrape, so `gc doctor` must say so. If upstream declines the binary, the fork carries `.12`, `.35` and `.36` behind the switch. The contract and readers (`.10`, `.9`, `.14`) remain upstreamable.
6. **Events volume.** Shadow events are consumed by SSE, long-poll and `exec:` event providers. The volume is estimated, and the queue and on-change rule bound it. The shadow abort criteria include events-log growth.
7. **Codex turn-start timing.** If Codex reports `working` only after its `UserPromptSubmit` hook, `.21` narrows the Escape window but does not close it.
8. **Upstream sign-off on behavior widenings.** These are approved for this fork:
   - busy Claude wait-idle queues
   - blocked refusal for families without a busy indicator
   - those families may prove idle and sleep
   - prefixless readiness

   Upstream PRs must call each one out.

## Appendix: corrections to the synthesized draft

- **Probe.** Tap-only capture cannot enable Claude. The relay is v1 scope, with a relay flag in provider data.
- **Switch values.** They are `off|shadow|on`, not `off|auto`. Every reading carries its mode, so consumers never re-read config.
- **Text.** Reports with control characters in `msg` or `title` are dropped whole, as rev 0.4 requires. The draft stripped them instead. Bidi and invisible characters are still stripped for display.
- **Zero value.** The validity zero value is `Unread`, not "valid". The draft's `ProgramKnown` flag is folded into it.
- **Mismatched nonce.** A record whose nonce does not match reads `silent` (treated as absent), not `reader_lost`.
- **`readAgentPane`** resolves session names with `list-panes`, not `display-message` (`tmux.go:4220`).
- **Diagnostics files.** `nudge-unconfirmed.log` is overwritten through temp-and-rename and holds raw nudge text (`internal/runtime/privatedir.go:81-97`). It is not a usable time-series sink, so the shadow uses events.
- **Fingerprint guards.** `runtime.Config` cannot gain a second `map[string]string` field (`fingerprint_config_env_test.go:13-33`), and every new field must be classified (`fingerprint_partition_test.go:139`).
- **Conformance home.** `runtimetest` cases cannot live in the ledger-proved `TestXxxConformance` functions, and phase-2 workertest scenarios cannot express per-profile capabilities. See [§12](#12-conformance).
