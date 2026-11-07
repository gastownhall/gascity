package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/warmup"
)

func TestDefaultMailProviderUsesStartedCityPath(t *testing.T) {
	t.Setenv("GC_MAIL", "")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[mail]\nprovider = \"fake\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	provider := defaultMailProvider(cityDir)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	if _, err := provider.Send("gc-start-warmup", "mayor", "subject", "body"); err != nil {
		t.Fatalf("fake mail provider Send returned error: %v", err)
	}
}

func TestStartWarmupOptsMailsTheConfiguredRecipient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		doctor string
		wantTo string
	}{
		{name: "configured", doctor: "\n[doctor]\nwarmup_mail_to = \"ops.lead\"\n", wantTo: "ops.lead"},
		{name: "unset", wantTo: "mayor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte("[workspace]\nname = \"demo\"\n" + tc.doctor))
			if err != nil {
				t.Fatalf("parse city config: %v", err)
			}
			mailer := mail.NewFake()
			var stderr bytes.Buffer
			opts := startWarmupOpts(cfg, []doctor.Check{failingWarmupCheck{name: "rig-root-branch"}}, mailer, &stderr)
			if _, err := warmup.RunWarmupChecks(context.Background(), t.TempDir(), cfg, opts); err != nil {
				t.Fatalf("warmup runner: %v", err)
			}

			sent := mailer.Messages()
			if len(sent) != 1 {
				t.Fatalf("sent mail count = %d, want 1; stderr=%q", len(sent), stderr.String())
			}
			if sent[0].To != tc.wantTo {
				t.Errorf("mail To = %q, want %q", sent[0].To, tc.wantTo)
			}
			if want := "see mail to " + tc.wantTo; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
		})
	}
}
