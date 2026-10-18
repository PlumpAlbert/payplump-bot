#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("validator", ROOT / "scripts/validate-release-config.py")
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)
IMAGE = "registry.example/payplump@sha256:" + "a" * 64


def config():
    return {"services": {"payplump": {"image": IMAGE, "user": "65532:65532", "read_only": True,
        "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"], "privileged": False,
        "environment": {**{x: "false" for x in validator.FLAGS}, "PGHOST": "db.internal", "PGUSER": "bot",
          "PGDATABASE": "botdb", "PGSSLMODE": "verify-full", "PGPASSFILE": "/run/secrets/pgpass"},
        "logging": {"driver": "json-file", "options": {"max-size": "10m", "max-file": "3"}},
        "secrets": ["pgpass"]}}, "secrets": {"pgpass": {"file": "/private/pgpass"}}}


class Helpers(unittest.TestCase):
    def check(self, value, valid):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "resolved.json"
            p.write_text(json.dumps(value))
            if valid:
                validator.validate(str(p), IMAGE)
            else:
                with self.assertRaises(Exception):
                    validator.validate(str(p), IMAGE)

    def test_resolved_compose_cases(self):
        self.check(config(), True)
        mutations = [
            lambda c: c["services"]["payplump"].update(image="other:latest"),
            lambda c: c["services"]["payplump"].update(privileged=True),
            lambda c: c["services"]["payplump"].update(read_only=False),
            lambda c: c["services"]["payplump"]["environment"].update(TELEGRAM_ENABLED="true"),
            lambda c: c["services"]["payplump"].update(command=["overridden"]),
            lambda c: c["services"]["payplump"].pop("read_only"),
            lambda c: c["services"].update(unused={"image": IMAGE}),
            lambda c: c["services"]["payplump"]["environment"].update(API_TOKEN="synthetic"),
            lambda c: c["services"]["payplump"].update(network_mode="host"),
            lambda c: c["services"]["payplump"]["logging"]["options"].update(**{"max-file": "0"}),
            lambda c: c["services"]["payplump"].update(volumes=[{"type": "bind", "source": "/tmp", "target": "/x"}]),
            lambda c: c["services"]["payplump"]["environment"].update(PGPASSFILE="/tmp/pgpass"),
        ]
        for mutate in mutations:
            c = config(); mutate(c); self.check(c, False)
        c = config(); c["services"]["payplump"]["environment"].pop("LIFECYCLE_ENABLED"); self.check(c, False)
        c = config(); c["services"]["payplump"]["environment"].pop("TELEGRAM_ENABLED"); c["services"]["payplump"]["environment"]["COMMENT"] = "TELEGRAM_ENABLED: false"; self.check(c, False)

    def test_release_shell_with_fake_docker(self):
        with tempfile.TemporaryDirectory() as d:
            bindir = Path(d) / "bin"; bindir.mkdir()
            fake = bindir / "docker"
            fake.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >"$FAKE_LOG"\ncat "$FAKE_CONFIG"\n')
            fake.chmod(0o700)
            c = Path(d) / "config.json"; c.write_text(json.dumps(config()))
            log = Path(d) / "args"
            env = dict(os.environ, PATH=str(bindir) + os.pathsep + os.environ["PATH"], FAKE_LOG=str(log), FAKE_CONFIG=str(c))
            result = subprocess.run([str(ROOT / "scripts/validate-release-config.sh"), IMAGE], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = log.read_text()
            self.assertIn("--env-file /dev/null", args)
            self.assertIn("config --format json", args)
            self.assertNotIn("secret-value", result.stdout + result.stderr)

    def test_restore_dryrun_fake_tools_never_connects(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            log = Path(d) / "calls"
            restore = b / "pg_restore"
            restore.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >>"$CALL_LOG"\necho "TABLE public.synthetic"\n')
            restore.chmod(0o700)
            psql = b / "psql"
            psql.write_text('#!/bin/sh\necho psql-called >>"$CALL_LOG"\nexit 99\n'); psql.chmod(0o700)
            archive = Path(d) / "archive"; archive.write_text("synthetic"); archive.chmod(0o600)
            env = dict(os.environ, PATH=str(b) + os.pathsep + os.environ["PATH"], CALL_LOG=str(log), PGDATABASE="source_db", PGHOST="localhost", PGPORT="5432", PGPASSWORD="do-not-log")
            result = subprocess.run([str(ROOT / "scripts/restore-check.sh"), str(archive), "payplump_restore_check_test"], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn("TABLE", result.stdout + result.stderr)
            self.assertNotIn("do-not-log", result.stdout + result.stderr)
            self.assertNotIn("psql-called", log.read_text())
            for source, target in [("postgresql://x", "payplump_restore_check_test"), ("source_db", "source_db"), ("source_db", "payplump_restore_check_" + "x" * 50)]: 
                env["PGDATABASE"] = source
                bad = subprocess.run([str(ROOT / "scripts/restore-check.sh"), str(archive), target], cwd=ROOT, env=env, capture_output=True)
                self.assertNotEqual(bad.returncode, 0)
            env["PGDATABASE"] = "source_db"; env["PGHOST"] = "remote.example"
            bad = subprocess.run([str(ROOT / "scripts/restore-check.sh"), str(archive), "payplump_restore_check_test"], cwd=ROOT, env=env, capture_output=True)
            self.assertNotEqual(bad.returncode, 0)


if __name__ == "__main__":
    unittest.main()
