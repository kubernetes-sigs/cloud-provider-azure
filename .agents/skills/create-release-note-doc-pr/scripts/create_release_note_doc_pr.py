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
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


class CommandError(RuntimeError):
    pass


def quote_cmd(cmd: list[str]) -> str:
    return " ".join(shlex.quote(part) for part in cmd)


def run(
    cmd: list[str],
    *,
    cwd: Path,
    env: dict[str, str] | None = None,
    dry_run: bool = False,
    capture: bool = False,
) -> str:
    if dry_run:
        print(f"+ {quote_cmd(cmd)}")
        return ""

    proc = subprocess.run(
        cmd,
        cwd=cwd,
        env=env,
        text=True,
        capture_output=capture,
    )
    if proc.returncode != 0:
        stderr = (proc.stderr or "").strip()
        stdout = (proc.stdout or "").strip()
        details = stderr or stdout or f"command exited {proc.returncode}"
        raise CommandError(f"{quote_cmd(cmd)}: {details}")
    return (proc.stdout or "").rstrip("\n")


def run_optional(cmd: list[str], *, cwd: Path) -> bool:
    proc = subprocess.run(cmd, cwd=cwd, text=True, capture_output=True)
    return proc.returncode == 0


def ensure_repo_root() -> Path:
    try:
        root = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            text=True,
            capture_output=True,
            check=True,
        ).stdout.strip()
    except subprocess.CalledProcessError as exc:
        stderr = (exc.stderr or "").strip()
        raise CommandError(stderr or "This script must run from a checkout of this repository.") from exc
    return Path(root)


def parse_remote_owner_repo(repo_root: Path, remote: str) -> str:
    remote_url = run(["git", "remote", "get-url", remote], cwd=repo_root, capture=True).strip()
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
    raise CommandError(
        f"Failed to derive owner/repo from remote {remote!r}. Provide explicit override flags."
    )


def remote_exists(repo_root: Path, remote: str) -> bool:
    return run_optional(["git", "remote", "get-url", remote], cwd=repo_root)


def remote_has_branch(repo_root: Path, remote: str, branch: str) -> bool:
    return run_optional(["git", "ls-remote", "--exit-code", "--heads", remote, branch], cwd=repo_root)


def ensure_clean_worktree(repo_root: Path) -> None:
    status = run(["git", "status", "--porcelain"], cwd=repo_root, capture=True)
    if status.strip():
        raise CommandError("Working tree is not clean. Commit or stash changes first.")


def ensure_command(name: str) -> None:
    if not shutil.which(name):
        raise CommandError(f"{name} is required but was not found in PATH.")


def load_github_env(repo_root: Path) -> dict[str, str]:
    env = os.environ.copy()
    if env.get("GITHUB_TOKEN"):
        return env
    if env.get("GH_TOKEN"):
        env["GITHUB_TOKEN"] = env["GH_TOKEN"]
        return env

    token = subprocess.run(
        ["gh", "auth", "token"],
        cwd=repo_root,
        text=True,
        capture_output=True,
    )
    if token.returncode == 0 and token.stdout.strip():
        env["GITHUB_TOKEN"] = token.stdout.strip()
        return env

    raise CommandError("Could not resolve GitHub token. Provide GITHUB_TOKEN, GH_TOKEN, or run 'gh auth login'.")


def resolve_base_remote(repo_root: Path, requested: str, base_branch: str) -> str:
    if requested:
        if not remote_exists(repo_root, requested):
            raise CommandError(f"Base remote {requested!r} does not exist.")
        if not remote_has_branch(repo_root, requested, base_branch):
            raise CommandError(
                f"Base branch {base_branch!r} was not found on remote {requested!r}."
            )
        return requested

    if remote_exists(repo_root, "upstream") and remote_has_branch(repo_root, "upstream", base_branch):
        return "upstream"

    if not remote_exists(repo_root, "origin"):
        raise CommandError("Base remote 'origin' does not exist.")
    if not remote_has_branch(repo_root, "origin", base_branch):
        raise CommandError(f"Base branch {base_branch!r} was not found on remote 'origin'.")
    return "origin"


def resolve_target_repo(repo_root: Path, explicit: str, base_remote: str) -> str:
    return explicit or parse_remote_owner_repo(repo_root, base_remote)


def resolve_head_owner(repo_root: Path, explicit: str, push_remote: str) -> str:
    if explicit:
        return explicit
    return parse_remote_owner_repo(repo_root, push_remote).split("/", 1)[0]


def validate_generated_site_file(site_file: Path) -> None:
    if not site_file.is_file() or site_file.stat().st_size == 0:
        raise CommandError(f"Expected docs release note file is missing or empty: {site_file}")

    in_frontmatter = False
    seen_heading = False
    with site_file.open("r", encoding="utf-8") as handle:
        for index, line in enumerate(handle):
            line = line.rstrip("\n")
            if index == 0 and line == "---":
                in_frontmatter = True
                continue
            if in_frontmatter and line == "---":
                in_frontmatter = False
                continue
            if in_frontmatter:
                continue
            if line.startswith("Full Changelog:"):
                continue
            if line.startswith("## "):
                seen_heading = True
                break
    if not seen_heading:
        raise CommandError(f"Generated file lacks release note sections ('## ...'): {site_file}")


def get_existing_pr(
    repo_root: Path,
    *,
    github_env: dict[str, str],
    target_repo: str,
    head_owner: str,
    head_branch: str,
    base_branch: str,
) -> str:
    output = run(
        [
            "gh",
            "pr",
            "list",
            "--repo",
            target_repo,
            "--head",
            f"{head_owner}:{head_branch}",
            "--base",
            base_branch,
            "--state",
            "open",
            "--json",
            "number,url",
        ],
        cwd=repo_root,
        env=github_env,
        capture=True,
    )
    prs = json.loads(output)
    if not prs:
        return ""
    pr = prs[0]
    return f"{pr['number']} {pr['url']}"


