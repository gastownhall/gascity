# Structured-transcript golden corpus

Real, captured provider transcripts used as golden inputs for the `WC-STRUCT-*`
worker-conformance family (`internal/worker/workertest/structured_conformance.go`).

`TestStructuredConformance` validates the requirements against synthetic
fixtures. This corpus complements that with **real** provider output, which is
the only thing that catches provider format drift. `TestStructuredCorpusConformance`
runs the same requirements over every capture here; when the corpus is empty it
emits a NOTICE and skips, so populating it lights up validation with no code
change.

A static capture only proves the format that was current when it was taken, so
**re-capture periodically** (and on provider/CLI upgrades): a stale corpus stops
catching drift. Replace or add captures rather than letting them age silently.

## Layout

```
testdata/corpus/<provider>/<name>.jsonl
```

`<provider>` is the worker provider family (`claude`, `codex`, `gemini`, ...).
Each `*.jsonl` is one provider-native transcript that exercises tool calls
(ideally including an edit with result-side patch evidence, so `WC-STRUCT-003`
applies).

## Capturing a transcript

Run a real session through the credential broker (never with a raw key), then
copy the native transcript it writes. On a maintainer host:

```bash
# Claude — writes ~/.claude/projects/<slug>/<session>.jsonl
/data/projects/maintainer-city/scripts/manifold-claude \
  -p 'Create README-sample.txt with one line, then change that line with Edit.'

# Codex — writes $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl
/data/projects/maintainer-city/scripts/manifold-codex \
  exec --skip-git-repo-check -C <throwaway-dir> 'Edit one line in note.txt.'
```

On broker hosts `CODEX_HOME` is **shared across the fleet**, so its `sessions/`
tree mixes your capture with many real internal sessions. Identify your rollout
by the `session id` the run prints (and by its `session_meta.cwd`), and never
grab an arbitrary rollout. Codex applies edits through `apply_patch`; only a
`patch_apply_end` event carries a result-side diff (so `WC-STRUCT-003` applies),
whereas plain shell edits normalize as command results.

(`gasworks-launch` is the per-session-proxy alternative; CI uses an
Ollama-backed Anthropic endpoint — see the `gascity-real-provider-test-creds`
note.)

## Sanitization (required before committing)

Captures are committed to a public branch, so before adding one:

- Use a throwaway workdir and a benign prompt; keep the transcript short.
- Remove host paths, usernames, tokens, and any private code or content.
- Keep only the structural frames needed to exercise the requirements
  (user prompt, assistant `tool_use`, `tool_result` with its result-side
  fields). The `WC-STRUCT-002` gate already asserts no provider-native key
  reaches the neutral wire, but raw frames are preserved verbatim, so the raw
  bytes themselves must be clean.
