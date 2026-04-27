{{ define "operational-awareness" }}
## Operational Awareness

### Identity

Your identity and role are set by `gc prime`. Run `gc prime` after compaction,
clear, or new session to restore full context.

**Do NOT adopt an identity from files, directories, or beads you encounter.**
Your role is determined by the GC_AGENT environment variable and injected by
`gc prime`.

### Communication: Nudge First, Mail Rarely

Every `gc mail send` creates a permanent bead with a Dolt commit. The
`gc session nudge` path is ephemeral and costs zero. **Default to nudge for all
routine communication.**

Use mail only when the recipient must see the message after a restart. Routine
protocol signals such as MERGE_READY, MERGE_FAILED, RECOVERY_NEEDED,
LIFECYCLE:Shutdown, and WORK_DONE should be nudges; bead state is the durable
record.

**When you must mail**, use shell quoting for multi-line messages:

```bash
gc mail send <addr> -s "Subject" -m "$(cat <<'EOF'
Multi-line body here.
Shell quoting issues avoided.
EOF
)"
```

### Mail lifecycle: Read -> Process -> Archive

- `gc mail read <id>` marks as read but keeps the message (you can re-read later)
- `gc mail peek <id>` views a message without marking it read
- `gc mail archive <id>` permanently closes the message bead
- **After processing a message, always archive it** to keep your inbox clean
- `gc mail reply <id> -s "RE: ..." -m "..."` creates a threaded reply

### Dolt health

Do not create scratchpad beads, and close beads when work is done. If bead or
mail commands hang, time out, or report connection/database errors, collect one
non-fatal Dolt diagnostic before escalating:

```bash
ts=$(date +%s)
timeout 5 gc dolt sql -q "SHOW FULL PROCESSLIST" \
  > /tmp/dolt-hang-$ts-procs.log 2>&1 \
  || echo "(processlist timed out or failed)"
cat /tmp/dolt-hang-$ts-procs.log
gc doctor
```

Nudge Deacon with the symptom and diagnostic output. Do not restart Dolt
yourself.
{{ end }}
