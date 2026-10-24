#!/usr/bin/env python3
import builtins
import importlib.util
import json
import os
import shlex
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("validator", ROOT / "scripts/validate-release-config.py")
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)
smoke_spec = importlib.util.spec_from_file_location("smoke", ROOT / "scripts/smoke.py")
smoke = importlib.util.module_from_spec(smoke_spec)
smoke_spec.loader.exec_module(smoke)
IMAGE = "registry.example/payplump@sha256:" + "a" * 64


def fake_env(**extra):
    # CI may carry live database credentials: fake executables never inherit them.
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": tempfile.gettempdir()}
    env.update(extra)
    return env


def config():
    return {"services": {"payplump": {"image": IMAGE, "user": "65532:65532", "read_only": True,
        "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"], "privileged": False,
        "env_file": [{"path": "/tmp/runtime.env", "required": True}],
        "environment": {**dict.fromkeys(validator.CLEARED_DATABASE_ENV, ""), "PGHOST": "db.internal", "PGUSER": "bot",
          "PGDATABASE": "botdb", "PGSSLMODE": "verify-full", "PGPASSFILE": "/run/secrets/pgpass"},
        "logging": {"driver": "json-file", "options": {"max-size": "10m", "max-file": "3"}},
        "secrets": ["pgpass"], "healthcheck": {"test": ["CMD", "/payplump", "healthcheck"]}}},
        "secrets": {"pgpass": {"file": "/private/pgpass"}}}


