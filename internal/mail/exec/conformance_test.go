package exec //nolint:revive // internal package, always imported with alias

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

// statefulScript returns a shell script body that maintains message state
// in a temp directory. Each message is stored as a file with line-based
// format: id\nfrom\nto\nsubject\nbody\ntimestamp\nstatus\nthread_id\nreply_to
func statefulScript(stateDir string) string {
	return `#!/bin/sh
set -e
STATE="` + stateDir + `"
op="$1"
shift

# Initialize next_id if missing.
if [ ! -f "$STATE/next_id" ]; then
  echo 1 > "$STATE/next_id"
fi
mkdir -p "$STATE/messages"

case "$op" in
  ensure-running)
    ;; # no-op
  send)
    to="$1"
    # Read JSON from stdin, extract fields.
    input=$(cat)
    from=$(echo "$input" | sed 's/.*"from":"\([^"]*\)".*/\1/')
    # Extract subject — may be empty string.
    subject=""
    if echo "$input" | grep -q '"subject"'; then
      subject=$(echo "$input" | sed 's/.*"subject":"\([^"]*\)".*/\1/')
    fi
    body=""
    if echo "$input" | grep -q '"body"'; then
      body=$(echo "$input" | sed 's/.*"body":"\([^"]*\)".*/\1/')
    fi
    id=$(cat "$STATE/next_id")
    echo $((id + 1)) > "$STATE/next_id"
    msgid="msg-$id"
    ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    thread_id="thread-$id"
    printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$msgid" "$from" "$to" "$subject" "$body" "$ts" "open" "$thread_id" "" > "$STATE/messages/$msgid"
    printf '{"id":"%s","from":"%s","to":"%s","subject":"%s","body":"%s","created_at":"%s","thread_id":"%s"}\n' "$msgid" "$from" "$to" "$subject" "$body" "$ts" "$thread_id"
    ;;
  inbox|check)
    recipient="$1"
    result=""
    for f in "$STATE"/messages/*; do
      [ -f "$f" ] || continue
      status=$(sed -n '7p' "$f")
      [ "$status" = "open" ] || continue
      msg_to=$(sed -n '3p' "$f")
      [ "$msg_to" = "$recipient" ] || continue
      msgid=$(sed -n '1p' "$f")
      from=$(sed -n '2p' "$f")
      subject=$(sed -n '4p' "$f")
      body=$(sed -n '5p' "$f")
      ts=$(sed -n '6p' "$f")
      thread_id=$(sed -n '8p' "$f")
      reply_to=$(sed -n '9p' "$f")
      if [ -n "$result" ]; then
        result="$result,"
      fi
      result="${result}{\"id\":\"$msgid\",\"from\":\"$from\",\"to\":\"$msg_to\",\"subject\":\"$subject\",\"body\":\"$body\",\"created_at\":\"$ts\",\"thread_id\":\"$thread_id\",\"reply_to\":\"$reply_to\"}"
    done
    if [ -n "$result" ]; then
      printf '[%s]\n' "$result"
    fi
    ;;
  get)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    from=$(sed -n '2p' "$f")
    msg_to=$(sed -n '3p' "$f")
    subject=$(sed -n '4p' "$f")
    body=$(sed -n '5p' "$f")
    ts=$(sed -n '6p' "$f")
    status=$(sed -n '7p' "$f")
    thread_id=$(sed -n '8p' "$f")
    reply_to=$(sed -n '9p' "$f")
    read_flag="false"
    if [ "$status" = "read" ]; then
      read_flag="true"
    fi
    printf '{"id":"%s","from":"%s","to":"%s","subject":"%s","body":"%s","created_at":"%s","read":%s,"thread_id":"%s","reply_to":"%s"}\n' "$msgid" "$from" "$msg_to" "$subject" "$body" "$ts" "$read_flag" "$thread_id" "$reply_to"
    ;;
  read)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    from=$(sed -n '2p' "$f")
    msg_to=$(sed -n '3p' "$f")
    subject=$(sed -n '4p' "$f")
    body=$(sed -n '5p' "$f")
    ts=$(sed -n '6p' "$f")
    thread_id=$(sed -n '8p' "$f")
    reply_to=$(sed -n '9p' "$f")
    # Mark as read.
    sed '7s/.*/read/' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
    printf '{"id":"%s","from":"%s","to":"%s","subject":"%s","body":"%s","created_at":"%s","read":true,"thread_id":"%s","reply_to":"%s"}\n' "$msgid" "$from" "$msg_to" "$subject" "$body" "$ts" "$thread_id" "$reply_to"
    ;;
  mark-read)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    sed '7s/.*/read/' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
    ;;
  mark-unread)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    sed '7s/.*/open/' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
    ;;
  archive|delete)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    status=$(sed -n '7p' "$f")
    if [ "$status" = "archived" ]; then
      echo "already archived" >&2
      exit 1
    fi
    sed '7s/.*/archived/' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
    ;;
  reply)
    msgid="$1"
    f="$STATE/messages/$msgid"
    if [ ! -f "$f" ]; then
      echo "message \"$msgid\" not found" >&2
      exit 1
    fi
    orig_from=$(sed -n '2p' "$f")
    orig_thread=$(sed -n '8p' "$f")
    # Read JSON from stdin.
    input=$(cat)
    from=$(echo "$input" | sed 's/.*"from":"\([^"]*\)".*/\1/')
    subject=""
    if echo "$input" | grep -q '"subject"'; then
      subject=$(echo "$input" | sed 's/.*"subject":"\([^"]*\)".*/\1/')
    fi
    body=""
    if echo "$input" | grep -q '"body"'; then
      body=$(echo "$input" | sed 's/.*"body":"\([^"]*\)".*/\1/')
    fi
    id=$(cat "$STATE/next_id")
    echo $((id + 1)) > "$STATE/next_id"
    new_msgid="msg-$id"
    ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$new_msgid" "$from" "$orig_from" "$subject" "$body" "$ts" "open" "$orig_thread" "$msgid" > "$STATE/messages/$new_msgid"
    printf '{"id":"%s","from":"%s","to":"%s","subject":"%s","body":"%s","created_at":"%s","thread_id":"%s","reply_to":"%s"}\n' "$new_msgid" "$from" "$orig_from" "$subject" "$body" "$ts" "$orig_thread" "$msgid"
    ;;
  thread)
    id="$1"
    thread_id="$id"
    if [ -f "$STATE/messages/$id" ]; then
      thread_id=$(sed -n '8p' "$STATE/messages/$id")
    fi
    result=""
    for f in "$STATE"/messages/*; do
      [ -f "$f" ] || continue
      msg_thread=$(sed -n '8p' "$f")
      [ "$msg_thread" = "$thread_id" ] || continue
      msgid=$(sed -n '1p' "$f")
      from=$(sed -n '2p' "$f")
      msg_to=$(sed -n '3p' "$f")
      subject=$(sed -n '4p' "$f")
      body=$(sed -n '5p' "$f")
      ts=$(sed -n '6p' "$f")
      reply_to=$(sed -n '9p' "$f")
      if [ -n "$result" ]; then
        result="$result,"
      fi
      result="${result}{\"id\":\"$msgid\",\"from\":\"$from\",\"to\":\"$msg_to\",\"subject\":\"$subject\",\"body\":\"$body\",\"created_at\":\"$ts\",\"thread_id\":\"$thread_id\",\"reply_to\":\"$reply_to\"}"
    done
    if [ -n "$result" ]; then
      printf '[%s]\n' "$result"
    fi
    ;;
  count)
    recipient="$1"
    total=0
    unread=0
    for f in "$STATE"/messages/*; do
      [ -f "$f" ] || continue
      status=$(sed -n '7p' "$f")
      [ "$status" = "archived" ] && continue
      msg_to=$(sed -n '3p' "$f")
      [ "$msg_to" = "$recipient" ] || continue
      total=$((total + 1))
      if [ "$status" = "open" ]; then
        unread=$((unread + 1))
      fi
    done
    printf '{"total":%d,"unread":%d}\n' "$total" "$unread"
    ;;
  *)
    exit 2 ;; # unknown operation
esac
`
}

func TestExecConformance(t *testing.T) {
	mailtest.RunProviderTests(t, func(t *testing.T) mail.Provider {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		if err := os.MkdirAll(filepath.Join(stateDir, "messages"), 0o755); err != nil {
			t.Fatal(err)
		}

		scriptPath := filepath.Join(dir, "mail-provider")
		if err := os.WriteFile(scriptPath, []byte(statefulScript(stateDir)), 0o755); err != nil {
			t.Fatal(err)
		}

		return NewProvider(scriptPath)
	})
}
