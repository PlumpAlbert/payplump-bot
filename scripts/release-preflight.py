#!/usr/bin/env python3
"""Fail-closed, read-only preflight for a future manual image publication."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

MAX_API_BYTES = 2 * 1024 * 1024
MAX_API_PAGES = 100
REQUIRED_CHECKS = ("fast-checks", "postgres-integration", "test")
WORKFLOW_PATH = ".github/workflows/ci.yml"
WORKFLOW_NAME = "CI"
CONFIRMATION = "publish-reviewed-commit"
DIGEST_RE = re.compile(r"^ghcr\.io/[a-z0-9_.-]+/[a-z0-9_.-]+@sha256:[0-9a-f]{64}$")
RUN_NUMBER_RE = re.compile(r"^[1-9][0-9]{0,11}$")

class PreflightError(Exception):
    pass

def fail(message):
    raise PreflightError(message)

def run_readonly(args, runner=subprocess.run):
    try:
        result = runner(args, cwd=None, capture_output=True, text=True, timeout=20, check=False)
    except (OSError, subprocess.TimeoutExpired):
        fail("required read-only command failed")
    if result.returncode:
        fail("required read-only command failed")
    return result.stdout

def validate_args(args, env):
    if env.get("GITHUB_ACTIONS") != "true": fail("must run in GitHub Actions")
    if env.get("GITHUB_REF") != "refs/heads/main": fail("workflow must run from main")
    repo = env.get("GITHUB_REPOSITORY", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or args.repository != repo:
        fail("repository does not match GitHub context")
    if not re.fullmatch(r"[0-9a-f]{40}", args.commit): fail("commit must be lowercase full SHA")
    if args.platform not in ("linux/amd64", "linux/arm64"): fail("unsupported platform")
    if args.publish not in ("false", "true"): fail("publish must be canonical true or false")
    if not RUN_NUMBER_RE.fullmatch(env.get("GITHUB_RUN_ID", "")): fail("invalid workflow run id")
    if not RUN_NUMBER_RE.fullmatch(env.get("GITHUB_RUN_ATTEMPT", "")): fail("invalid workflow run attempt")
    previous = args.previous_image or None
    if previous is not None and (not DIGEST_RE.fullmatch(previous) or previous.split("@", 1)[0] != f"ghcr.io/{repo.lower()}"):
        fail("invalid previous image digest")
    if args.publish == "true":
        if env.get("GITHUB_RUN_ATTEMPT") != "1": fail("request a new workflow dispatch for publication")
        if args.confirm != CONFIRMATION: fail("publication confirmation is required")
        if not previous: fail("previous image digest is required for publication")
    elif args.confirm:
        fail("confirmation is only accepted for requested publication")
    return repo, previous

def gh_json(path, runner=subprocess.run, paginated=False):
    argv = ["gh", "api"]
    if paginated: argv.extend(("--paginate", "--slurp"))
    argv.append(path)
    raw = run_readonly(argv, runner)
    if len(raw.encode("utf-8")) > MAX_API_BYTES: fail("GitHub API response exceeds limit")
    try: return json.loads(raw)
    except (ValueError, UnicodeError): fail("invalid GitHub API response")

def api_pages(path, runner=subprocess.run):
    pages = gh_json(path, runner, paginated=True)
    if not isinstance(pages, list) or len(pages) > MAX_API_PAGES: fail("invalid paginated GitHub API response")
    return pages

def validate_ci_run(run, workflow_id, commit):
    if not isinstance(run, dict): fail("invalid CI workflow run")
    if (type(run.get("id")) is not int or run["id"] <= 0
            or type(run.get("workflow_id")) is not int or run["workflow_id"] != workflow_id or run.get("path") != WORKFLOW_PATH
            or run.get("head_sha") != commit or run.get("head_branch") != "main"
            or run.get("event") != "push" or not RUN_NUMBER_RE.fullmatch(str(run.get("run_attempt", "")))):
        fail("CI workflow run identity is invalid")
    return run["id"], str(run["run_attempt"])

def validate_current_ci_run(run, workflow_id, run_id, attempt, commit):
    actual_id, actual_attempt = validate_ci_run(run, workflow_id, commit)
    if actual_id != run_id or actual_attempt != attempt: fail("current CI workflow run changed during verification")
    if run.get("status") != "completed" or run.get("conclusion") != "success":
        fail("latest CI workflow run is not successful")

def verify_ci_jobs(repo, run_id, attempt, commit, runner=subprocess.run):
    path = f"repos/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100"
    latest = {}
    for page in api_pages(path, runner):
        if not isinstance(page, dict) or not isinstance(page.get("jobs"), list): fail("invalid CI jobs page")
        for job in page["jobs"]:
            if not isinstance(job, dict): fail("invalid CI job")
            ident = job.get("id")
            if (type(ident) is not int or ident <= 0 or type(job.get("run_id")) is not int or job.get("run_id") != run_id
                    or str(job.get("run_attempt")) != attempt or job.get("head_sha") != commit
                    or job.get("workflow_name") != WORKFLOW_NAME):
                fail("CI job identity is invalid")
            name = job.get("name")
            if name not in REQUIRED_CHECKS: continue
            item = {"id": ident, "status": job.get("status"), "conclusion": job.get("conclusion")}
            if name not in latest or ident > latest[name]["id"]:
                latest[name] = item
            elif ident == latest[name]["id"] and item != latest[name]:
                fail("ambiguous duplicate CI job identity")
    for name in REQUIRED_CHECKS:
        item = latest.get(name)
        if not item or item["status"] != "completed" or item["conclusion"] != "success":
            fail("required jobs are not all successful in the selected CI run attempt")

def verify_checks(repo, commit, runner=subprocess.run):
    workflow = gh_json(f"repos/{repo}/actions/workflows/ci.yml", runner)
    if not isinstance(workflow, dict) or type(workflow.get("id")) is not int or workflow["id"] <= 0:
        fail("CI workflow identity is unavailable")
    workflow_id = workflow["id"]
    if workflow.get("path") != WORKFLOW_PATH or workflow.get("state") != "active":
        fail("expected CI workflow is not active at the expected path")
    path = f"repos/{repo}/actions/workflows/{workflow_id}/runs?head_sha={commit}&event=push&branch=main&per_page=100"
    runs = []
    for page in api_pages(path, runner):
        if not isinstance(page, dict) or not isinstance(page.get("workflow_runs"), list): fail("invalid workflow runs page")
        for run in page["workflow_runs"]:
            run_id, attempt = validate_ci_run(run, workflow_id, commit)
            runs.append((run_id, attempt))
    if not runs: fail("no exact-commit run of the required CI workflow")
    latest_run_id = max(run_id for run_id, _ in runs)
    selected_attempts = {attempt for run_id, attempt in runs if run_id == latest_run_id}
    if len(selected_attempts) != 1: fail("latest CI workflow run metadata is ambiguous")
    attempt = next(iter(selected_attempts))
    detail_path = f"repos/{repo}/actions/runs/{latest_run_id}"
    current = gh_json(detail_path, runner)
    validate_current_ci_run(current, workflow_id, latest_run_id, attempt, commit)
    verify_ci_jobs(repo, latest_run_id, attempt, commit, runner)
    current = gh_json(detail_path, runner)
    validate_current_ci_run(current, workflow_id, latest_run_id, attempt, commit)

def verify_environment(repo, runner=subprocess.run):
    data = gh_json(f"repos/{repo}/environments/release", runner)
    if not isinstance(data, dict): fail("release environment unavailable")
    protection = data.get("protection_rules")
    if not isinstance(protection, list): fail("release environment rules unavailable")
    reviewers = [r for r in protection if isinstance(r, dict) and r.get("type") == "required_reviewers"]
    if not reviewers or any(not r.get("reviewers") or r.get("prevent_self_review") is not True for r in reviewers):
        fail("release environment reviewers are not safely configured")
    users = set()
    for rule in reviewers:
        configured = rule.get("reviewers")
        if not isinstance(configured, list): fail("release environment reviewers are unavailable")
        for reviewer in configured:
            if not isinstance(reviewer, dict) or reviewer.get("type") != "User":
                fail("release environment supports only explicitly configured user reviewers")
            details = reviewer.get("reviewer")
            if not isinstance(details, dict): fail("release environment reviewer identity is invalid")
            ident = details.get("id")
            if type(ident) is not int or ident <= 0: fail("release environment reviewer identity is invalid")
            users.add(str(ident))
    if not users: fail("release environment has no explicitly configured user reviewers")
    branch_policy = data.get("deployment_branch_policy")
    if (not isinstance(branch_policy, dict) or branch_policy.get("protected_branches") is not True
            or branch_policy.get("custom_branch_policies") is not False):
        fail("release environment protected-branch policy is not enabled exclusively")
    return {"id": data.get("id"), "name": data.get("name"), "reviewer_ids": users}

def verify_approval(repo, run_id, environment, reviewer_ids, actor_id, actor, runner=subprocess.run):
    # The public environment GET omits administrator-bypass configuration. Require a
    # run-specific approval record from a configured, independent user instead.
    if type(environment) is not dict or type(environment.get("id")) is not int or environment["id"] <= 0 or environment.get("name") != "release":
        fail("release environment identity is unavailable")
    approvals = gh_json(f"repos/{repo}/actions/runs/{run_id}/approvals", runner)
    if not isinstance(approvals, list): fail("workflow approval records are unavailable")
    if not re.fullmatch(r"[1-9][0-9]{0,11}", str(actor_id)) or not isinstance(actor, str) or not actor:
        fail("GitHub actor identity is unavailable")
    approved = False
    for record in approvals:
        if not isinstance(record, dict) or not isinstance(record.get("environments"), list): fail("invalid workflow approval record")
        matches = []
        for item in record["environments"]:
            if not isinstance(item, dict): fail("invalid workflow approval environment")
            if item.get("id") == environment["id"] or item.get("name") == "release":
                if item.get("id") != environment["id"] or item.get("name") != "release":
                    fail("workflow approval environment identity mismatch")
                matches.append(item)
        if not matches: continue
        user = record.get("user")
        if not isinstance(user, dict): fail("invalid workflow approval reviewer")
        ident = user.get("id")
        login = user.get("login")
        state = record.get("state")
        if (str(ident) not in reviewer_ids or str(ident) == str(actor_id)
                or not isinstance(login, str) or login.casefold() == actor.casefold()
                or state != "approved"):
            fail("release environment approval is not from an eligible independent reviewer")
        approved = True
    if not approved: fail("no eligible approval for this workflow run and release environment")

def create_output(path, data):
    target = Path(path)
    if not target.is_absolute(): fail("output path must be absolute")
    try:
        if target.is_symlink() or target.exists(): fail("output file must be new and not a symlink")
        fd = os.open(str(target), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            os.chmod(target, 0o600)
            json.dump(data, stream, sort_keys=True, separators=(",", ":"))
            stream.write("\n"); stream.flush(); os.fsync(stream.fileno())
    except PreflightError: raise
    except OSError: fail("could not safely create output file")

def main(argv=None, env=None, runner=subprocess.run):
    parser = argparse.ArgumentParser()
    parser.add_argument("--commit", required=True); parser.add_argument("--repository", required=True)
    parser.add_argument("--platform", required=True); parser.add_argument("--publish", required=True)
    parser.add_argument("--confirm", default=""); parser.add_argument("--previous-image", default="")
    parser.add_argument("--require-approval", action="store_true")
    parser.add_argument("--output", required=True)
    args = parser.parse_args(argv); env = os.environ if env is None else env
    try:
        repo, previous = validate_args(args, env)
        if args.require_approval:
            if args.publish != "true": fail("approval verification requires requested publication")
            if not RUN_NUMBER_RE.fullmatch(env.get("GITHUB_ACTOR_ID", "")) or not env.get("GITHUB_ACTOR"):
                fail("GitHub actor identity is unavailable")
        root = Path(__file__).resolve().parents[1]
        run_readonly(["git", "-C", str(root), "cat-file", "-e", args.commit + "^{commit}"], runner)
        run_readonly(["git", "-C", str(root), "merge-base", "--is-ancestor", args.commit, "origin/main"], runner)
        verify_checks(repo, args.commit, runner)
        if args.publish == "true":
            environment = verify_environment(repo, runner)
            if args.require_approval:
                verify_approval(repo, env["GITHUB_RUN_ID"], environment, environment["reviewer_ids"],
                                env.get("GITHUB_ACTOR_ID"), env.get("GITHUB_ACTOR"), runner)
        rid, run_attempt = env["GITHUB_RUN_ID"], env["GITHUB_RUN_ATTEMPT"]
        data = {"version": 1, "repository": repo, "commit": args.commit, "platform": args.platform,
                "publish_requested": args.publish == "true", "previous_image": previous,
                "run_id": rid, "run_attempt": run_attempt,
                "image_tag": f"ghcr.io/{repo.lower()}:git-{args.commit}-run-{rid}-{run_attempt}"}
        create_output(args.output, data)
        return 0
    except PreflightError as exc:
        print(f"release preflight denied: {exc}", file=sys.stderr); return 1

if __name__ == "__main__": raise SystemExit(main())
