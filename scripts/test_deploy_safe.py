"""Failure-path checks for the independent deployment worker, without live services."""
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile
import unittest


class DeploymentRecovery(unittest.TestCase):
    def run_worker(self, failure):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, home, tools = root / "repo", root / "home", root / "tools"
            run = repo / ".yesmem/deploy/test"
            data = home / ".claude/yesmem"
            for directory in [repo / "scripts", run, data, tools, home / ".local/bin"]:
                directory.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(Path(__file__).with_name("deploy-safe.sh"), repo / "scripts/deploy-safe.sh")
            db = sqlite3.connect(data / "yesmem.db")
            db.execute("CREATE TABLE agents (status TEXT)")
            db.commit()
            db.close()
            binary = home / ".local/bin/yesmem"
            binary.write_text("#!/bin/sh\necho previous\n")
            binary.chmod(0o755)
            commands = {
                "systemctl": 'printf "%s\\n" "$*" >> "$TEST_EVENTS"\n',
                "rclone": 'printf "rclone %s\\n" "$1" >> "$TEST_EVENTS"\n'
                          'if [ "$1" = check ] && [ "$TEST_FAIL" = archive ]; then exit 23; fi\n',
                "make": 'printf "#!/bin/sh\\necho new\\n" > "$HOME/.local/bin/yesmem"\n'
                        'if [ "$TEST_FAIL" = install ]; then exit 42; fi\n',
                "curl": 'printf \'{"status":"ok"}\\n\'\n',
            }
            for name, body in commands.items():
                path = tools / name
                path.write_text("#!/bin/sh\n" + body)
                path.chmod(0o755)
            events = root / "events"
            env = dict(os.environ, HOME=str(home), PATH=str(tools) + ":" + os.environ["PATH"],
                       TEST_EVENTS=str(events), TEST_FAIL=failure)
            result = subprocess.run(["bash", str(repo / "scripts/deploy-safe.sh"), "--worker",
                                     str(run), "test-remote:backups"], env=env, capture_output=True)
            return (result.returncode, events.read_text(), binary.read_text(),
                    (run / "backup/yesmem.db").exists(), (run / "exit-code").read_text().strip(),
                    (run / "deploy.log").read_text())

    def test_failed_install_restores_binary_and_services(self):
        code, events, binary, backup, status, log = self.run_worker("install")
        self.assertEqual(code, 42, log)
        self.assertEqual(status, "42")
        self.assertIn("previous", binary)
        self.assertIn("restart yesmem.service yesmem-proxy.service", events)
        self.assertTrue(backup)

    def test_failed_archive_keeps_local_backup_and_new_binary(self):
        code, events, binary, backup, status, log = self.run_worker("archive")
        self.assertEqual(code, 23, log)
        self.assertIn("new", binary)
        self.assertTrue(backup)
        self.assertNotIn("restart yesmem.service yesmem-proxy.service", events)

    def test_success_removes_only_verified_backup(self):
        code, events, binary, backup, status, log = self.run_worker("")
        self.assertEqual(code, 0, log)
        self.assertEqual(status, "0")
        self.assertIn("rclone check", events)
        self.assertIn("new", binary)
        self.assertFalse(backup)


if __name__ == "__main__":
    unittest.main()
