package runtime

import "testing"

func TestTransportForRuntimeName(t *testing.T) {
	tests := map[string]string{
		"":                        "tmux",
		"tmux":                    "tmux",
		"k8s":                     "tmux",
		"exec:/opt/runtime":       "tmux",
		"acp":                     "acp",
		"t3bridge":                "t3",
		"exec:/opt/gc-session-t3": "t3",
		"exec:gc-session-t3":      "t3",
	}
	for name, want := range tests {
		if got := TransportForRuntimeName(name); got != want {
			t.Errorf("TransportForRuntimeName(%q) = %q, want %q", name, got, want)
		}
	}
	if got := TransportForRuntimeName(" exec:/x/gc-session-t3  "); got != "t3" {
		t.Errorf("TransportForRuntimeName() did not trim surrounding whitespace: got %q, want t3", got)
	}
}
