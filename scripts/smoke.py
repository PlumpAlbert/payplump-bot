"""Check the real binary against a disposable database; no external APIs called."""

import os
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request


def check_cycle():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    settings = {
        **os.environ,
        "HTTP_ADDR": f"127.0.0.1:{port}",
        "HEALTHCHECK_URL": base + "/readyz",
    }
    binary = os.environ.get("PAYPLUMP_BINARY", "./bin/payplump")
    process = subprocess.Popen(
        [binary], env=settings, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("service exited during startup: " + process.communicate()[0])
            try:
                with urllib.request.urlopen(base + "/readyz", timeout=3) as response:
                    if response.status == 200 and response.read() == b"ok\n":
                        break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError("service did not become ready")

        with urllib.request.urlopen(base + "/livez", timeout=3) as response:
            if response.status != 200 or response.read() != b"ok\n":
                raise RuntimeError("unexpected liveness response")
        subprocess.run([binary, "healthcheck"], env=settings, check=True, timeout=5)
        process.send_signal(signal.SIGTERM)
        output = process.communicate(timeout=12)[0]
        if process.returncode != 0:
            raise RuntimeError("unclean SIGTERM shutdown: " + output)
        stopped = subprocess.run(
            [binary, "healthcheck"], env=settings, stdout=subprocess.DEVNULL, timeout=5,
        )
        if stopped.returncode == 0:
            raise RuntimeError("healthcheck accepted a stopped service")
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=5)


if __name__ == "__main__":
    if not os.environ.get("DATABASE_URL"):
        raise SystemExit("Set DATABASE_URL to a disposable development database")
    check_cycle()
    check_cycle()  # Migrations must remain repeatable on the same database.
    print("PASS: startup, migrations, HTTP health, healthcheck exit status, SIGTERM, restart")
