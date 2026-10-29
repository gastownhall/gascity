package main

// The supervisor's tmux socket directory: no gc command replaces or starts
// the supervisor where its tmux sockets would move away from the sessions
// the supervisor before it preserved.
//
// tmux keeps a -L socket under TMUX_TMPDIR (tmux.SocketDir), so a supervisor
// reaches the city's tmux servers only in the directory its own TMUX_TMPDIR
// names. Stopping the supervisor leaves its sessions running (KillMode=process
// on systemd, the preserve-on-signal env on both platforms), so the
// supervisor after it has to keep its sockets where they are. In another
// directory it finds none of those servers, and the tmux provider claims each
// preserved pane as a live runtime it cannot list (LiveUnlisted), which
// refuses that session's restart (SESSION-START-024) until an operator ends
// the pane.
//
// The service files record TMUX_TMPDIR (withRecordedTmuxTmpdir), so a start
// by the service manager itself (a crash restart, a reboot) keeps the
// directory the file names. Before gc replaces or starts the supervisor it
// compares the directory the new one would use with where the sessions are:
// the running supervisor's own TMUX_TMPDIR, or with none running, the
// installed service's while a tmux server there still answers.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

var (
	// supervisorProcessEnvValue reads one variable of a running process's
	// environment. Exposed as a package var so tests stub the process read.
	supervisorProcessEnvValue = proctable.ProcessEnvValue
	// supervisorSystemdManagerEnvironment dumps the systemd user manager's
	// environment (`systemctl --user show-environment`), which a unit
	// inherits for every variable it does not set; a failure carries
	// systemctl's stderr. Exposed as a package var so tests stub the manager.
	supervisorSystemdManagerEnvironment = func() (string, error) {
		out, err := exec.Command("systemctl", "--user", "show-environment").Output()
		if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
			if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		return string(out), err
	}
	// supervisorLiveTmuxSockets lists the sockets in a socket directory that
	// a tmux server may still answer on. Exposed as a package var so tests
	// stub the default directory, which the host's own tmux servers share.
	supervisorLiveTmuxSockets = tmux.LiveSockets
)

// supervisorTmuxSocketProbeTimeout bounds the dials that look for a tmux
// server still answering in the installed service's socket directory.
const supervisorTmuxSocketProbeTimeout = 2 * time.Second

// errSystemdUserManagerUnreachable reports that `systemctl --user
// show-environment` failed: the systemd user manager cannot be reached from
// this process (cron, sudo -u and su leave no XDG_RUNTIME_DIR to find it by).
// installSupervisorSystemd's availability check runs the same command.
var errSystemdUserManagerUnreachable = errors.New("systemctl --user could not reach the systemd user manager")

// The remedies a refusal names. A drift restart of a service-managed
// supervisor starts the replacement from its service file, which takes the
// service manager's TMUX_TMPDIR when the file records none (one an earlier gc
// wrote), whatever the shell running gc start has; an install records the
// value, which keeps every later start in place.
const (
	supervisorRunningTmuxSocketDirRemedy = "To keep the sessions where they are, rerun with TMUX_TMPDIR set as the active supervisor has it. To move them, stop or drain agents intentionally with 'gc supervisor stop --wait', then rerun."
	supervisorIdleTmuxSocketDirRemedy    = "To keep the sessions where they are, rerun with TMUX_TMPDIR set as the installed service has it. To move them, start the supervisor that way, stop or drain agents intentionally with 'gc supervisor stop --wait', then rerun."
	supervisorRestartTmuxSocketDirRemedy = "To keep the sessions where they are, run 'gc supervisor install' with TMUX_TMPDIR set as the active supervisor has it, so its service file records that value, then rerun 'gc start'. To move them, stop or drain agents intentionally with 'gc supervisor stop --wait', then rerun 'gc start'."
)

// withRecordedTmuxTmpdir returns env with a TMUX_TMPDIR entry, empty when no
// tier set one: tmux reads an empty TMUX_TMPDIR as unset. A service file that
// records it leaves the service manager nothing to hand the supervisor in its
// place, so every start of the service keeps the socket directory the file
// names, the one the install guard compares.
func withRecordedTmuxTmpdir(env []supervisorServiceEnvVar) []supervisorServiceEnvVar {
	if slices.ContainsFunc(env, func(v supervisorServiceEnvVar) bool { return v.Name == tmux.TmpdirEnv }) {
		return env
	}
	env = append(env, supervisorServiceEnvVar{Name: tmux.TmpdirEnv})
	slices.SortFunc(env, func(a, b supervisorServiceEnvVar) int { return strings.Compare(a.Name, b.Name) })
	return env
}

