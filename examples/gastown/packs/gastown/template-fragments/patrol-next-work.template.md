{{ define "patrol-next-work" }}
## Finding the Next Work Bead (Race-Free)

The event-watch loop has a window: if a bead is routed between the list
check and the `gc events --watch` call, the assignment event arrives before
the subscription starts and is silently missed. The refinery then idles for
up to `event_timeout` seconds before the next poll.

Fix: capture `SEQ` **before** the list check, not after. Any event that
fires during the list→watch gap is already within the subscribed range.

```bash
SEQ=$(gc events --seq)
WORK=$(gc bd list --assignee=$GC_AGENT --status=open \
  --exclude-type=epic --limit=1 --json | jq -r '.[0].id // empty')

if [ -n "$WORK" ]; then
  # Work was already assigned (possibly in the SEQ-to-list gap).
  # Proceed to next step.
  :
else
  gc events --watch --type=bead.updated \
    --after=$SEQ --timeout ${EVENT_TIMEOUT:-30}s
  # On event or timeout: re-run the list check.
fi
```

**Why the order matters:**

```
timeline ──────────────────────────────────────────────────────►
  [wrong order]   list-check ─── SEQ-capture ─── watch-start
                                  ↑ polecat routes bead here → MISSED

  [correct order] SEQ-capture ─── list-check ─── watch-start
                                   ↑ polecat routes bead here → CAUGHT
                                     (event falls within the subscribed range)
```

**Belt-and-suspenders:** the polecat's `submit-and-exit` step sends
`gc session nudge` to the refinery immediately after reassigning the bead.
This wakes the refinery from its event-watch before the next event loop
even fires.
{{ end }}
