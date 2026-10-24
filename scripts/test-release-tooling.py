import importlib.util
import json
import hashlib
import os
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stderr
import io
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
def load(name, file):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / file)
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module); return module
pre = load("release_preflight", "release-preflight.py")
manifest = load("release_manifest", "release-manifest.py")

class Result:
    returncode = 0
    def __init__(self, stdout): self.stdout = stdout

class CIAPIMock:
    def __init__(self, commit, runs=None, jobs=None, workflow=None, details=None):
        self.commit=commit; self.workflow_id=387650116; self.run_id=40053518866; self.attempt=1; self.calls=[]
        self.workflow=workflow or {"id":self.workflow_id,"path":pre.WORKFLOW_PATH,"state":"active"}
        self.runs=runs if runs is not None else [self.run()]
        self.jobs=jobs if jobs is not None else [self.job(i+1,n) for i,n in enumerate(pre.REQUIRED_CHECKS)]
        self.details=details or [self.detail()]
        self.detail_calls=0
    def run(self, **changes):
        data={"id":self.run_id,"run_attempt":self.attempt,"workflow_id":self.workflow_id,"path":pre.WORKFLOW_PATH,"head_sha":self.commit,"head_branch":"main","event":"push","status":"completed","conclusion":"success"}
        data.update(changes); return data
    def detail(self, **changes): return self.run(**changes)
    def job(self, ident, name, **changes):
        data={"id":ident,"name":name,"head_sha":self.commit,"run_id":self.run_id,"run_attempt":self.attempt,"workflow_name":pre.WORKFLOW_NAME,"status":"completed","conclusion":"success"}
        data.update(changes); return data
    def __call__(self, argv, **kwargs):
        self.calls.append(argv)
        self.assert_common(argv,kwargs)
        path=argv[-1]
        paginated="--paginate" in argv and "--slurp" in argv
        if path==f"repos/Owner/Repo/actions/workflows/ci.yml":
            if paginated: raise AssertionError("workflow lookup must be plain GET")
            value=self.workflow
        elif path.startswith(f"repos/Owner/Repo/actions/workflows/{self.workflow_id}/runs?"):
            if not paginated: raise AssertionError("workflow run list must be paginated/slurped")
            value=[{"total_count":len(self.runs),"workflow_runs":self.runs}]
        elif path==f"repos/Owner/Repo/actions/runs/{self.run_id}":
            if paginated: raise AssertionError("run details must be plain GET")
            ix=min(self.detail_calls,len(self.details)-1); value=self.details[ix]; self.detail_calls+=1
        elif path==f"repos/Owner/Repo/actions/runs/{self.run_id}/attempts/{self.attempt}/jobs?per_page=100":
            if not paginated: raise AssertionError("attempt jobs must be paginated/slurped")
            value=[{"total_count":len(self.jobs),"jobs":self.jobs}]
        else: raise AssertionError(f"unexpected API path: {path}")
        return Result(json.dumps(value if not paginated else value))
    @staticmethod
    def assert_common(argv, kwargs):
        if argv[:2] != ["gh","api"]: raise AssertionError(argv)
        if kwargs.get("timeout") != 20 or kwargs.get("capture_output") is not True: raise AssertionError(kwargs)

