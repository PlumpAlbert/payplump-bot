"""Check the real binary against a disposable database; no external APIs called."""

import os
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request


FEATURE_GATES = (
    "TELEGRAM_ENABLED",
    "REGISTRATION_ENABLED",
    "PAYMENTS_ENABLED",
    "ACTIVATION_ENABLED",
    "CONFIGURATION_DELIVERY_ENABLED",
    "BROADCAST_ENABLED",
    "INVOICE_LIFECYCLE_ENABLED",
    "INVOICE_OVERDUE_ENFORCEMENT_ENABLED",
    "INVOICE_RENEWAL_ENABLED",
)


def smoke_environment(port, inherited=None):
    base = f"http://127.0.0.1:{port}"
    settings = dict(os.environ if inherited is None else inherited)
    settings.update({
        "HTTP_ADDR": f"127.0.0.1:{port}",
        "HEALTHCHECK_URL": base + "/readyz",
        **{name: "false" for name in FEATURE_GATES},
    })
    return settings


def check_cycle():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    settings = smoke_environment(port)
    binary = os.environ.get("PAYPLUMP_BINARY", "./bin/payplump")
    process = subprocess.Popen(
        [binary, "serve-health"], env=settings, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                process.communicate()
                raise RuntimeError("service exited during startup")
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
        subprocess.run(
            [binary, "healthcheck"], env=settings, check=True, timeout=5,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=12)
        if process.returncode != 0:
            raise RuntimeError("unclean SIGTERM shutdown")
        stopped = subprocess.run(
            [binary, "healthcheck"], env=settings, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, timeout=5,
        )
        if stopped.returncode == 0:
            raise RuntimeError("healthcheck accepted a stopped service")
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=5)


if __name__ == "__main__":
    if not os.environ.get("DATABASE_URL") and not all(
        os.environ.get(name) for name in ("PGHOST", "PGUSER", "PGDATABASE")
    ):
        raise SystemExit("Set DATABASE_URL or PGHOST, PGUSER and PGDATABASE for a disposable database")
    check_cycle()
    check_cycle()  # Migrations must remain repeatable on the same database.
    print("PASS: startup, migrations, HTTP health, healthcheck exit status, SIGTERM, restart")
