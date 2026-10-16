# Session transcript reconstruction

Claude parallel tool results belong to their selected parent invocation. Retain
those result-only attachments without importing text or results from discarded
conversation branches. A result attachment must not displace a later tool
invocation of the same response as the active tip; a tool invocation from
another response (a different `message.id`) forks the conversation instead.

Claude Code splits one API response into rows that share `message.id`, so a
sibling row of a selected response belongs to the selected conversation. Drop
such a row when none of its tool calls has a result and the selected branch has
moved past its response: a later user prompt or an assistant row of another
response. Until then its calls may still be running and stay open.

# Turn identity

`ScanTurns` and `TurnFollower` read the ids each provider records for its own
turns. An id is compared only for equality and is never read from delivered
text. A read that cannot tell the current turn is an error, never an idle scan:
a torn last record, or a truncated window holding no turn record or, in a
Gemini JSONL file, no snapshot. `TurnScan.Open` means the transcript records
the turn's start and no end; whether the agent is still running is the caller's
call.
