# Federated scale_check follow-ups (ga-olozsj)

## Goal and current state

Round-two review of the federated `scale_check` change passed at commit
`fc68d3d85d` (`ga-n7w5kw`, build bead `ga-i7b6sl`). The reviewer recorded three
minor follow-ups: a misleading partial-result message, repeated rig environment
resolution within one desired-state pass, and an untested command-prefix
contract. The reviewed change is awaiting deployment under `ga-c6p9zc`.

Operators should see the count that was actually used when one store fails.
The remaining work protects the cost and test contract of the same fan-out
without delaying the reviewed fix.

## Work packages

| Order | Bead | Route | Acceptance focus |
| --- | --- | --- | --- |
| 1 | `ga-q3p7ot` (P2) | Builder, `ready-to-build` | Partial-result diagnostics state the count actually applied; tests cover mixed success and all-failure cases. |
| 2 | `ga-a9begz` (P3) | Validator, `needs-tests` | A recording-runner test checks each probe's own Dolt command prefix and fails if that prefix is removed. |
| 3 | `ga-mnr4ju` (P3) | Builder, `ready-to-build` | Multiple pools resolve each active rig environment at most once per desired-state pass, with fresh data on the next pass and unchanged store isolation. |

Each bead has a `discovered-from` edge to `ga-olozsj` and a `blocks` dependency
on deploy bead `ga-c6p9zc`. The three follow-ups are independent after that
deployment lands. The child notes carry the full measurable acceptance criteria.

## Coordination and risks

- `ga-3rv7ov` is an older attempt at the same store-isolation fix. Mayor parked
  it under `hold:mayor`, removed its builder intake label, and linked it to
  `ga-c6p9zc`. Its named exit is closure as superseded after the replacement
  lands. No duplicate builder work is needed.
- The original fix is under deploy review. A closed build or review bead alone
  does not establish that the change landed; the follow-up dependencies point
  to the deploy bead.
- The reviewer classified all three findings as nonblocking. Prioritize the
  operator-visible diagnostic first; the test contract and resolution cost
  follow as smaller maintenance work.

No external tracker skill was installed for this PM pass, so there was no
tracker import or tracker-sync manifest change.
