---
name: gc-mail
description: Sending and reading messages between agents
---

# Messaging (Mail)

Mail is bead-based messaging between agents. Messages are beads with
type=message, stored in the bead store.

## Sending

```
{{binary}} mail send <to> -m "message body"                    # Send a message
{{binary}} mail send <to> -s "Subject" -m "message body"       # Send with subject
{{binary}} mail reply <id> -m "reply body"                     # Reply to a message
{{binary}} mail reply <id> -s "Re: topic" -m "reply body"      # Reply with subject
```

## Reading

```
{{binary}} mail inbox                          # List unread messages
{{binary}} mail count                          # Count unread messages
{{binary}} mail peek <id>                      # Preview a message without marking read
{{binary}} mail read <id>                      # Read a message (marks as read)
{{binary}} mail thread <id>                    # Show full conversation thread
```

## Managing

```
{{binary}} mail archive <id>                   # Archive a message
{{binary}} mail mark-read <id>                 # Mark as read without displaying
{{binary}} mail mark-unread <id>              # Mark as unread
{{binary}} mail delete <id>                    # Delete a message
{{binary}} mail check                          # Check for new mail (used in hooks)
```
