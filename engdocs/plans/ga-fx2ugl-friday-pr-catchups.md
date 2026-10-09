# Friday PR catch-up merges

PM root: **ga-fx2ugl**. Source: mayor mails **gm-wisp-cjj244l**,
**gm-wisp-nb4yrng**, and **gm-wisp-7j8mbjk**.

Prepare two existing PRs for the **2026-10-09 10:00Z** MPR reset
(Friday 03:00 PDT). The mayor expressly authorized Codex routing for these
catch-ups before reset. Both remain on the outage deny list until reset;
changing that list or merging a PR belongs to the existing MPR/operator path.

## Verified baseline and scopes

PM fetched main at `1cfa66847f7def9d45569fd02c5d657545e92a43` and both
live PR heads. GitHub still reports their original heads; its mergeability
response was `UNKNOWN`. Local `git merge-tree --write-tree --name-only`
independently identified exactly these conflicts, without editing source.

| PR | Catch-up bead | Priority | Pinned head and lease | Conflict |
| --- | --- | --- | --- | --- |
| #7334 | **ga-5tqdjw** | P1 | `2a8c4c7322eb84fbcf6c94f7423272471cfdea16` | `internal/runtime/proctable/scan_linux_test.go` |
| #3842 | **ga-m3rfm0** | P2 | `f41c95e78bd04375c92ea8342ac92f6f4de1e088` | `cmd/gc/BUILD.bazel` |

Both are independent `ready-to-build` scopes routed to **gascity/codex**.
Their complete acceptance criteria and Build instructions live in their notes.
Priority expresses order; neither catch-up blocks the other's implementation.
Existing tests supply verification, so no duplicate test-first or build bead
is required. Two bounded scopes meet the planning workflow's 1–10-child exit
criterion; no additional architecture, UX, or feature work is introduced.

For **#7334**, start from its pinned head and merge fresh main. Keep both
sides' tests exactly once and preserve the settled **ga-f0tblg** parent-read
contract and upstream #7365 sweep behavior. Production `scan_linux.go`
auto-merges. Verify the automatic merge as well as the manual test resolution,
run `go test ./internal/runtime/proctable/...`, and record actual affected test
results. The child retains `gc.fixes_tracker=ga-q17kpi` because its head carries
the existing orphan-selection fix. Tracker closure requires main landing.

For **#3842**, start from its pinned head and merge fresh main. Regenerate
`BUILD.bazel` from the merged source with the repository's Bazel sync/Gazelle
workflow, preserving both sides' sources and dependencies. Never choose one
side of generated data. Verify generation leaves no drift, build `cmd/gc`, and
run the affected continuation, claim, and turn-bound tests. Its old June
changes-requested review is not additional catch-up scope.

## Lineage and publication

- **ga-5tqdjw** carries `discovered-from` edges to this PM root and the
  existing mayor-owned conflict audit **ga-ap88oi**. Do not file another tracker.
- **ga-m3rfm0** traces to this root and prior deploy **ga-3oog6w**. No current
  open #3842 conflict audit was found; add its real edge if one appears later.
- Each worker creates an owned isolated branch from the pinned PR head and
  **merges main without rebasing**. Prove that both the original cleared head
  and the chosen merged-main SHA are ancestors of the resulting head.
- Each resolution receives a fresh quick review, followed by a fresh deployer
  gate for the resulting SHA. Existing clearances **55861287894** (#7334) and
  **55890099459** (#3842) remain evidence for their original heads only.
- The **mayor alone** lease-pushes the gated new head to
  `quad341:deploy/ga-tngr86-gate` (#7334) or
  `gastownhall/gascity:work/ga-zdunsh-hook-claim-continuation-nudge` (#3842),
  using the full pinned leases above, and verifies remote head identity.
  The deployer posts clearance for the newly published SHA. Mayor closes
  **ga-ap88oi** after verified gated-head publication, not at build completion.

Re-read live PR and bead phase before mutation. If a head or lease moved,
reconcile with the mayor rather than overriding it. PM writes requirements
and this plan; workers resolve and test code. Neither PM nor Codex pushes an
original PR branch, opens a replacement PR, merges it, or reuses stale clearance.
No installed external tracker skill was found; tracker import is a no-op.