class Helpers(unittest.TestCase):
    def check(self, value, valid):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "resolved.json"
            runtime = Path(d) / "runtime.env"
            runtime.write_text("SYNTHETIC_SECRET=value\\n")
            runtime.chmod(0o600)
            pgpass = Path(d) / "pgpass"
            pgpass.write_text("synthetic pgpass\\n")
            pgpass.chmod(0o600)
            env_files = value["services"]["payplump"].get("env_file", [])
            if env_files and env_files[0].get("path") == "/tmp/runtime.env":
                env_files[0]["path"] = str(runtime)
            if value["secrets"]["pgpass"]["file"] == "/private/pgpass":
                value["secrets"]["pgpass"]["file"] = str(pgpass)
            p.write_text(json.dumps(value))
            if valid:
                validator.validate(str(p), IMAGE)
            else:
                with self.assertRaises(Exception):
                    validator.validate(str(p), IMAGE)

    def test_validator_never_reads_runtime_env_file(self):
        with tempfile.TemporaryDirectory() as d:
            runtime = Path(d) / "runtime.env"; runtime.write_text("SYNTHETIC_SENTINEL_DO_NOT_READ=value\\n"); runtime.chmod(0o600)
            pgpass = Path(d) / "pgpass"; pgpass.write_text("synthetic\\n"); pgpass.chmod(0o600)
            value = config()
            value["services"]["payplump"]["env_file"][0]["path"] = str(runtime)
            value["secrets"]["pgpass"]["file"] = str(pgpass)
            resolved = Path(d) / "resolved.json"; resolved.write_text(json.dumps(value))
            real_open = builtins.open
            def guarded_open(path, *args, **kwargs):
                if Path(path) == runtime:
                    raise AssertionError("validator attempted to read runtime env contents")
                return real_open(path, *args, **kwargs)
            with mock.patch("builtins.open", side_effect=guarded_open):
                validator.validate(str(resolved), IMAGE)

    def test_resolved_compose_cases(self):
        self.check(config(), True)
        normalized = config()
        normalized["services"]["payplump"].update(command=None, entrypoint=None)
        normalized["services"]["payplump"]["secrets"] = [{"source": "pgpass", "target": "/run/secrets/pgpass"}]
        self.check(normalized, True)
        # Actual Compose v5.6.1 omits the default required=true in JSON.
        default_required = config()
        default_required["services"]["payplump"]["env_file"][0].pop("required")
        self.check(default_required, True)
        mutations = [
            lambda c: c["services"]["payplump"].update(image="other:latest"),
            lambda c: c["services"]["payplump"].update(privileged=True),
            lambda c: c["services"]["payplump"].update(cap_add=["SYS_ADMIN"]),
            lambda c: c["services"]["payplump"].update(security_opt=["no-new-privileges:true", "seccomp:unconfined"]),
            lambda c: c["services"]["payplump"].update(volumes=[{"type": "volume", "source": "host-root", "target": "/x"}]),
            lambda c: c["services"]["payplump"].update(build={"context": "."}),
            lambda c: c["services"]["payplump"].update(post_start=[{"command": "unsafe"}]),
            lambda c: c["services"]["payplump"]["environment"].update(UNREVIEWED_SETTING="true"),
            lambda c: c["services"]["payplump"].update(env_file=[{"path": "/tmp/ambiguous.env", "required": True}]),
            lambda c: c["services"]["payplump"]["env_file"].append({"path": "/tmp/second.env", "required": True}),
            lambda c: c["services"]["payplump"].update(read_only=False),
            lambda c: c["services"]["payplump"]["env_file"][0].update(required=False),
            lambda c: c["services"]["payplump"]["env_file"][0].update(format="raw"),
            lambda c: c["services"]["payplump"]["environment"].update(TELEGRAM_BOT_TOKEN="synthetic-token"),
            lambda c: c["services"]["payplump"].update(command=["overridden"]),
            lambda c: c["services"]["payplump"].update(command=[]),
            lambda c: c["services"]["payplump"].update(entrypoint=[]),
            lambda c: c["services"]["payplump"].update(secrets=[{"source": "pgpass", "target": "/run/secrets/other"}]),
            lambda c: c["services"]["payplump"].pop("read_only"),
            lambda c: c["services"].update(unused={"image": IMAGE}),
            lambda c: c["services"]["payplump"]["environment"].update(API_TOKEN="synthetic"),
            lambda c: c["services"]["payplump"].update(network_mode="host"),
            lambda c: c["services"]["payplump"]["logging"]["options"].update(**{"max-file": "0"}),
            lambda c: c["services"]["payplump"].update(volumes=[{"type": "bind", "source": "/tmp", "target": "/x"}]),
            lambda c: c["services"]["payplump"]["environment"].update(PGPASSFILE="/tmp/pgpass"),
            lambda c: c["services"]["payplump"]["healthcheck"].update(test=["CMD", "/payplump", "migrate"]),
            lambda c: c["services"]["payplump"]["healthcheck"].update(test=["CMD-SHELL", "echo unsafe"]),
            lambda c: c["services"]["payplump"]["healthcheck"].update(test=["CMD", "/bin/sh", "-c", "unsafe"]),
            lambda c: c["services"]["payplump"]["healthcheck"].update(disable=True),
            lambda c: c["services"]["payplump"]["healthcheck"].update(test=["NONE"]),
            lambda c: c["services"]["payplump"].pop("healthcheck"),
        ]
        for mutate in mutations:
            c = config(); mutate(c); self.check(c, False)
        for secret_name in ("TELEGRAM_BOT_TOKEN", "INVOICE_NINJA_API_TOKEN", "SUI_API_TOKEN", "PAYMENT_PHONE"):
            c = config(); c["services"]["payplump"]["environment"][secret_name] = "SYNTHETIC_ONLY"; self.check(c, False)
        for key in validator.CLEARED_DATABASE_ENV:
            c = config(); c["services"]["payplump"]["environment"][key] = "SYNTHETIC_OVERRIDE"; self.check(c, False)
            c = config(); c["services"]["payplump"]["environment"].pop(key); self.check(c, False)
        c = config(); c["services"]["payplump"]["environment"]["DATABASE_URL"] = "postgres://inline-secret"; self.check(c, False)
        c = config(); c["services"]["payplump"]["environment"]["PGPASSWORD"] = "inline-secret"; self.check(c, False)
        c = config(); c["services"]["payplump"]["env_file"][0]["path"] = str(ROOT / ".env.example"); self.check(c, False)
        c = config(); c["services"]["payplump"].pop("env_file"); self.check(c, False)

    def test_release_template_declares_imported_gates_and_image_user(self):
        compose = (ROOT / "compose.release.yaml").read_text()
        self.assertIn("PAYPLUMP_ENV_FILE", compose)
        self.assertIn("path:", compose)
        self.assertNotIn("ENABLED: \"false\"", compose)
        dockerfile = (ROOT / "Dockerfile").read_text()
        self.assertEqual(dockerfile.count("USER 65532:65532"), 1)
        self.assertLess(dockerfile.index("COPY --from=build /out/payplump /payplump"), dockerfile.index("USER 65532:65532"))

    def test_release_compose_real_json_without_reading_runtime_contents(self):
        docker = shutil.which("docker")
        if not docker:
            self.skipTest("Docker Compose unavailable; no local config renderer")
        with tempfile.TemporaryDirectory() as d:
            runtime = Path(d) / "runtime.env"
            # Rendering metadata must not parse even a malformed secret file.
            runtime.write_text('UNTERMINATED="SYNTHETIC_SENTINEL_FROM_FILE\\n')
            runtime.chmod(0o600)
            pgpass = Path(d) / "pgpass"
            pgpass.write_text("synthetic.invalid:5432:synthetic:synthetic:SYNTHETIC_ONLY\\n")
            pgpass.chmod(0o600)
            env = fake_env(PATH=str(Path(docker).parent) + os.pathsep + os.defpath,
                HOME=d, DOCKER_CONFIG=str(Path(d) / "docker-config"),
                DOCKER_HOST="unix://" + str(Path(d) / "nonexistent.sock"),
                PAYPLUMP_IMAGE=IMAGE, PAYPLUMP_ENV_FILE=str(runtime), PGPASSFILE=str(pgpass),
                PGHOST="synthetic.invalid", PGPORT="5432", PGUSER="synthetic", PGDATABASE="synthetic", PGSSLMODE="verify-full")
            result = subprocess.run([docker, "compose", "--env-file", "/dev/null", "-f", "compose.release.yaml",
                "config", "--no-env-resolution", "--format", "json"], cwd=ROOT, env=env,
                capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, "synthetic release Compose metadata render failed")
            self.assertNotIn("SYNTHETIC_SENTINEL_FROM_FILE", result.stdout + result.stderr)
            resolved = Path(d) / "resolved.json"
            resolved.write_text(result.stdout)
            validator.validate(str(resolved), IMAGE)
            actual = json.loads(result.stdout)["services"]["payplump"]["environment"]
            for key in validator.CLEARED_DATABASE_ENV:
                self.assertEqual(actual.get(key), "", "runtime PG override not clamped: " + key)

    def test_dev_compose_resolves_lifecycle_settings_without_starting_containers(self):
        docker = shutil.which("docker")
        if not docker:
            self.skipTest("Docker Compose unavailable; no local config renderer")
        with tempfile.TemporaryDirectory() as d:
            env = fake_env(
                PATH=str(Path(docker).parent) + os.pathsep + os.defpath,
                HOME=d,
                DOCKER_CONFIG=str(Path(d) / ".docker"),
                PGHOST="synthetic.invalid",
                PGPORT="15432",
                PGUSER="synthetic_user",
                PGDATABASE="synthetic_db",
                PGPASSWORD="SYNTHETIC_ONLY",
                PGSSLMODE="disable",
                INVOICE_LIFECYCLE_WORKERS="2",
                INVOICE_OVERDUE_WORKERS="3",
            )
            result = subprocess.run(
                [docker, "compose", "--env-file", "/dev/null", "-f", "compose.dev.yaml", "config", "--format", "json"],
                cwd=ROOT, env=env, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, "synthetic Compose config render failed")
            service = json.loads(result.stdout)["services"]["app"]
            self.assertEqual(service.get("command"), ["serve-health"])
            resolved = service["environment"]
            expected = {
                "INVOICE_LIFECYCLE_ENABLED": "false",
                "INVOICE_LIFECYCLE_WEBHOOK_SECRET": "",
                "INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS": "",
                "INVOICE_LIFECYCLE_WORKERS": "2",
                "INVOICE_LIFECYCLE_POLL_INTERVAL": "1s",
                "INVOICE_LIFECYCLE_RECHECK_INTERVAL": "5m",
                "INVOICE_LIFECYCLE_HTTP_TIMEOUT": "5s",
                "INVOICE_LIFECYCLE_MAX_ACCOUNTS": "1000",
                "INVOICE_OVERDUE_ENFORCEMENT_ENABLED": "false",
                "INVOICE_OVERDUE_EXCLUSIVE_WRITER": "false",
                "INVOICE_OVERDUE_WORKERS": "3",
                "INVOICE_OVERDUE_MAX_ACCOUNTS": "1000",
                "INVOICE_OVERDUE_POLL_INTERVAL": "1s",
                "INVOICE_OVERDUE_HTTP_TIMEOUT": "5s",
                "INVOICE_RENEWAL_ENABLED": "false",
                "INVOICE_RENEWAL_EXCLUSIVE_WRITER": "false",
                "INVOICE_RENEWAL_WORKERS": "1",
                "INVOICE_RENEWAL_MAX_ACCOUNTS": "1000",
                "INVOICE_RENEWAL_POLL_INTERVAL": "1s",
                "INVOICE_RENEWAL_HTTP_TIMEOUT": "5s",
            }
            for name, value in expected.items():
                self.assertEqual(resolved.get(name), value, "unexpected resolved setting: " + name)

    def test_smoke_captures_disabled_feature_gates_and_silences_probes(self):
        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self):
                return b"ok\n"

        class Process:
            returncode = 0

            def poll(self):
                return None

            def send_signal(self, signum):
                self.signal = signum

            def communicate(self, timeout=None):
                return "synthetic probe output", None

            def kill(self):
                pass

        process = Process()
        inherited = {name: "true" for name in smoke.FEATURE_GATES}
        inherited["PGPASSWORD"] = "SYNTHETIC_CREDENTIAL"
        with mock.patch.dict(os.environ, inherited, clear=True):
            with mock.patch.object(smoke.subprocess, "Popen", return_value=process) as popen:
                with mock.patch.object(smoke.subprocess, "run", side_effect=[
                    subprocess.CompletedProcess([], 0), subprocess.CompletedProcess([], 1),
                ]) as run:
                    with mock.patch.object(smoke.urllib.request, "urlopen", return_value=Response()):
                        smoke.check_cycle()
        self.assertEqual(popen.call_args.args[0], ["./bin/payplump", "serve-health"])
        captured = popen.call_args.kwargs["env"]
        self.assertIs(popen.call_args.kwargs["stdout"], subprocess.DEVNULL)
        self.assertIs(popen.call_args.kwargs["stderr"], subprocess.DEVNULL)
        for gate in smoke.FEATURE_GATES:
            self.assertEqual(captured[gate], "false", gate)
        self.assertEqual(captured["PGPASSWORD"], "SYNTHETIC_CREDENTIAL")
        self.assertEqual(len(run.call_args_list), 2)
        for call in run.call_args_list:
            self.assertIs(call.kwargs["stdout"], subprocess.DEVNULL)
            self.assertIs(call.kwargs["stderr"], subprocess.DEVNULL)

    def test_smoke_does_not_report_inherited_credentials_or_probe_output(self):
        class Process:
            def poll(self):
                return 1

            def communicate(self, timeout=None):
                return "SYNTHETIC_CREDENTIAL synthetic probe output", None

        with mock.patch.dict(os.environ, {"PGPASSWORD": "SYNTHETIC_CREDENTIAL"}, clear=True):
            with mock.patch.object(smoke.subprocess, "Popen", return_value=Process()):
                with self.assertRaises(RuntimeError) as raised:
                    smoke.check_cycle()
        self.assertNotIn("SYNTHETIC_CREDENTIAL", str(raised.exception))
        self.assertNotIn("synthetic probe output", str(raised.exception))

    def test_release_shell_with_fake_docker(self):
        with tempfile.TemporaryDirectory() as d:
            bindir = Path(d) / "bin"; bindir.mkdir()
            fake = bindir / "docker"
            fake.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >>"$FAKE_LOG"\ncat "$FAKE_CONFIG"\n')
            fake.chmod(0o700)
            c = Path(d) / "config.json"
            runtime = Path(d) / "runtime.env"; runtime.write_text("SYNTHETIC_SECRET=value\\n"); runtime.chmod(0o600)
            pgpass = Path(d) / "pgpass"; pgpass.write_text("synthetic pgpass\\n"); pgpass.chmod(0o600)
            fixture = config(); fixture["services"]["payplump"]["env_file"][0]["path"] = str(runtime); fixture["secrets"]["pgpass"]["file"] = str(pgpass)
            c.write_text(json.dumps(fixture))
            log = Path(d) / "args"
            env = fake_env(PATH=str(bindir) + os.pathsep + os.environ["PATH"], FAKE_LOG=str(log), FAKE_CONFIG=str(c))
            result = subprocess.run([str(ROOT / "scripts/validate-release-config.sh"), IMAGE], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = log.read_text()
            self.assertIn("--env-file /dev/null", args)
            self.assertIn("config --no-env-resolution --format json", args)
            self.assertEqual(len(args.splitlines()), 2)
            self.assertIn("probe.yaml", args.splitlines()[0])
            self.assertIn("compose.release.yaml", args.splitlines()[1])
            self.assertNotIn("secret-value", result.stdout + result.stderr)

    def test_release_renderer_rejects_unsafe_compose_before_runtime_config(self):
        with tempfile.TemporaryDirectory() as d:
            bindir = Path(d) / "bin"; bindir.mkdir()
            fake = bindir / "docker"
            marker = Path(d) / "actual-runtime-rendered"
            fake.write_text('#!/bin/sh\ncase "$*" in *probe.yaml*) exit 1;; esac\ntouch "$ACTUAL_RENDER_MARKER"\n')
            fake.chmod(0o700)
            env = fake_env(PATH=str(bindir) + os.pathsep + os.environ["PATH"], ACTUAL_RENDER_MARKER=str(marker))
            result = subprocess.run([str(ROOT / "scripts/validate-release-config.sh"), IMAGE], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists(), "unsafe Compose reached runtime configuration")
            self.assertIn("opaque env_file", result.stderr)

    def test_restore_dryrun_fake_tools_never_connects(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            log = Path(d) / "calls"
            restore = b / "pg_restore"
            restore.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >>"$CALL_LOG"\necho "TABLE public.synthetic"\n')
            restore.chmod(0o700)
            psql = b / "psql"
            psql.write_text('#!/bin/sh\necho psql-called >>"$CALL_LOG"\nexit 99\n'); psql.chmod(0o700)
            archive = Path(d) / "archive"; archive.write_bytes(b"PGDMPSYNTHETIC_ONLY"); archive.chmod(0o600)
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], CALL_LOG=str(log), PGDATABASE="source_db", PGHOST="localhost", PGPORT="5432", PGPASSWORD="do-not-log")
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

    def test_restore_apply_uses_private_preflight_result(self):
        # Executables below are local stubs. No PostgreSQL or Docker is contacted.
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            log = Path(d) / "calls"
            restore = b / "pg_restore"
            restore.write_text('#!/bin/sh\ncase "$1" in --list) echo "TABLE public.synthetic";; *) echo restore-apply >>"$CALL_LOG";; esac\n')
            restore.chmod(0o700)
            psql = b / "psql"
            psql.write_text('#!/bin/sh\n[ "$PGHOST" = 127.0.0.1 ] || exit 91\n[ "${PGHOSTADDR-unset}" = unset ] || exit 92\n[ "${PGSERVICE-unset}" = unset ] || exit 93\n[ "${PGSERVICEFILE-unset}" = unset ] || exit 94\n[ "${PGOPTIONS-unset}" = unset ] || exit 95\n[ "$PGSSLMODE" = verify-full ] || exit 96\nprintf "%s" "$*" >"$QUERY_LOG"\nprintf "1\\n"\n')
            psql.chmod(0o700)
            archive = Path(d) / "archive"; archive.write_bytes(b"PGDMPSYNTHETIC_ONLY"); archive.chmod(0o600)
            query = Path(d) / "query"
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], CALL_LOG=str(log), QUERY_LOG=str(query),
                       PGDATABASE="source_db", PGHOST="localhost", PGPORT="5432", PGSSLMODE="verify-full",
                       PGHOSTADDR="198.51.100.9", PGSERVICE="synthetic-private", PGSERVICEFILE="synthetic-private", PGOPTIONS="synthetic-private")
            result = subprocess.run([str(ROOT / "scripts/restore-check.sh"), "--apply", str(archive), "payplump_restore_check_test"], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, "fake restore apply failed")
            self.assertEqual(log.read_text().strip(), "restore-apply")
            self.assertIn("!~ '^pg_'", query.read_text())
            self.assertNotIn("synthetic-private", result.stdout + result.stderr)

    def test_measurement_strips_goflags_and_disables_module_downloads(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            captured = Path(d) / "environment"
            go = b / "go"
            go.write_text('#!/bin/sh\nif [ "$1" = version ]; then printf "go version go1.24.2 linux/amd64\\n"; else printf "%s\\n" "${GOFLAGS-unset}" "$GOENV" "$GOWORK" "$GOTOOLCHAIN" "$GOPROXY" "$GOSUMDB" "$CGO_ENABLED" "$*" > ' + shlex.quote(str(captured)) + '; printf "{\\\"Action\\\":\\\"pass\\\",\\\"Package\\\":\\\"./internal/importplan\\\",\\\"Test\\\":\\\"TestSynthetic\\\"}\\n"; fi\n')
            go.chmod(0o700)
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], GOFLAGS="-exec=SYNTHETIC_UNSAFE")
            result = subprocess.run([str(ROOT / "scripts/measure-safe.py"), "--run", "--packages", "./internal/importplan"], cwd=ROOT, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(captured.read_text().splitlines(), ["unset", "off", "off", "local", "off", "off", "0", "test -json -count=1 -timeout=60s ./internal/importplan"])
            report = json.loads(result.stdout)
            self.assertTrue(report["executed"])
            self.assertEqual(report["workload"]["kind"], "go_test")
            self.assertEqual(report["go_version"], "go1.24.2")

    def test_archive_name_cannot_override_restore_database(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            log = Path(d) / "arguments"
            restore = b / "pg_restore"
            restore.write_text('#!/bin/sh\nprintf "BEGIN\\n" >>"$CALL_LOG"\nprintf "%s\\n" "$@" >>"$CALL_LOG"\n[ "$1" != --list ] || echo "TABLE public.synthetic"\nexit 0\n')
            restore.chmod(0o700)
            psql = b / "psql"; psql.write_text('#!/bin/sh\necho 1\n'); psql.chmod(0o700)
            archive_name = "--dbname=production"
            archive = Path(d) / archive_name; archive.write_bytes(b"PGDMPSYNTHETIC_ONLY"); archive.chmod(0o600)
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], CALL_LOG=str(log), PGHOST="localhost", PGDATABASE="source_db", PGPORT="5432")
            result = subprocess.run([str(ROOT / "scripts/restore-check.sh"), "--apply", archive_name, "payplump_restore_check_test"], cwd=d, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, "fake option-shaped archive restore failed")
            calls = [x.splitlines() for x in log.read_text().split("BEGIN\n") if x]
            self.assertEqual(len(calls), 2)
            for args in calls:
                self.assertEqual(args[-2:], ["--", archive_name])
            self.assertIn("--dbname=payplump_restore_check_test", calls[1][:-2])

    def test_restore_uses_gnu_stat_without_mixed_fallback_stdout(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            for name, body in {
                "uname": '#!/bin/sh\necho Linux\n',
                "stat": '#!/bin/sh\nif [ "$1" = -c ] && [ "$2" = %a ]; then echo "$FAKE_MODE"; else echo GNU-filesystem-report; exit 1; fi\n',
                "pg_restore": '#!/bin/sh\necho "TABLE public.synthetic"\n',
            }.items():
                p = b / name; p.write_text(body); p.chmod(0o700)
            archive = Path(d) / "archive"; archive.write_bytes(b"PGDMPSYNTHETIC_ONLY"); archive.chmod(0o600)
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], PGHOST="localhost", PGDATABASE="source_db", PGPORT="5432", FAKE_MODE="600")
            args = [str(ROOT / "scripts/restore-check.sh"), str(archive), "payplump_restore_check_test"]
            result = subprocess.run(args, cwd=d, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, "GNU mode-0600 archive was incorrectly denied")
            env["FAKE_MODE"] = "644"
            denied = subprocess.run(args, cwd=d, env=env, capture_output=True, text=True)
            self.assertNotEqual(denied.returncode, 0, "non-private archive mode was accepted")

    def test_restore_rejects_noncustom_archive_before_tools(self):
        with tempfile.TemporaryDirectory() as d:
            b = Path(d) / "bin"; b.mkdir()
            marker = Path(d) / "invoked"
            restore = b / "pg_restore"
            restore.write_text('#!/bin/sh\ntouch "$CALL_LOG"\n')
            restore.chmod(0o700)
            archive = Path(d) / "not-custom"; archive.write_text("SYNTHETIC_NOT_PGDMP"); archive.chmod(0o600)
            env = fake_env(PATH=str(b) + os.pathsep + os.environ["PATH"], CALL_LOG=str(marker), PGHOST="localhost", PGDATABASE="source_db")
            result = subprocess.run([str(ROOT / "scripts/restore-check.sh"), str(archive), "payplump_restore_check_test"], cwd=d, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists(), "noncustom archive reached pg_restore")

    def test_restore_rehearsal_requires_github_actions_before_docker(self):
        with tempfile.TemporaryDirectory() as d:
            bindir = Path(d) / "bin"; bindir.mkdir()
            marker = Path(d) / "docker-called"
            docker = bindir / "docker"
            docker.write_text('#!/bin/sh\nprintf called >"$CALL_MARKER"\nexit 99\n')
            docker.chmod(0o700)
            script = str(ROOT / "scripts/restore-rehearsal-ci.sh")
            for value in (None, "false"):
                env = fake_env(PATH=str(bindir) + os.pathsep + os.defpath, CALL_MARKER=str(marker))
                if value is not None:
                    env["GITHUB_ACTIONS"] = value
                result = subprocess.run(["sh", script], cwd=ROOT, env=env, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(marker.exists(), "Docker was reached without GitHub Actions")
                self.assertNotIn("synthetic-local-password", result.stdout + result.stderr)

    def test_restore_rehearsal_rejects_invalid_preflight_metadata_before_docker(self):
        with tempfile.TemporaryDirectory() as d:
            bindir = Path(d) / "bin"; bindir.mkdir()
            marker = Path(d) / "docker-called"
            docker = bindir / "docker"
            docker.write_text('#!/bin/sh\nprintf called >"$CALL_MARKER"\nexit 99\n')
            docker.chmod(0o700)
            base = {
                "PATH": str(bindir) + os.pathsep + os.defpath,
                "CALL_MARKER": str(marker),
                "GITHUB_ACTIONS": "true",
                "RESTORE_REHEARSAL_CONTAINER": "0123456789abcdef",
                "GITHUB_RUN_ID": "12345",
                "GITHUB_RUN_ATTEMPT": "1",
                "PGHOST": "127.0.0.1",
                "PGPORT": "15433",
                "PGDATABASE": "workflow_test",
                "PGUSER": "workflow_test",
                "PGPASSWORD": "synthetic-local-password",
                "PGSSLMODE": "disable",
                "PGOPTIONS": "",
                "DATABASE_URL": "",
            }
            invalid = [
                ("RESTORE_REHEARSAL_CONTAINER", "not-a-container-id"),
                ("GITHUB_RUN_ID", "0"), ("GITHUB_RUN_ID", "01"),
                ("GITHUB_RUN_ID", "1;touch"), ("GITHUB_RUN_ID", "1" * 13),
                ("GITHUB_RUN_ATTEMPT", "0"), ("GITHUB_RUN_ATTEMPT", "01"),
                ("GITHUB_RUN_ATTEMPT", "$(id)"), ("GITHUB_RUN_ATTEMPT", "1" * 13),
                ("PGHOST", "synthetic.invalid"), ("PGPORT", "5432"),
                ("PGDATABASE", "production"), ("PGUSER", "other"),
                ("PGPASSWORD", "not-the-synthetic-password"),
                ("PGSSLMODE", "verify-full"), ("PGOPTIONS", "-c search_path=public"),
                ("DATABASE_URL", "postgres://untrusted.invalid/live"),
            ]
            cases = [(name, value) for name, value in invalid]
            cases.append(("PGOPTIONS", None))
            for name, value in cases:
                with self.subTest(name=name, value=value):
                    env = dict(base)
                    if value is None:
                        env.pop(name)
                    else:
                        env[name] = value
                    marker.unlink(missing_ok=True)
                    result = subprocess.run(["sh", str(ROOT / "scripts/restore-rehearsal-ci.sh")], cwd=ROOT, env=env, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0, name + " was accepted")
                    self.assertFalse(marker.exists(), name + " reached Docker")
                    self.assertNotIn("synthetic-local-password", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
