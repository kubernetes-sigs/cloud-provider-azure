#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import os
import json
import re
import shlex
import shutil
import subprocess
import sys
import time
import tempfile
from pathlib import Path


EXPECTED_ASSETS = [
    "azure-cloud-controller-manager-linux-amd64",
    "azure-cloud-controller-manager-linux-arm",
    "azure-cloud-controller-manager-linux-arm64",
    "azure-cloud-node-manager-linux-amd64",
    "azure-cloud-node-manager-linux-arm",
    "azure-cloud-node-manager-linux-arm64",
    "azure-cloud-node-manager-windows-amd64.exe",
    "azure-acr-credential-provider-linux-amd64",
    "azure-acr-credential-provider-linux-arm",
    "azure-acr-credential-provider-linux-arm64",
    "azure-acr-credential-provider-windows-amd64.exe",
]
STABLE_TAG_RE = re.compile(r"^v\d+\.\d+\.\d+$")


class CommandError(RuntimeError):
    pass


def err(message: str) -> None:
    print(f"[ERROR] {message}", file=sys.stderr)


def info(message: str) -> None:
    print(f"[INFO] {message}", file=sys.stderr)


def quote_cmd(cmd: list[str]) -> str:
    return " ".join(shlex.quote(part) for part in cmd)


def print_cmd(cmd: list[str]) -> None:
    print(f"+ {quote_cmd(cmd)}")


def run(
    cmd: list[str],
    *,
    cwd: Path,
    env: dict[str, str] | None = None,
    dry_run: bool = False,
    capture: bool = False,
) -> str:
    if dry_run:
        print_cmd(cmd)
        return ""

    proc = subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=capture)
    if proc.returncode != 0:
        stderr = (proc.stderr or "").strip()
        stdout = (proc.stdout or "").strip()
        details = stderr or stdout or f"command exited {proc.returncode}"
        raise CommandError(f"{quote_cmd(cmd)}: {details}")
    return (proc.stdout or "").rstrip("\n")


def ensure_command(name: str) -> None:
    if shutil.which(name) is None:
        raise CommandError(f"Required command not found in PATH: {name}")


def load_github_env(repo_root: Path) -> dict[str, str]:
    # Release-note generation now runs locally in the skill, so resolve the same
    # GitHub token sources the docs helper uses and prefer automatic toolchain
    # upgrades over the workflow's fixed runner Go version.
    env = os.environ.copy()
    if env.get("GITHUB_TOKEN"):
        env.setdefault("GOTOOLCHAIN", "auto")
        return env
    if env.get("GH_TOKEN"):
        env["GITHUB_TOKEN"] = env["GH_TOKEN"]
        env.setdefault("GOTOOLCHAIN", "auto")
        return env

    token = subprocess.run(["gh", "auth", "token"], cwd=repo_root, text=True, capture_output=True)
    if token.returncode == 0 and token.stdout.strip():
        env["GITHUB_TOKEN"] = token.stdout.strip()
        env.setdefault("GOTOOLCHAIN", "auto")
        return env

    raise CommandError("Could not resolve GitHub token. Provide GITHUB_TOKEN, GH_TOKEN, or run 'gh auth login'.")


