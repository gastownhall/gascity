package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestRenderBeadsMigrateCommandFillsEveryFieldAndQuotes pins what an
// operator's [beads] migrate_command template sees: every field of
// BeadsMigrateCommandData by name, raw, and shq quoting a value for a POSIX
// shell only where it needs it.
func TestRenderBeadsMigrateCommandFillsEveryFieldAndQuotes(t *testing.T) {
	data := BeadsMigrateCommandData{
		City:     "/srv/my city",
		Rig:      "my-project",
		ScopeDir: "/srv/my-project",
		BeadsDir: "/srv/my-project/.beads",
		Database: "mp",
		Host:     "127.0.0.1",
		Port:     "3307",
		Socket:   "",
		Bd:       "/opt/beads/bd",
	}
	tmpl := "cd {{shq .ScopeDir}} && BEADS_DIR={{shq .BeadsDir}} BEADS_DOLT_SERVER_HOST={{.Host}} BEADS_DOLT_SERVER_PORT={{.Port}} " +
		"/opt/migrator/bd migrate schema # {{shq .City}} {{.Rig}} {{.Database}} [{{.Socket}}] {{.Bd}}"
	got, err := RenderBeadsMigrateCommand(tmpl, data)
	if err != nil {
		t.Fatalf("RenderBeadsMigrateCommand: %v", err)
	}
	want := "cd /srv/my-project && BEADS_DIR=/srv/my-project/.beads BEADS_DOLT_SERVER_HOST=127.0.0.1 BEADS_DOLT_SERVER_PORT=3307 " +
		"/opt/migrator/bd migrate schema # '/srv/my city' my-project mp [] /opt/beads/bd"
	if got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}

	quoted, err := RenderBeadsMigrateCommand("{{shq .ScopeDir}} {{shq .Rig}}", BeadsMigrateCommandData{ScopeDir: "/srv/it's here"})
	if err != nil {
		t.Fatalf("RenderBeadsMigrateCommand: %v", err)
	}
	if want := `'/srv/it'\''s here' ''`; quoted != want {
		t.Fatalf("shq rendered %q, want %q", quoted, want)
	}
}

// TestRenderBeadsMigrateCommandTrimsTheResult pins that leading or trailing
// space a template's actions leave behind is not part of the command.
func TestRenderBeadsMigrateCommandTrimsTheResult(t *testing.T) {
	got, err := RenderBeadsMigrateCommand("  {{if .Rig}}--rig {{.Rig}}{{end}} bd migrate schema ", BeadsMigrateCommandData{})
	if err != nil {
		t.Fatalf("RenderBeadsMigrateCommand: %v", err)
	}
	if got != "bd migrate schema" {
		t.Fatalf("rendered %q, want %q", got, "bd migrate schema")
	}
}

// TestBeadsMigrateCommandValidatedAtLoad pins that a migrate_command that
// cannot render a one-line command for every scope fails config load on both
// load paths, naming the field, instead of surfacing first in an operator
// notice; and that unset and well-formed values load.
func TestBeadsMigrateCommandValidatedAtLoad(t *testing.T) {
	bad := map[string]string{
		"syntax error":                  `bd {{.Database migrate schema`,
		"unknown field":                 `bd --db {{.DatabaseName}} migrate schema`,
		"unknown function":              `bd {{quote .Database}} migrate schema`,
		"blank":                         `   `,
		"multi-line":                    `cd {{shq .ScopeDir}}\nbd migrate schema`,
		"renders blank":                 `{{if false}}bd migrate schema{{end}}`,
		"renders blank for city scope":  `{{if .Rig}}bd migrate schema{{end}}`,
		"renders multi-line":            `{{"bd\\nmigrate"}}`,
		"function error at render time": `{{index .Database 99}}`,
	}
	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			layer := "[beads]\nmigrate_command = \"" + strings.ReplaceAll(value, `"`, `\"`) + "\"\n"
			if _, err := Parse([]byte(layer)); err == nil || !strings.Contains(err.Error(), "beads.migrate_command") {
				t.Fatalf("Parse: err = %v, want the beads.migrate_command validation error", err)
			}
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test\"\n\n" + layer)
			if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), "beads.migrate_command") {
				t.Fatalf("LoadWithIncludes: err = %v, want the beads.migrate_command validation error", err)
			}
		})
	}

	if _, err := Parse([]byte("[beads]\nmigrate_command = '''\n/opt/migrator/bd migrate schema\n'''\n")); err != nil {
		t.Fatalf("a multi-line TOML string holding one command line must load: %v", err)
	}
	for name, value := range map[string]string{
		"unset":     "",
		"plain":     "/opt/migrator/bd migrate schema",
		"templated": "cd {{shq .ScopeDir}} && BEADS_DIR={{shq .BeadsDir}} /opt/migrator/bd migrate schema",
	} {
		t.Run(name, func(t *testing.T) {
			layer := "[workspace]\nname = \"test\"\n"
			if value != "" {
				layer += "\n[beads]\nmigrate_command = \"" + value + "\"\n"
			}
			cfg, err := Parse([]byte(layer))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.Beads.MigrateCommand != value {
				t.Fatalf("MigrateCommand = %q, want %q", cfg.Beads.MigrateCommand, value)
			}
		})
	}
}

// TestLoadWithIncludesPreservesMigrateCommandAcrossBeadsFragment pins the
// [beads] fragment merge for migrate_command: a fragment that defines an
// unrelated [beads] key keeps the root's command, and one that sets
// migrate_command wins.
func TestLoadWithIncludesPreservesMigrateCommandAcrossBeadsFragment(t *testing.T) {
	root := `
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
migrate_command = "/opt/root/bd migrate schema"
`
	for name, tc := range map[string]struct {
		fragment string
		want     string
	}{
		"unrelated key keeps the root's": {fragment: "[beads]\nconditional_writes = \"auto\"\n", want: "/opt/root/bd migrate schema"},
		"fragment's own value wins":      {fragment: "[beads]\nmigrate_command = \"/opt/frag/bd migrate schema\"\n", want: "/opt/frag/bd migrate schema"},
	} {
		t.Run(name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(root)
			fs.Files["/city/fragment.toml"] = []byte(tc.fragment)
			cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			if cfg.Beads.MigrateCommand != tc.want {
				t.Fatalf("MigrateCommand = %q, want %q", cfg.Beads.MigrateCommand, tc.want)
			}
		})
	}
}
