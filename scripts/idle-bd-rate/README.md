# Idle bd call-rate measurement

Proves the idle-city bound for ticket **04** / upstream [#2463](https://github.com/gastownhall/gascity/issues/2463) / [#4133](https://github.com/gastownhall/gascity/issues/4133).

## What broke

Event-triggered orders (`nudge-on-route` on `bead.updated`,
`cascade-nudge-on-blocker-close` on `bead.closed`) counted lifecycle events from
their own **order-tracking** beads. Each fire created/updated tracking beads →
more matching events → another fire → tens of `bd`/`gc` procs per second on a
fresh idle city, Dolt pegged, and `gc status` session-snapshot timeouts.

## Production fix

`internal/orders.CheckTrigger` / `checkEvent` skips payloads labeled
`order-tracking` (flat bead JSON and wrapped `{"bead":…}`). Regression tests in
`internal/orders/triggers_test.go` lock the seam.

## Measure an idle city

```bash
# Fresh throwaway city, no user work
city="$HOME/tmp/idle-bd-rate-city"
rm -rf "$city"
gc init --template minimal --yes "$city"   # adjust flags for your build
export GC_BD_TRACE="$city/bd-trace.log"
gc start --city "$city"
# let it idle 2–5 minutes
sleep 180
gc stop --city "$city"

python3 scripts/idle-bd-rate/aggregate.py --max-per-sec 10 "$GC_BD_TRACE"
```

`--max-per-sec 10` is a conservative laptop-safe bound: catastrophic storms were
~50–70 procs/s (#4133) or ~7+ `bd`/s sustaining ~463 Dolt queries/s (#2463).
A healthy idle city after the filter should sit far below that; raise or lower
the threshold once you have a local baseline.

Unit self-check (no city required):

```bash
python3 scripts/idle-bd-rate/aggregate.py --self-test
```
