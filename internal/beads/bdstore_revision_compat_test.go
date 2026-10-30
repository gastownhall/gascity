package beads

import (
	"encoding/json"
	"testing"
)

func TestBD130RevisionWireCompatibility(t *testing.T) {
	for _, tt := range []struct {
		wire string
		want int64
		bad  bool
	}{
		{`"9223372036854775807"`, 9223372036854775807, false},
		{`9223372036854775807`, 9223372036854775807, false},
		{`"-3"`, -3, false},
		{`null`, 0, false},
		{`""`, 0, true},
		{`"abc"`, 0, true},
		{`"9223372036854775808"`, 0, true},
		{`1.5`, 0, true},
	} {
		t.Run(tt.wire, func(t *testing.T) {
			var issue bdIssue
			err := json.Unmarshal([]byte(`{"id":"gc-1","revision":`+tt.wire+`}`), &issue)
			if (err != nil) != tt.bad {
				t.Fatalf("decode revision %s: %v", tt.wire, err)
			}
			if !tt.bad && issue.toBead().Revision != tt.want {
				t.Fatalf("revision = %d, want %d", issue.toBead().Revision, tt.want)
			}
		})
	}
}
