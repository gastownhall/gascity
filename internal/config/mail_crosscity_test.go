package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadParsesMailCrossCity(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "qlandia"

[mail.crosscity]
cities = ["gastown", "westeros"]
`)
	cfg, err := Load(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cc := cfg.Mail.CrossCity
	if cc == nil {
		t.Fatal("Mail.CrossCity = nil, want parsed section")
	}
	if cc.City != "" {
		t.Errorf("City = %q, want empty (defaults to effective city name)", cc.City)
	}
	if len(cc.Cities) != 2 || cc.Cities[0] != "gastown" || cc.Cities[1] != "westeros" {
		t.Errorf("Cities = %v, want [gastown westeros]", cc.Cities)
	}
}

func TestMailCityRoster(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *City
		fallback  string
		wantLocal string
		wantPeers []string
	}{
		{
			name:      "nil config is disabled",
			cfg:       nil,
			fallback:  "qlandia",
			wantLocal: "",
			wantPeers: nil,
		},
		{
			name:      "absent section is disabled",
			cfg:       &City{},
			fallback:  "qlandia",
			wantLocal: "",
			wantPeers: nil,
		},
		{
			name: "explicit city wins over workspace name",
			cfg: &City{
				Workspace: Workspace{Name: "workspace-name"},
				Mail: MailConfig{CrossCity: &MailCrossCityConfig{
					City:   "qlandia",
					Cities: []string{"gastown"},
				}},
			},
			fallback:  "dir-name",
			wantLocal: "qlandia",
			wantPeers: []string{"gastown"},
		},
		{
			name: "workspace name fills an empty city",
			cfg: &City{
				Workspace: Workspace{Name: "qlandia"},
				Mail: MailConfig{CrossCity: &MailCrossCityConfig{
					Cities: []string{"gastown"},
				}},
			},
			fallback:  "dir-name",
			wantLocal: "qlandia",
			wantPeers: []string{"gastown"},
		},
		{
			name: "fallback fills when nothing else names the city",
			cfg: &City{
				Mail: MailConfig{CrossCity: &MailCrossCityConfig{
					Cities: []string{"gastown"},
				}},
			},
			fallback:  "qlandia",
			wantLocal: "qlandia",
			wantPeers: []string{"gastown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, peers := tt.cfg.MailCityRoster(tt.fallback)
			if local != tt.wantLocal {
				t.Errorf("local = %q, want %q", local, tt.wantLocal)
			}
			if len(peers) != len(tt.wantPeers) {
				t.Fatalf("peers = %v, want %v", peers, tt.wantPeers)
			}
			for i := range peers {
				if peers[i] != tt.wantPeers[i] {
					t.Errorf("peers[%d] = %q, want %q", i, peers[i], tt.wantPeers[i])
				}
			}
		})
	}
}

func TestValidateMailCrossCity(t *testing.T) {
	valid := func() *City {
		return &City{
			Workspace: Workspace{Name: "qlandia"},
			Mail: MailConfig{CrossCity: &MailCrossCityConfig{
				Cities: []string{"gastown", "westeros"},
			}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*City)
		wantErr string
	}{
		{
			name:   "valid section passes",
			mutate: func(*City) {},
		},
		{
			name:   "absent section passes",
			mutate: func(c *City) { c.Mail.CrossCity = nil },
		},
		{
			name:    "empty cities list is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.Cities = nil },
			wantErr: "cities",
		},
		{
			name:    "duplicate city is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.Cities = []string{"gastown", "gastown"} },
			wantErr: "duplicate",
		},
		{
			name:    "invalid city name is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.Cities = []string{"gas town"} },
			wantErr: "invalid",
		},
		{
			name:    "city name with a slash is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.Cities = []string{"gas/town"} },
			wantErr: "invalid",
		},
		{
			name:    "explicit local city listed as a peer is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.City = "gastown" },
			wantErr: "own city",
		},
		{
			name:    "effective local city listed as a peer is refused",
			mutate:  func(c *City) { c.Mail.CrossCity.Cities = []string{"qlandia"} },
			wantErr: "own city",
		},
		{
			name:    "directory-derived local city listed as a peer is refused",
			mutate:  func(c *City) { c.Workspace.Name = ""; c.Mail.CrossCity.Cities = []string{"cityroot"} },
			wantErr: "own city",
		},
		{
			name:    "rig named like a peer city is refused",
			mutate:  func(c *City) { c.Rigs = []Rig{{Name: "gastown", Path: "rigs/gastown"}} },
			wantErr: "rig",
		},
		{
			name:    "rig named like the local city is refused",
			mutate:  func(c *City) { c.Rigs = []Rig{{Name: "qlandia", Path: "rigs/qlandia"}} },
			wantErr: "rig",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(cfg)
			err := ValidateMailCrossCity(cfg, "/tmp/cityroot")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMailCrossCity: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMailCrossCity: nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// The rig-collision rule must hold at the composed load path, not only when a
// caller remembers to invoke the validator: a rig colliding with a roster
// city is a load-time validation error, not a silent mis-bind.
func TestLoadWithIncludesRejectsRigNamedLikeRosterCity(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "qlandia"

[mail.crosscity]
cities = ["gastown"]

[[rigs]]
name = "gastown"
path = "rigs/gastown"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes: nil error, want rig/city collision refusal")
	}
	if !strings.Contains(err.Error(), "gastown") {
		t.Errorf("error %q should name the colliding city", err.Error())
	}
}
