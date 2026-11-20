"""Bounded, logged execution of the qualifier's published shell exercises."""
import os
from pathlib import Path
import signal
import subprocess
import time


def _session_pids(session_id):
    # Only signal this explicitly created session. City infrastructure that
    # daemonizes into another session is outside this command's ownership.
    listing = subprocess.check_output(["ps", "-axo", "pid="], text=True, timeout=5)
    found = []
    for value in listing.split():
        pid = int(value)
        try:
            if os.getsid(pid) == session_id:
                found.append(pid)
        except (ProcessLookupError, PermissionError):
            pass
    return found


def _signal_session(session_id, sig):
    for pid in _session_pids(session_id):
        try:
            # Recheck identity after the enumeration; never signal an
            # unrelated process merely because a PID was seen earlier.
            if os.getsid(pid) == session_id:
                os.kill(pid, sig)
        except ProcessLookupError:
            pass


def run_logged(command, *, cwd, env, stdout_path, stderr_path, timeout, grace=2):
    """Run one synchronous exercise; retain partial output on failure."""
    with Path(stdout_path).open("w") as stdout, Path(stderr_path).open("w") as stderr:
        child = subprocess.Popen(command, cwd=cwd, env=env, stdout=stdout,
                                 stderr=stderr, start_new_session=True)
        try:
            return child.wait(timeout=timeout)
        except BaseException:
            # Keep the leader unreaped during cleanup, retaining its identity.
            # GC's exec runner may start another group inside this session.
            try:
                _signal_session(child.pid, signal.SIGTERM)
                time.sleep(grace)
            finally:
                try:
                    _signal_session(child.pid, signal.SIGKILL)
                finally:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    child.wait(timeout=5)
            raise
