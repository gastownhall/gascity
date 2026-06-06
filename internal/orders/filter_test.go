package orders

import "testing"

func TestFilterEnabled(t *testing.T) {
	t.Parallel()

	disabled := boolPtr(false)
	enabled := boolPtr(true)

	tests := []struct {
		name  string
		input []Order
		want  []string
	}{
		{
			name:  "nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty",
			input: []Order{},
			want:  []string{},
		},
		{
			name: "keeps default and explicit enabled",
			input: []Order{
				{Name: "default"},
				{Name: "enabled", Enabled: enabled},
			},
			want: []string{"default", "enabled"},
		},
		{
			name: "drops disabled",
			input: []Order{
				{Name: "before"},
				{Name: "disabled", Enabled: disabled},
				{Name: "after"},
			},
			want: []string{"before", "after"},
		},
		{
			name: "all disabled",
			input: []Order{
				{Name: "one", Enabled: disabled},
				{Name: "two", Enabled: disabled},
			},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			original := make([]Order, len(tt.input))
			copy(original, tt.input)

			got := FilterEnabled(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("FilterEnabled() returned %d orders, want %d: %#v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if got[i].Name != want {
					t.Fatalf("FilterEnabled()[%d].Name = %q, want %q", i, got[i].Name, want)
				}
			}
			if tt.input != nil && len(tt.input) == 0 && got == nil {
				t.Fatal("FilterEnabled(empty) returned nil, want the empty input slice")
			}
			if len(tt.input) != len(original) {
				t.Fatalf("input length changed to %d, want %d", len(tt.input), len(original))
			}
			for i := range original {
				if tt.input[i].Name != original[i].Name {
					t.Fatalf("FilterEnabled mutated input at %d: got %q, want %q", i, tt.input[i].Name, original[i].Name)
				}
			}
		})
	}
}
