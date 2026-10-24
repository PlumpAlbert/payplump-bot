#!/usr/bin/env python3
"""Offline orchestration tests for release-build.sh using only temporary fakes."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
BUILD_SCRIPT = ROOT / "scripts" / "release-build.sh"
COMMIT = "a" * 40
TOKEN = "synthetic-test-token-never-log-this"

FAKE_PREFLIGHT = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
def value(flag): return args[args.index(flag) + 1]
publish = value("--publish")
if publish == "true" and "--require-approval" not in args:
    raise SystemExit("publish preflight omitted mandatory approval verification")
if publish != "true" and "--require-approval" in args:
    raise SystemExit("dry preflight must not require approval API")
log = pathlib.Path(os.environ["FAKE_LOG"])
state = pathlib.Path(os.environ["FAKE_STATE"])
count = int(state.read_text() or "0") + 1 if state.exists() else 1
state.write_text(str(count))
with log.open("a") as f: f.write(json.dumps({"event":"preflight", "count":count, "approval":"--require-approval" in args}) + "\n")
if os.environ.get("FAKE_PREFLIGHT_FAIL") == str(count): raise SystemExit("synthetic preflight failure")
repo, commit, platform = value("--repository"), value("--commit"), value("--platform")
rid, attempt = os.environ["GITHUB_RUN_ID"], os.environ["GITHUB_RUN_ATTEMPT"]
tag = f"ghcr.io/{repo.lower()}:git-{commit}-run-{rid}-{attempt}"
if os.environ.get("FAKE_PREFLIGHT_MISMATCH") == str(count): tag += "-changed"
data = {"version":1,"repository":repo,"commit":commit,"platform":platform,
        "publish_requested":publish == "true","previous_image":value("--previous-image") or None,
        "run_id":rid,"run_attempt":attempt,"image_tag":tag}
out = pathlib.Path(value("--output"))
fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd,"w") as f: json.dump(data,f)
'''

FAKE_MANIFEST = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
def value(flag): return args[args.index(flag)+1]
intent=json.loads(pathlib.Path(value("--intent")).read_text())
published=value("--published")
if published == "true":
    log=pathlib.Path(os.environ["FAKE_LOG"])
    events=[json.loads(x) for x in log.read_text().splitlines()]
    if not any(e["event"] == "push" for e in events): raise SystemExit("published manifest attempted before push")
    digest=value("--digest")
    if digest != "ghcr.io/owner/repo@sha256:" + "d"*64: raise SystemExit("unexpected synthetic digest")
else:
    if value("--digest"): raise SystemExit("local validation must not claim a registry digest")
out=pathlib.Path(value("--output"))
fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,"w") as f: json.dump({"published":published == "true","intent":intent,"digest":value("--digest") or None},f)
with pathlib.Path(os.environ["FAKE_LOG"]).open("a") as f: f.write(json.dumps({"event":"manifest","published":published,"output":out.name})+"\n")
'''

FAKE_GIT = r'''#!/usr/bin/env python3
import os, sys
args=sys.argv[1:]
if "--verify" in args: print(os.environ["RELEASE_COMMIT"])
elif "--show-toplevel" in args: print(os.environ["SOURCE_DIR"])
elif "--porcelain" in args: pass
elif args[:2] == ["ls-files", "--"]: print("internal/postgres/migrations/001_test.sql")
else: raise SystemExit("unexpected fake git arguments: " + repr(args))
'''

FAKE_DOCKER = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
event=" ".join(args[:2]) if args[:1] == ["image"] else args[0]
log=pathlib.Path(os.environ["FAKE_LOG"])
def record(name):
    with log.open("a") as f: f.write(json.dumps({"event":name})+"\n")
if args[:2] == ["buildx", "build"]: record("buildx-build")
elif args[:2] == ["image", "inspect"] and "--format" in args:
    print(json.dumps(["ghcr.io/owner/repo@sha256:" + "d"*64]))
elif args[:2] == ["image", "inspect"]:
    tag=args[-1]
    cfg={"Os":"linux","Architecture":"amd64","Id":"sha256:"+"b"*64,"RepoTags":[tag],
         "Config":{"User":"65532:65532","Entrypoint":["/payplump"],"Healthcheck":{"Test":["CMD","/payplump","healthcheck"]},
                   "Labels":{"org.opencontainers.image.source":"https://github.com/Owner/Repo","org.opencontainers.image.revision":os.environ["RELEASE_COMMIT"]}}}
    print(json.dumps([cfg])); record("inspect")
elif args[:3] == ["image", "save", "--output"]:
    pathlib.Path(args[3]).write_bytes(b"synthetic-image-archive"); record("save")
elif args[:1] == ["login"]:
    supplied=sys.stdin.read()
    if supplied != os.environ["GITHUB_TOKEN"]: raise SystemExit("wrong synthetic login token")
    record("login")
elif args[:1] == ["push"]: record("push")
elif args[:1] == ["logout"]: record("logout")
else: raise SystemExit("unexpected fake docker command: " + repr(args))
'''

class ReleaseBuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.tools = self.root / "trusted-tools"
        self.bin = self.root / "fake-bin"
        self.source = self.root / "source"
        self.output = self.root / "output"
        self.tools.mkdir(); self.bin.mkdir(); self.source.mkdir(); self.output.mkdir(mode=0o700)
        shutil.copy2(BUILD_SCRIPT, self.tools / "release-build.sh")
        (self.tools / "release-preflight.py").write_text(FAKE_PREFLIGHT)
        (self.tools / "release-manifest.py").write_text(FAKE_MANIFEST)
        (self.source / "Dockerfile").write_text("# intentionally never built by Docker\n")
        migration = self.source / "internal/postgres/migrations/001_test.sql"
        migration.parent.mkdir(parents=True); migration.write_text("SELECT 1;\n")
        self.log = self.root / "events.jsonl"
        self.intent = self.output / "intent.json"
        self.publish = False
        self._write_intent()
        for name, content in (("git", FAKE_GIT), ("docker", FAKE_DOCKER)):
            path = self.bin / name
            path.write_text(content); path.chmod(0o700)
        python_dir = str(Path(sys.executable).parent)
        self.env = {
            "PATH": str(self.bin) + os.pathsep + python_dir + os.pathsep + "/usr/bin:/bin",
            "HOME": str(self.root / "home"), "TMPDIR": str(self.root),
            "GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main",
            "GITHUB_REPOSITORY": "Owner/Repo", "GITHUB_RUN_ID": "123",
            "GITHUB_RUN_ATTEMPT": "1", "GITHUB_ACTOR": "release-requester",
            "GITHUB_ACTOR_ID": "10", "GH_TOKEN": "synthetic-read-token",
            "GITHUB_TOKEN": TOKEN, "RELEASE_CONFIRM": "publish-reviewed-commit",
            "RELEASE_COMMIT": COMMIT, "SOURCE_DIR": str(self.source),
            "FAKE_LOG": str(self.log), "FAKE_STATE": str(self.root / "preflight-count"),
        }
        self.assertEqual(Path(shutil.which("docker", path=self.env["PATH"])).parent, self.bin)
        self.assertEqual(Path(shutil.which("git", path=self.env["PATH"])).parent, self.bin)

    def tearDown(self):
        self.tmp.cleanup()

    def _write_intent(self):
        data = {"version":1,"repository":"Owner/Repo","commit":COMMIT,"platform":"linux/amd64",
                "publish_requested":self.publish,"previous_image":"ghcr.io/owner/repo@sha256:"+"c"*64 if self.publish else None,
                "run_id":"123","run_attempt":"1",
                "image_tag":f"ghcr.io/owner/repo:git-{COMMIT}-run-123-1"}
        self.intent.write_text(json.dumps(data)); self.intent.chmod(0o600)

    def run_build(self):
        return subprocess.run([str(self.tools / "release-build.sh"), "--intent", str(self.intent),
                               "--source-dir", str(self.source), "--output-dir", str(self.output)],
                              env=self.env, text=True, capture_output=True, check=False)

    def events(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_dry_build_validates_and_only_emits_unpublished_manifest(self):
        result = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        events = self.events()
        self.assertEqual([e["event"] for e in events].count("preflight"), 1)
        self.assertEqual([e["event"] for e in events].count("manifest"), 1)
        self.assertEqual([e for e in events if e["event"] == "manifest"][0]["published"], "false")
        self.assertFalse(any(e["event"] in ("login", "push", "logout") for e in events))
        self.assertTrue((self.output / "release-manifest.json").is_file())
        self.assertTrue((self.output / "image.tar").is_file())
        self.assertFalse((self.output / "local-validation-manifest.json").exists())
        self.assertTrue(all(e.get("approval") is False for e in events if e["event"] == "preflight"))

    def test_publish_uses_unique_fresh_intents_validates_before_push_and_final_manifest_after(self):
        self.publish = True; self._write_intent()
        result = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        events = self.events()
        preflights = [e for e in events if e["event"] == "preflight"]
        self.assertEqual(len(preflights), 2)
        self.assertTrue(all(e["approval"] for e in preflights))
        self.assertTrue((self.output / "fresh-intent-1.json").is_file())
        self.assertTrue((self.output / "fresh-intent-2.json").is_file())
        self.assertNotEqual(self.output / "fresh-intent-1.json", self.output / "fresh-intent-2.json")
        names = [e["event"] for e in events]
        self.assertLess(names.index("manifest"), names.index("preflight", names.index("preflight") + 1))
        self.assertLess(names.index("preflight", names.index("preflight") + 1), names.index("login"))
        self.assertLess(names.index("login"), names.index("push"))
        self.assertLess(names.index("push"), max(i for i, name in enumerate(names) if name == "manifest"))
        manifests = [e for e in events if e["event"] == "manifest"]
        self.assertEqual([e["published"] for e in manifests], ["false", "true"])
        self.assertEqual(manifests[0]["output"], "local-validation-manifest.json")
        self.assertEqual(manifests[1]["output"], "release-manifest.json")
        self.assertNotIn(TOKEN, self.log.read_text())
        self.assertFalse((self.output / ".docker-config").exists())

    def test_fresh_intent_mismatch_never_uses_docker(self):
        self.env["FAKE_PREFLIGHT_MISMATCH"] = "1"
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(e["event"].startswith(("buildx", "inspect", "save", "login", "push")) for e in self.events()))

    def test_preflight_failure_never_uses_docker(self):
        self.env["FAKE_PREFLIGHT_FAIL"] = "1"
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(e["event"] in ("buildx-build", "inspect", "save", "login", "push") for e in self.events()))

    def test_publish_preflight_failure_after_local_validation_never_logs_in_or_pushes(self):
        self.publish = True; self._write_intent()
        self.env["FAKE_PREFLIGHT_FAIL"] = "2"
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        events = self.events()
        self.assertTrue(any(e["event"] == "manifest" and e["published"] == "false" for e in events))
        self.assertFalse(any(e["event"] in ("login", "push") for e in events))
        self.assertFalse((self.output / "release-manifest.json").exists())

    def test_publish_rerun_is_rejected_before_preflight_or_docker(self):
        self.publish = True; self._write_intent()
        self.env["GITHUB_RUN_ATTEMPT"] = "2"
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("new workflow run", result.stderr)
        self.assertEqual(self.events(), [])

    def test_unexpected_output_file_is_rejected_without_subprocesses(self):
        (self.output / "unrelated").write_text("must not be overwritten or uploaded")
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.events(), [])

if __name__ == "__main__":
    unittest.main()
