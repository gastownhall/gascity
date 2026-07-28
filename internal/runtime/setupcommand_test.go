package runtime

import "testing"

// TestCommandOutputTail pins the bounded-tail capture RunSetupCommand folds
// into setup-command failure messages. Moved from the tmux package with the
// extraction of its runSetupCommand core.
func TestCommandOutputTail(t *testing.T) {
	cases := []struct {
		name   string
		limit  int
		writes []string
		label  string
		want   string
	}{
		{name: "no output", limit: 8, writes: nil, label: "stderr", want: ""},
		{name: "whitespace only", limit: 8, writes: []string{" \n\t "}, label: "stderr", want: ""},
		{name: "under limit", limit: 8, writes: []string{"abc"}, label: "stderr", want: "stderr: abc"},
		{name: "exact limit has no marker", limit: 4, writes: []string{"abcd"}, label: "stderr", want: "stderr: abcd"},
		{name: "oversized single write keeps tail", limit: 4, writes: []string{"abcdefgh"}, label: "stderr", want: "stderr: ... efgh"},
		{name: "rollover across writes", limit: 4, writes: []string{"abc", "def"}, label: "stderr", want: "stderr: ... cdef"},
		{name: "many small writes", limit: 3, writes: []string{"a", "b", "c", "d", "e"}, label: "stdout", want: "stdout: ... cde"},
		{name: "zero limit drops content", limit: 0, writes: []string{"abc"}, label: "stderr", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := newCommandOutputTail(tc.limit)
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d", w, n, len(w))
				}
			}
			if got := tail.Detail(tc.label); got != tc.want {
				t.Fatalf("Detail(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}
