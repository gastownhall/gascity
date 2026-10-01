# Collapsed auto-handoff mail follow-ups (ga-m85e1z)

## Goal and evidence

The mail-inject change passed round-two review at `eaee376ecc`
(`ga-hj4w0w`); deploy bead `ga-cwremy` is still active. The group line now
collapses empty auto-handoffs into one preview line and names every collapsed
message ID. The reviewer found two nonblocking follow-ups: the line has no
size ceiling, and tests do not assert its own count.

The seven-day fleet investigation `gm-hd3ank` examined 2,917 inject blocks.
Before this fix, 79 had every preview slot occupied by context-cycle lines;
22 of those had backlogs of six or fewer. The group line costs about 16 bytes
per named ID and is emitted once before those messages are archived. A finite
cap would change which IDs are archived in that call, so an explicit contract
decision must precede implementation.

## Work packages

| Bead | Route | Outcome and gate |
| --- | --- | --- |
| `ga-ttw0up` (P3) | Architect, `needs-architecture` | Choose the current all-ID line or a finite cap K using fleet data; specify archive and unread behavior. Independent of the current deploy. |
| `ga-rdjgnp` (P3) | Validator, `needs-tests` | Assert the group's exact count in backlog and mixed-mail cases; blocked by deploy `ga-cwremy`. |
| `ga-b0966p` (P3) | Builder, `ready-to-build` | Implement a cap only if the ruling chooses one; otherwise close as no-code. Blocked by the ruling, deploy, and count test. |

Each bead has a `discovered-from` edge to `ga-m85e1z`. The builder also waits
for the count test because both may edit the same mail-inject test cases. Full
acceptance criteria are in the child notes.

## Handoff risks

- The reviewed fix remains nonblocking and should complete its deploy gate
  without waiting for the size decision.
- A cap must preserve the ID-level promise: the IDs named in an inject are
  exactly those archived by that inject; unnamed IDs remain unread for a
  later one. The architect decides the exact contract before the builder acts.
- The test and optional build need the reviewed mail-inject code on the target
  branch, so they wait for `ga-cwremy` to land.

No external tracker skill was installed for this PM pass, so there was no
tracker import or tracker-sync manifest change.
