package api

import "testing"

// TestCacheKeyForDerivesFromStructTags verifies that cacheKeyFor produces a
// deterministic key that includes all query/path/header parameters on the
// input struct — the whole point of Fix 4 is that adding a new parameter
// to an input struct automatically participates in the cache key.
func TestCacheKeyForDerivesFromStructTags(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{
			name:  "empty input",
			input: AgentListInput{},
			want:  "agents",
		},
		{
			name: "single filter",
			input: AgentListInput{
				Rig: "myrig",
			},
			want: "agents?query:rig=myrig",
		},
		{
			name: "multiple filters sorted",
			input: AgentListInput{
				Pool: "deacon",
				Rig:  "myrig",
			},
			want: "agents?query:pool=deacon&query:rig=myrig",
		},
		{
			name: "embedded mixin fields included",
			input: AgentListInput{
				BlockingParam: BlockingParam{Index: "5"},
				Rig:           "myrig",
			},
			want: "agents?query:index=5&query:rig=myrig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheKeyFor("agents", tt.input)
			if got != tt.want {
				t.Errorf("cacheKeyFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCacheKeyForIgnoresBodyField verifies that request bodies don't
// contribute to the cache key (large bodies would be wasteful; cacheable
// identity is the request's path/query/headers).
func TestCacheKeyForIgnoresBodyField(t *testing.T) {
	input := AgentCreateInput{}
	input.Body.Name = "foo"
	input.Body.Provider = "bar"
	got := cacheKeyFor("agents", input)
	if got != "agents" {
		t.Errorf("cacheKeyFor() = %q, want %q (body should not affect key)", got, "agents")
	}
}