def ensure_repo_root() -> Path:
    script_dir = Path(__file__).resolve().parent
    proc = subprocess.run(
        ["git", "-C", str(script_dir), "rev-parse", "--show-toplevel"],
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0 or not proc.stdout.strip():
        raise CommandError("release.py must run from a checkout of this repository.")
    return Path(proc.stdout.strip())


def validate_tag(tag: str) -> None:
    if not STABLE_TAG_RE.match(tag):
        raise CommandError(f"Expected stable tag in the form vX.Y.Z, got {tag!r}.")


def parse_remote_owner_repo(repo_root: Path, remote: str) -> str:
    remote_url = run(["git", "-C", str(repo_root), "remote", "get-url", remote], cwd=repo_root, capture=True)
    normalized = remote_url.removesuffix(".git")
    patterns = (
        r"^git@[^:]+:([^/]+)/([^/]+)$",
        r"^https?://[^/]+/([^/]+)/([^/]+)$",
        r"^ssh://git@[^/]+/([^/]+)/([^/]+)$",
    )
    for pattern in patterns:
        match = re.match(pattern, normalized)
        if match:
            return f"{match.group(1)}/{match.group(2)}"
    raise CommandError(f"Could not derive owner/repo from remote {remote!r}. Pass --github-repo explicitly.")


def resolve_github_repo(repo_root: Path, remote: str, explicit_repo: str) -> str:
    if explicit_repo:
        return explicit_repo
    remote_check = subprocess.run(
        ["git", "-C", str(repo_root), "remote", "get-url", remote],
        text=True,
        capture_output=True,
    )
    if remote_check.returncode != 0:
        raise CommandError(f"Remote {remote!r} does not exist.")
    return parse_remote_owner_repo(repo_root, remote)


def build_create_release_tags_cmd(
    *,
    python: str,
    script: Path,
    repo_root: Path,
    branch: str,
    remote: str,
    tag: str,
    force_branch: bool,
    no_fetch: bool,
    message: str,
    sign: bool,
    lightweight: bool,
    push: bool,
) -> list[str]:
    cmd = [python, str(script), "--repo", str(repo_root), "--remote", remote, "--branch", branch]
    if tag:
        cmd.extend(["--tag", tag])
    if force_branch:
        cmd.append("--force-branch")
    if no_fetch:
        cmd.append("--no-fetch")
    if message:
        cmd.extend(["--message", message])
    if sign:
        cmd.append("--sign")
    if lightweight:
        cmd.append("--lightweight")
    if push:
        cmd.append("--push")
    return cmd


def plan_next_tag(
    *,
    python: str,
    create_release_tags_script: Path,
    repo_root: Path,
    branch: str,
    remote: str,
    force_branch: bool,
) -> str:
    cmd = build_create_release_tags_cmd(
        python=python,
        script=create_release_tags_script,
        repo_root=repo_root,
        branch=branch,
        remote=remote,
        tag="",
        force_branch=force_branch,
        no_fetch=True,
        message="",
        sign=False,
        lightweight=False,
        push=False,
    )
    proc = subprocess.run(cmd, cwd=repo_root, text=True, capture_output=True)
    output = (proc.stdout or "") + (proc.stderr or "")
    if proc.returncode != 0:
        if output:
            print(output, file=sys.stderr, end="" if output.endswith("\n") else "\n")
        raise CommandError(
            "Could not compute the next tag from local refs. Pass --tag explicitly or refresh local refs."
        )

    if output:
        print(output, file=sys.stderr, end="" if output.endswith("\n") else "\n")

    for line in output.splitlines():
        if line.startswith("next_tag:"):
            next_tag = line.split(":", 1)[1].strip()
            validate_tag(next_tag)
            return next_tag
    raise CommandError("Could not parse next_tag from create-release-tags output.")


def gh_json(repo_root: Path, args: list[str]) -> object:
    output = run(["gh", *args], cwd=repo_root, capture=True)
    return json.loads(output)


def gh_release_view(repo_root: Path, github_repo: str, tag: str) -> dict[str, object]:
    output = run(
        [
            "gh",
            "release",
            "view",
            tag,
            "-R",
            github_repo,
            "--json",
            "databaseId,tagName,isDraft,isPrerelease,url,assets",
        ],
        cwd=repo_root,
        capture=True,
    )
    return json.loads(output)


def find_workflow_run(repo_root: Path, github_repo: str, workflow: str, tag: str, dispatched_after: str) -> tuple[str, str]:
    attempts = 30
    while attempts > 0:
        response = gh_json(
            repo_root,
            ["api", f"repos/{github_repo}/actions/workflows/{workflow}/runs?event=workflow_dispatch&per_page=30"],
        )
        runs = response.get("workflow_runs", [])
        candidates = [
            run_info
            for run_info in runs
            if run_info.get("head_branch") == tag and run_info.get("created_at", "") >= dispatched_after
        ]
        candidates.sort(key=lambda item: item.get("created_at", ""))
        if candidates:
            latest = candidates[-1]
            return str(latest.get("id", "")), latest.get("html_url", "")
        attempts -= 1
        time.sleep(5)
    raise CommandError(f"Timed out waiting for workflow run {workflow!r} for tag {tag!r} in {github_repo}.")


def fetch_release_json(repo_root: Path, github_repo: str, tag: str) -> dict[str, object]:
    return gh_release_view(repo_root, github_repo, tag)


def _payload_value(payload: dict[str, object], *keys: str) -> object | None:
    # `gh release view --json ...` and the REST releases API use different key
    # names, so normalize both shapes in one place.
    for key in keys:
        if key in payload:
            return payload.get(key)
    return None


def check_release_payload(tag: str, expect_draft: bool, payload: dict[str, object], expected_assets: list[str]) -> tuple[str, str]:
    payload_tag = _payload_value(payload, "tag_name", "tagName")
    if payload_tag != tag:
        raise CommandError(f"Release payload tag mismatch: expected {tag}, got {payload_tag}")

    draft = bool(_payload_value(payload, "draft", "isDraft"))
    if draft != expect_draft:
        state = "draft" if draft else "published"
        expected = "draft" if expect_draft else "published"
        raise CommandError(f"Release {tag} is {state}, expected {expected}")

    if bool(_payload_value(payload, "prerelease", "isPrerelease")):
        raise CommandError(f"Release {tag} is marked as a prerelease")

    asset_names = {asset.get("name") for asset in payload.get("assets", []) if isinstance(asset, dict)}
    missing = [name for name in expected_assets if name not in asset_names]
    if missing:
        raise CommandError(f"Missing assets: {', '.join(missing)}")

    release_id = _payload_value(payload, "databaseId", "id")
    if not release_id:
        raise CommandError(f"Release {tag} does not have an id")
    release_url = _payload_value(payload, "url", "html_url")
    if not release_url:
        raise CommandError(f"Release {tag} does not have a URL")
    return str(release_id), str(release_url)


def find_downloaded_asset(download_dir: Path, asset_name: str) -> Path:
    for candidate in download_dir.rglob(asset_name):
        if candidate.is_file():
            return candidate
    raise CommandError(f"Missing downloaded asset file: {asset_name}")


def download_workflow_artifacts(repo_root: Path, github_repo: str, run_id: str, download_dir: Path, *, dry_run: bool = False) -> None:
    run(["gh", "run", "download", run_id, "-R", github_repo, "-D", str(download_dir)], cwd=repo_root, dry_run=dry_run)


def generate_release_notes(repo_root: Path, tag: str, notes_file: Path, *, github_env: dict[str, str], dry_run: bool = False) -> None:
    notes_file.parent.mkdir(parents=True, exist_ok=True)
    run(
        ["./hack/generate-release-note.sh", tag, str(notes_file)],
        cwd=repo_root,
        env=github_env,
        dry_run=dry_run,
    )


def ensure_draft_release(repo_root: Path, github_repo: str, tag: str, notes_file: Path, *, dry_run: bool = False) -> None:
    payload = None
    try:
        payload = gh_release_view(repo_root, github_repo, tag)
    except CommandError as exc:
        if "release not found" not in str(exc).lower():
            raise

    if payload is None:
        run(
            [
                "gh",
                "release",
                "create",
                tag,
                "-R",
                github_repo,
                "--draft",
                "--title",
                tag,
                "--notes-file",
                str(notes_file),
                "--verify-tag",
            ],
            cwd=repo_root,
            dry_run=dry_run,
        )
        return

    # Re-running prepare against an existing draft should refresh the notes
    # instead of forcing operators to delete and recreate the release.
    if not payload.get("isDraft"):
        raise CommandError(f"Release {tag} already exists and is not a draft")

    run(
        [
            "gh",
            "release",
            "edit",
            tag,
            "-R",
            github_repo,
            "--title",
            tag,
            "--notes-file",
            str(notes_file),
        ],
        cwd=repo_root,
        dry_run=dry_run,
    )


def upload_release_assets(repo_root: Path, github_repo: str, tag: str, asset_paths: list[Path], *, dry_run: bool = False) -> None:
    for asset_path in asset_paths:
        run(
            [
                "gh",
                "release",
                "upload",
                tag,
                "-R",
                github_repo,
                "--clobber",
                str(asset_path),
            ],
            cwd=repo_root,
            dry_run=dry_run,
        )


def wait_for_draft_release(repo_root: Path, github_repo: str, tag: str) -> tuple[str, str]:
    attempts = 24
    while attempts > 0:
        try:
            payload = fetch_release_json(repo_root, github_repo, tag)
            return check_release_payload(tag, True, payload, EXPECTED_ASSETS)
        except CommandError:
            attempts -= 1
            if attempts <= 0:
                break
            time.sleep(5)
    raise CommandError(f"Timed out waiting for draft release {tag!r} with the expected assets.")


def latest_policy_for_tag(tag: str, releases_json: object) -> bool:
    match = re.match(r"^v(\d+)\.(\d+)\.(\d+)$", tag)
    if not match:
        return False

    target_series = (int(match.group(1)), int(match.group(2)))
    releases: list[dict[str, object]] = []
    if isinstance(releases_json, list):
        for page in releases_json:
            if isinstance(page, list):
                releases.extend(item for item in page if isinstance(item, dict))
            elif isinstance(page, dict):
                releases.append(page)
    elif isinstance(releases_json, dict):
        releases.append(releases_json)

    highest_series: tuple[int, int] | None = None
    for release in releases:
        if release.get("draft") or release.get("prerelease"):
            continue
        release_tag = str(release.get("tag_name", ""))
        stable = re.match(r"^v(\d+)\.(\d+)\.(\d+)$", release_tag)
        if not stable:
            continue
        series = (int(stable.group(1)), int(stable.group(2)))
        if highest_series is None or series > highest_series:
            highest_series = series

    return highest_series is None or target_series >= highest_series


def prepare_release(
    args: argparse.Namespace,
    *,
    repo_root: Path,
    python: str,
    create_release_tags_script: Path,
) -> int:
    branch = args.branch
    tag = args.tag or plan_next_tag(
        python=python,
        create_release_tags_script=create_release_tags_script,
        repo_root=repo_root,
        branch=branch,
        remote=args.remote,
        force_branch=args.force_branch,
    )
    validate_tag(tag)
    github_repo = resolve_github_repo(repo_root, args.remote, args.github_repo)

    tag_cmd = build_create_release_tags_cmd(
        python=python,
        script=create_release_tags_script,
        repo_root=repo_root,
        branch=branch,
        remote=args.remote,
        tag=tag,
        force_branch=args.force_branch,
        no_fetch=args.no_fetch,
        message=args.message or "",
        sign=args.sign,
        lightweight=args.lightweight,
        push=True,
    )

    if args.dry_run:
        info(f"Dry run: prepare {tag} from {branch} in {github_repo}")
        print_cmd(tag_cmd)
        print_cmd(["gh", "workflow", "run", args.workflow, "-R", github_repo, "--ref", tag])
        print("[OK] Dry run: would wait for the workflow run, download artifacts, and create or update the draft release:")
        print_cmd(["gh", "run", "watch", "<run-id>", "-R", github_repo, "--exit-status", "--compact"])
        print_cmd(["gh", "run", "download", "<run-id>", "-R", github_repo, "-D", "<artifacts-dir>"])
        print_cmd(["./hack/generate-release-note.sh", tag, "<release-notes.md>"])
        print_cmd(
            [
                "gh",
                "release",
                "create",
                tag,
                "-R",
                github_repo,
                "--draft",
                "--title",
                tag,
                "--notes-file",
                "<release-notes.md>",
                "--verify-tag",
            ]
        )
        for asset in EXPECTED_ASSETS:
            print_cmd(
                [
                    "gh",
                    "release",
                    "upload",
                    tag,
                    "-R",
                    github_repo,
                    "--clobber",
                    f"<artifacts-dir>/{asset}",
                ]
            )
        print("[OK] Dry run: would verify that the draft release contains these assets:")
        for asset in EXPECTED_ASSETS:
            print(f"  - {asset}")
        return 0

    ensure_command("gh")
    ensure_command("git")
    output = run(tag_cmd, cwd=repo_root, capture=True)
    if output:
        print(output)

    dispatched_after = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    run(["gh", "workflow", "run", args.workflow, "-R", github_repo, "--ref", tag], cwd=repo_root)
    info(f"Dispatched {args.workflow} for {tag} in {github_repo}")

    run_id, run_url = find_workflow_run(repo_root, github_repo, args.workflow, tag, dispatched_after)
    if run_url:
        info(f"Watching workflow run {run_id}: {run_url}")
    else:
        info(f"Watching workflow run {run_id}")
    run(["gh", "run", "watch", run_id, "-R", github_repo, "--exit-status", "--compact"], cwd=repo_root)

    github_env = load_github_env(repo_root)
    with tempfile.TemporaryDirectory(prefix=f"release-{tag}-") as temp_dir:
        # Keep all intermediate files ephemeral; the draft release on GitHub is
        # the durable handoff between `prepare` and `publish`.
        temp_root = Path(temp_dir)
        artifacts_dir = temp_root / "artifacts"
        notes_file = temp_root / "release-notes.md"

        download_workflow_artifacts(repo_root, github_repo, run_id, artifacts_dir)
        artifact_paths = [find_downloaded_asset(artifacts_dir, asset) for asset in EXPECTED_ASSETS]
        generate_release_notes(repo_root, tag, notes_file, github_env=github_env)
        ensure_draft_release(repo_root, github_repo, tag, notes_file)
        upload_release_assets(repo_root, github_repo, tag, artifact_paths)

    release_id, release_url = wait_for_draft_release(repo_root, github_repo, tag)
    info(f"Draft release {tag} is ready: {release_url}")
    info(f"Draft release id: {release_id}")
    return 0


def publish_release(
    args: argparse.Namespace,
    *,
    repo_root: Path,
    python: str,
    create_release_note_doc_pr_script: Path,
) -> int:
    validate_tag(args.tag)
    github_repo = resolve_github_repo(repo_root, args.remote, args.github_repo)
    doc_pr_cmd = [python, str(create_release_note_doc_pr_script), "--tag", args.tag]
    if args.base_remote:
        doc_pr_cmd.extend(["--base-remote", args.base_remote])
    if args.push_remote:
        doc_pr_cmd.extend(["--push-remote", args.push_remote])
    if args.base_branch:
        doc_pr_cmd.extend(["--base-branch", args.base_branch])
    if args.target_repo:
        doc_pr_cmd.extend(["--target-repo", args.target_repo])
    if args.head_owner:
        doc_pr_cmd.extend(["--head-owner", args.head_owner])

    if args.dry_run:
        info(f"Dry run: publish {args.tag} in {github_repo}")
        print("[OK] Dry run: would verify that the draft release exists and includes the expected assets.")
        if args.latest == "auto":
            print("[OK] Dry run: would publish with make_latest decided from the highest published stable major.minor series.")
        elif args.latest == "always":
            print("[OK] Dry run: would publish with make_latest=true.")
        else:
            print("[OK] Dry run: would publish with make_latest=false.")
        print_cmd(
            [
                "gh",
                "api",
                "--method",
                "PATCH",
                f"repos/{github_repo}/releases/<release-id>",
                "-F",
                "draft=false",
                "-F",
                "make_latest=<true|false>",
            ]
        )
        print_cmd(doc_pr_cmd)
        return 0

    ensure_command("gh")

    try:
        release_json = fetch_release_json(repo_root, github_repo, args.tag)
    except CommandError as exc:
        raise CommandError(f"Draft release {args.tag!r} was not found in {github_repo}.") from exc

    release_id, release_url = check_release_payload(args.tag, True, release_json, EXPECTED_ASSETS)
    info(f"Publishing draft release {args.tag}: {release_url}")

    if args.latest == "always":
        make_latest = "true"
    elif args.latest == "never":
        make_latest = "false"
    else:
        releases_json = gh_json(repo_root, ["api", "--paginate", "--slurp", f"repos/{github_repo}/releases?per_page=100"])
        make_latest = "true" if latest_policy_for_tag(args.tag, releases_json) else "false"

    info(f"Publishing {args.tag} with make_latest={make_latest}")
    run(
        [
            "gh",
            "api",
            "--method",
            "PATCH",
            f"repos/{github_repo}/releases/{release_id}",
            "-F",
            "draft=false",
            "-F",
            f"make_latest={make_latest}",
        ],
        cwd=repo_root,
    )
    run(doc_pr_cmd, cwd=repo_root)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Draft-first release orchestration for cloud-provider-azure.")
    subparsers = parser.add_subparsers(dest="subcommand")

    prepare = subparsers.add_parser("prepare", help="Create/push a release tag and prepare a draft release")
    prepare.add_argument("--branch", required=True, help="Release branch to tag, for example release-1.35")
    prepare.add_argument("--tag", help="Stable tag to create, for example v1.35.7")
    prepare.add_argument("--remote", default="upstream", help="Git remote for tag push and repo inference")
    prepare.add_argument("--github-repo", default="", help="GitHub repo as owner/repo")
    prepare.add_argument("--workflow", default="release.yaml", help="Workflow file or name for manual dispatch")
    prepare.add_argument("--force-branch", action="store_true", help="Reset local branch to <remote>/<branch> if needed")
    prepare.add_argument("--no-fetch", action="store_true", help="Skip fetch in create-release-tags")
    prepare.add_argument("--message", default="", help="Annotated tag message override")
    prepare.add_argument("--sign", action="store_true", help="Create a signed tag")
    prepare.add_argument("--lightweight", action="store_true", help="Create a lightweight tag")
    prepare.add_argument("--dry-run", action="store_true", help="Print the plan without mutating state")

    publish = subparsers.add_parser("publish", help="Publish a draft release and open the docs PR")
    publish.add_argument("--tag", required=True, help="Stable tag to publish")
    publish.add_argument("--remote", default="upstream", help="Git remote for repo inference")
    publish.add_argument("--github-repo", default="", help="GitHub repo as owner/repo")
    publish.add_argument(
        "--latest",
        choices=["auto", "always", "never"],
        default="auto",
        help="Latest-release policy",
    )
    publish.add_argument("--base-remote", default="", help="Forwarded to create-release-note-doc-pr")
    publish.add_argument("--push-remote", default="", help="Forwarded to create-release-note-doc-pr")
    publish.add_argument("--base-branch", default="documentation", help="Forwarded to create-release-note-doc-pr")
    publish.add_argument("--target-repo", default="", help="Forwarded to create-release-note-doc-pr")
    publish.add_argument("--head-owner", default="", help="Forwarded to create-release-note-doc-pr")
    publish.add_argument("--dry-run", action="store_true", help="Print the plan without mutating state")

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    if not args.subcommand:
        parser.print_help()
        return 0

    repo_root = ensure_repo_root()
    python = sys.executable or "python3"
    create_release_tags_script = repo_root / ".agents" / "skills" / "create-release-tags" / "scripts" / "create_release_tags.py"
    create_release_note_doc_pr_script = (
        repo_root / ".agents" / "skills" / "create-release-note-doc-pr" / "scripts" / "create_release_note_doc_pr.py"
    )
    if not create_release_tags_script.is_file():
        raise CommandError(f"Missing dependency script: {create_release_tags_script}")
    if not create_release_note_doc_pr_script.is_file():
        raise CommandError(f"Missing dependency script: {create_release_note_doc_pr_script}")

    if args.subcommand == "prepare":
        return prepare_release(
            args,
            repo_root=repo_root,
            python=python,
            create_release_tags_script=create_release_tags_script,
        )
    return publish_release(
        args,
        repo_root=repo_root,
        python=python,
        create_release_note_doc_pr_script=create_release_note_doc_pr_script,
    )


if __name__ == "__main__":
    try:
        sys.exit(main())
    except CommandError as exc:
        err(str(exc))
        sys.exit(1)
