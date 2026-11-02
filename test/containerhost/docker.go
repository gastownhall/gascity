package containerhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Docker runs one docker CLI invocation against the host named by RootEnv
// and returns its exit status. Statuses follow the docker CLI: 125 for a
// CLI or daemon error, the command's own status for exec, 126/127 when the
// exec'd command cannot run.
func Docker(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	d := dockerCLI{stdin: stdin, stdout: stdout, stderr: stderr}
	h, err := FromEnv()
	if err != nil {
		return d.fail(125, "docker (emulated): %v", err)
	}
	d.host = h
	if len(args) == 0 {
		return d.fail(125, "docker (emulated): no command")
	}
	switch args[0] {
	case "image":
		if len(args) >= 2 && args[1] == "inspect" {
			return d.imageInspect(args[2:])
		}
	case "inspect":
		return d.inspect(args[1:])
	case "run":
		return d.run(args[1:])
	case "exec":
		return d.exec(args[1:])
	case "stop":
		return d.stop(args[1:])
	case "rm":
		return d.rm(args[1:])
	case "ps":
		return d.ps(args[1:])
	case "logs":
		return d.logs(args[1:])
	}
	return d.unsupported(args)
}

type dockerCLI struct {
	host   *Host
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (d dockerCLI) fail(code int, format string, a ...any) int {
	_, _ = fmt.Fprintf(d.stderr, format+"\n", a...)
	return code
}

func (d dockerCLI) unsupported(args []string) int {
	return d.fail(125, "docker (emulated): unsupported command: %q", args)
}

// flagSet is a minimal docker-style flag parser: options precede the
// positional arguments, and the first positional argument ends parsing
// (docker run IMAGE CMD..., docker exec CONTAINER CMD...).
type flagSet struct {
	bools  map[string]*bool
	values map[string]func(string) error
}

func newFlagSet() *flagSet {
	return &flagSet{bools: map[string]*bool{}, values: map[string]func(string) error{}}
}

func (f *flagSet) boolFlag(p *bool, names ...string) {
	for _, n := range names {
		f.bools[n] = p
	}
}

func (f *flagSet) valueFlag(set func(string) error, names ...string) {
	for _, n := range names {
		f.values[n] = set
	}
}

func (f *flagSet) str(p *string, names ...string) {
	f.valueFlag(func(v string) error { *p = v; return nil }, names...)
}

func (f *flagSet) list(p *[]string, names ...string) {
	f.valueFlag(func(v string) error { *p = append(*p, v); return nil }, names...)
}

// parse returns the positional arguments. Combined short booleans (-it)
// are accepted, as docker accepts them.
func (f *flagSet) parse(args []string, stopAtPositional bool) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(pos, args[i+1:]...), nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if stopAtPositional {
				return append(pos, args[i:]...), nil
			}
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if p, ok := f.bools[name]; ok {
			*p = !hasValue || value == "true"
			continue
		}
		if set, ok := f.values[name]; ok {
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", name)
				}
				i++
				value = args[i]
			}
			if err := set(value); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasPrefix(a, "--") && len(a) > 2 {
			all := true
			for _, r := range a[1:] {
				if _, ok := f.bools["-"+string(r)]; !ok {
					all = false
					break
				}
			}
			if all {
				for _, r := range a[1:] {
					*f.bools["-"+string(r)] = true
				}
				continue
			}
		}
		return nil, fmt.Errorf("unknown flag: %s", name)
	}
	return pos, nil
}

func (d dockerCLI) imageInspect(args []string) int {
	var format string
	fs := newFlagSet()
	fs.str(&format, "-f", "--format")
	names, err := fs.parse(args, false)
	if err != nil || len(names) == 0 {
		return d.fail(125, "docker image inspect: %v", err)
	}
	var views []any
	for _, n := range names {
		img, ok := d.host.LookupImage(n)
		if !ok {
			if len(views) > 0 {
				d.render(format, views)
			}
			return d.fail(1, "Error response from daemon: No such image: %s", n)
		}
		views = append(views, imageView{ID: "sha256:" + imageKey(img.Name), RepoTags: []string{img.Name}, Config: imageConfig{Env: img.Env}})
	}
	return d.render(format, views)
}

type imageConfig struct {
	Env []string
}

type imageView struct {
	ID       string `json:"Id"`
	RepoTags []string
	Config   imageConfig
}

type containerState struct {
	Status   string
	Running  bool
	Pid      int
	ExitCode int
}

type containerConfig struct {
	Hostname   string
	User       string
	Env        []string
	Cmd        []string
	Image      string
	WorkingDir string
	Labels     map[string]string
}

type hostConfig struct {
	Binds       []string
	NetworkMode string
	Init        bool
}

type containerView struct {
	ID         string `json:"Id"`
	Created    string
	Name       string
	Image      string
	State      containerState
	Config     containerConfig
	HostConfig hostConfig
}

