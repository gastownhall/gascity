// Package containerhost emulates a container host for tests of Gas City's
// container session providers (scripts/gc-session-docker,
// contrib/session-scripts/gc-session-k8s) on machines that have no container
// runtime, no privileges and no user namespaces, such as the rbe-west
// executors.
//
// A container is a group of host processes tagged with the container's ID in
// their environment (MarkerEnv). The tag scopes everything a runtime scopes to
// a container: exec'd commands, the in-container process table (the pgrep
// this package provides), stop and remove. Images are directories whose bin/
// is the container's whole PATH, so a command an image does not ship is not
// found inside it. A container's private roots (/run and /root for a Docker
// container; also /tmp, /home and every volume mount for a pod) are
// relocated per container under the host's state directory wherever a
// container path appears in a command line or environment value, and bind
// mounts must map a host path to the same container path, which is how the
// Docker session provider mounts work directories. Everything else (absolute
// paths like /bin/sh) is the host's.
//
// Every container process runs under this package's init shim
// (containerhost-init), which records its exit status, as tini or a kubelet
// would.
//
// The CLI fronts (docker.go, kubectl.go) parse and answer like the real CLIs for
// the commands the providers use and fail closed (exit 125, "unsupported") on
// anything else, so a provider change that starts relying on runtime
// behavior this emulation does not model fails loudly instead of passing.
package containerhost

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RootEnv names the state directory shared by every CLI invocation of one
// emulated host.
const RootEnv = "CONTAINERHOST_ROOT"

// MarkerEnv tags every process of a container with its ID.
const MarkerEnv = "CONTAINERHOST_CONTAINER"

// DockerPrivateRoots are a Docker container's private paths: /run is the
// container-local tmpfs gc-session-docker keeps its tmux sockets in because
// it is not shared with the host even when /tmp is bind-mounted, and /root
// is the default HOME.
var DockerPrivateRoots = []string{"/run", "/root"}

// PodPrivateRoots are a pod container's private paths before its volume
// mounts: nothing of the host's filesystem is shared with a pod.
var PodPrivateRoots = []string{"/run", "/root", "/home", "/tmp", "/var/tmp"}

// Host is one emulated container host rooted at a state directory.
type Host struct {
	Root string
}

// Image is a stored image: a root directory whose bin/ is the PATH of every
// container started from it.
type Image struct {
	Name string   `json:"name"`
	Root string   `json:"root"`
	Env  []string `json:"env"`
}

// ImageSpec describes an image to build.
type ImageSpec struct {
	// Tools are host executables (looked up on the host PATH) the image
	// ships in its bin/. "pgrep" is this package's container-scoped pgrep.
	Tools []string
	// Files maps a path relative to the image root to its contents; files
	// under bin/ are executable.
	Files map[string]string
}

// Mount is an identity bind mount.
type Mount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

// Container is the persisted state of one container.
type Container struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Labels     map[string]string `json:"labels"`
	Env        []string          `json:"env"`
	Mounts     []Mount           `json:"mounts"`
	WorkingDir string            `json:"working_dir"`
	User       string            `json:"user"`
	Network    string            `json:"network"`
	Init       bool              `json:"init"`
	Cmd        []string          `json:"cmd"`
	// PrivateRoots are the container paths relocated under its state
	// directory, longest first.
	PrivateRoots []string `json:"private_roots"`
	// Processes maps each started process (a Docker container's "init", a
	// pod's containers) to its init-shim PID.
	Processes map[string]int `json:"processes"`
	Created   time.Time      `json:"created"`
	Stopped   bool           `json:"stopped"`
}

// New returns the host rooted at root, creating its layout.
func New(root string) (*Host, error) {
	for _, dir := range []string{"images", "containers"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("creating container host %s: %w", dir, err)
		}
	}
	return &Host{Root: root}, nil
}

// FromEnv returns the host named by RootEnv.
func FromEnv() (*Host, error) {
	root := os.Getenv(RootEnv)
	if root == "" {
		return nil, fmt.Errorf("%s is not set", RootEnv)
	}
	return New(root)
}

