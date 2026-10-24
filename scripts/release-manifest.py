"""Create safe, local metadata describing a built image and its source."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

LIMIT_JSON = 2 * 1024 * 1024
LIMIT_MIGRATION = 1024 * 1024
LIMIT_ARCHIVE = 1024 * 1024 * 1024
DIGEST_RE = re.compile(r"^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_.-]+@sha256:[0-9a-f]{64}$")
RUN_NUMBER_RE = re.compile(r"^[1-9][0-9]{0,11}$")
INTENT_FIELDS = {"version", "repository", "commit", "platform", "publish_requested", "previous_image", "run_id", "run_attempt", "image_tag"}

class ManifestError(Exception): pass
def fail(message): raise ManifestError(message)

def read_json(path, private=False):
    p = Path(path)
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(str(p), flags)
        with os.fdopen(fd, "rb") as f:
            info = os.fstat(f.fileno())
            if not stat.S_ISREG(info.st_mode): fail("metadata input must be a regular non-symlink file")
            if private and stat.S_IMODE(info.st_mode) != 0o600: fail("intent file permissions must be exactly 0600")
            if info.st_size > LIMIT_JSON: fail("metadata input exceeds size limit")
            raw = f.read(LIMIT_JSON + 1)
            if len(raw) > LIMIT_JSON: fail("metadata input exceeds size limit")
        return json.loads(raw.decode("utf-8"))
    except ManifestError: raise
    except (OSError, ValueError, UnicodeError): fail("could not read valid metadata input")

def git(source, *args):
    try:
        p = subprocess.run(["git", "-C", str(source), *args], capture_output=True, text=True, timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired): fail("source repository verification failed")
    if p.returncode: fail("source repository verification failed")
    return p.stdout.strip()

def regular_file(path):
    if path.is_symlink() or not path.is_file(): fail("migration must be a regular non-symlink file")

def migrations(source):
    root = source / "internal" / "postgres" / "migrations"
    if any((source / part).is_symlink() for part in ("internal", "internal/postgres", "internal/postgres/migrations")) or not root.is_dir():
        fail("migration directory is missing or unsafe")
    tracked = set(git(source, "ls-files", "--", "internal/postgres/migrations").splitlines())
    files = sorted(root.iterdir(), key=lambda p: p.name)
    result = []
    for p in files:
        if not re.fullmatch(r"[0-9]{3}_[a-z0-9_]+\.sql", p.name): fail("unexpected migration filename")
        regular_file(p)
        if p.relative_to(source).as_posix() not in tracked: fail("migration is not tracked in source checkout")
        if p.stat().st_size > LIMIT_MIGRATION: fail("migration exceeds size limit")
        result.append({"name": p.name, "sha256": hashlib.sha256(p.read_bytes()).hexdigest()})
    if not result: fail("no migrations found")
    return result

def inspect_image(intent, cfg):
    if not isinstance(cfg, list) or len(cfg) != 1 or not isinstance(cfg[0], dict): fail("invalid image inspection data")
    image = cfg[0]; arch = intent["platform"].split("/")[1]
    if image.get("Os") != "linux" or image.get("Architecture") != arch: fail("image platform mismatch")
    config = image.get("Config")
    if not isinstance(config, dict) or config.get("User") != "65532:65532": fail("image user mismatch")
    if config.get("Entrypoint") != ["/payplump"]: fail("image entrypoint mismatch")
    health = config.get("Healthcheck")
    if not isinstance(health, dict) or health.get("Test") != ["CMD", "/payplump", "healthcheck"]: fail("image healthcheck mismatch")
    labels = config.get("Labels")
    if not isinstance(labels, dict) or labels.get("org.opencontainers.image.source") != f"https://github.com/{intent['repository']}": fail("image source label mismatch")
    if labels.get("org.opencontainers.image.revision") != intent["commit"]: fail("image revision label mismatch")
    ident = image.get("Id", "")
    if not isinstance(ident, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", ident): fail("invalid local image id")
    tags = image.get("RepoTags")
    if not isinstance(tags, list) or any(not isinstance(tag, str) for tag in tags) or intent["image_tag"] not in tags:
        fail("intent image tag absent from image")
    return ident

def hash_archive(path):
    p = Path(path)
    digest = hashlib.sha256(); size = 0
    try:
        fd = os.open(str(p), os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "rb") as f:
            if not stat.S_ISREG(os.fstat(f.fileno()).st_mode): fail("archive must be regular non-symlink file")
            while True:
                block = f.read(1024 * 1024)
                if not block: break
                size += len(block)
                if size > LIMIT_ARCHIVE: fail("archive exceeds size limit")
                digest.update(block)
    except ManifestError: raise
    except OSError: fail("could not read archive")
    if size == 0: fail("archive is empty")
    return digest.hexdigest(), size

def create_output(path, data):
    p = Path(path)
    if not p.is_absolute(): fail("output path must be absolute")
    try:
        if p.is_symlink() or p.exists(): fail("output file must be new and not a symlink")
        fd = os.open(str(p), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            os.chmod(p, 0o600); json.dump(data, f, sort_keys=True, separators=(",", ":")); f.write("\n"); f.flush(); os.fsync(f.fileno())
    except ManifestError: raise
    except OSError: fail("could not safely create manifest")

def validate(intent, source, config, archive, digest, published, runner=git):
    if not isinstance(intent, dict) or set(intent) != INTENT_FIELDS or type(intent["version"]) is not int or intent["version"] != 1: fail("invalid release intent")
    if (not isinstance(intent["repository"], str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", intent["repository"])
            or not isinstance(intent["commit"], str) or not re.fullmatch(r"[0-9a-f]{40}", intent["commit"])
            or intent["platform"] not in ("linux/amd64", "linux/arm64")
            or type(intent["publish_requested"]) is not bool
            or not isinstance(intent["run_id"], str) or not RUN_NUMBER_RE.fullmatch(intent["run_id"])
            or not isinstance(intent["run_attempt"], str) or not RUN_NUMBER_RE.fullmatch(intent["run_attempt"])): fail("invalid release intent fields")
    expected_tag = f"ghcr.io/{intent['repository'].lower()}:git-{intent['commit']}-run-{intent['run_id']}-{intent['run_attempt']}"
    if intent["image_tag"] != expected_tag: fail("release intent image tag mismatch")
    if intent["previous_image"] is not None and (not isinstance(intent["previous_image"], str) or not DIGEST_RE.fullmatch(intent["previous_image"]) or intent["previous_image"].split("@", 1)[0] != f"ghcr.io/{intent['repository'].lower()}"):
        fail("invalid previous image in intent")
    if intent["publish_requested"] and intent["previous_image"] is None: fail("publication intent is missing previous image")
    if published not in ("false", "true"): fail("published must be canonical true or false")
    if published == "false" and digest: fail("unpublished manifest must not claim a registry digest")
    source = Path(source)
    if source.is_symlink() or not source.is_dir(): fail("source checkout must be a directory")
    if Path(runner(source, "rev-parse", "--show-toplevel")).resolve() != source.resolve(): fail("source checkout must be the canonical repository root")
    if runner(source, "rev-parse", "HEAD") != intent["commit"]: fail("source checkout commit differs from intent")
    if runner(source, "status", "--porcelain"): fail("source checkout is not clean")
    mig = migrations(source)
    image_id = inspect_image(intent, config)
    archive_hash, archive_size = hash_archive(archive)
    if published == "true":
        if intent["publish_requested"] is not True or not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest): fail("published digest is not authorized by intent")
        if digest.split("@", 1)[0] != f"ghcr.io/{intent['repository'].lower()}": fail("published digest repository mismatch")
        registry_digest = digest
    else: registry_digest = None
    return {"version": 1, "repository": intent["repository"], "commit": intent["commit"], "platform": intent["platform"],
            "os": "linux", "target_arch": intent["platform"].split("/")[1], "postgres_major": 17,
            "application_commit": intent["commit"], "run_id": intent["run_id"], "run_attempt": intent["run_attempt"],
            "previous_image": intent["previous_image"], "published": published == "true", "registry_digest": registry_digest,
            "local_image_id": image_id, "archive_sha256": archive_hash, "archive_bytes": archive_size,
            "migrations": mig}

def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--intent", required=True); p.add_argument("--source-dir", required=True); p.add_argument("--image-config", required=True)
    p.add_argument("--archive", required=True); p.add_argument("--digest", required=True); p.add_argument("--published", required=True); p.add_argument("--output", required=True)
    a = p.parse_args(argv)
    try:
        intent = read_json(a.intent, private=True); config = read_json(a.image_config)
        result = validate(intent, a.source_dir, config, a.archive, a.digest, a.published)
        create_output(a.output, result); return 0
    except ManifestError as exc:
        print(f"release manifest denied: {exc}", file=sys.stderr); return 1
if __name__ == "__main__": raise SystemExit(main())
