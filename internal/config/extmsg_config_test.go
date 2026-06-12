package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadParsesExtMsgDefaultRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	content := `[workspace]
name = "test"

[[extmsg.default_route]]
provider = "telegram"
agent = "myrig/frontdesk"

[[extmsg.default_route]]
provider = "telegram"
account_id = "ops"
agent = "myrig/operator"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ExtMsg.DefaultRoutes) != 2 {
		t.Fatalf("len(DefaultRoutes) = %d, want 2", len(cfg.ExtMsg.DefaultRoutes))
	}
	if cfg.ExtMsg.DefaultRoutes[0].Provider != "telegram" || cfg.ExtMsg.DefaultRoutes[0].Agent != "myrig/frontdesk" {
		t.Fatalf("DefaultRoutes[0] = %#v", cfg.ExtMsg.DefaultRoutes[0])
	}
	if cfg.ExtMsg.DefaultRoutes[1].AccountID != "ops" {
		t.Fatalf("DefaultRoutes[1] = %#v", cfg.ExtMsg.DefaultRoutes[1])
	}
}

func TestExtMsgDefaultRouteAgentPrecedence(t *testing.T) {
	cfg := &City{
		ExtMsg: ExtMsgConfig{
			DefaultRoutes: []ExtMsgDefaultRoute{
				{Provider: "telegram", Agent: "myrig/frontdesk"},
				{Provider: "telegram", AccountID: "ops", Agent: "myrig/operator"},
				{Provider: "discord", AccountID: "main", Agent: "myrig/helper"},
			},
		},
	}

	// Exact (provider, account) match wins over the provider-wide route.
	if got := cfg.ExtMsgDefaultRouteAgent("telegram", "ops"); got != "myrig/operator" {
		t.Fatalf("agent(telegram, ops) = %q, want myrig/operator", got)
	}
	// Other accounts fall back to the provider-wide route.
	if got := cfg.ExtMsgDefaultRouteAgent("telegram", "default"); got != "myrig/frontdesk" {
		t.Fatalf("agent(telegram, default) = %q, want myrig/frontdesk", got)
	}
	// Account-scoped routes do not match other accounts of the provider.
	if got := cfg.ExtMsgDefaultRouteAgent("discord", "other"); got != "" {
		t.Fatalf("agent(discord, other) = %q, want empty", got)
	}
	if got := cfg.ExtMsgDefaultRouteAgent("slack", "default"); got != "" {
		t.Fatalf("agent(slack, default) = %q, want empty", got)
	}

	var empty City
	if got := empty.ExtMsgDefaultRouteAgent("telegram", "default"); got != "" {
		t.Fatalf("agent on empty config = %q, want empty", got)
	}
}
