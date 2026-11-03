# Provider hooks

Managed commands must honor the launching binary through quoted
`"${GC_BIN:-gc}"`; append fallback install directories to the existing PATH.
The controller and its hook children must not silently use different builds.

Upgrade only exact known managed command shapes. Preserve user wrappers,
additional arguments, chained commands and custom hook events. Keep upgrades
idempotent and avoid duplicate hook execution. Legacy regression fixtures must
retain actual old bytes instead of deriving old commands from today's template.

## Native question observation

Record native session, prompt and tool-invocation IDs before displaying a managed
Claude question. Fence observation writes and cleanup to the launching pane's
incarnation; each invocation owns its own option. A late callback may clear only
its own invocation. Keep projections bounded and disposable: native transcripts
and terminal state remain authoritative. Refuse stale, completed or ambiguous
bindings and retain the ordinary-message accidental-answer guard. Qualification
must include real request-bound choice/free-text replies through the supervisor.

Codex reconciliation must reject managed-command or wrapper metadata it cannot
migrate without changing its scope. Preserve both live and configured inputs on
refusal; report the input, event and fields. User-only hooks and unrelated
metadata remain untouched. Do not silently strip extra fields while rebuilding
managed commands.

Direct configured-home creation reconciles only at the actual started-create
boundary inside its identity lock. Deferred creation and runtime observation
stay read-only. Retain the canonical handle and created ID for retry and attach.

Prepare configured project hooks during rig registration, before setup review
and input pinning. Use composed config and canonical agent lookup; classify
homes without creating unrelated directories. A shared document must be a fixed
point for every selected profile. Refuse conflicts before publication, preserve
operator inputs, and keep install_agent_hooks settings unchanged.
