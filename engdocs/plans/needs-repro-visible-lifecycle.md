# Plan: visible needs-repro and needs-info lifecycle

Root bead: `ga-brv0kv`
Source issue: https://github.com/gastownhall/gascity/issues/3142
Reference incident: https://github.com/gastownhall/gascity/issues/2201

## Problem

Automation can leave an issue with `status/needs-repro` or
`status/needs-info` without making a visible request to the reporter. The
stale-close workflow then closes the issue after 14 days and says the issue was
closed because requested details were not provided. From the contributor's
point of view, the request was silent and the close feels arbitrary.

This is a contributor-trust problem, not just a labeling problem. The desired
outcome is that the reporter always sees what is needed, how long they have to
respond, and how to continue after a stale close.

## Scope

- Make automated `status/needs-repro` and `status/needs-info` labeling visible
  to the reporter with one clear request comment.
- Make stale closure depend on that visible request and the full response
  window.
- Preserve author-response behavior: an author reply or PR update clears the
  relevant label and prevents stale close.
- Document the lifecycle in contributor-facing language.

## Non-Goals

- Do not redesign issue triage strategy.
- Do not decide implementation structure here; builder owns the implementation
  path.
- Do not require automation to solve every repro itself in this slice. The
  minimum product contract is visible, respectful information gathering.

## Child Beads

| Bead | Route | Purpose | Depends On |
| --- | --- | --- | --- |
| `ga-brv0kv.1` | `gascity/validator` | Add regression coverage for needs-repro and needs-info request/close behavior. | none |
| `ga-brv0kv.2` | `gascity/builder` | Notify reporters when automation requests repro or info. | `ga-brv0kv.1` |
| `ga-brv0kv.3` | `gascity/builder` | Make stale closure depend on a visible request and the full 14-day window. | `ga-brv0kv.2` |
| `ga-brv0kv.4` | `gascity/builder` | Document the contributor-facing lifecycle. | `ga-brv0kv.2`, `ga-brv0kv.3` |

## Acceptance Summary

- A reporter never has to infer that more information is needed from a label
  mutation alone.
- A stale-close comment never claims details were requested unless a visible
  request exists after the latest relevant label event.
- Repeated automation runs are idempotent and do not spam duplicate request
  comments.
- Author replies or PR updates keep clearing `status/needs-info` and
  `status/needs-repro`.
- Contributor docs match the bot request and stale-close wording.

## Handoff Notes

Validator should make the current silent-label path fail first. Builder should
treat those tests and the bead acceptance criteria as the product contract.
Docs should land after the shipped behavior is known so wording stays accurate.
