package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBackstopSeatAwaitsHumanInput(t *testing.T) {
	const sess = "worker-1"
	for _, tt := range []struct {
		name     string
		newFake  func() *runtime.Fake
		want     bool
		wantNote string
	}{
		{
			name:    "idle seat is nudgeable",
			newFake: runtime.NewFake,
			want:    false,
		},
		{
			name: "seat at an approval prompt is refused",
			newFake: func() *runtime.Fake {
				f := runtime.NewFake()
				f.SetPendingInteraction(sess, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"})
				return f
			},
			want:     true,
			wantNote: "awaiting human input",
		},
		{
			name:     "probe failure is refused, not assumed idle",
			newFake:  runtime.NewFailFake,
			want:     true,
			wantNote: "probing pending interaction",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := tt.newFake()
			var out bytes.Buffer
			got := backstopSeatAwaitsHumanInput(sp, sess, "test-backstop", &out)
			if got != tt.want {
				t.Fatalf("backstopSeatAwaitsHumanInput = %v, want %v (out=%q)", got, tt.want, out.String())
			}
			if tt.wantNote != "" && !strings.Contains(out.String(), tt.wantNote) {
				t.Fatalf("refusal must be observable; out = %q, want it to mention %q", out.String(), tt.wantNote)
			}
		})
	}
}
