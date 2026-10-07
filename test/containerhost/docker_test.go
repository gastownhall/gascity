package containerhost

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFlagSetParsesDockerStyleOptions(t *testing.T) {
	var interactive, tty bool
	var workdir string
	var envs []string
	fs := newFlagSet()
	fs.boolFlag(&interactive, "-i", "--interactive")
	fs.boolFlag(&tty, "-t", "--tty")
	fs.str(&workdir, "-w", "--workdir")
	fs.list(&envs, "-e", "--env")

	pos, err := fs.parse([]string{"-it", "-e", "A=1", "--env=B=2", "-w", "/x", "ctr", "tmux", "-u", "-e", "C=3"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !interactive || !tty {
		t.Errorf("combined -it: interactive=%v tty=%v, want both", interactive, tty)
	}
	if workdir != "/x" {
		t.Errorf("workdir = %q, want /x", workdir)
	}
	if want := []string{"A=1", "B=2"}; !reflect.DeepEqual(envs, want) {
		t.Errorf("envs = %q, want %q", envs, want)
	}
	// Options after the first positional argument belong to the command.
	if want := []string{"ctr", "tmux", "-u", "-e", "C=3"}; !reflect.DeepEqual(pos, want) {
		t.Errorf("positional = %q, want %q", pos, want)
	}
}

func TestFlagSetRejectsUnknownAndIncompleteFlags(t *testing.T) {
	for _, args := range [][]string{{"--privileged", "img"}, {"-w"}, {"-iz", "ctr"}} {
		var b bool
		var s string
		fs := newFlagSet()
		fs.boolFlag(&b, "-i")
		fs.str(&s, "-w")
		if _, err := fs.parse(args, true); err == nil {
			t.Errorf("parse(%q) succeeded, want an error", args)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	for in, want := range map[string]string{
		"gc-agent":                  "gc-agent:latest",
		"gc-agent:v1":               "gc-agent:v1",
		"registry:5000/gc-agent":    "registry:5000/gc-agent:latest",
		"registry:5000/gc-agent:v2": "registry:5000/gc-agent:v2",
		"alpine@sha256:abc":         "alpine@sha256:abc",
	} {
		if got := NormalizeImage(in); got != want {
			t.Errorf("NormalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRelocateMovesOnlyContainerPrivatePaths(t *testing.T) {
	c := &Container{}
	dir := "/state/c/abc"
	for in, want := range map[string]string{
		"/run/gc-tmux": "/state/c/abc/run/gc-tmux",
		"/run":         "/state/c/abc/run",
		"mkdir -p '/run/gc-tmux' && chmod 1777 '/run/gc-tmux'": "mkdir -p '/state/c/abc/run/gc-tmux' && chmod 1777 '/state/c/abc/run/gc-tmux'",
		"TMUX_TMPDIR=/run/gc-tmux":                             "TMUX_TMPDIR=/state/c/abc/run/gc-tmux",
		"/root":                                                "/state/c/abc/root",
		"/root/.config":                                        "/state/c/abc/root/.config",
		"/runner/x":                                            "/runner/x",
		"/tmp/run/x":                                           "/tmp/run/x",
		"/rootless":                                            "/rootless",
		"/work/dir":                                            "/work/dir",
	} {
		if got := c.relocate(dir, in); got != want {
			t.Errorf("relocate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeEnvLastAssignmentWins(t *testing.T) {
	got := mergeEnv([]string{"PATH=/img/bin", "TERM=xterm"}, []string{"TERM=screen", "A=1", "A=2"})
	if want := []string{"PATH=/img/bin", "TERM=screen", "A=2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
}

func TestMatchFiltersFollowsDockerPsSemantics(t *testing.T) {
	c := &Container{ID: "0123456789abcdef", Name: "gc-docker-test"}
	v := containerView{
		State:  containerState{Status: "running", Running: true},
		Config: containerConfig{Labels: map[string]string{"gc.managed": "true", "gc.agent": "a"}},
	}
	for _, tt := range []struct {
		filters []string
		want    bool
	}{
		{[]string{"status=running", "label=gc.managed=true"}, true},
		{[]string{"label=gc.managed"}, true},
		{[]string{"label=gc.managed=false"}, false},
		{[]string{"status=exited"}, false},
		// Repeated keys OR together; different keys AND together.
		{[]string{"status=exited", "status=running"}, true},
		{[]string{"name=docker-test", "id=0123"}, true},
		{[]string{"name=other"}, false},
	} {
		got, err := matchFilters(tt.filters, c, v)
		if err != nil {
			t.Fatalf("matchFilters(%q): %v", tt.filters, err)
		}
		if got != tt.want {
			t.Errorf("matchFilters(%q) = %v, want %v", tt.filters, got, tt.want)
		}
	}
	if _, err := matchFilters([]string{"health=healthy"}, c, v); err == nil {
		t.Error("an unmodelled filter key was accepted")
	}
}

func TestRenderExecutesTemplatesOverTheAPIObject(t *testing.T) {
	var out bytes.Buffer
	d := dockerCLI{stdout: &out, stderr: &out}
	v := containerView{
		ID:      "abc",
		Created: time.Unix(0, 0).UTC().Format(time.RFC3339Nano),
		State:   containerState{Running: true},
		Config:  containerConfig{Env: []string{"TERM=xterm", "A=1"}, Labels: map[string]string{"gc.agent": "x"}},
	}
	format := `{{.Id}} {{.State.Running}} {{index .Config.Labels "gc.agent"}} {{range .Config.Env}}{{println .}}{{end}}`
	if code := d.render(format, []any{v}); code != 0 {
		t.Fatalf("render exit %d: %s", code, out.String())
	}
	if want := "abc true x TERM=xterm\nA=1\n\n"; out.String() != want {
		t.Fatalf("render = %q, want %q", out.String(), want)
	}
}

func TestPgrepRejectsUnsupportedUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-u", "root", "x"}, {"a", "b"}} {
		var out, errOut bytes.Buffer
		if code := Pgrep(args, &out, &errOut); code != 2 {
			t.Errorf("Pgrep(%q) = %d, want 2 (usage)", args, code)
		}
	}
	t.Setenv(MarkerEnv, "")
	var out, errOut bytes.Buffer
	if code := Pgrep([]string{"-x", "sleep"}, &out, &errOut); code != 3 || !strings.Contains(errOut.String(), "not running inside") {
		t.Errorf("Pgrep outside a container = %d (%q), want 3", code, errOut.String())
	}
}
