package beads

import "testing"

func TestBdStdoutErrorDetail(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "empty",
			out:  "",
			want: "",
		},
		{
			name: "non json",
			out:  "bd failed",
			want: "",
		},
		{
			name: "malformed json",
			out:  `{"error":`,
			want: "",
		},
		{
			name: "missing error",
			out:  `{"schema_version":1}`,
			want: "",
		},
		{
			name: "null error",
			out:  `{"error":null,"schema_version":1}`,
			want: "",
		},
		{
			name: "blank error",
			out:  `{"error":"   ","schema_version":1}`,
			want: "",
		},
		{
			name: "error envelope",
			out:  `{"error":" no issue found bd-42 ","schema_version":1}`,
			want: "no issue found bd-42",
		},
		{
			name: "preamble before envelope",
			out:  "bd warning before json\n{\"error\":\"resolving dependency: no issue found bd-42\",\"schema_version\":1}",
			want: "resolving dependency: no issue found bd-42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdStdoutErrorDetail([]byte(tt.out)); got != tt.want {
				t.Fatalf("bdStdoutErrorDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}
