# Gas City Mail Scripts

Exec mail provider scripts for Gas City's pluggable mail system
(`GC_MAIL=exec:<script>`).

## gc-mail-mcp-agent-mail

Bridges Gas City mail to [mcp_agent_mail](https://github.com/Dicklesworthstone/mcp_agent_mail)
— a standalone agent coordination tool with SQLite+Git storage, full-text
search, file reservations, and a web UI.

### Prerequisites

- A running mcp_agent_mail server (default: `http://127.0.0.1:8765`)
- `curl` and `jq` on PATH

### Quick start

```bash
# Start mcp_agent_mail server (separate terminal)
am  # or: uv run python -m mcp_agent_mail.http

# Start city with mcp_agent_mail as mail backend
GC_MAIL=exec:contrib/mail-scripts/gc-mail-mcp-agent-mail \
  gc start --foreground

# Send mail
gc mail send deacon -s "Patrol check" -m "Check patrol status"

# Check inbox
gc mail inbox mayor

# View in web UI
open http://127.0.0.1:8765/mail
```

### Environment variables

| Variable              | Default                 | Purpose                        |
| --------------------- | ----------------------- | ------------------------------ |
| `GC_MCP_MAIL_URL`     | `http://127.0.0.1:8765` | mcp_agent_mail server URL      |
| `GC_MCP_MAIL_TOKEN`   | (empty)                 | Bearer token for auth          |
| `GC_MCP_MAIL_PROJECT` | `$(pwd)`                | project_key for mcp_agent_mail |

### Operation mapping

| gc operation        | mcp_agent_mail tool                       | Notes                                         |
| ------------------- | ----------------------------------------- | --------------------------------------------- |
| `ensure-running`    | `GET /health/liveness` + `ensure_project` | Verify server, create project                 |
| `send <to>`         | `send_message`                            | Auto-registers agents; stdin includes subject |
| `inbox <recipient>` | `fetch_inbox`                             | Unread messages only (local filtering)        |
| `check <recipient>` | `fetch_inbox`                             | Same as inbox (no mark-read)                  |
| `get <id>`          | `fetch_inbox`                             | View without marking read                     |
| `read <id>`         | `acknowledge_message` + `fetch_inbox`     | Ack + return message                          |
| `mark-read <id>`    | `acknowledge_message`                     | Local + server ack                            |
| `mark-unread <id>`  | (local only)                              | Remove local read state                       |
| `reply <id>`        | `send_message`                            | Uses `in_reply_to_id`                         |
| `thread <id>`       | (not supported)                           | Returns `[]`                                  |
| `count <recipient>` | `fetch_inbox`                             | Returns `{"total":N,"unread":N}`              |
| `archive <id>`      | `acknowledge_message`                     | Ack only, no output                           |
| `delete <id>`       | `acknowledge_message`                     | Alias for archive                             |

### Testing

```bash
./gc-mail-mcp-agent-mail.test
```

Uses a mock curl — no running server required.
