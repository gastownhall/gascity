package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func claudeWorkspaceTrustFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/claude-2.1.263-workspace-trust.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeWorkspaceTrustMenu(t *testing.T) {
	withZeroDialogTimings(t)
	fixture := claudeWorkspaceTrustFixture(t)
	for _, tc := range []struct {
		name, content string
		want          []string
		wantErr       bool
	}{
		{name: "current exit selected", content: fixture, want: []string{"Down", "Enter"}},
		{name: "current trust selected", content: strings.ReplaceAll(fixture, "❯ No, exit\n   Yes, I trust this folder", "  No, exit\n ❯ Yes, I trust this folder"), want: []string{"Enter"}},
		{name: "legacy trust first", content: strings.ReplaceAll(fixture, "❯ No, exit\n   Yes, I trust this folder", "❯ 1. Yes, I trust this folder\n   2. No, exit"), want: []string{"Enter"}},
		{name: "legacy exit selected", content: strings.ReplaceAll(fixture, "❯ No, exit\n   Yes, I trust this folder", "  1. Yes, I trust this folder\n ❯ 2. No, exit"), want: []string{"Up", "Enter"}},
		{name: "unknown option", content: strings.ReplaceAll(fixture, "Yes, I trust this folder", "Yes, trust all folders"), wantErr: true},
		{name: "extra option", content: strings.ReplaceAll(fixture, "Yes, I trust this folder", "Yes, I trust this folder\n   Trust parent folder"), wantErr: true},
		{name: "unselected", content: strings.ReplaceAll(fixture, "❯", " "), wantErr: true},
		{name: "truncated", content: strings.Split(fixture, "Enter to confirm")[0], wantErr: true},
		{name: "stale scrollback", content: fixture + "\n❯ Current user prompt\n", wantErr: true},
	} {
		for _, streaming := range []bool{false, true} {
			name := tc.name + "/poll"
			if streaming {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				var sent []string
				send := func(keys ...string) error { sent = append(sent, keys...); return nil }
				var err error
				if streaming {
					stream := &replayableSnapshotStream{update: make(chan struct{})}
					stream.publish(tc.content)
					stream.finish()
					_, err = acceptWorkspaceTrustDialogFromStream(context.Background(), time.Second, newReplayableSnapshotCursorFromStream(stream), send)
				} else {
					err = acceptWorkspaceTrustDialog(context.Background(), time.Second, func(int) (string, error) { return tc.content, nil }, send)
				}
				if (err != nil) != tc.wantErr {
					t.Fatalf("error=%v, wantErr=%v", err, tc.wantErr)
				}
				if tc.wantErr && !errors.Is(err, ErrUnrecognizedWorkspaceTrust) {
					t.Fatalf("error=%v, want launch-blocking trust error", err)
				}
				if !reflect.DeepEqual(sent, tc.want) {
					t.Fatalf("keys=%v, want %v", sent, tc.want)
				}
			})
		}
	}
}
