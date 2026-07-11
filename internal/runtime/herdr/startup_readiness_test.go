package herdr

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestWaitForStartupReadyDoesNotAcceptBootstrapShell(t *testing.T) {
	var observations []string
	processPolls := 0
	promptPolls := 0

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := waitForStartupReady(ctx, runtime.Config{
		ProcessNames:      []string{"codex"},
		ReadyPromptPrefix: "› ",
	}, time.Second, startupReadinessProbes{
		processes: func(context.Context) ([]proc, error) {
			observations = append(observations, "process")
			processPolls++
			if processPolls == 1 {
				return []proc{{Name: "sh"}}, nil
			}
			return []proc{{Name: "sh"}, {Name: "codex"}}, nil
		},
		visible: func(context.Context) (string, error) {
			observations = append(observations, "prompt")
			promptPolls++
			if promptPolls == 1 {
				return "OpenAI Codex\nloading", nil
			}
			return "OpenAI Codex\n\n› Write tests", nil
		},
		waitIdle: func(context.Context, time.Duration) error {
			t.Fatal("native idle must not replace configured prompt readiness")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("waitForStartupReady: %v", err)
	}

	want := []string{"process", "process", "prompt", "prompt"}
	if !reflect.DeepEqual(observations, want) {
		t.Fatalf("observations = %v, want %v", observations, want)
	}
}

func TestContainsReadyPromptNormalizesRenderedPrompt(t *testing.T) {
	tests := []struct {
		name    string
		visible string
		prefix  string
		want    bool
	}{
		{name: "codex", visible: "› Write tests", prefix: "› ", want: true},
		{name: "non-breaking space", visible: "❯\u00a0", prefix: "❯ ", want: true},
		{name: "box border", visible: "│ ❯ Continue", prefix: "❯ ", want: true},
		{name: "banner only", visible: "OpenAI Codex", prefix: "› ", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsReadyPrompt(tt.visible, tt.prefix); got != tt.want {
				t.Fatalf("containsReadyPrompt(%q, %q) = %v, want %v", tt.visible, tt.prefix, got, tt.want)
			}
		})
	}
}
