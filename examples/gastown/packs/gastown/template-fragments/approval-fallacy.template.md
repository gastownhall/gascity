{{ define "approval-fallacy-crew" }}
## No Approval Step

When work is done, finish the cycle. Do not summarize and wait for permission.

- Commit and push your work.
- Continue with the next task, or send handoff context and exit:
  `gc mail send -s "HANDOFF: <brief>" -m "<context>" && gc runtime drain-ack && exit`
- Do not ask "should I commit this?"
- Do not sit idle after finishing.
{{ end }}

{{ define "approval-fallacy-polecat" }}
## No Idle Polecats

When implementation and checks are done, run the done sequence immediately.
There is no approval wait. An idle polecat blocks the refinery and wastes the
pool slot.

```bash
git push origin HEAD
gc bd update <work-bead> \
  --set-metadata branch=$(git branch --show-current) \
  --set-metadata target={{ .DefaultBranch }} \
  --notes "Implemented: <brief summary>"
REFINERY_TARGET="${GC_RIG:+$GC_RIG/}{{ .BindingPrefix }}refinery"
gc bd update <work-bead> --status=open --assignee="$REFINERY_TARGET" --set-metadata gc.routed_to=""
gc runtime drain-ack
exit
```

This pushes your branch, sets metadata so the Refinery knows what to merge,
reassigns the work bead to the Refinery, and signals the reconciler to kill
this session. `gc runtime drain-ack` makes the shutdown immediate. Polecats
do not push to main, close beads, create MR beads, or wait around.

If work appears already merged, still reassign it to the Refinery with a note.
Only the Refinery verifies patch identity and closes beads.
{{ end }}
