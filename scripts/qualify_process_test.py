import os
from pathlib import Path
import signal
import re
import subprocess
import sys
import tempfile
import unittest

from qualify_process import run_logged


class LoggedExerciseTests(unittest.TestCase):
    def test_normal_result_keeps_both_streams(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            result = run_logged([sys.executable, "-c",
                                 "import sys; print('visible'); print('diagnostic', file=sys.stderr); sys.exit(7)"],
                                cwd=root, env=os.environ, stdout_path=root/"stdout",
                                stderr_path=root/"stderr", timeout=10)
            self.assertEqual(result, 7)
            self.assertIn("visible", (root/"stdout").read_text())
            self.assertIn("diagnostic", (root/"stderr").read_text())

    def test_published_manual_order_cannot_hide_failure_with_session_listing(self):
        doc = Path(__file__).absolute().parent.parent / "docs/tutorials/07-orders.md"
        blocks = re.findall(r"```(?:bash|shell)\n(.*?)\n```", doc.read_text(), re.S)
        selected = [block for block in blocks if "cat > orders/practice-check.toml <<'ORDER'" in block]
        self.assertEqual(len(selected), 1)
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            fake = root/"bin"
            fake.mkdir()
            command = fake/"gc"
            command.write_text("#!/bin/sh\nif [ \"$1\" = order ]; then echo 'deliberate order failure' >&2; exit 17; fi\necho 'session listing succeeded'\n")
            command.chmod(0o755)
            script = root/"published.sh"
            script.write_text(selected[0]+"\n")
            env = dict(os.environ, PATH=str(fake)+":"+os.environ["PATH"])
            result = run_logged(["/bin/bash", str(script)], cwd=root, env=env,
                                stdout_path=root/"stdout", stderr_path=root/"stderr", timeout=10)
            self.assertEqual(result, 17, "a successful later read must not hide an order failure")
            self.assertIn("deliberate order failure", (root/"stderr").read_text())

    def test_timeout_cleans_a_new_group_and_preserves_unrelated_process(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            sentinel = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"],
                                        start_new_session=True)
            code = '''
import os, pathlib, signal, subprocess, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
pathlib.Path('leader').write_text(str(os.getpid()))
subprocess.Popen([sys.executable, '-c', """
import os, pathlib, signal, time
os.setpgid(0, 0)
signal.signal(signal.SIGTERM, signal.SIG_IGN)
pathlib.Path('group-child').write_text(str(os.getpid()))
print('child partial output', flush=True)
time.sleep(60)
"""])
print('parent partial output', flush=True)
print('partial error', file=sys.stderr, flush=True)
time.sleep(60)
'''
            try:
                with self.assertRaises(subprocess.TimeoutExpired):
                    run_logged([sys.executable, "-c", code], cwd=root, env=os.environ,
                               stdout_path=root/"stdout", stderr_path=root/"stderr",
                               timeout=2, grace=0.2)
                self.assertIsNone(sentinel.poll(), "unrelated session was touched")
                self.assertIn("partial output", (root/"stdout").read_text())
                self.assertIn("partial error", (root/"stderr").read_text())
                for name in ("leader", "group-child"):
                    self.assertTrue((root/name).is_file(), name)
                    pid = int((root/name).read_text())
                    state = subprocess.run(["ps", "-p", str(pid), "-o", "stat="],
                                           capture_output=True, text=True, check=False).stdout.strip()
                    self.assertTrue(not state or state.startswith("Z"), (name, pid, state))
            finally:
                for name in ("leader", "group-child"):
                    if (root/name).is_file() and (root/"leader").is_file():
                        pid = int((root/name).read_text())
                        try:
                            if os.getsid(pid) == int((root/"leader").read_text()):
                                os.kill(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                if sentinel.poll() is None:
                    sentinel.kill()
                sentinel.wait()


if __name__ == "__main__":
    unittest.main()