class ReleaseToolingTests(unittest.TestCase):
    def setUp(self):
        self.env = {"GITHUB_ACTIONS":"true", "GITHUB_REF":"refs/heads/main", "GITHUB_REPOSITORY":"Owner/Repo", "GITHUB_RUN_ID":"123", "GITHUB_RUN_ATTEMPT":"2"}
        self.sha = "a" * 40
        self.args = type("Args", (), {"repository":"Owner/Repo", "commit":self.sha, "platform":"linux/amd64", "publish":"false", "confirm":"", "previous_image":""})()
        self.good_digest = "ghcr.io/owner/repo@sha256:" + "c" * 64

    def test_preflight_rejects_bad_context_and_options_before_commands(self):
        variants = [("GITHUB_ACTIONS", "false"), ("GITHUB_REF", "refs/heads/feature"), ("GITHUB_REPOSITORY", "other/repo"), ("GITHUB_RUN_ID", "0"), ("GITHUB_RUN_ID", "1"*13), ("GITHUB_RUN_ATTEMPT", "01")]
        for key, value in variants:
            env = dict(self.env, **{key:value})
            with self.assertRaises(pre.PreflightError): pre.validate_args(self.args, env)
            with redirect_stderr(io.StringIO()):
                self.assertEqual(pre.main(["--commit",self.sha,"--repository","Owner/Repo","--platform","linux/amd64","--publish","false","--output","/tmp/not-created"],env,lambda *a,**k:self.fail("runner must not be called")),1)
        for field, value in (("repository", "other/repo"), ("commit", "A"*40), ("platform", "linux/386"), ("publish", "False"), ("previous_image", "ghcr.io/other/repo@sha256:"+"d"*64), ("previous_image", "latest")):
            args = type("Args", (), dict(vars(type(self.args))))(); setattr(args, field, value)
            with self.assertRaises(pre.PreflightError): pre.validate_args(args, self.env)
        for confirmation, previous in (("", self.good_digest), (pre.CONFIRMATION, "")):
            args = type("Args", (), dict(vars(type(self.args))))(); args.publish="true"; args.confirm=confirmation; args.previous_image=previous
            with self.assertRaises(pre.PreflightError): pre.validate_args(args, self.env)
        args = type("Args", (), dict(vars(type(self.args))))(); args.confirm="wrong"
        with self.assertRaises(pre.PreflightError): pre.validate_args(args, self.env)
        publish = type("Args", (), dict(vars(type(self.args))))()
        publish.publish="true"; publish.confirm=pre.CONFIRMATION; publish.previous_image=self.good_digest
        with self.assertRaisesRegex(pre.PreflightError,"new workflow dispatch"):
            pre.validate_args(publish,self.env)
        dryrun = type("Args", (), dict(vars(type(self.args))))()
        self.assertEqual(pre.validate_args(dryrun,self.env)[1],None)
        with self.assertRaises(pre.PreflightError): pre.validate_args(publish,dict(self.env,GITHUB_RUN_ATTEMPT="01"))
        with redirect_stderr(io.StringIO()):
            self.assertEqual(pre.main(["--commit",self.sha,"--repository","Owner/Repo","--platform","linux/amd64","--publish","true","--confirm",pre.CONFIRMATION,"--previous-image",self.good_digest,"--output","/tmp/not-created"],self.env,lambda *a,**k:self.fail("runner must not be called")),1)
        with redirect_stderr(io.StringIO()):
            self.assertEqual(pre.main(["--commit",self.sha,"--repository","Owner/Repo","--platform","linux/amd64","--publish","false","--require-approval","--output","/tmp/not-created"],self.env,lambda *a,**k:self.fail("runner must not be called")),1)

    def test_expected_ci_workflow_run_and_attempt_scoped_jobs(self):
        runner=CIAPIMock(self.sha)
        pre.verify_checks("Owner/Repo",self.sha,runner)
        paths=[call[-1] for call in runner.calls]
        self.assertEqual(paths[0],"repos/Owner/Repo/actions/workflows/ci.yml")
        self.assertIn("/actions/workflows/387650116/runs?head_sha="+self.sha+"&event=push&branch=main&per_page=100",paths[1])
        self.assertEqual(paths[2],"repos/Owner/Repo/actions/runs/40053518866")
        self.assertEqual(paths[3],"repos/Owner/Repo/actions/runs/40053518866/attempts/1/jobs?per_page=100")
        self.assertEqual(paths[4],paths[2])
        self.assertFalse("--slurp" in runner.calls[0]); self.assertTrue(all("--slurp" in runner.calls[i] for i in (1,3)))

        failures=[]
        failures.append(CIAPIMock(self.sha,workflow={"id":9,"path":".github/workflows/other.yml","state":"active"}))
        failures.append(CIAPIMock(self.sha,runs=[CIAPIMock(self.sha).run(workflow_id=9)]))
        failures.append(CIAPIMock(self.sha,runs=[CIAPIMock(self.sha).run(head_branch="feature")]))
        failures.append(CIAPIMock(self.sha,runs=[CIAPIMock(self.sha).run(event="pull_request")]))
        failures.append(CIAPIMock(self.sha,runs=[CIAPIMock(self.sha).run(head_sha="b"*40)]))
        failures.append(CIAPIMock(self.sha,details=[CIAPIMock(self.sha).detail(conclusion="failure")]))
        failures.append(CIAPIMock(self.sha,details=[CIAPIMock(self.sha).detail(status="in_progress",conclusion=None)]))
        for bad in failures:
            with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,bad)

        older=CIAPIMock(self.sha).run(id=40053518860,conclusion="success")
        latest=CIAPIMock(self.sha).run(id=40053518866,conclusion="failure")
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,CIAPIMock(self.sha,runs=[older,latest],details=[latest]))
        pending=CIAPIMock(self.sha).run(id=40053518866,status="in_progress",conclusion=None)
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,CIAPIMock(self.sha,runs=[older,pending],details=[pending]))
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,CIAPIMock(self.sha,runs=[]))

        for bad_job in (CIAPIMock(self.sha).job(1,"fast-checks",run_id=7),CIAPIMock(self.sha).job(1,"fast-checks",run_attempt=2),CIAPIMock(self.sha).job(1,"fast-checks",head_sha="b"*40),CIAPIMock(self.sha).job(1,"fast-checks",workflow_name="Other")):
            bad=CIAPIMock(self.sha); bad.jobs=[bad_job,*bad.jobs[1:]]
            with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,bad)
        incomplete=CIAPIMock(self.sha); incomplete.jobs=incomplete.jobs[1:]
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,incomplete)
        ci_rerun=CIAPIMock(self.sha); ci_rerun.attempt=2; ci_rerun.runs=[ci_rerun.run(run_attempt=2)]; ci_rerun.details=[ci_rerun.detail(run_attempt=2)]; ci_rerun.jobs=[ci_rerun.job(i+1,n,run_attempt=2) for i,n in enumerate(pre.REQUIRED_CHECKS)]
        pre.verify_checks("Owner/Repo",self.sha,ci_rerun)
        rerun_mismatch=CIAPIMock(self.sha); rerun_mismatch.attempt=2; rerun_mismatch.runs=[rerun_mismatch.run(run_attempt=2)]; rerun_mismatch.details=[rerun_mismatch.detail(run_attempt=2)]; rerun_mismatch.jobs=[rerun_mismatch.job(i+1,n,run_attempt=1) for i,n in enumerate(pre.REQUIRED_CHECKS)]
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,rerun_mismatch)
        old_green=CIAPIMock(self.sha); old_green.jobs=[old_green.job(i+1,n) for i,n in enumerate(pre.REQUIRED_CHECKS)]+[old_green.job(99,"fast-checks",status="in_progress",conclusion=None)]
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,old_green)
        ambiguous=CIAPIMock(self.sha); ambiguous.jobs += [ambiguous.job(1,"fast-checks",conclusion="failure")]
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,ambiguous)
        midread=CIAPIMock(self.sha); midread.details=[midread.detail(),midread.detail(run_attempt=2)]
        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,midread)

        with self.assertRaises(pre.PreflightError): pre.verify_checks("Owner/Repo",self.sha,lambda *a,**k:Result("not json"))

    def test_environment_uses_plain_get_and_fails_closed_on_protection_gaps(self):
        rules=[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"id":1,"login":"reviewer"}}],"prevent_self_review":True}]
        valid={"id":17,"name":"release","protection_rules":rules,"deployment_branch_policy":{"protected_branches":True,"custom_branch_policies":False}}
        def call(data):
            def runner(argv, **kwargs):
                self.assertEqual(argv,["gh","api","repos/Owner/Repo/environments/release"])
                self.assertNotIn("--slurp",argv); return Result(json.dumps(data))
            pre.verify_environment("Owner/Repo",runner)
        call(valid)
        bad_cases=(dict(valid,protection_rules=[]),dict(valid,protection_rules=[dict(rules[0],reviewers=[])]),dict(valid,protection_rules=[dict(rules[0],reviewers=[{"type":"Team","reviewer":{"id":5}}])]),dict(valid,protection_rules=[dict(rules[0],prevent_self_review=False)]),dict(valid,deployment_branch_policy={"protected_branches":False,"custom_branch_policies":False}),dict(valid,deployment_branch_policy={"protected_branches":True,"custom_branch_policies":True}),dict(valid,deployment_branch_policy=None))
        for data in bad_cases:
            with self.assertRaises(pre.PreflightError): call(data)
        with self.assertRaises(pre.PreflightError): pre.verify_environment("Owner/Repo",lambda *a,**k:Result("[]"))

    def test_run_approval_must_match_environment_and_independent_configured_user(self):
        env={"id":17,"name":"release"}; reviewers={"1"}
        record={"environments":[{"id":17,"name":"release"}],"state":"approved","user":{"id":1,"login":"reviewer"},"comment":"approved"}
        def verify(records):
            def runner(argv, **kwargs):
                self.assertEqual(argv,["gh","api","repos/Owner/Repo/actions/runs/123/approvals"])
                self.assertNotIn("--slurp",argv); return Result(json.dumps(records))
            pre.verify_approval("Owner/Repo","123",env,reviewers,"2","requester",runner)
        verify([record])
        for records in ([],[dict(record,state="pending")],[dict(record,state="rejected")],[dict(record,user={"id":2,"login":"requester"})],[dict(record,user={"id":9,"login":"outsider"})],[dict(record,environments=[{"id":99,"name":"release"}])],[dict(record,environments=[{"id":17,"name":"other"}])]):
            with self.assertRaises(pre.PreflightError): verify(records)
        with self.assertRaises(pre.PreflightError): pre.verify_approval("Owner/Repo","123",env,reviewers,"1","reviewer",lambda *a,**k:Result(json.dumps([record])))
        with self.assertRaises(pre.PreflightError): pre.verify_approval("Owner/Repo","123",env,reviewers,"2","requester",lambda *a,**k:Result("{}"))
        env_payload={"id":17,"name":"release","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"id":1,"login":"reviewer"}}],"prevent_self_review":True}],"deployment_branch_policy":{"protected_branches":True,"custom_branch_policies":False}}
        checks=[{"id":i+1,"name":n,"app":{"slug":"github-actions"},"head_sha":self.sha,"status":"completed","conclusion":"success"} for i,n in enumerate(pre.REQUIRED_CHECKS)]
        with tempfile.TemporaryDirectory() as td:
            calls=[]; ci=CIAPIMock(self.sha)
            def runner(argv,**kwargs):
                calls.append(argv)
                if argv[0] == "git": return Result("")
                if argv[-1] == "repos/Owner/Repo/environments/release": return Result(json.dumps(env_payload))
                if argv[-1] == "repos/Owner/Repo/actions/runs/123/approvals": return Result(json.dumps([record]))
                return ci(argv,**kwargs)
            publish_env=dict(self.env,GITHUB_RUN_ATTEMPT="1",GITHUB_ACTOR_ID="2",GITHUB_ACTOR="requester")
            argv=["--commit",self.sha,"--repository","Owner/Repo","--platform","linux/amd64","--publish","true","--confirm",pre.CONFIRMATION,"--previous-image",self.good_digest,"--require-approval","--output",str(Path(td)/"intent.json")]
            self.assertEqual(pre.main(argv,publish_env,runner),0)
            self.assertIn(["gh","api","repos/Owner/Repo/environments/release"],calls)
            self.assertIn(["gh","api","repos/Owner/Repo/actions/runs/123/approvals"],calls)
            retry_env=dict(publish_env,GITHUB_RUN_ATTEMPT="2")
            stale_calls=[]
            def stale_runner(argv,**kwargs):
                stale_calls.append(argv)
                return Result(json.dumps([record]))
            with redirect_stderr(io.StringIO()):
                self.assertEqual(pre.main(argv,retry_env,stale_runner),1)
            self.assertEqual(stale_calls,[])

    def test_preflight_cli_intent_round_trips_into_manifest(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td); intent_path=root/"intent.json"
            calls=[]; ci=CIAPIMock(self.sha)
            def runner(argv, **kwargs):
                calls.append(argv)
                if argv[0] == "git": return Result("")
                return ci(argv, **kwargs)
            args=["--commit",self.sha,"--repository","Owner/Repo","--platform","linux/amd64","--publish","false","--confirm","","--previous-image","","--output",str(intent_path)]
            self.assertEqual(pre.main(args,self.env,runner),0)
            self.assertEqual(sum(1 for c in calls if c[:2] == ["gh","api"]),5)
            self.assertFalse(any("environments/release" in c[-1] for c in calls if c[:2] == ["gh","api"]))
            intent=manifest.read_json(intent_path,private=True)
            self.assertEqual(set(intent),manifest.INTENT_FIELDS)
            self.assertEqual(intent["run_attempt"],"2")
            self.assertEqual(intent["image_tag"],f"ghcr.io/owner/repo:git-{self.sha}-run-123-2")
            self.assertEqual(intent_path.stat().st_mode & 0o777,0o600)
            source=root/"source"; mig=source/"internal/postgres/migrations"; mig.mkdir(parents=True); (mig/"001_first.sql").write_bytes(b"SELECT 1;\n")
            archive=root/"image.tar"; archive.write_bytes(b"toy archive not a real image")
            config=[{"Os":"linux","Architecture":"amd64","Id":"sha256:"+"b"*64,"RepoTags":[intent["image_tag"]],"Config":{"User":"65532:65532","Entrypoint":["/payplump"],"Healthcheck":{"Test":["CMD","/payplump","healthcheck"]},"Labels":{"org.opencontainers.image.source":"https://github.com/Owner/Repo","org.opencontainers.image.revision":self.sha}}}]
            def git_runner(source,*args):
                if args == ("rev-parse","--show-toplevel"): return str(source)
                if args == ("rev-parse","HEAD"): return self.sha
                if args == ("status","--porcelain"): return ""
                raise AssertionError(args)
            with patch.object(manifest,"git",return_value="internal/postgres/migrations/001_first.sql"):
                out=manifest.validate(intent,source,config,archive,"","false",git_runner)
            self.assertEqual(out["run_attempt"],"2"); self.assertFalse(out["published"]); self.assertIsNone(out["registry_digest"])
            self.assertEqual(out["archive_sha256"],hashlib.sha256(archive.read_bytes()).hexdigest())
            self.assertNotEqual(out["archive_sha256"],out["local_image_id"].split(":",1)[1])

    def test_manifest_rejects_invalid_source_config_and_registry_claims(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td); mig=root/"internal/postgres/migrations"; mig.mkdir(parents=True); sql=mig/"001_first.sql"; sql.write_bytes(b"SELECT 1")
            intent={"version":1,"repository":"Owner/Repo","commit":self.sha,"platform":"linux/amd64","publish_requested":False,"previous_image":None,"run_id":"123","run_attempt":"2","image_tag":f"ghcr.io/owner/repo:git-{self.sha}-run-123-2"}
            cfg=[{"Os":"linux","Architecture":"amd64","Id":"sha256:"+"b"*64,"RepoTags":[intent["image_tag"]],"Config":{"User":"65532:65532","Entrypoint":["/payplump"],"Healthcheck":{"Test":["CMD","/payplump","healthcheck"]},"Labels":{"org.opencontainers.image.source":"https://github.com/Owner/Repo","org.opencontainers.image.revision":self.sha}}}]
            archive=root/"image.tar"; archive.write_bytes(b"nonempty")
            def git_runner(source,*args):
                if args == ("rev-parse","--show-toplevel"): return str(source)
                if args == ("rev-parse","HEAD"): return self.sha
                return "" if args == ("status","--porcelain") else ""
            def valid(intent=intent,cfg=cfg,archive=archive,digest="",published="false",runner=git_runner):
                with patch.object(manifest,"git",return_value="internal/postgres/migrations/001_first.sql"):
                    return manifest.validate(intent,root,cfg,archive,digest,published,runner)
            self.assertIsNone(valid()["registry_digest"])
            for changes in ({"repository":"Other/Repo"},{"run_attempt":"01"},{"image_tag":"tag"},{"previous_image":"ghcr.io/other/repo@sha256:"+"d"*64},{"extra":"field"}):
                bad=dict(intent,**changes)
                with self.assertRaises(manifest.ManifestError): valid(intent=bad)
            intent_pub=dict(intent,publish_requested=True,previous_image=self.good_digest)
            self.assertEqual(valid(intent=intent_pub,digest=self.good_digest,published="true")["registry_digest"],self.good_digest)
            for digest,published in (("ghcr.io/other/repo@sha256:"+"d"*64,"true"),("","true"),(self.good_digest,"false")):
                with self.assertRaises(manifest.ManifestError): valid(intent=intent_pub,digest=digest,published=published)
            for change in (("User","0:0"),("Entrypoint",["/bin/sh"]),("Architecture","arm64"),("RepoTags",[])):
                bad=[dict(cfg[0], Config=dict(cfg[0]["Config"]))]
                if change[0] in ("User","Entrypoint"):
                    bad[0]["Config"][change[0]]=change[1]
                else: bad[0][change[0]]=change[1]
                with self.assertRaises(manifest.ManifestError): valid(cfg=bad)
            bad=[dict(cfg[0],Config=dict(cfg[0]["Config"],Healthcheck={"Test":["CMD","/bad"]}))]
            with self.assertRaises(manifest.ManifestError): valid(cfg=bad)
            bad=[dict(cfg[0],Config=dict(cfg[0]["Config"],Labels={}))]
            with self.assertRaises(manifest.ManifestError): valid(cfg=bad)
            with self.assertRaises(manifest.ManifestError): valid(runner=lambda s,*a:self.sha if a==("rev-parse","HEAD") else "dirty")
            with self.assertRaises(manifest.ManifestError): valid(runner=lambda s,*a:str(s) if a==("rev-parse","--show-toplevel") else "b"*40 if a==("rev-parse","HEAD") else "")
            archive.write_bytes(b"")
            with self.assertRaises(manifest.ManifestError): valid()
            archive.write_bytes(b"nonempty")
            (mig/"bad.sql").write_bytes(b"bad")
            with self.assertRaises(manifest.ManifestError): valid()
            (mig/"bad.sql").unlink()
            (mig/"002_link.sql").symlink_to(sql)
            with self.assertRaises(manifest.ManifestError): valid()

    def test_metadata_input_permissions_symlinks_and_archive_symlink(self):
        with tempfile.TemporaryDirectory() as td:
            root=Path(td); good=root/"good.json"; good.write_text("{}",encoding="utf-8"); good.chmod(0o600)
            self.assertEqual(manifest.read_json(good,private=True),{})
            good.chmod(0o400)
            with self.assertRaises(manifest.ManifestError): manifest.read_json(good,private=True)
            link=root/"link.json"; link.symlink_to(good)
            with self.assertRaises(manifest.ManifestError): manifest.read_json(link)
            archive=root/"archive"; archive.write_bytes(b"x"); alink=root/"archive-link"; alink.symlink_to(archive)
            with self.assertRaises(manifest.ManifestError): manifest.hash_archive(alink)

if __name__ == "__main__": unittest.main()
