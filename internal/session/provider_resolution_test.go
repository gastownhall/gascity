package session

import "testing"

func TestUseAgentTemplateForProviderResolution(t *testing.T) {
	tests := []struct {
		name              string
		kind              string
		metadata          map[string]string
		persistedProvider string
		templateProvider  string
		templateFound     bool
		want              bool
	}{
		{
			name: "explicit provider kind skips agent template",
			kind: "provider",
			want: false,
		},
		{
			name: "explicit agent kind uses agent template",
			kind: "agent",
			want: true,
		},
		{
			name: "legacy nil metadata preserves agent template behavior",
			want: true,
		},
		{
			name: "configured named session uses agent template",
			metadata: map[string]string{
				NamedSessionMetadataKey: "true",
				"session_origin":        "manual",
			},
			want: true,
		},
		{
			name: "manual provider session with template collision skips agent template",
			metadata: map[string]string{
				"session_origin": "manual",
			},
			persistedProvider: "stored-provider",
			templateProvider:  "agent-provider",
			templateFound:     true,
			want:              false,
		},
		{
			name: "manual session with matching provider but no agent metadata stays provider backed",
			metadata: map[string]string{
				"session_origin": "manual",
			},
			persistedProvider: "agent-provider",
			templateProvider:  "agent-provider",
			templateFound:     true,
			want:              false,
		},
		{
			name: "manual session with agent name preserves agent template",
			metadata: map[string]string{
				"agent_name":     "worker",
				"session_origin": "manual",
			},
			persistedProvider: "agent-provider",
			templateProvider:  "agent-provider",
			templateFound:     true,
			want:              true,
		},
		{
			name: "manual session without matching template is provider backed",
			metadata: map[string]string{
				"session_origin": "manual",
			},
			persistedProvider: "agent-provider",
			templateProvider:  "",
			templateFound:     false,
			want:              false,
		},
		{
			name: "non-manual legacy metadata preserves agent template behavior",
			metadata: map[string]string{
				"session_origin": "ephemeral",
			},
			persistedProvider: "agent-provider",
			templateProvider:  "agent-provider",
			templateFound:     true,
			want:              true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UseAgentTemplateForProviderResolution(tt.kind, tt.metadata, tt.persistedProvider, tt.templateProvider, tt.templateFound)
			if got != tt.want {
				t.Fatalf("UseAgentTemplateForProviderResolution() = %v, want %v", got, tt.want)
			}
		})
	}
}