func (d dockerCLI) view(c *Container) containerView {
	running := d.host.Running(c)
	status, pid, exit := "exited", 0, 137
	if running {
		status, pid, exit = "running", c.Processes["init"], 0
	} else if code, ok := d.host.ProcessExit(c, "init"); ok {
		exit = code
	}
	var binds []string
	for _, m := range c.Mounts {
		b := m.Source + ":" + m.Destination
		if m.ReadOnly {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	labels := c.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	network := c.Network
	if network == "" {
		network = "bridge"
	}
	return containerView{
		ID: c.ID, Created: c.Created.Format(time.RFC3339Nano), Name: "/" + c.Name, Image: c.Image,
		State: containerState{Status: status, Running: running, Pid: pid, ExitCode: exit},
		Config: containerConfig{
			Hostname: c.ID[:12], User: c.User, Env: c.Env, Cmd: c.Cmd, Image: c.Image,
			WorkingDir: c.WorkingDir, Labels: labels,
		},
		HostConfig: hostConfig{Binds: binds, NetworkMode: network, Init: c.Init},
	}
}

func (d dockerCLI) inspect(args []string) int {
	var format, kind string
	fs := newFlagSet()
	fs.str(&format, "-f", "--format")
	fs.str(&kind, "--type")
	names, err := fs.parse(args, false)
	if err != nil || len(names) == 0 {
		return d.fail(125, "docker inspect: %v", err)
	}
	var views []any
	for _, n := range names {
		if kind == "" || kind == "container" {
			if c, err := d.host.Lookup(n); err == nil {
				views = append(views, d.view(c))
				continue
			}
		}
		if kind == "" || kind == "image" {
			if img, ok := d.host.LookupImage(n); ok {
				views = append(views, imageView{ID: "sha256:" + imageKey(img.Name), RepoTags: []string{img.Name}, Config: imageConfig{Env: img.Env}})
				continue
			}
		}
		if len(views) > 0 {
			d.render(format, views)
		}
		return d.fail(1, "Error: No such object: %s", n)
	}
	return d.render(format, views)
}

// templateFuncs are the docker CLI's template functions the providers can
// reach (github.com/docker/cli/templates).
var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"join":  strings.Join,
	"split": strings.Split,
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
}

// render prints each view through format (a Go template, as docker's
// --format), or the JSON array docker prints without one.
func (d dockerCLI) render(format string, views []any) int {
	if format == "" {
		b, err := json.MarshalIndent(views, "", "    ")
		if err != nil {
			return d.fail(125, "docker: %v", err)
		}
		_, _ = fmt.Fprintln(d.stdout, string(b))
		return 0
	}
	tmpl, err := template.New("").Funcs(templateFuncs).Parse(format)
	if err != nil {
		return d.fail(125, "template parsing error: %v", err)
	}
	for _, v := range views {
		// docker executes inspect templates over the decoded JSON object,
		// so fields are the API's (.Id, .State.Running), not Go's.
		raw, err := json.Marshal(v)
		if err != nil {
			return d.fail(125, "docker: %v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return d.fail(125, "docker: %v", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, obj); err != nil {
			return d.fail(125, "template: %v", err)
		}
		_, _ = fmt.Fprintln(d.stdout, buf.String())
	}
	return 0
}

func (d dockerCLI) run(args []string) int {
	var detach, init, rm, interactive, tty bool
	var name, workdir, user, network string
	var envs, labels, volumes []string
	fs := newFlagSet()
	fs.boolFlag(&detach, "-d", "--detach")
	fs.boolFlag(&init, "--init")
	fs.boolFlag(&rm, "--rm")
	fs.boolFlag(&interactive, "-i", "--interactive")
	fs.boolFlag(&tty, "-t", "--tty")
	fs.str(&name, "--name")
	fs.str(&workdir, "-w", "--workdir")
	fs.str(&user, "-u", "--user")
	fs.str(&network, "--network", "--net")
	fs.list(&envs, "-e", "--env")
	fs.list(&labels, "-l", "--label")
	fs.list(&volumes, "-v", "--volume")
	pos, err := fs.parse(args, true)
	if err != nil {
		return d.fail(125, "unknown flag or bad usage: %v\nSee 'docker run --help'.", err)
	}
	if len(pos) == 0 {
		return d.fail(125, "\"docker run\" requires at least 1 argument.")
	}
	if !detach || rm || interactive || tty {
		return d.fail(125, "docker (emulated): only detached runs (-d, no --rm/-i/-t) are supported")
	}
	image, cmd := pos[0], pos[1:]
	if len(cmd) == 0 {
		return d.fail(125, "docker (emulated): images have no default command; pass one")
	}
	if _, ok := d.host.LookupImage(image); !ok {
		return d.fail(125, "Unable to find image '%s' locally\ndocker: Error response from daemon: pull access denied for %s, repository does not exist or may require 'docker login'.", NormalizeImage(image), strings.Split(image, ":")[0])
	}
	labelMap := map[string]string{}
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		labelMap[k] = v
	}
	var envList []string
	for _, e := range envs {
		if !strings.Contains(e, "=") {
			if v, ok := os.LookupEnv(e); ok {
				envList = append(envList, e+"="+v)
			}
			continue
		}
		envList = append(envList, e)
	}
	var mounts []Mount
	for _, v := range volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return d.fail(125, "docker: Error response from daemon: invalid volume specification: '%s'.", v)
		}
		m := Mount{Source: parts[0], Destination: parts[1]}
		if len(parts) == 3 {
			switch parts[2] {
			case "ro":
				m.ReadOnly = true
			case "rw":
			default:
				return d.fail(125, "docker: Error response from daemon: invalid mode: %s.", parts[2])
			}
		}
		mounts = append(mounts, m)
	}
	unlock, err := d.host.Lock()
	if err != nil {
		return d.fail(125, "docker: %v", err)
	}
	defer unlock()
	c, err := d.host.Create(CreateOptions{
		Name: name, Image: image, Labels: labelMap, Env: envList, Mounts: mounts,
		WorkingDir: workdir, User: user, Network: network, Init: init, Cmd: cmd,
	})
	if err != nil {
		if errors.Is(err, ErrExecNotFound) {
			return d.fail(127, "docker: Error response from daemon: failed to create task for container: %v: unknown.", err)
		}
		return d.fail(125, "docker: Error response from daemon: %v.", err)
	}
	_, _ = fmt.Fprintln(d.stdout, c.ID)
	return 0
}

