#!/usr/bin/env python3
"""Measure bounded Go unit-test profiles; this is not an application/VPS benchmark."""
import argparse
import json
import os
import platform
import re
import resource
import selectors
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path

SAFE_PACKAGES = ("./internal/importplan", "./internal/postgres", "./cmd/payplump")
PROFILES = {
    "unit-smoke": (SAFE_PACKAGES, None, ()),
    "worker-poll": (("./internal/broadcast",), r"^TestWorkerPollsWithinConcurrencyAndJoinsCancellation$", ("TestWorkerPollsWithinConcurrencyAndJoinsCancellation",)),
    "invoice-observation": (("./internal/payments",), r"^(TestWorkerVerifiedRereadsAllResourcesBeforeFinish|TestWorkerNegativeAndReadFailuresNeverCreatePositiveProof)$", ("TestWorkerVerifiedRereadsAllResourcesBeforeFinish", "TestWorkerNegativeAndReadFailuresNeverCreatePositiveProof")),
    "restart-fences": (("./internal/lifecycle",), r"^(TestEnforcementWorkerConfirmationFailureAndRestartDoNotReplay|TestRenewalWorkerReclaimedUnknownIntentDoesNotReplay)$", ("TestEnforcementWorkerConfirmationFailureAndRestartDoNotReplay", "TestRenewalWorkerReclaimedUnknownIntentDoesNotReplay")),
    "dispatch-retries": (("./internal/outbox",), r"^(TestWorkerIndependentOutcomes|TestWorkerErrorClassification|TestWorkerPersistsCooldownDuringShutdown|TestWorkerBoundedParallelBatches)$", ("TestWorkerIndependentOutcomes", "TestWorkerErrorClassification", "TestWorkerPersistsCooldownDuringShutdown", "TestWorkerBoundedParallelBatches")),
}
MAX_OUTPUT = 32 * 1024 * 1024
TIMEOUT_SECONDS = 90
TEST_TIMEOUT = "60s"