// serviceDataTmuxTmpdir is the TMUX_TMPDIR the service file data renders
// gives the supervisor.
func serviceDataTmuxTmpdir(data *supervisorServiceData) string {
	for _, v := range data.ExtraEnv {
		if v.Name == tmux.TmpdirEnv {
			return v.Value
		}
	}
	return ""
}

// supervisorTmuxService is what the guards read off one platform's installed
// supervisor service: where its file is, the TMUX_TMPDIR a file records, and
// the service manager's own value, which a file that records none (one an
// earlier gc wrote) hands every start of the supervisor.
type supervisorTmuxService struct {
	path     string
	recorded func(content []byte) (string, bool)
	manager  func() (string, error)
}

func systemdTmuxService() supervisorTmuxService {
	return supervisorTmuxService{
		path:     supervisorSystemdServicePath(),
		recorded: func(content []byte) (string, bool) { return systemdUnitEnvValue(content, tmux.TmpdirEnv) },
		manager:  systemdManagerTmuxTmpdir,
	}
}

func launchdTmuxService() supervisorTmuxService {
	return supervisorTmuxService{
		path:     supervisorLaunchdPlistPath(),
		recorded: func(content []byte) (string, bool) { return launchdPlistEnvValue(content, tmux.TmpdirEnv) },
		manager:  func() (string, error) { return supervisorLaunchctlGetenv(tmux.TmpdirEnv), nil },
	}
}

// tmuxTmpdir is the TMUX_TMPDIR a start of the service whose file holds
// content gives the supervisor.
func (s supervisorTmuxService) tmuxTmpdir(content []byte) (string, error) {
	if value, ok := s.recorded(content); ok {
		return value, nil
	}
	return s.manager()
}

// systemdManagerTmuxTmpdir is the TMUX_TMPDIR the systemd user manager hands
// a unit that sets none, "" when it holds none. systemd prints a value that
// holds whitespace or a shell metacharacter as $'...', which is reported
// rather than guessed at.
func systemdManagerTmuxTmpdir() (string, error) {
	out, err := supervisorSystemdManagerEnvironment()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errSystemdUserManagerUnreachable, err)
	}
	for _, line := range strings.Split(out, "\n") {
		value, ok := strings.CutPrefix(line, tmux.TmpdirEnv+"=")
		if !ok {
			continue
		}
		if strings.HasPrefix(value, "$'") {
			return "", fmt.Errorf("the systemd user manager's %s is shell-escaped (%s)", tmux.TmpdirEnv, value)
		}
		return value, nil
	}
	return "", nil
}

// describeTmuxTmpdir names a TMUX_TMPDIR value for an operator message.
func describeTmuxTmpdir(value string) string {
	if value == "" {
		return "TMUX_TMPDIR unset"
	}
	return "TMUX_TMPDIR=" + strconv.Quote(value)
}

// supervisorRunningTmuxSocketDirMove describes how a supervisor started with
// TMUX_TMPDIR next, in place of the running supervisor pid, would keep its
// tmux sockets in another directory than the sessions pid preserves, or
// returns "" when it would not.
func supervisorRunningTmuxSocketDirMove(pid int, next string) (string, error) {
	running, err := supervisorProcessEnvValue(pid, tmux.TmpdirEnv)
	if err != nil {
		return "", fmt.Errorf("cannot read active supervisor pid %d %s: %w", pid, tmux.TmpdirEnv, err)
	}
	from, to := tmux.SocketDir(running), tmux.SocketDir(next)
	if from == to {
		return "", nil
	}
	return fmt.Sprintf("active supervisor pid %d keeps its tmux sockets in %s (%s), and its replacement would keep them in %s (%s), where it cannot reach the sessions the active supervisor preserves.",
		pid, from, describeTmuxTmpdir(running), to, describeTmuxTmpdir(next)), nil
}