func (d dockerCLI) exec(args []string) int {
	var interactive, tty, detach bool
	var workdir, user string
	var envs []string
	fs := newFlagSet()
	fs.boolFlag(&interactive, "-i", "--interactive")
	fs.boolFlag(&tty, "-t", "--tty")
	fs.boolFlag(&detach, "-d", "--detach")
	fs.str(&workdir, "-w", "--workdir")
	fs.str(&user, "-u", "--user")
	fs.list(&envs, "-e", "--env")
	pos, err := fs.parse(args, true)
	if err != nil {
		return d.fail(125, "unknown flag or bad usage: %v\nSee 'docker exec --help'.", err)
	}
	if len(pos) < 2 {
		return d.fail(125, "\"docker exec\" requires at least 2 arguments.")
	}
	if detach {
		return d.fail(125, "docker (emulated): exec -d is not supported")
	}
	c, err := d.host.Lookup(pos[0])
	if err != nil {
		return d.fail(1, "Error response from daemon: No such container: %s", pos[0])
	}
	if !d.host.Running(c) {
		return d.fail(1, "Error response from daemon: container %s is not running", c.ID)
	}
	cmd, err := d.host.Command(c, ExecOptions{Env: envs, WorkingDir: workdir, User: user}, pos[1:])
	if err != nil {
		if errors.Is(err, ErrExecNotFound) {
			return d.fail(127, "OCI runtime exec failed: exec failed: unable to start container process: %v: unknown", err)
		}
		return d.fail(126, "OCI runtime exec failed: exec failed: unable to start container process: %v: unknown", err)
	}
	if interactive {
		cmd.Stdin = d.stdin
	}
	// Direct descriptors, not pipes: a daemon the command starts (a tmux
	// server) must not keep this CLI waiting for EOF.
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return d.fail(126, "OCI runtime exec failed: %v", err)
	}
	return exitStatus(err)
}

func (d dockerCLI) stop(args []string) int {
	timeout := 10
	fs := newFlagSet()
	fs.valueFlag(func(v string) error {
		n, err := strconv.Atoi(v)
		timeout = n
		return err
	}, "-t", "--time", "--timeout")
	names, err := fs.parse(args, false)
	if err != nil || len(names) == 0 {
		return d.fail(125, "docker stop: %v", err)
	}
	unlock, err := d.host.Lock()
	if err != nil {
		return d.fail(125, "docker: %v", err)
	}
	defer unlock()
	code := 0
	for _, n := range names {
		c, err := d.host.Lookup(n)
		if err != nil {
			code = d.fail(1, "Error response from daemon: No such container: %s", n)
			continue
		}
		if err := d.host.Stop(c, time.Duration(timeout)*time.Second); err != nil {
			code = d.fail(1, "Error response from daemon: cannot stop container: %s: %v", n, err)
			continue
		}
		_, _ = fmt.Fprintln(d.stdout, n)
	}
	return code
}