def safe_env():
    names = ("PATH", "HOME", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "TMPDIR", "SSL_CERT_FILE")
    env = {name: os.environ[name] for name in names if name in os.environ}
    env.update({"GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "CGO_ENABLED": "0"})
    return env


def git_value(args):
    result = subprocess.run(["git", *args], capture_output=True, text=True, check=True, timeout=10, env=safe_env())
    return result.stdout.strip()


def cgroup_sample():
    # A single runner cgroup sample after the test, not a peak or application/container metric.
    for path in (Path("/sys/fs/cgroup/memory.current"), Path("/sys/fs/cgroup/memory/memory.usage_in_bytes")):
        try:
            raw = path.open("rb").read(65)
            value = raw.strip()
            if len(raw) <= 64 and value.isdecimal():
                return int(value)
        except OSError:
            pass
    return None


def kill_process_group(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        if process.poll() is None:
            process.kill()


def run_go_test(command):
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=safe_env(), start_new_session=True, bufsize=0)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    captured = bytearray()
    output_exceeded = False
    timed_out = False
    deadline = time.monotonic() + TIMEOUT_SECONDS
    try:
        while selector.get_map() or process.poll() is None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                timed_out = True
                kill_process_group(process)
                break
            if not selector.get_map():
                try:
                    process.wait(timeout=min(remaining, 0.1))
                except subprocess.TimeoutExpired:
                    pass
                continue
            for key, _ in selector.select(min(remaining, 0.1)):
                chunk = os.read(key.fd, 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                if not output_exceeded:
                    available = MAX_OUTPUT - len(captured)
                    captured.extend(chunk[:available])
                    if len(chunk) > available:
                        output_exceeded = True
                        kill_process_group(process)
                        break
            if output_exceeded:
                break
        return (124 if timed_out else process.wait()), bytes(captured), timed_out, output_exceeded
    finally:
        selector.close()
        if process.stdout and not process.stdout.closed:
            process.stdout.close()
        if process.poll() is None:
            kill_process_group(process)
            process.wait()


def go_version():
    try:
        result = subprocess.run(["go", "version"], capture_output=True, text=True, timeout=10, env=safe_env(), check=True)
        if len(result.stdout) > 1024:
            return None
        text = result.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return None
    match = re.fullmatch(r"go version (go[0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[a-z][a-z0-9.]*)?) [A-Za-z0-9_/-]+", text)
    return match.group(1) if match else None


def ci_context():
    if os.environ.get("GITHUB_ACTIONS") != "true":
        return None
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "")
    if re.fullmatch(r"[1-9][0-9]{0,19}", run_id) and re.fullmatch(r"[1-9][0-9]{0,3}", attempt):
        return {"run_id": run_id, "run_attempt": attempt}
    return {"run_id": None, "run_attempt": None}


def test_event_counts(output, expected_tests):
    statuses = {}
    expected = set(expected_tests)
    for raw in output:
        try:
            event = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            continue
        if not isinstance(event, dict):
            continue
        name = event.get("Test")
        action = event.get("Action")
        if not isinstance(name, str) or action not in ("pass", "fail") or (expected and name not in expected):
            continue
        if name in statuses:
            statuses[name] = "duplicate"
        else:
            statuses[name] = action
    selected = len(statuses)
    passed = sum(status == "pass" for status in statuses.values())
    failed = selected - passed
    complete = (set(statuses) == expected and passed == len(expected)) if expected else (selected > 0 and passed == selected)
    return selected, passed, failed, complete


def main():
    parser = argparse.ArgumentParser(allow_abbrev=False)
    parser.add_argument("--run", action="store_true", help="execute the selected safe go test workload")
    parser.add_argument("--packages", nargs="+", help="legacy allowlisted packages (cannot combine with --profile)")
    parser.add_argument("--profile", choices=tuple(PROFILES), help="closed, named unit-test workload")
    parser.add_argument("--image-file", help="optional existing regular local archive; reports only file size")
    for option in ("--profile", "--packages", "--image-file"):
        if sum(arg == option or arg.startswith(option + "=") for arg in sys.argv[1:]) > 1:
            parser.error(option + " may be specified only once")
    args = parser.parse_args()
    source_head_sha = os.environ.get("MEASUREMENT_SOURCE_HEAD") or None
    if source_head_sha and not re.fullmatch(r"[0-9a-f]{40}", source_head_sha):
        parser.error("MEASUREMENT_SOURCE_HEAD must be a canonical 40-character lowercase commit SHA")

    if args.profile and args.packages:
        parser.error("--profile and --packages cannot be combined")
    packages, pattern, expected_tests = PROFILES[args.profile or "unit-smoke"] if not args.packages else (tuple(args.packages), None, ())
    if len(set(packages)) != len(packages):
        parser.error("duplicate packages are not allowed")
    if any(pkg not in SAFE_PACKAGES for pkg in packages) and not args.profile:
        parser.error("only the legacy allowlisted packages are supported")
    if args.packages and args.profile:
        parser.error("--profile and --packages cannot be combined")

    if args.image_file:
        image_path = Path(args.image_file)
        try:
            image_stat = image_path.lstat()
            if stat.S_ISLNK(image_stat.st_mode) or not stat.S_ISREG(image_stat.st_mode):
                parser.error("--image-file must be an existing regular non-symlink file")
            image_bytes = image_stat.st_size if args.run else None
            image_reason = None if args.run else "not measured (dry run)"
        except OSError:
            parser.error("--image-file must be an existing regular non-symlink file")
    else:
        image_bytes, image_reason = None, "not supplied; no archive size measured"

    command = ["go", "test", "-json", "-count=1", "-timeout=" + TEST_TIMEOUT]
    if pattern:
        command.extend(("-run", pattern))
    command.extend(packages)
    report = {
        "report_version": 1,
        "measurement_scope": "synthetic_go_unit_test_and_toolchain_process_tree_not_application_or_vps",
        "runner_architecture": platform.machine(),
        "runner_os": platform.system(),
        "source_commit": None,
        "source_head_sha": source_head_sha,
        "go_version": None,
        "go_version_reason": "not measured (dry run)" if not args.run else "go version unavailable or noncanonical",
        "ci_context": ci_context(),
        "tracked_worktree_dirty": None,
        "workload": {"kind": "go_test", "profile": args.profile or ("legacy-packages" if args.packages else "unit-smoke"), "packages": list(packages), "test_regex": pattern, "expected_tests": list(expected_tests), "command": command},
        "executed": False,
        "exit_code": None,
        "duration_seconds": None,
        "child_max_rss_bytes": None,
        "child_max_rss_scope": "OS-reported completed-child ru_maxrss; not aggregate tree peak or application RSS",
        "child_cpu_seconds": None,
        "child_cpu_scope": "cumulative child CPU for go test and its toolchain/test descendants; not application CPU",
        "runner_cgroup_memory_current_bytes": None,
        "runner_cgroup_memory_reason": "not measured (dry run)" if not args.run else "runner cgroup sample unavailable",
        "selected_tests": 0 if args.run else None,
        "passed_selected_tests": 0 if args.run else None,
        "failed_selected_tests": 0 if args.run else None,
        "test_count_scope": "exact named top-level tests for closed profiles; Go JSON test events otherwise",
        "limits": {"wall_timeout_seconds": TIMEOUT_SECONDS, "go_test_timeout": TEST_TIMEOUT, "combined_output_capture_max_bytes": MAX_OUTPUT, "output_overflow_action": "terminate Go test process group; no child file-size limit"},
        "test_count_reason": "not measured (dry run)" if not args.run else None,
        "image_file_bytes": image_bytes,
        "image_file_reason": image_reason,
        "container_memory_bytes": None,
        "container_memory_reason": "not measured: this runner measurement is not an application container metric",
        "compressed_image_bytes": None,
        "compressed_image_reason": "not measured; compression is not inferred",
    }

    if args.run:
        # Run only go test before snapshotting child usage; version/git queries follow it.
        before = resource.getrusage(resource.RUSAGE_CHILDREN)
        started = time.monotonic()
        spawn_failed = False
        try:
            returncode, output, timed_out, output_exceeded = run_go_test(command)
        except (OSError, subprocess.SubprocessError, ValueError):
            spawn_failed = True
            returncode, output, timed_out, output_exceeded = 127, b"", False, False
        elapsed = time.monotonic() - started
        after = resource.getrusage(resource.RUSAGE_CHILDREN)
        # Snapshot runner child high-water before any go version/git metadata children.
        selected, passed, failed, tests_complete = test_event_counts(output.splitlines(), expected_tests)
        report.update({
            "executed": True,
            "exit_code": returncode,
            "duration_seconds": round(elapsed, 3),
            "child_max_rss_bytes": None if spawn_failed else int(after.ru_maxrss * (1 if sys.platform == "darwin" else 1024)),
            "child_cpu_seconds": None if spawn_failed else round((after.ru_utime - before.ru_utime) + (after.ru_stime - before.ru_stime), 3),
            "selected_tests": selected,
            "passed_selected_tests": passed,
            "failed_selected_tests": failed,
        })
        if timed_out:
            report["failure_reason"] = "Go test exceeded the 90-second wall-clock limit"
        elif spawn_failed:
            report["failure_reason"] = "Go test could not be started with the configured resource limit"
        elif output_exceeded:
            report["exit_code"] = 1
            report["failure_reason"] = "Go test output exceeded the bounded 32 MiB capture limit"
        elif not tests_complete:
            report["exit_code"] = 1
            report["failure_reason"] = "zero tests observed or requested tests did not each pass exactly once"
        report["test_count_reason"] = None if selected else "no test events observed"
        report["go_version"] = go_version()
        if report["go_version"] is not None:
            report["go_version_reason"] = None
        # cgroup sample is intentionally a one-time runner sample, not a peak.
        sample = cgroup_sample()
        if sample is not None:
            report["runner_cgroup_memory_current_bytes"] = sample
            report["runner_cgroup_memory_reason"] = "single post-run runner cgroup sample; not peak, application, or container memory"

    # Metadata children run only after resource.getrusage has been snapshotted.
    try:
        commit = git_value(["rev-parse", "HEAD"])
        if re.fullmatch(r"[0-9a-f]{40}", commit):
            report["source_commit"] = commit
        else:
            report["source_metadata_reason"] = "git commit metadata was not a canonical SHA"
        report["tracked_worktree_dirty"] = bool(git_value(["status", "--porcelain", "--untracked-files=no"]))
    except (OSError, subprocess.SubprocessError):
        report["source_metadata_reason"] = "git metadata unavailable"
    print(json.dumps(report, sort_keys=True))
    return report["exit_code"] or 0


if __name__ == "__main__":
    raise SystemExit(main())
