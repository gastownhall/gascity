package sessionlog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Kills an owner check that trusts a Codex rollout's filename over its
// session_meta, accepts a blank key, or reads an unreadable file as owned.
func TestTurnTranscriptOwnedBy(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const key = "019a0000-0000-7000-8000-000000000000"
	own := write("own.jsonl", `{"type":"session_meta","payload":{"id":"`+key+`","cwd":"/w"}}`+"\n")
	copied := write("copied.jsonl", `{"type":"session_meta","payload":{"id":"019a9999-0000-7000-8000-000000000000","cwd":"/w"}}`+"\n")
	noMeta := write("nometa.jsonl", `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n")
	torn := write("torn.jsonl", `{"type":"session_me`)
	cases := []struct {
		name, provider, path, key string
		want                      bool
		wantErr                   bool
	}{
		{"codex rollout of the key", "codex", own, key, true, false},
		{"codex rollout naming another conversation", "codex", copied, key, false, false},
		{"codex rollout without session_meta", "codex", noMeta, key, false, false},
		{"codex rollout torn in its first record", "codex", torn, key, false, false},
		{"codex rollout gone", "codex", filepath.Join(dir, "gone.jsonl"), key, false, true},
		{"blank key", "codex", own, " ", false, false},
		{"claude: the key built the path", "claude", filepath.Join(dir, key+".jsonl"), key, true, false},
		{"claude, blank key", "claude", own, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TurnTranscriptOwnedBy(tc.provider, tc.path, tc.key)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("TurnTranscriptOwnedBy = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("error %v does not wrap the read failure", err)
			}
		})
	}
}
