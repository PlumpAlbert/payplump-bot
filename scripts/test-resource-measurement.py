#!/usr/bin/env python3
"""Offline policy and process-boundary tests for measure-safe.py."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

SCRIPT = Path(__file__).resolve().parent / "measure-safe.py"


class MeasurementPolicy(unittest.TestCase):
    def run_measure(self, directory, *args, extra_env=None):
        env = {"PATH": str(directory) + os.pathsep + os.environ.get("PATH", os.defpath), "HOME": directory}
        env.update(extra_env or {})
        return subprocess.run([sys.executable, str(SCRIPT), *args], env=env, capture_output=True, text=True, timeout=15)

    def fake_go(self, directory, content):
        executable = Path(directory) / "go"
        executable.write_text("#!" + sys.executable + "\nimport sys\nif sys.argv[1:] == ['version']:\n print('go version go1.24.2 linux/amd64')\n raise SystemExit(0)\n" + content)
        executable.chmod(0o700)
        return executable

    def load_measurement(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("measurement", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_event_counts_require_each_expected_test_exactly_once(self):
        measurement = self.load_measurement()
        expected = ("TestA", "TestB")
        def events(*pairs):
            return [json.dumps({"Action": action, "Test": name}) for action, name in pairs]
        self.assertEqual(measurement.test_event_counts(events(("pass", "TestA"), ("pass", "TestB")), expected), (2, 2, 0, True))
        self.assertFalse(measurement.test_event_counts(events(("pass", "TestA"), ("pass", "TestA")), expected)[3])
        self.assertFalse(measurement.test_event_counts(events(("pass", "TestA"), ("pass", "Other")), expected)[3])
        self.assertFalse(measurement.test_event_counts(events(("fail", "TestA"), ("pass", "TestB")), expected)[3])
        self.assertFalse(measurement.test_event_counts(events(("skip", "TestA"), ("pass", "TestB")), expected)[3])
        self.assertEqual(measurement.test_event_counts(["null", "[]", "{}"], ()), (0, 0, 0, False))

    def test_invalid_source_sha_rejected_before_go(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "called"
            self.fake_go(directory, f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
            result = self.run_measure(directory, "--run", "--profile", "worker-poll", extra_env={"MEASUREMENT_SOURCE_HEAD": "synthetic-token"})
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())
            self.assertNotIn("synthetic-token", result.stderr)

    def test_dry_run_is_null_and_does_not_execute(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "called"
            self.fake_go(directory, f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
            result = self.run_measure(directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads(result.stdout)
            self.assertFalse(report["executed"])
            self.assertIsNone(report["duration_seconds"])
            self.assertIsNone(report["child_max_rss_bytes"])
            self.assertIsNone(report["child_cpu_seconds"])
            self.assertIsNone(report["go_version"])
            self.assertIsNone(report["selected_tests"])
            self.assertIsNone(report["passed_selected_tests"])
            self.assertIn("not aggregate tree peak", report["child_max_rss_scope"])
            self.assertFalse(marker.exists())

    def test_arguments_rejected_before_child_or_git_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "called"
            self.fake_go(directory, f"from pathlib import Path\nPath({str(marker)!r}).touch()\n")
            invalid = [
                ("--run", "--packages", "./internal/importplan", "./internal/importplan"),
                ("--run", "--packages", "./internal/broadcast"),
                ("--run", "--profile", "unit-smoke", "--packages", "./internal/importplan"),
                ("--run", "--profile", "unit-smoke", "--profile", "worker-poll"),
            ]
            for args in invalid:
                with self.subTest(args=args):
                    result = self.run_measure(directory, *args)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(marker.exists())

    def test_abbreviated_options_are_rejected_before_go_or_git(self):
        with tempfile.TemporaryDirectory() as directory:
            go_marker = Path(directory) / "go-called"
            git_marker = Path(directory) / "git-called"
            self.fake_go(directory, f"from pathlib import Path\nPath({str(go_marker)!r}).touch()\n")
            fake_git = Path(directory) / "git"
            fake_git.write_text("#!" + sys.executable + "\nfrom pathlib import Path\nPath(" + repr(str(git_marker)) + ").touch()\n")
            fake_git.chmod(0o700)
            cases = (
                ("--run", "--profile", "worker-poll", "--prof", "restart-fences"),
                ("--run", "--pack", "./internal/importplan"),
            )
            for args in cases:
                with self.subTest(args=args):
                    result = self.run_measure(directory, *args, extra_env={"MEASUREMENT_SOURCE_HEAD": "a" * 40})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("unrecognized arguments", result.stderr)
                    self.assertFalse(go_marker.exists())
                    self.assertFalse(git_marker.exists())

    def test_named_profile_counts_passes_without_exposing_raw_go_output(self):
        with tempfile.TemporaryDirectory() as directory:
            self.fake_go(directory, "import json,sys\nprint(json.dumps({'Action':'pass','Package':'./internal/broadcast','Test':'TestWorkerPollsWithinConcurrencyAndJoinsCancellation'}))\nprint('private synthetic go output', file=sys.stderr)\n")
            result = self.run_measure(directory, "--run", "--profile", "worker-poll", extra_env={"GOFLAGS": "-unsafe", "PGPASSWORD": "secret"})
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads(result.stdout)
            self.assertEqual(report["selected_tests"], 1)
            self.assertEqual(report["passed_selected_tests"], 1)
            self.assertNotIn("private synthetic", result.stdout)
            self.assertNotIn("secret", result.stdout)
            self.assertEqual(report["workload"]["test_regex"], "^TestWorkerPollsWithinConcurrencyAndJoinsCancellation$")

    def test_legacy_package_run_fails_closed_when_no_tests_are_observed(self):
        with tempfile.TemporaryDirectory() as directory:
            self.fake_go(directory, "print('null')\nprint('[]')\n")
            result = self.run_measure(directory, "--run", "--packages", "./internal/importplan")
            self.assertNotEqual(result.returncode, 0)
            report = json.loads(result.stdout)
            self.assertEqual(report["selected_tests"], 0)
            self.assertIn("zero tests observed", report["failure_reason"])

    def test_named_profile_fails_on_zero_tests_and_go_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            self.fake_go(directory, "print('{}')\n")
            result = self.run_measure(directory, "--run", "--profile", "worker-poll")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads(result.stdout)["selected_tests"], 0)
            self.fake_go(directory, "import json,sys\nprint(json.dumps({'Action':'fail','Test':'TestWorkerPollsWithinConcurrencyAndJoinsCancellation'}))\nsys.exit(1)\n")
            result = self.run_measure(directory, "--run", "--profile", "worker-poll")
            self.assertNotEqual(result.returncode, 0)
            report = json.loads(result.stdout)
            self.assertEqual(report["failed_selected_tests"], 1)
            self.assertNotIn("Traceback", result.stdout)

    def test_timeout_still_applies_after_go_closes_output_descriptors(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "closed-output-orphan-wrote"
            child_code = f"import time; time.sleep(0.8); open({str(marker)!r}, 'w').write('orphan')"
            go_code = (
                "import os,subprocess,sys,time\n"
                f"subprocess.Popen([sys.executable, '-c', {child_code!r}], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)\n"
                "os.close(1); os.close(2); time.sleep(30)\n"
            )
            self.fake_go(directory, go_code)
            import contextlib
            import importlib.util
            import io
            from unittest import mock
            spec = importlib.util.spec_from_file_location("measurement_closed_output", SCRIPT)
            measurement = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(measurement)
            measurement.TIMEOUT_SECONDS = 0.2
            output = io.StringIO()
            started = time.monotonic()
            with (mock.patch.object(sys, "argv", [str(SCRIPT), "--run", "--profile", "worker-poll"]),
                  mock.patch.dict(os.environ, {"PATH": str(directory) + os.pathsep + os.environ.get("PATH", os.defpath), "HOME": directory}, clear=True),
                  contextlib.redirect_stdout(output)):
                self.assertEqual(measurement.main(), 124)
            self.assertLess(time.monotonic() - started, 2)
            report = json.loads(output.getvalue())
            self.assertEqual(report["exit_code"], 124)
            self.assertIn("90-second wall-clock limit", report["failure_reason"])
            time.sleep(0.9)
            self.assertFalse(marker.exists(), "descendant survived timeout after output descriptors closed")

    def test_timeout_kills_entire_go_process_group(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "orphan-wrote"
            pid_file = Path(directory) / "child.pid"
            self.fake_go(directory, f"import subprocess,sys,time\nchild=subprocess.Popen([sys.executable,'-c',{('import time; time.sleep(1); open(' + repr(str(marker)) + ', \'w\').write(\'orphan\')')!r}])\nopen({str(pid_file)!r},'w').write(str(child.pid))\ntime.sleep(10)\n")
            import contextlib
            import importlib.util
            import io
            from unittest import mock
            spec = importlib.util.spec_from_file_location("measurement", SCRIPT)
            measurement = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(measurement)
            measurement.TIMEOUT_SECONDS = 0.2
            output = io.StringIO()
            start = time.monotonic()
            with (mock.patch.object(sys, "argv", [str(SCRIPT), "--run", "--profile", "worker-poll"]),
                  mock.patch.dict(os.environ, {"PATH": str(directory) + os.pathsep + os.environ.get("PATH", os.defpath), "HOME": directory}, clear=True),
                  contextlib.redirect_stdout(output)):
                self.assertEqual(measurement.main(), 124)
            self.assertLess(time.monotonic() - start, 2)
            time.sleep(1.1)
            self.assertFalse(marker.exists(), "descendant survived timeout and ran its delayed write")
            self.assertEqual(json.loads(output.getvalue())["exit_code"], 124)


if __name__ == "__main__":
    unittest.main()
