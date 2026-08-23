---
name: gc-mail
description: Sending and reading messages between agents
---

# Messaging (Mail)

Mail is bead-based messaging between agents. Messages are beads with
type=message, stored in the bead store.

## Sending

```
gc mail send <to> -m "message body"                    # Send a message
gc mail send <to> -s "Subject" -m "message body"       # Send with subject
gc mail reply <id> -m "reply body"                     # Reply to a message
gc mail reply <id> -s "Re: topic" -m "reply body"      # Reply with subject
```

For repeating notifications (a patrol or cooldown order that re-detects the
same condition every run), add `--dedup <key>`: the send is suppressed (exit
0) while a previous message with the same key to the same recipient is still
live, so the recipient gets at most one copy per stream instead of one per
interval. Once they archive it, the stream may alert again.

```
gc mail send mayor -s "disk warning" -m "..." --dedup "disk-warn:hq"
```

## Reading

```
gc mail inbox                          # List unread messages
gc mail count                          # Count unread messages
gc mail peek <id>                      # Preview a message without marking read
gc mail read <id>                      # Read a message (marks as read)
gc mail thread <id>                    # Show full conversation thread
```

## Managing

```
gc mail archive <id>                   # IRRECOVERABLE: deletes the underlying bead, despite the name
gc mail mark-read <id>                 # Mark as read without displaying
gc mail mark-unread <id>              # Mark as unread
gc mail delete <id>                    # IRRECOVERABLE: alias for archive; deletes the underlying bead
gc mail check                          # Check for new mail (used in hooks)
```

`archive` and `delete` are the same operation under two names — both delete
the message's underlying bead outright; neither files it away for later
reading. There is no reversible "put this away" path. Prefer `mark-read` when
you want a message out of the unread count without destroying it.
