# Hosted Gas City onboarding — feasibility + design

*Status: exploration/design. No code. Produced by an 11-agent Fable
exploration→design→red-team pass (workflow `wf_4f5a1628-ce3`), grounded across
OSS `gc` (this repo), the remote-gc worktree (PR #4053), and crucible.*

## 0. Verdict

The vision is **feasible, elegant, and cleanly separable** from commercial code
— but three truths shape everything:

1. **OSS `gc` already contains a complete, commercial-free hosted-service
   client.** It is trapped inside `gc pack registry login`
   (`cmd/gc/cmd_registry_auth.go`): a browser-callback loopback flow, an
   RFC-8628 device flow, a whoami probe, and an atomic-0600 per-URL token store
   — with `defaultRegistryPublishURL = "https://registry.gascity.com"`
   (`cmd/gc/cmd_registry.go:25`) as a sanctioned default-URL constant. The
   whole onboarding vision is **"generalize that"**: extract it, point it at
   `gascity.com`, add two resources (cities, runs). This is the git/github,
   docker/docker.io shape — a URL is configuration data, not commercial code.

2. **The OSS client is ~10% of the work; the hosted server is ~90%.** Both
   flows are mostly private-side (crucible/gasworks). What ships in OSS is
   mechanical; what's hard is the server.

3. **Flow 1 (onboard-to-city) is first and cheap; Flow 2 (anonymous instant
   run) is the killer demo but a multi-month, security-critical program.** An
   anonymous endpoint that runs arbitrary user formulas with a platform LLM is
   literally RCE-and-inference-as-a-service to strangers. It is **not safe to
   expose** until a specific server-side control set exists — credential-less
   LLM egress *first*.

**Recommended path:** ship a *web playground* as the true zero-install first
touch (Flow 0), sequence Flow 1 (`gc init --at`) as the first CLI onboarding,
and treat Flow 2 (`gc run --at`) as the flagship that lands after its safety
rails. All three ride **one** generic protocol with **zero** commercial logic
in OSS.

## 1. The elegant core

Every commercial thing the user sees arrives as an **opaque server-authored
`message` string and `links` map** that the CLI prints verbatim. "You have $5
of trial credits", "expires in 72h — run `gc login` to keep it" — none of that
wording, and none of the credit math, plan names, or expiry policy, lives in
OSS. The CLI opens URLs the server returns, polls status the server reports,
and prints strings the server authored. That single discipline is what keeps
the boundary clean while the product surface stays fully server-evolvable
without a CLI release.

## 2. The flows

### Flow 0 — web playground (the real "no install" first touch)

Julian's stated ideal was *value without installing a CLI*. The design already
mandates, server-side, everything a zero-install path needs: an anonymous run
endpoint and a public watch page. So the truly-instant first touch is a **"Run
this" button on `gascity.com/demos/<name>`** that hits the *same* anonymous run
endpoint and lands on the *same* watch page — ~10s to a live run, zero install.
The CLI is the *second* touch, for users already convinced. The watch page's
terminal state advertises the copy-paste CLI one-liner and `gc login` to claim.

### Flow 1 — `gc init --at` → hosted wizard → provisioned city

`gc init --at` (bare `--at` = `https://gascity.com`) ensures login (auto-runs
the browser flow), opens the server-rendered **city-configuration wizard**,
polls provisioning to ready, prints the dashboard link, and (once PR #4053 is
on main) writes a local `contexts.toml` pointer so subsequent `gc` commands
target the hosted city. All provisioning/trial/pack/workspace logic is
server-side; the CLI opened one URL and polled a status.

**Why cheap:** crucible's create-city pipeline is largely built — a real
`cityControllerResolver` maps cityID→controller sandbox, `CityRecord` carries
`ControllerSandboxID`, write-plane routes are registered, and the hosted
controller runs *stock OSS `gc`*. Only the CLI-auth edge + wizard callback +
status-translation edge are new.

### Flow 2 — `gc run <formula> --at` → anonymous ephemeral run + watch link

The hosted generalization of the local `gc run` one-shot. Instead of
manufacturing a local transient city, `gc` **submits the formula** (+ vars +
repo refs) to a public Run Service; the server executes **the stock OSS `gc run`
one-shot inside a warm sandbox with a platform-injected `--agent-cmd`** — the
hosted run *is* the local run, with the platform supplying the one thing the
anonymous user can't: the LLM. Returns a receipt `{run_id, watch_url,
events_url, run_token, claim}`; the CLI streams SSE to `gc.outcome`, exits 0 on
pass, and saves a receipt for later claiming.

**Anonymous is already a supported client shape:** the remote client treats a
nil TokenSource as "send no Authorization header" (PR #4053
`client_remote.go:37-40`), so anonymous-first needs zero new auth machinery.

### `gc login` / `gc link` — value-first, account-later

`gc login` = the human browser/device flow (generalized registry login) that
mints and stores an opaque bearer. **Best beat: `gc login` auto-claims any
unclaimed run receipts on success**, so `gc link` never has to be typed (it
remains as plumbing for explicit codes / cross-device paste). One verb to
learn. Claiming an anonymous run — ownership transfer, credit attribution,
retention — is 100% server policy.

## 3. The one reconciled protocol (`gascity.dev/service/v0`)

> The three designers produced three namespaces, three claim mechanisms, and
> two receipt files. **These must be reconciled into ONE versioned spec before
> any code.** The picks below are the reconciliation.

Published as `docs/reference/specs/service-protocol-v0.md` in OSS so any third
party can implement a conforming server — the strongest proof of genericity.
Version constant `serviceproto.Version = "gascity.dev/service/v0"` (sibling of
the existing `gascity.dev/client-auth/v1`). All wire types are typed Go structs
(no `map[string]any`), errors are `application/problem+json` over the existing
`urn:gascity:error:*` apierr contract (PR #4103). Every request carries
`X-GC-Service-Version`.

**Auth** (generalizes `cmd_registry_auth.go` verbatim; only paths change):
- `GET {base}/gc/v0/auth/cli?redirect_uri&state&label` — server-rendered
  sign-in/up page; callback forwards token to the CLI loopback `/token`.
- `POST {base}/gc/v0/auth/device/code` / `.../token` — RFC-8628 device flow.
- `GET {base}/gc/v0/me` → `{handle, display_name, message?, links?}`. **No
  org/tenant field** — accounts are an opaque `handle` (keeps `check-core-boundary`
  trivially green). Token is an opaque bearer; STS/EIA/DPoP exchange is
  edge-internal, never in OSS.

**Cities** (generalizes OSS `POST /v0/city`'s 202+request_id convention):
- `POST {base}/gc/v0/cities` `{request_id, name?, template?}` → 202
  `{city_id, phase, wizard_url?, status_url, message?}`.
- `GET {base}/gc/v0/cities/{id}` → `{phase, message?, links{dashboard}, api?{base_url,city}, error?}`.
  Phase enum = crucible's frozen `pending|provisioning|ready|error` plus an
  optional pre-crucible edge state `configuring` (keep crucible's `error`; do
  **not** invent `failed`). `api.*` is what the CLI writes into a context on ready.

**Runs** (generalizes `run_execute.go`'s materialize→sling→watch→reap):
- `POST {base}/gc/v0/runs` — **Authorization optional**. `{request_id, formula:{filename,body}, vars?, inputs?:[{name,kind:git|upload,url?,ref?}]}`
  → 201 `{run_id, phase, watch_url, status_url, events_url?, run_token?, claim?:{code,url,expires_at}, limits?, message?, expires_at?}`.
- `GET {base}/gc/v0/runs/{id}` (Bearer account **or** run_token) → phase
  `queued|provisioning|running|pass|fail|error|expired|canceled` mapping 1:1 to
  local `gc.outcome`; exit 0 iff `pass`.
- `GET {base}/gc/v0/runs/{id}/events` — SSE `run.phase|run.note|run.done`.
- `POST {base}/gc/v0/runs/{id}/claim` (Bearer account) `{run_token}` → claim.

**Default endpoint config** — one CLI-visible host:
```go
// cmd/gc/service_endpoints.go — URL strings are config data, not commercial code
const defaultServiceURL = "https://gascity.com" // login, cities, AND runs
```
Resolution ladder cloned from `resolveRegistryPublishBaseURL`
(`cmd_registry.go:459-476`): flag → `GC_SERVICE_URL` → stored default →
constant. `work.gascity.com` becomes **server-side routing**, not a user
concept (see §6).

## 4. OSS vs commercial boundary

**OSS ships (all mechanical):**
- `internal/serviceproto` — typed wire structs + thin net/http+SSE client +
  version constant + receipt store `~/.gc/runs.json`.
- `internal/cliauth` — login flows + shared credential store
  `~/.gc/credentials.json`, extracted from `cmd_registry_auth.go` (registry
  login refactors onto it, keeping `registry.json` read-compat).
- Commands: `gc login`, `gc whoami`, `gc init --at`, `gc run --at`, `gc link`
  (+ hidden `gc auth token-helper` implementing the existing client-auth exec
  contract, bridging login → the #4053 context substrate).
- The default-URL constant(s) and the public spec doc.

**Commercial stays private** (crucible/gasworks): every HTML page (sign-in/up,
wizard, watch), account + trial-credit creation, anonymous capability minting,
run intake + scheduling + sandboxing + reaping, metering/billing/quota, claim
semantics, and **all human-readable policy copy** via `message`/`links`.

**How CI guards stay green** (verified against the actual scripts):
- `check-core-boundary.sh` (a/d) no commercial imports; (b) no `org_` token —
  v0 wire types carry no tenancy field; (c) no TenantSlug joins; (e)
  EvaluationContext untouched.
- `check-eventexport-isolation.sh` — new brand URLs live in
  `cmd/gc/service_endpoints.go`, **not** the four guarded files; its header
  (lines 8-10) explicitly sanctions "registry defaults" as legitimate.

**⚠ The guards are STRUCTURAL, not semantic.** A field like `TrialCreditsRemaining
int` / `Plan string` / `QuotaRemaining int` in a generic-looking OSS wire
struct passes *all five* checks — and product pressure ("show $5 in the CLI")
pushes exactly toward adding it "just for rendering", collapsing the whole
opacity discipline. **Must ship in the FIRST serviceproto PR (same commit
series), not later:**
- **Check (f):** a commercial-semantics denylist
  (`trial|billing|credit|plan|invoice|subscription|quota`) scoped to
  `internal/serviceproto`, `internal/cliauth`, and the new `cmd/gc` onboarding
  files, with a `// boundary:allow` escape hatch.
