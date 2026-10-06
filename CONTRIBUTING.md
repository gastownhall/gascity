# Contributing to Gas City

Gas City is experimental software, but the repo is now structured for external
contributors. Before making changes, read:

- [docs/index.mdx](docs/index.mdx)
- [engdocs/contributors/index.md](engdocs/contributors/index.md)
- [engdocs/contributors/codebase-map.md](engdocs/contributors/codebase-map.md)
- [engdocs/architecture/index.md](engdocs/architecture/index.md)
- [TESTING.md](TESTING.md)
- [ROADMAP.md](ROADMAP.md) for what is planned and how to propose work

## Getting Started

1. Fork the repository.
2. Clone your fork.
3. Install prerequisites from
   [docs/getting-started/installation.md](docs/getting-started/installation.md).
4. Set up tooling and hooks: `make setup`
5. Build and run the fast quality gates: `make build && make check`
6. Optional: if you use Bazel, opt in to the project's anonymous, read-only
   build cache by adding `build --config=fork-cache` to your gitignored
   `.bazelrc.local` (or pass `--config=fork-cache` per command). Results CI
   already computed become cache hits; nothing you build is uploaded. See
   [engdocs/bazel-quickstart.md](engdocs/bazel-quickstart.md).

`make setup` installs a pre-commit hook at `.githooks/pre-commit` that
auto-formats staged Go files and, when any Go file is staged,
regenerates `internal/api/openapi.json` and `docs/reference/schema/openapi.json`
from the live supervisor. The hook stages both spec copies so the
committed spec never drifts from what the server actually serves. It also
runs the fast CI-equivalent gates for local changes: `make lint`,
`make vet`, and `make test` for Go changes, and `make check-docs` for
Markdown/docs/spec changes.

**Dashboard SPA.** The dashboard at `internal/api/dashboardspa/web/` is a
TypeScript SPA that talks directly to the supervisor's OpenAPI-typed
endpoints. When `internal/api/openapi.json` changes, the hook regenerates
`internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client/`
(the typed API client) and, when that or the SPA source changes, rebuilds
`internal/api/dashboardspa/dist/` (the compiled bundle that the Go static
server embeds via `go:embed`). The hook needs Node / npm on your PATH; if
npm is missing and a spec change is staged, the hook now fails closed with
the recovery command, since a stale client would otherwise ship silently
until CI catches it — for unrelated (docs/Go-only) changes it still just
warns and skips the rebuild. The hook runs dashboard typecheck, Vitest, and
production build for dashboard/API-schema changes. Run `make dashboard-dev`
to iterate with Vite HMR, `make dashboard-build` to produce a fresh
bundle, `make dashboard-check` for typecheck + build + test. For
API-schema changes, run `make dashboard-ci` instead — it also regenerates
the typed client from the spec and fails if that or `dist/` is stale,
which `dashboard-check` alone does not catch. For dashboard or API-schema
changes, also smoke the built app with
`npm run preview -- --host 127.0.0.1 --port <port>` from
`internal/api/dashboardspa/web/` and load the served page before pushing.

## Development Workflow

GitHub Issues is the public tracker. Use an issue when it adds context
reviewers need: a user-visible bug, a behavior or design change worth
discussing first, or work that spans several pull requests. The issue holds
why the change is needed, what it affects, and how we will know it works, and
it does not need maintainer approval before you open the pull request. Small,
self-explanatory changes (typos, flaky tests, refactors, CI or docs tweaks)
can go straight to a pull request whose description explains the why.

1. If the change warrants an issue, find or file one. The issue forms ask for
   the motivation, impact, risk, and verification plan (or, for a bug, the
   reproduction and impact), which is most of what review needs. Changes that
   add SDK surface should explain how they pass the
   [Primitive Test](engdocs/contributors/primitive-test.md).