func (d dockerCLI) rm(args []string) int {
	var force bool
	fs := newFlagSet()
	fs.boolFlag(&force, "-f", "--force")
	names, err := fs.parse(args, false)
	if err != nil || len(names) == 0 {
		return d.fail(125, "docker rm: %v", err)
	}
	unlock, err := d.host.Lock()
	if err != nil {
		return d.fail(125, "docker: %v", err)
	}
	defer unlock()
	code := 0
	for _, n := range names {
		c, err := d.host.Lookup(n)
		if err != nil {
			if !force {
				code = d.fail(1, "Error response from daemon: No such container: %s", n)
			}
			continue
		}
		if d.host.Running(c) && !force {
			code = d.fail(1, "Error response from daemon: cannot remove container %q: container is running: stop the container before removing or force remove", "/"+c.Name)
			continue
		}
		if err := d.host.Remove(c); err != nil {
			code = d.fail(1, "Error response from daemon: %v", err)
			continue
		}
		_, _ = fmt.Fprintln(d.stdout, n)
	}
	return code
}

type psView struct {
	ID     string
	Names  string
	Image  string
	Status string
	Labels string
	labels map[string]string
}

// Label returns one label's value, as docker ps's {{.Label "k"}}.
func (p psView) Label(key string) string { return p.labels[key] }

func (d dockerCLI) ps(args []string) int {
	var all, quiet bool
	var format string
	var filters []string
	fs := newFlagSet()
	fs.boolFlag(&all, "-a", "--all")
	fs.boolFlag(&quiet, "-q", "--quiet")
	fs.str(&format, "--format")
	fs.list(&filters, "-f", "--filter")
	if pos, err := fs.parse(args, false); err != nil || len(pos) > 0 {
		return d.fail(125, "docker ps: unexpected arguments %q (%v)", pos, err)
	}
	cs, err := d.host.List()
	if err != nil {
		return d.fail(125, "docker: %v", err)
	}
	if quiet && format == "" {
		format = "{{.ID}}"
	}
	if format == "" {
		format = "{{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}"
	}
	tmpl, err := template.New("").Funcs(templateFuncs).Parse(format)
	if err != nil {
		return d.fail(125, "template parsing error: %v", err)
	}
	for _, c := range cs {
		v := d.view(c)
		if !all && !v.State.Running {
			hasStatusFilter := false
			for _, f := range filters {
				hasStatusFilter = hasStatusFilter || strings.HasPrefix(f, "status=")
			}
			if !hasStatusFilter {
				continue
			}
		}
		ok, err := matchFilters(filters, c, v)
		if err != nil {
			return d.fail(125, "Error response from daemon: %v", err)
		}
		if !ok {
			continue
		}
		var labelPairs []string
		for k, val := range v.Config.Labels {
			labelPairs = append(labelPairs, k+"="+val)
		}
		row := psView{ID: c.ID[:12], Names: c.Name, Image: c.Image, Status: v.State.Status, Labels: strings.Join(labelPairs, ","), labels: v.Config.Labels}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, row); err != nil {
			return d.fail(125, "template: %v", err)
		}
		_, _ = fmt.Fprintln(d.stdout, buf.String())
	}
	return 0
}

// matchFilters applies docker ps filters: repeated keys OR together,
// different keys AND together.
func matchFilters(filters []string, c *Container, v containerView) (bool, error) {
	byKey := map[string][]string{}
	for _, f := range filters {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			return false, fmt.Errorf("bad format of filter (expected name=value): %s", f)
		}
		byKey[k] = append(byKey[k], val)
	}
	for key, vals := range byKey {
		matched := false
		for _, val := range vals {
			switch key {
			case "status":
				matched = matched || v.State.Status == val
			case "label":
				lk, lv, hasValue := strings.Cut(val, "=")
				got, present := v.Config.Labels[lk]
				matched = matched || (present && (!hasValue || got == lv))
			case "name":
				matched = matched || strings.Contains(c.Name, strings.TrimPrefix(val, "/"))
			case "id":
				matched = matched || strings.HasPrefix(c.ID, val)
			default:
				return false, fmt.Errorf("invalid filter '%s'", key)
			}
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func (d dockerCLI) logs(args []string) int {
	tail := -1
	fs := newFlagSet()
	fs.valueFlag(func(v string) error {
		if v == "all" {
			return nil
		}
		n, err := strconv.Atoi(v)
		tail = n
		return err
	}, "-n", "--tail")
	names, err := fs.parse(args, false)
	if err != nil || len(names) != 1 {
		return d.fail(125, "docker logs: %v", err)
	}
	c, err := d.host.Lookup(names[0])
	if err != nil {
		return d.fail(1, "Error response from daemon: No such container: %s", names[0])
	}
	data, err := d.host.Logs(c)
	if err != nil {
		return d.fail(1, "Error response from daemon: %v", err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if tail >= 0 && tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	_, _ = fmt.Fprint(d.stdout, strings.Join(lines, ""))
	return 0
}
