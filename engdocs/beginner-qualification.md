# Installed beginner qualification

Run `scripts/qualify-beginner` on a disposable, authenticated Linux or macOS
host with the documented Gas City prerequisites. Use the exact candidate
binary and its companion from the same build. The selected provider must
already authenticate through its normal environment.

```bash
scripts/qualify-beginner \
  --gc /absolute/candidate/gc \
  --expected-commit "$(git rev-parse HEAD)" \
  --work-dir /absolute/new-qualification-directory \
  --port 18503 --provider claude
```

Choose an unused loopback port and a new work directory. The runner refuses
existing data, missing binaries, occupied ports and instruction drift. It
isolates GC_HOME, verifies the running build, and stops only its owned profile.
It retains the city, repository and evidence for diagnosis. Provider credentials
remain caller-managed and are not copied into artifacts.

The runner follows the published beginner choice, immediate greeting retry,
dependent API/client tasks, city pause/resume, actual tests/client output,
manual order and a five-minute idle observation. Each lesson has a fixed
20-minute deadline. Native review, verification, integration and cleanup must
agree on the exact commit before source closure. No approvals or completion
metadata are manufactured.

A failure is a failed run even if the retained workflow later finishes.
Continuation observations are separate evidence. Collect evidence before
discarding the disposable host. Native usage snapshots report recorded tokens
and local cost estimates; they are not account invoices.

## Automation and ownership

The normal scripts Bazel suite checks runner safety, published instruction
compatibility and release dependency wiring without model credentials. This
keeps pull-request checks usable by external contributors.

`.github/workflows/beginner-qualification.yml` runs the native installed
journey in Nightly, RC Gate and both publication workflows. Publication checks
out the immutable commit returned by the prerequisite qualification job.
The job fails closed on missing provider configuration, workflow failure or
deadline expiry and retains evidence on failure. It uses the existing Ollama
Claude-compatible CI credentials and model variables; this is explicitly
different from qualifying the commercial Claude/Codex release matrix.

The existing CODEOWNERS owner for `/scripts/`, `/.github/workflows/` and
`/.github/actions/` is `@gastownhall/gascity-admin`. A branch author owns
triage and reproduction of a red candidate before requesting release review.
Link the failed run, exact build/dependency/provider identities and retained
evidence when escalating. Fix the failure; do not raise deadlines or bypass
the prerequisite to publish.

## Qualification scope

The automated native CI cell is Linux with Claude's CLI and the configured
Ollama backend. The runner also supports native Claude and Codex on Linux and
macOS. Each advertised release cell still requires an actual recorded run;
the CI cell is not evidence for an unexecuted provider or platform.

The primary journey is separate from adversarial review exhaustion, revoked
authentication, stale integration, parked continuation and migration cases.
Those cases retain their own exact-version receipts and failures. Passing
this runner alone does not establish the entire RFC acceptance matrix.