2. Create a branch from `main` (see [Branch Naming](#branch-naming)) and make
   the change.
3. Run `make check`, and `make check-docs` if you touched docs, navigation,
   or cross-links.
4. Open a pull request that explains the change, shows evidence that it works
   end to end, and says `Closes #<issue>` when there is one.

Using an AI agent is fine; you are accountable for what it produces. Agents
working in this repo read [AGENTS.md](AGENTS.md), which carries the same
rules.

What is planned next is in [ROADMAP.md](ROADMAP.md).

### Git hook ownership

**`.githooks` is the single owner of `core.hooksPath`.** Install it with
`make setup`; verify it with `make check-hooks`.

Only one directory can own `core.hooksPath`, and beads' installer claims it
for `.beads/hooks`. Those hooks exec `bd hooks run <hook>` without chaining
onward, so while beads owns the path every gate in `.githooks` — staged-Go
formatting, `lint-changed`, the three codegen+stage steps, `make vet`, and the
push-time suite — is skipped on every commit. Nothing reports this: git simply
stops invoking the hooks, so commits look clean while spec-derived drift lands
on the mainline until a later suite failure surfaces the drift.

Reclaiming the path does not disable beads. Each `.githooks` hook forwards to
`.githooks/lib/beads-chain.sh`, which runs `bd hooks run <hook>` with the same
timeout and exit-code carve-outs beads' own integration block used. Adding a
hook that beads manages means adding its `.githooks` counterpart too —
`TestGitHooksCoverEveryBeadsManagedHook` in `scripts/` fails otherwise.

Beads' installer can reclaim `core.hooksPath` at any time. When it does,
`make check-hooks` fails and `make setup` puts it back.

`make spec-ci` (run by the required `preflight-generated` CI job) is the
backstop for spec/client drift, but it only sees work that reaches a PR —
locally merged branches depend on the pre-commit gate actually running.

### Branch Naming

Never open a PR from your fork's `main` branch. Use a dedicated branch per PR:

```bash
git checkout -b fix/session-startup upstream/main
git checkout -b docs/mintlify-nav upstream/main
```

Suggested prefixes:

- `fix/*`
- `feat/*`
- `refactor/*`
- `docs/*`

## Code Style

- Follow standard Go conventions.
- Keep functions focused and small.
- Add tests for behavior changes.
- Add comments only when the logic is not self-evident.

## Design Philosophy

Gas City follows two project-level principles that should shape changes:

### Zero Framework Cognition

Go handles transport, not reasoning. If the behavior belongs in the model or
prompt, do not encode it as framework intelligence in Go.

### Bitter Lesson Alignment

Prefer durable infrastructure, observability, and composition over brittle
heuristics that a stronger model should eventually handle better.

For the capability boundary, use the
[Primitive Test](engdocs/contributors/primitive-test.md).

## Docs Workflow

The docs tree is now Mintlify-based.

- Config lives in `docs/docs.json`
- Preview locally with `cd docs && ./mint.sh dev`
- Run docs checks with `make check-docs`

When updating docs:

- Architecture docs describe current behavior
- Design docs describe proposed behavior
- Archive docs keep historical notes out of the main onboarding path
- Updating `GastownCity()`'s `Imports` or `DefaultRigImports` map requires
  updating the auto-import table in `engdocs/design/packv2/migration.mdx`

### Docs link conventions

`docs/` is published to **[docs.gascityhall.com](https://docs.gascityhall.com)**
via Mintlify — it is **not** meant to be read directly on GitHub. The published
site serves **route-based, extensionless URLs**, so internal page links must be
written that way:

| Context | Correct | Wrong |
|---|---|---|
| `docs/` page link (Mintlify) | `/tutorials/01-beads` | `/tutorials/01-beads.md` |
| `engdocs/` and root `.md` (GitHub-only) | `engdocs/architecture/index.md` | `engdocs/architecture/index` |

**Why this matters — and why you usually do _not_ want to "fix" a docs path:**
a `.md`/`.mdx` suffix on a `docs/` page link breaks Mintlify navigation on the
live site **even though the file exists on disk**. A link that looks "broken" on
GitHub is very often correct for the deployed site, so reformatting `docs/` links
to be GitHub-friendly is the most common way to *silently break the published
docs*.

Two checks enforce this, both failing **only on net-new** breakage your change
introduces (pre-existing issues won't block you):

- `make check-docs` (`test/docsync`) — on-disk check that `docs/` page links are
  extensionless and that `engdocs/`/root links resolve.
- The **Docs render check** CI Action (`.github/workflows/docs-render.yml`) —
  runs Mintlify's own `broken-links` against your branch vs `main`.

If a `docs/` link is **genuinely** broken on the live site, note it in your PR
and a maintainer will fix it Mintlify-side — don't change the on-disk path to
work around GitHub rendering.

## Make Targets

Run `make help` for the full list. The most useful targets are:

| Command | What it does |
|---|---|
| `make setup` | Install local tools and git hooks |
| `make build` | Build `gc` with version metadata |
| `make install` | Install `gc` into `$(go env GOPATH)/bin` |
| `make check` | Fast Go quality gates |
| `make check-docs` | Docs sync tests (on-disk link checker; does not run `mint broken-links`) |
| `make check-all` | Extended quality gates including integration tests |
| `make test` | Unit and repo-level Go tests |
| `make test-integration` | Integration tests |
| `make test-integration-huma` | Supervisor binary smoke test (builds `gc`, boots the supervisor, asserts `/openapi.json` + `gc cities` work) |
| `make dashboard-build` | Compile the dashboard bundle and sync it into the embedded `dist/` |
| `make dashboard-dev` | Vite dev server for SPA iteration |
| `make dashboard-check` | Typecheck + build + test the dashboard |
| `make dashboard-ci` | `dashboard-check` plus fail-on-drift for the generated API client and `dist/` — the gate for openapi.json/dashboard changes |
| `make cover` | Coverage run |

> **`make install` writes to the shared `$(go env GOPATH)/bin`.** It (and
> `go install ./cmd/gc`) install `gc` there, and `make install` also re-points
> an existing `~/.local/bin/gc` at the result — so when that path is the binary
> a running deployment uses (commonly `~/.local/bin/gc` → `~/go/bin/gc`),
> installing from any checkout silently replaces the live `gc`, and every later
> `gc` exec runs the just-installed build. To redirect when that isn't
> intended, run `make install INSTALL_DIR=<dir>` (the `install` target writes
> `$(go env GOPATH)/bin` and ignores `GOBIN`), or for a plain
> `go install ./cmd/gc` set `GOBIN=<dir>`. `make build` (→ `./bin/gc`) is
> unaffected.

## macOS Local Development

On macOS, `make build` signs `gc` with a stable local codesigning identity
when one is available. Stable signing helps macOS TCC remember local
permission grants, such as App Management and Apple Events, across rebuilds.

The build auto-detects the first valid certificate in your keychain, in this
order: `Apple Development:`, `Developer ID Application:`, then `GasCity Dev`.
Override the selection with `GC_SIGN_IDENTITY=<certificate name>`.
The signing identifier defaults to `com.gascity.gc`; override it with
`GC_SIGN_IDENTIFIER=<identifier>` only when you intentionally want a separate
local TCC identity. After a successful stable or opt-in ad-hoc signing pass,
the script removes the `com.apple.provenance` extended attribute when present
so macOS does not retain stale local-build provenance metadata.

If no stable identity is available, the build leaves Go's linker-produced
macOS signature unchanged. It does not automatically ad-hoc re-sign the
binary, because ad-hoc signing creates a fresh identity and can cause repeated
TCC prompts. If you need the old behavior for a local experiment, opt in with
`GC_ADHOC_SIGN=1`.

Getting a free local certificate does not require paid Apple Developer Program
membership:

- **Apple Development**: Xcode -> Settings -> Accounts -> sign in with an
  Apple ID -> Manage Certificates -> `+` -> Apple Development.
- **Self-signed**: Keychain Access -> Certificate Assistant -> Create a
  Certificate. Use Identity Type **Self Signed Root** and Certificate Type
  **Code Signing**. Name it `GasCity Dev` for auto-detection, or set
  `GC_SIGN_IDENTITY` to its name.

For official distribution, local development signing is not enough; release
artifacts need a Developer ID certificate and notarization.

## macOS Release Verification

Before tagging a release, run the macOS smoke test on a Mac:

```bash
./scripts/smoke-macos.sh                     # latest release, arm64
GC_VERSION=v0.13.4 ./scripts/smoke-macos.sh  # specific version
GC_ARCH=amd64 ./scripts/smoke-macos.sh       # Intel binary
```

The script downloads the release archive, extracts the `gc` binary, and runs it
inside a `sandbox-exec` jail that denies network access and restricts filesystem
writes to a temp directory. Tests: `version`, `help`, `doctor`, `init`.

Run this after changing build/packaging scripts or upgrading the Go toolchain.

## Commit Messages

- Use [Conventional Commits](https://www.conventionalcommits.org/):
  `type(scope): summary`, e.g. `fix(session): keep work beads on close`
- Use present tense
- Keep the first line under 72 characters
- Explain *why* in the body; reviewers and `git blame` readers see the commit,
  not the PR thread
- Reference the issue (`Closes #123`) in the pull request description when
  there is one

## Issue Triage Labels

Every new issue gets `status/needs-triage`. Maintainers then move it along
this ladder:

| Label | Meaning | Who applies it |
|---|---|---|
| `status/needs-triage` | Inbox — not looked at yet | Automation, on open |
| `status/needs-info` | Waiting on the reporter for details | Maintainers / automation |
| `status/needs-repro` | Cannot be investigated without a reproduction | Maintainers / automation |
| `status/needs-design` | Real need, but the approach must be agreed before code | Maintainers |
| `status/accepted` | Confirmed and on our radar | Maintainers only |
| `status/help-wanted` | Accepted and explicitly open to outside contributors | Maintainers only |

`kind/*` says what the issue is (bug, feature, docs, chore, ...) and
`priority/p0`–`priority/p3` says how urgent it is.

When you file an issue, automation may apply labels that indicate missing
information. Here is what to expect.

### `status/needs-repro`

Applied when the issue cannot be investigated without a minimal reproduction.
Automation will leave a request comment explaining what is needed. Please reply
within 14 days — the 14-day window starts from that comment, not from when
the label was applied.

### `status/needs-info`

Applied when additional details are required. Automation will leave a request
comment explaining what information is needed. Please reply within 14 days of
that comment.

### What happens next

- **You reply or open a PR that addresses the question**: automation removes
  the label and the stale-close path is canceled. You can always respond even
  after the 14 days have passed.
- **14 days pass with no response**: the issue is closed as "not planned"
  with a comment that references the original request. Replying to the closed
  issue reopens the conversation; include the requested details so triage can
  continue.

Both labels are removed automatically when the original reporter comments on
the issue or pushes a synchronizing commit to a linked pull request.

## Pull Request Pipeline Labels

Maintainers run an automated review-and-merge pipeline. These labels are
applied by maintainers and by that automation; contributors should not add or
remove them.

| Label | Meaning |
|---|---|
| `status/needs-review` | Review requested |
| `status/needs-review-auto` | Review requested with auto approval |
| `status/reviewing` | Automated review is running |
| `status/review-failed` | Review workflow failed before merge-ready |
| `status/merge-ready` | Review passed; ready for the merge queue |
| `status/merge-queued` | Queued for the deterministic merge |
| `status/merge-failed` | Merge queue needs operator attention |
| `status/human-review-required` | Opt-out from auto-merge; waits for a co-maintainer |
| `needs-architectural-review` | Permanent hold pending architectural review; blocks auto-merge |
| `status/needs-bugflow` | Bugflow investigation requested on an issue |
| `needs-mac`, `needs-review-formulas` | Run optional CI lanes on this PR |

## Questions

Open an issue if you need clarification before a larger change.