// BuildImage stores an image named name (a tag defaults to :latest).
// self is the executable that serves this package's tools (pgrep).
func (h *Host) BuildImage(name string, spec ImageSpec, self string) error {
	ref := NormalizeImage(name)
	root := filepath.Join(h.Root, "images", imageKey(ref))
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("replacing image %s: %w", ref, err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return fmt.Errorf("creating image %s: %w", ref, err)
	}
	for _, tool := range spec.Tools {
		target := self
		if tool != "pgrep" {
			found, err := exec.LookPath(tool)
			if err != nil {
				return fmt.Errorf("image %s: host tool %q: %w", ref, tool, err)
			}
			target = found
		}
		if err := os.Symlink(target, filepath.Join(bin, tool)); err != nil {
			return fmt.Errorf("image %s: linking %s: %w", ref, tool, err)
		}
	}
	for rel, contents := range spec.Files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("image %s: %w", ref, err)
		}
		mode := fs.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			return fmt.Errorf("image %s: writing %s: %w", ref, rel, err)
		}
	}
	img := Image{Name: ref, Root: root, Env: []string{"PATH=" + bin}}
	return writeJSON(root+".json", img)
}

// LookupImage returns the stored image, or ok=false.
func (h *Host) LookupImage(name string) (Image, bool) {
	var img Image
	err := readJSON(filepath.Join(h.Root, "images", imageKey(NormalizeImage(name))+".json"), &img)
	return img, err == nil
}

// NormalizeImage adds the implicit :latest tag.
func NormalizeImage(name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		return name
	}
	return name + ":latest"
}

func imageKey(ref string) string {
	return strings.NewReplacer("/", "_", ":", "+", "@", "=").Replace(ref)
}

func (h *Host) containerDir(id string) string {
	return filepath.Join(h.Root, "containers", id[:12])
}

// Lookup finds a container by name, full ID or unique ID prefix.
func (h *Host) Lookup(ref string) (*Container, error) {
	all, err := h.List()
	if err != nil {
		return nil, err
	}
	ref = strings.TrimPrefix(ref, "/")
	for _, c := range all {
		if c.Name == ref || c.ID == ref {
			return c, nil
		}
	}
	var match *Container
	for _, c := range all {
		if len(ref) >= 3 && strings.HasPrefix(c.ID, ref) {
			if match != nil {
				return nil, fmt.Errorf("multiple IDs found with provided prefix: %s", ref)
			}
			match = c
		}
	}
	if match == nil {
		return nil, errNoSuchContainer
	}
	return match, nil
}

var errNoSuchContainer = errors.New("no such container")