// supervisorIdleTmuxSocketDirMove is supervisorRunningTmuxSocketDirMove with
// no supervisor running: the sessions the last one preserved are on tmux
// servers in the directory of the TMUX_TMPDIR svc's file, installed, gives
// the supervisor, so a supervisor started with next strands them while one
// of those servers still answers.
//
// A file that records no TMUX_TMPDIR gives the supervisor the service
// manager's. Where the systemd user manager cannot be reached, the error says
// so (errSystemdUserManagerUnreachable), unless assumeManagerDefault: then
// the manager's default, which holds none, stands in for its value, and a
// move names that assumption and how to reach the manager.
func supervisorIdleTmuxSocketDirMove(svc supervisorTmuxService, installed []byte, next string, assumeManagerDefault bool) (string, error) {
	value, err := svc.tmuxTmpdir(installed)
	assumption := ""
	if assumeManagerDefault && errors.Is(err, errSystemdUserManagerUnreachable) {
		assumption = fmt.Sprintf(" The service file records no %s, so a start of the service takes the systemd user manager's, which cannot be read (%v); the manager's default, %s unset, is assumed. To compare the manager's own value, rerun where systemctl --user reaches it (for example with XDG_RUNTIME_DIR=/run/user/%d).",
			tmux.TmpdirEnv, err, tmux.TmpdirEnv, os.Getuid())
		value, err = "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot tell where the installed supervisor service %s keeps its tmux sockets: %w", svc.path, err)
	}
	from, to := tmux.SocketDir(value), tmux.SocketDir(next)
	if from == to {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), supervisorTmuxSocketProbeTimeout)
	defer cancel()
	live, err := supervisorLiveTmuxSockets(ctx, from)
	if err != nil {
		return "", fmt.Errorf("looking for tmux servers in %s: %w", from, err)
	}
	if len(live) == 0 {
		return "", nil
	}
	return fmt.Sprintf("tmux servers still answer in %s (%s), where the installed supervisor service keeps its tmux sockets (%s), and a supervisor started now would keep them in %s (%s), where it cannot reach those sessions.%s",
		from, strings.Join(live, ", "), describeTmuxTmpdir(value), to, describeTmuxTmpdir(next), assumption), nil
}