- A **serviceproto JSON golden test** pinning the wire field set (any new field
  is a reviewed, deliberate diff).
- Codify in AGENTS.md: *"commercial policy travels only in opaque
  `message`/`links`; default endpoint URLs are configuration, not commercial
  code."* Today that rule is only implied by a comment.

## 5. Security — the anonymous-run exposure (gates Flow 2)

The execution engine documents itself as unsafe for untrusted callers
(`cmd/gc/cmd_run.go` Long help: *"gc run is local single-user only; do not
expose to untrusted callers without an authorization gate"*, *"--agent-cmd runs
an arbitrary command AS YOU with your environment"*). The anonymous flow is
exactly that exposure: a hostile formula is just a task phrased to a
shell-capable agent, and the attacker reads its output live via `watch_url`.

**FATAL sequencing defect (as designed):** milestones ordered "cold-sandbox
run first, AGX warm-fork second" leak the platform LLM credential on day one —
because the AGX **credential-less egress gateway** is the *only* control keeping
a platform key out of a sandbox an anonymous formula can drive. `scrubbedEnv`
(`run_execute.go:117-135`) scrubs `GC_*`/`BEADS_*` for *local* isolation, not
hostile multi-tenant secret containment.

**Hard pre-launch checklist (all server-side; none in OSS):**
1. **Credential-less LLM egress FIRST** — keys never inside any sandbox an
   anonymous formula can drive. This is milestone ZERO, not two.
2. Per-run cgroup CPU/mem/disk + wall-clock quotas (default local `--timeout`
   is 30m of free multi-core compute per submission).
3. Egress deny-by-default with link-local/metadata-IP (169.254.169.254) and
   known-mining-pool blocking; per-run network namespace, no shared FS, no
   control-plane reach.
4. Per-run LLM-token ceiling + **global trial-pool spend circuit-breaker/kill-switch**.
5. IP/ASN rate-limiting + a **browser/PoW challenge** (the `403 {challenge}`
   hook) shipped in v0, plus a **headless `{challenge:{kind:"code"}}` variant**
   so SSH users aren't walled mid-demo.
6. ToS/consent + secret-scanning at first anonymous submission (users *will*
   paste API keys as `--var` and private repos as `--folder`; the platform
   becomes involuntary custodian). Short retention with explicit `expires_at`.

**Trial-farming is bounded, not eliminated** — anonymous = IP/ASN is the only
axis, and "sign up N times = N× credits" is inherent to zero-account value.
Accept it as a deliberately-sized global-cap cost; require device/payment
verification before credits are *granted* (not before a run is *watched*).

**Do not** ship the proposed Phase-2 "OSS reference run-server" as a zero-config
open listener — that's an OSS RCE footgun. The public spec doc alone is the
genericity proof; if a reference server ships, it defaults to auth-required +
loopback-bind + a prominent warning.

## 6. Must-fix design corrections

- **Drop `--at` NoOptDefVal — it's a confirmed pflag landmine.** With
  NoOptDefVal, `--at <url>` (space form) does *not* bind the value — it silently
  targets the default and treats the URL as a stray positional (breaking the
  designs' own transcripts and, under `gc run`'s `ExactArgs(1)`, erroring
  "accepts 1 arg, received 2"). Use **`--hosted` boolean** (implies the default
  endpoint) + **`--at <url>` value-required** for non-default servers:
  `gc run hello.toml --hosted` is the demo line; `--at https://gc.corp` is the
  escape hatch.
- **One CLI-visible host, one env var, one account verb** — `gascity.com` for
  login/cities/runs (`work.gascity.com` = server-side routing); `GC_SERVICE_URL`
  only; `gc login` auto-claims (so `gc link` is plumbing, absent from first-run
  copy).
- **`gc run` accepts an `https://` formula argument** (fetch + validate TOML
  like a local path, then submit) so the whole post-install demo is one line.
- **Capability-gate printed next-steps** — never print `gc sling`/`gc rig add`
  if the resolved binary+server pair can't execute them (crucible's cityproxy
  is still read-only/GET-only; the write plane needs #4053 + minter enablement).
  Default to the dashboard link alone.
- **End on an artifact, not a phase stream.** Add server-authored
  `links.result`; every homepage demo formula must produce a visible, shareable
  thing (a rendered preview, a diff, a generated repo). The wow is the artifact.
- **Handle the `gc` = `git commit --verbose` alias** (Oh My Zsh git plugin) —
  install script detects it and prints the fix; homepage snippets use
  `command gc`; first-run banner repeats the hint.
- **A static, dependency-free binary for the hosted path.** The Homebrew
  formula pulls six runtime deps (tmux, jq, git, dolt, bd, flock) + a CGO ICU
  dep — none needed for `gc run --at`/`gc login`. `curl …/install | sh`
  installs only `gc`; dependency checks go lazy (local `gc run`/`gc init`
  verify at need; hosted verbs never do).

## 7. Sequencing — the smallest first slice

Both OSS foundations the design leans on are **not on main today**: the local
`gc run` one-shot is branch-local (`feat/gc-run-oneshot`), and PR #4053
(remote substrate) is open. The registry login machinery **is** on main.

| # | Slice | Depends on | Commercial code in OSS |
|---|-------|-----------|------------------------|
| 0 | **Web playground** on `gascity.com/demos` → anonymous run endpoint + watch page | Flow-2 server (long pole) | none (no OSS at all) |
| 1 | Land `feat/gc-run-oneshot` to main | — | none |
| 2 | Extract `internal/cliauth`; ship `gc login` + `gc whoami` + check (f) + golden test + AGENTS.md rule | main only (registry machinery) | none |
| 3 | `gc init --at` = login → open wizard URL → poll status edge → dashboard link (degraded ending: no context write until #4053) | crucible create-city edge + CLI-auth server | none |
| 4 | `gc run --at` client (typed submit + SSE) | Run Service + full §5 checklist | none |
| 5 | `gc login` auto-claim + `gc link` | claim/ownership server verbs | none |

**Slice 2 is the true unlock** — it depends only on code already on main and is
shared by everything downstream. It ships visible value (top-level `gc login`)
with zero server dependencies beyond the CLI-auth edge (which is a small,
portable lift from the existing registry app).

Ranking the flows: **Flow 1 first** (server pipeline mostly exists; only the
edge + wizard callback are new — weeks). **Flow 2 second** (a multi-month
private-side program: run intake + anonymous identity + watch tokens + claim +
the entire §5 abuse/credential-isolation rail; the OSS client is the last, small
piece). **Do not gate Flow 1 or the login slice on any Flow-2 infrastructure.**

## 8. Open decisions for Julian

1. **Flow 0 web playground** — build it as the marketed first touch? (Highest
   value-per-effort; reuses Flow-2 server, no OSS.)
2. **One host or two?** Recommendation: one CLI-visible `gascity.com`,
   `work.gascity.com` server-internal. Confirm infra can route this.
3. **`--hosted` boolean vs `--at=URL`-only** — recommendation: both (`--hosted`
   for the default, `--at <url>` for third-party servers). Drop NoOptDefVal.
4. **Anonymous-run launch gate** — accept that Flow 2 does not go public until
   the full §5 checklist (credential-less egress FIRST) exists? This is the
   go/no-go for the killer demo.
5. **Trial-farming tolerance** — a deliberately-sized global-cap cost, with
   device/payment verification before credit *grant*?
6. **Start with slice 1+2** (land `gc run`, extract `cliauth`, ship `gc login`)
   as the first shippable increment while the server-side Flow-1 edge is scoped?