// List returns every container, oldest first.
func (h *Host) List() ([]*Container, error) {
	entries, err := os.ReadDir(filepath.Join(h.Root, "containers"))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	var out []*Container
	for _, e := range entries {
		var c Container
		if err := readJSON(filepath.Join(h.Root, "containers", e.Name(), "config.json"), &c); err != nil {
			continue // a container being created or removed
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Running reports whether the container's init process is alive.
func (h *Host) Running(c *Container) bool {
	return !c.Stopped && h.ProcessRunning(c, "init")
}

// ProcessRunning reports whether the named process of the container is alive.
func (h *Host) ProcessRunning(c *Container, name string) bool {
	pid, ok := c.Processes[name]
	return ok && processInContainer(pid, c.ID)
}

// ProcessExit returns the exit status the init shim recorded for the named
// process, or ok=false while it has not exited (or never started).
func (h *Host) ProcessExit(c *Container, name string) (code int, ok bool) {
	data, err := os.ReadFile(h.statusPath(c, name))
	if err != nil {
		return 0, false
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(data)))
	return code, err == nil
}

func (h *Host) statusPath(c *Container, name string) string {
	return filepath.Join(h.containerDir(c.ID), "status", name)
}

// Lock serializes state changes across CLI invocations.
func (h *Host) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(h.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening container host lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking container host: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// CreateOptions are the container settings a run command carries.
type CreateOptions struct {
	Name       string
	Image      string
	Labels     map[string]string
	Env        []string
	Mounts     []Mount
	WorkingDir string
	User       string
	Network    string
	Init       bool
	Cmd        []string
	// PrivateRoots defaults to DockerPrivateRoots.
	PrivateRoots []string
}

// Create starts a Docker-style container whose init process runs opts.Cmd.
// The caller holds the lock.
func (h *Host) Create(opts CreateOptions) (*Container, error) {
	c, err := h.NewSandbox(opts)
	if err != nil {
		return nil, err
	}
	if _, err := h.Start(c, "init", ExecOptions{}, c.Cmd); err != nil {
		_ = h.Remove(c)
		return nil, err
	}
	return c, nil
}

// NewSandbox records a container and creates its private directories
// without starting a process (a pod sandbox). The caller holds the lock.
func (h *Host) NewSandbox(opts CreateOptions) (*Container, error) {
	img, ok := h.LookupImage(opts.Image)
	if !ok {
		return nil, fmt.Errorf("no such image: %s", opts.Image)
	}
	if opts.Name != "" {
		if _, err := h.Lookup(opts.Name); err == nil {
			return nil, fmt.Errorf("Conflict. The container name %q is already in use", "/"+opts.Name) //nolint:staticcheck // the runtime's wording
		}
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if opts.Name == "" {
		opts.Name = "emulated_" + id[:8]
	}
	for _, m := range opts.Mounts {
		if filepath.Clean(m.Source) != filepath.Clean(m.Destination) {
			return nil, fmt.Errorf("bind mount %s:%s: the emulated host supports identity mounts only", m.Source, m.Destination)
		}
		if err := os.MkdirAll(m.Source, 0o755); err != nil {
			return nil, fmt.Errorf("bind mount source %s: %w", m.Source, err)
		}
	}
	roots := opts.PrivateRoots
	if roots == nil {
		roots = DockerPrivateRoots
	}
	roots = append([]string(nil), roots...)
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	defaults := []string{"HOME=/root", "HOSTNAME=" + id[:12]}
	if slices.Contains(roots, "/tmp") {
		// tmux and mktemp fall back to a compiled-in /tmp; point them at
		// the container's own.
		defaults = append(defaults, "TMPDIR=/tmp", "TMUX_TMPDIR=/tmp")
	}
	env := mergeEnv(mergeEnv(defaults, img.Env), opts.Env)
	c := &Container{
		ID: id, Name: opts.Name, Image: NormalizeImage(opts.Image), Labels: opts.Labels,
		Env: env, Mounts: opts.Mounts, WorkingDir: opts.WorkingDir, User: opts.User,
		Network: opts.Network, Init: opts.Init, Cmd: opts.Cmd, PrivateRoots: roots,
		Processes: map[string]int{}, Created: time.Now().UTC(),
	}
	if c.WorkingDir == "" {
		c.WorkingDir = "/"
	}
	dir := h.containerDir(id)
	for _, sub := range []string{"status", "fs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("creating container %s: %w", c.Name, err)
		}
	}
	for _, root := range roots {
		if err := os.MkdirAll(c.hostPath(dir, root), 0o755); err != nil {
			return nil, fmt.Errorf("creating container %s: %w", c.Name, err)
		}
	}
	if err := os.MkdirAll(c.hostPath(dir, c.WorkingDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating working directory %s: %w", c.WorkingDir, err)
	}
	if err := writeJSON(filepath.Join(dir, "config.json"), c); err != nil {
		return nil, err
	}
	return c, nil
}

// Start runs argv in the container as the named process, detached, under
// the init shim (which records its exit status), with output appended to the
// container's log. The caller holds the lock.
func (h *Host) Start(c *Container, name string, opts ExecOptions, argv []string) (int, error) {
	if _, running := c.Processes[name]; running && h.ProcessRunning(c, name) {
		return 0, fmt.Errorf("container %s: process %s is already running", c.Name, name)
	}
	dir := h.containerDir(c.ID)
	logFile, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("opening container log: %w", err)
	}
	defer logFile.Close() //nolint:errcheck // the process holds its own descriptor
	cmd, err := h.command(c, opts, argv)
	if err != nil {
		return 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locating the init shim: %w", err)
	}
	status := h.statusPath(c, name)
	_ = os.Remove(status)
	shim := exec.Command(self, append([]string{status, cmd.Path}, cmd.Args...)...)
	shim.Args[0] = initShimName
	shim.Env = cmd.Env
	shim.Dir = cmd.Dir
	shim.Stdout = logFile
	shim.Stderr = logFile
	shim.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := shim.Start(); err != nil {
		return 0, fmt.Errorf("starting container process %s: %w", name, err)
	}
	pid := shim.Process.Pid
	_ = shim.Process.Release()
	c.Processes[name] = pid
	if err := writeJSON(filepath.Join(dir, "config.json"), c); err != nil {
		return 0, err
	}
	return pid, nil
}

// Stop sends SIGTERM to every process of the container, waits up to grace,
// then kills what is left.
func (h *Host) Stop(c *Container, grace time.Duration) error {
	signalContainer(c.ID, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for len(containerPIDs(c.ID)) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := killContainer(c.ID); err != nil {
		return err
	}
	c.Stopped = true
	return writeJSON(filepath.Join(h.containerDir(c.ID), "config.json"), c)
}

// Remove kills the container's processes and deletes it.
func (h *Host) Remove(c *Container) error {
	if err := killContainer(c.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(h.containerDir(c.ID)); err != nil {
		return fmt.Errorf("removing container %s: %w", c.Name, err)
	}
	return nil
}

// RemoveAll removes every container (test cleanup).
func (h *Host) RemoveAll() error {
	all, err := h.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range all {
		errs = append(errs, h.Remove(c))
	}
	return errors.Join(errs...)
}

// Logs returns the init process output.
func (h *Host) Logs(c *Container) ([]byte, error) {
	return os.ReadFile(filepath.Join(h.containerDir(c.ID), "log"))
}

// ExecOptions are the per-exec settings.
type ExecOptions struct {
	Env        []string
	WorkingDir string
	User       string
}

// ErrExecNotFound reports a command the container's image does not ship.
var ErrExecNotFound = errors.New("executable file not found in $PATH")

// Command builds the host process for argv run inside the container.
func (h *Host) Command(c *Container, opts ExecOptions, argv []string) (*exec.Cmd, error) {
	return h.command(c, opts, argv)
}

func (h *Host) command(c *Container, opts ExecOptions, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command specified")
	}
	dir := h.containerDir(c.ID)
	env := mergeEnv(c.Env, opts.Env)
	for i, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		env[i] = k + "=" + c.relocate(dir, v)
	}
	env = append(env, MarkerEnv+"="+c.ID)
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = c.relocate(dir, a)
	}
	path, err := lookPathIn(args[0], envValue(env, "PATH"))
	if err != nil {
		return nil, fmt.Errorf("exec: %q: %w", argv[0], ErrExecNotFound)
	}
	wd := opts.WorkingDir
	if wd == "" {
		wd = c.WorkingDir
	}
	hostWD := c.hostPath(dir, wd)
	if st, err := os.Stat(hostWD); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("chdir to cwd (%q) set in config.json failed: no such file or directory", wd)
	}
	cmd := exec.Command(path, args[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = env
	cmd.Dir = hostWD
	return cmd, nil
}

// relocate maps the container paths in s to host paths: every occurrence
// of a private root as a path (the whole word, or a word of a shell script:
// "mkdir -p '/run/x'", "TMUX_TMPDIR=/run/x") moves under the container's
// state directory. One left-to-right pass: replaced text is never rescanned
// (the state directory itself may live under a private root such as /tmp).
func (c *Container) relocate(dir, s string) string {
	if len(c.PrivateRoots) == 0 {
		return s
	}
	quoted := make([]string, len(c.PrivateRoots))
	for i, r := range c.PrivateRoots {
		quoted[i] = regexp.QuoteMeta(r)
	}
	re := regexp.MustCompile(`(?:^|[\s'"=:;(])(` + strings.Join(quoted, "|") + `)`)
	fs := filepath.Join(dir, "fs")
	// The emulator's own files (image bin/ directories, other state) are
	// host paths even when the state directory is under a private root.
	stateRoot := filepath.Dir(filepath.Dir(dir))
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		rootStart, rootEnd := m[2], m[3]
		if rootEnd < len(s) && !strings.ContainsRune("/ \t\n'\";)", rune(s[rootEnd])) {
			continue // a longer name: /runner, /tmpfoo
		}
		if strings.HasPrefix(s[rootStart:], stateRoot+"/") {
			continue
		}
		b.WriteString(s[last:rootStart])
		b.WriteString(fs)
		last = rootStart
	}
	b.WriteString(s[last:])
	return b.String()
}

// HostPath returns the host path of a container path.
func (h *Host) HostPath(c *Container, p string) string {
	return c.hostPath(h.containerDir(c.ID), p)
}

func (c *Container) hostPath(dir, p string) string {
	return c.relocate(dir, filepath.Clean(p))
}

func lookPathIn(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		if st, err := os.Stat(name); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return name, nil
		}
		return "", ErrExecNotFound
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", ErrExecNotFound
}

// mergeEnv returns base with every KEY=VALUE of overrides applied in order
// (last wins), keeping first-seen key order.
func mergeEnv(base, overrides []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range overrides {
		key, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, existing := range out {
			if k, _, _ := strings.Cut(existing, "="); k == key {
				out[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func newID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating container ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// --- process table ---

// containerPIDs lists the live (non-zombie) processes tagged with id.
func containerPIDs(id string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if processInContainer(pid, id) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processInContainer reports whether pid is alive, not a zombie, and tagged
// with container id.
func processInContainer(pid int, id string) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := bytes.LastIndexByte(stat, ')'); i < 0 || i+2 >= len(stat) || stat[i+2] == 'Z' || stat[i+2] == 'X' {
		return false
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	want := []byte(MarkerEnv + "=" + id)
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}

func signalContainer(id string, sig syscall.Signal) {
	for _, pid := range containerPIDs(id) {
		_ = syscall.Kill(pid, sig)
	}
}

// killContainer SIGKILLs the container's processes until none is left
// (a process may fork while it is being killed).
func killContainer(id string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids := containerPIDs(id)
		if len(pids) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s: processes %v survived SIGKILL", id[:12], pids)
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