// installSupervisorService installs data as goos's platform service, unless
// the install would move the supervisor's tmux sockets away from the
// sessions it preserves (refuseSupervisorInstallTmuxSocketDirMove).
func installSupervisorService(goos string, data *supervisorServiceData, stdout, stderr io.Writer) int {
	switch goos {
	case "darwin":
		if refuseSupervisorInstallTmuxSocketDirMove(launchdTmuxService(), supervisorLaunchdTemplate, data, stderr) {
			return 1
		}
		return installSupervisorLaunchd(data, stdout, stderr)
	case "linux":
		if refuseSupervisorInstallTmuxSocketDirMove(systemdTmuxService(), supervisorSystemdTemplate, data, stderr) {
			return 1
		}
		return installSupervisorSystemd(data, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gc supervisor install: not supported on %s\n", goos) //nolint:errcheck // best-effort stderr
		return 1
	}
}

// refuseSupervisorInstallTmuxSocketDirMove reports on stderr, and refuses, an
// install of data as svc that would move the supervisor's tmux sockets away
// from the sessions it preserves. The install writes a changed service file,
// then replaces the running supervisor (a systemd warm refresh, a launchd
// reload) or starts one, either way with the file's TMUX_TMPDIR; an unchanged
// file restarts nothing. A file the install cannot render or read, and a
// systemd user manager it cannot reach, are the install's to report: without
// the manager it writes and starts nothing.
func refuseSupervisorInstallTmuxSocketDirMove(svc supervisorTmuxService, tmpl string, data *supervisorServiceData, stderr io.Writer) bool {
	content, err := renderSupervisorTemplate(tmpl, data)
	if err != nil {
		return false
	}
	installed, err := os.ReadFile(svc.path)
	switch {
	case err == nil && string(installed) == content:
		return false
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return false
	}
	next := serviceDataTmuxTmpdir(data)
	if pid := supervisorAliveHook(); pid != 0 {
		move, err := supervisorRunningTmuxSocketDirMove(pid, next)
		return refuseTmuxSocketDirMove("gc supervisor install", move, err, supervisorRunningTmuxSocketDirRemedy, stderr)
	}
	if err != nil {
		return false // nothing installed: no sessions of its to strand
	}
	move, err := supervisorIdleTmuxSocketDirMove(svc, installed, next, false)
	if errors.Is(err, errSystemdUserManagerUnreachable) {
		return false // installSupervisorSystemd's availability check reports it
	}
	return refuseTmuxSocketDirMove("gc supervisor install", move, err, supervisorIdleTmuxSocketDirRemedy, stderr)
}

// refuseForkedSupervisorTmuxSocketDirMove reports on stderr, and refuses,
// forking a supervisor outside its service, which inherits this process's
// TMUX_TMPDIR, while the sessions the installed service's supervisor
// preserved would be out of its reach. The caller has found no supervisor
// running. The fork does not go through the service manager, so a systemd
// user manager this process cannot reach (cron, sudo -u, su) does not refuse
// it: its default stands in for the TMUX_TMPDIR it would give a unit that
// records none.
func refuseForkedSupervisorTmuxSocketDirMove(goos string, stderr io.Writer) bool {
	var svc supervisorTmuxService
	switch goos {
	case "darwin":
		svc = launchdTmuxService()
	case "linux":
		svc = systemdTmuxService()
	default:
		return false
	}
	installed, err := os.ReadFile(svc.path)
	if err != nil {
		return false // nothing installed: no sessions of its to strand
	}
	move, err := supervisorIdleTmuxSocketDirMove(svc, installed, os.Getenv(tmux.TmpdirEnv), true)
	return refuseTmuxSocketDirMove("gc supervisor start", move, err, supervisorIdleTmuxSocketDirRemedy, stderr)
}

// refuseSupervisorRestartTmuxSocketDirMove refuses the drift restart spec
// describes when the replacement supervisor would keep its tmux sockets in
// another directory than the running one (supervisorRunningTmuxSocketDirMove).
func refuseSupervisorRestartTmuxSocketDirMove(spec restartSpec) error {
	remedy := supervisorRunningTmuxSocketDirRemedy
	if spec.SystemdManaged || spec.LaunchdManaged {
		remedy = supervisorRestartTmuxSocketDirRemedy
	}
	next, err := supervisorRestartTmuxTmpdir(spec)
	move := ""
	if err == nil {
		move, err = supervisorRunningTmuxSocketDirMove(spec.PID, next)
	}
	switch {
	case err != nil:
		return fmt.Errorf("%w; refusing the restart because a new tmux socket directory would strand the sessions the active supervisor preserves. %s", err, remedy)
	case move != "":
		return fmt.Errorf("%s Refusing the restart. %s", move, remedy)
	}
	return nil
}

// supervisorRestartTmuxTmpdir is the TMUX_TMPDIR the drift restart spec
// describes starts the replacement supervisor with: a service-managed one
// gets its installed service's (supervisorTmuxService.tmuxTmpdir), a direct
// one this process's, whose environment it inherits (spawnDetachedSupervisor).
func supervisorRestartTmuxTmpdir(spec restartSpec) (string, error) {
	if !spec.SystemdManaged && !spec.LaunchdManaged {
		return os.Getenv(tmux.TmpdirEnv), nil
	}
	svc := systemdTmuxService()
	if spec.LaunchdManaged {
		svc = launchdTmuxService()
	}
	installed, err := os.ReadFile(svc.path)
	if err != nil {
		return "", fmt.Errorf("reading the supervisor service file: %w", err)
	}
	return svc.tmuxTmpdir(installed)
}

// refuseTmuxSocketDirMove reports command's refusal of a start the guard
// found would move the tmux sockets (move), or could not check (err), with
// remedy, and whether it refused.
func refuseTmuxSocketDirMove(command, move string, err error, remedy string, stderr io.Writer) bool {
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v. Refusing to start the supervisor because a new tmux socket directory would strand the sessions it preserves. %s\n", command, err, remedy) //nolint:errcheck // best-effort stderr
	case move != "":
		fmt.Fprintf(stderr, "%s: %s Refusing to start the supervisor there. %s\n", command, move, remedy) //nolint:errcheck // best-effort stderr
	default:
		return false
	}
	return true
}
