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
gc mail archive <id>                   # PERMANENTLY DELETES the message bead — not reversible
gc mail mark-read <id>                 # Mark as read without displaying
gc mail mark-unread <id>              # Mark as unread
gc mail delete <id>                    # Alias for archive — also PERMANENTLY DELETES, despite
                                        # its own --help text saying it works "by closing the
                                        # beads": both archive and delete call the store's Delete,
                                        # not Close, on the message bead. A closed bead stays
                                        # readable; a deleted one does not.
gc mail check                          # Check for new mail (used in hooks)
```

`archive` and `delete` are the same operation and both are irrecoverable. If
you want a message gone from view but still readable later, close its bead
instead (e.g. `gc bd close <id>`) rather than archiving or deleting it.
