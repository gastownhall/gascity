package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func writeRemoteScript(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ops.log")
	script := filepath.Join(dir, "remote-provider")
	source := "#!/bin/sh\nset -eu\nop=$1\nname=${2:-}\nprintf '%s %s\\n' \"$op\" \"$name\" >> " + shellQuoteForTest(logPath) + "\ncase \"$op\" in\n" + body + "\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestRemoteCreateUsesTypedRPPEnvelope(t *testing.T) {
	script, logPath := writeRemoteScript(t, `
  protocol) printf '%s' '{"version":0,"capabilities":["remote.create","remote.status"]}' ;;
  remote-create)
    payload=$(cat)
    printf '%s' "$payload" > "${0}.payload"
    printf '%s' '{"ok":true,"result":{"ref":{"session_id":"opaque-session","run_id":"opaque-run"},"phase":"queued","updated_at":"2026-09-06T07:00:00Z"}}'
    ;;
`)
	p := NewProvider(script)

	got, err := p.RemoteCreate(context.Background(), "worker-1", runtime.RemoteCreateRequest{
		RequestID: "request-1",
		Fence:     runtime.RemoteOwnershipFence{Token: "owner-generation-1"},
		Prompt:    runtime.TextContent("implement the bead"),
		Source:    runtime.RemoteSource{Repository: "https://github.com/acme/repo", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("RemoteCreate: %v", err)
	}
	if got.Ref.SessionID != "opaque-session" || got.Ref.RunID != "opaque-run" || got.Phase != runtime.RemoteSessionQueued {
		t.Fatalf("RemoteCreate result = %+v", got)
	}
	payload, err := os.ReadFile(script + ".payload")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"request_id":"request-1"`, `"token":"owner-generation-1"`, `"repository":"https://github.com/acme/repo"`, `"text":"implement the bead"`} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("payload %s missing %s", payload, want)
		}
	}
	logData, _ := os.ReadFile(logPath)
	if got := string(logData); !strings.Contains(got, "protocol ") || !strings.Contains(got, "remote-create worker-1") {
		t.Fatalf("operation log = %q", got)
	}
}

func TestRemoteUnsupportedFailsClosedWithoutCallingOperation(t *testing.T) {
	script, logPath := writeRemoteScript(t, `
  protocol) printf '%s' '{"version":0,"capabilities":["remote.status"]}' ;;
`)
	p := NewProvider(script)
	_, err := p.RemoteFollowUp(context.Background(), "worker-1", runtime.RemoteFollowUpRequest{
		RequestID: "request-2",
		Ref:       runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Fence:     runtime.RemoteOwnershipFence{Token: "owner-generation-1"},
		Content:   runtime.TextContent("continue"),
	})
	if !errors.Is(err, runtime.ErrRemoteCapabilityUnsupported) {
		t.Fatalf("RemoteFollowUp error = %v, want ErrRemoteCapabilityUnsupported", err)
	}
	logData, _ := os.ReadFile(logPath)
	if strings.Contains(string(logData), "remote-follow-up") {
		t.Fatalf("unsupported operation was invoked: %s", logData)
	}
}

func TestRemoteProviderErrorIsClassifiedAndRedacted(t *testing.T) {
	const secret = "sk-test-NOT-A-REAL-CREDENTIAL-remote"
	t.Setenv("REMOTE_PROVIDER_TOKEN", secret)
	script, _ := writeRemoteScript(t, `
  protocol) printf '%s' '{"version":0,"capabilities":["remote.status"]}' ;;
  remote-status) printf '%s' '{"ok":false,"error":{"kind":"auth","message":"token `+secret+` rejected","retryable":false}}' ;;
`)
	p := NewProvider(script)
	_, err := p.RemoteStatus(context.Background(), "worker-1", runtime.RemoteSessionRef{SessionID: "opaque-session"})
	var remoteErr *runtime.RemoteSessionError
	if !errors.As(err, &remoteErr) || remoteErr.Kind != runtime.RemoteFailureAuth {
		t.Fatalf("RemoteStatus error = %#v, want classified auth error", err)
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), runtime.RedactedValue) {
		t.Fatalf("RemoteStatus leaked credential: %v", err)
	}
}

func TestRemoteProviderRejectsUnknownErrorClassification(t *testing.T) {
	script, _ := writeRemoteScript(t, `
  protocol) printf '%s' '{"version":0,"capabilities":["remote.status"]}' ;;
  remote-status) printf '%s' '{"ok":false,"error":{"kind":"billing-limit","message":"provider prose"}}' ;;
`)
	p := NewProvider(script)
	_, err := p.RemoteStatus(context.Background(), "worker-1", runtime.RemoteSessionRef{SessionID: "opaque-session"})
	if err == nil || !strings.Contains(err.Error(), "invalid failure kind") {
		t.Fatalf("RemoteStatus error = %v, want invalid failure kind", err)
	}
}

func TestRemoteTranscriptRejectsProviderPageBeyondRequestedBound(t *testing.T) {
	script, _ := writeRemoteScript(t, `
  protocol) printf '%s' '{"version":0,"capabilities":["remote.transcript"]}' ;;
  remote-transcript) printf '%s' '{"ok":true,"result":{"events":[{"id":"1","kind":"text","text":"one"},{"id":"2","kind":"text","text":"two"}],"next_cursor":"2"}}' ;;
`)
	p := NewProvider(script)
	_, err := p.RemoteTranscript(context.Background(), "worker-1", runtime.RemoteTranscriptQuery{
		Ref:   runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Limit: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "returned 2 events for limit 1") {
		t.Fatalf("RemoteTranscript error = %v, want provider-bound violation", err)
	}
}