def main() -> int:
    parser = argparse.ArgumentParser(description="Create/update docs release note and open a PR.")
    parser.add_argument("--tag", required=True, help="Release tag like v1.35.7")
    parser.add_argument("--no-generate", action="store_true", help="Skip running hack/generate-release-note.sh")
    parser.add_argument("--dry-run", action="store_true", help="Print commands without executing them")
    parser.add_argument("--force-push", action="store_true", help="Push with --force-with-lease")
    parser.add_argument(
        "--base-remote",
        default="",
        help="Remote that hosts the base branch (default: upstream if it has base branch, else origin)",
    )
    parser.add_argument("--push-remote", default="origin", help="Remote to push head branch to")
    parser.add_argument("--base-branch", default="documentation", help="Base branch for the docs PR")
    parser.add_argument("--target-repo", default="", help="PR target repo as owner/repo")
    parser.add_argument("--head-owner", default="", help="PR head owner")
    args = parser.parse_args()

    ensure_command("git")
    ensure_command("gh")

    repo_root = ensure_repo_root()
    ensure_clean_worktree(repo_root)

    site_file = repo_root / "content" / "en" / "blog" / "releases" / f"{args.tag}.md"
    head_branch = f"doc/release-note-{args.tag}"
    title = f"Update release notes for {args.tag}"
    body = f"This PR updates the release notes for version {args.tag}."
    labels = ["kind/documentation", "release-note-none"]

    if not remote_exists(repo_root, args.push_remote):
        raise CommandError(f"Push remote {args.push_remote!r} does not exist.")

    base_remote = resolve_base_remote(repo_root, args.base_remote, args.base_branch)
    target_repo = resolve_target_repo(repo_root, args.target_repo, base_remote)
    head_owner = resolve_head_owner(repo_root, args.head_owner, args.push_remote)
    github_env = load_github_env(repo_root)

    existing_pr = get_existing_pr(
        repo_root,
        github_env=github_env,
        target_repo=target_repo,
        head_owner=head_owner,
        head_branch=head_branch,
        base_branch=args.base_branch,
    )
    if existing_pr:
        print(
            f"[OK] PR already exists for {head_owner}:{head_branch} -> {target_repo}:{args.base_branch}: {existing_pr}"
        )
        return 0

    original_ref: str | None = None
    temp_path = Path(
        tempfile.NamedTemporaryFile(
            prefix=f"cloud-provider-azure-release-notes.{args.tag}.",
            suffix=".md",
            delete=False,
        ).name
    )
    try:
        # Capture original branch so we can restore it in the finally block.
        # This is guarded by dry_run because dry-run mode prints checkout
        # commands without executing them, so no branch switch actually happens
        # and no restore is needed.
        if not args.dry_run:
            _orig = run(
                ["git", "rev-parse", "--abbrev-ref", "HEAD"],
                cwd=repo_root,
                capture=True,
            )
            # --abbrev-ref returns literal "HEAD" when detached; fall back to SHA.
            original_ref = (
                _orig
                if _orig != "HEAD"
                else run(["git", "rev-parse", "HEAD"], cwd=repo_root, capture=True)
            )

        run(["git", "fetch", base_remote, args.base_branch], cwd=repo_root, dry_run=args.dry_run)
        run(
            ["git", "checkout", "-B", head_branch, f"{base_remote}/{args.base_branch}"],
            cwd=repo_root,
            dry_run=args.dry_run,
        )

        if not args.no_generate:
            run(
                ["./hack/generate-release-note.sh", args.tag, str(temp_path), "true"],
                cwd=repo_root,
                dry_run=args.dry_run,
            )

        if args.dry_run:
            print("[OK] Dry run: skipping generated content validation.")
        else:
            validate_generated_site_file(site_file)

        run(["git", "add", str(site_file)], cwd=repo_root, dry_run=args.dry_run)

        if not args.dry_run:
            staged = subprocess.run(
                ["git", "diff", "--cached", "--quiet"],
                cwd=repo_root,
                text=True,
                capture_output=True,
            )
            if staged.returncode == 0:
                print(f"[OK] No changes to commit for {site_file.relative_to(repo_root)}")
            elif staged.returncode == 1:
                run(["git", "commit", "-m", title], cwd=repo_root)
            else:
                stderr = (staged.stderr or "").strip()
                raise CommandError(stderr or "Could not determine whether there are staged changes.")

        push_cmd = ["git", "push"]
        if args.force_push:
            push_cmd.append("--force-with-lease")
        push_cmd.extend(["-u", args.push_remote, head_branch])
        run(push_cmd, cwd=repo_root, dry_run=args.dry_run)

        run(
            [
                "gh",
                "pr",
                "create",
                "--repo",
                target_repo,
                "--head",
                f"{head_owner}:{head_branch}",
                "--base",
                args.base_branch,
                "--title",
                title,
                "--body",
                body,
                "--label",
                labels[0],
                "--label",
                labels[1],
            ],
            cwd=repo_root,
            env=github_env,
            dry_run=args.dry_run,
        )
    finally:
        temp_path.unlink(missing_ok=True)
        if original_ref is not None:
            try:
                run(["git", "checkout", original_ref], cwd=repo_root)
            except CommandError as exc:
                print(f"[WARN] Could not restore original branch {original_ref}: {exc}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except CommandError as exc:
        print(f"[ERROR] {exc}", file=sys.stderr)
        sys.exit(1)
