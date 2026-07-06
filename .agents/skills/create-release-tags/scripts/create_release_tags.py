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
import re
import shlex
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path


STABLE_TAG_RE = re.compile(r"^v(?P<major>\d+)\.(?P<minor>\d+)\.(?P<patch>\d+)$")
RELEASE_BRANCH_RE = re.compile(r"^release-(?P<major>\d+)\.(?P<minor>\d+)$")
SERIES_RE = re.compile(r"^v?(?P<major>\d+)\.(?P<minor>\d+)$")


@dataclass(frozen=True)
class Series:
    major: int
    minor: int

    @property
    def prefix(self) -> str:
        return f"v{self.major}.{self.minor}."

    @property
    def glob(self) -> str:
        return f"v{self.major}.{self.minor}.*"


class GitError(RuntimeError):
    pass


def _quote_cmd(cmd: list[str]) -> str:
    return " ".join(shlex.quote(part) for part in cmd)


def run_git(repo: Path, args: list[str], *, capture: bool = True) -> str:
    cmd = ["git", "-C", str(repo)] + args
    proc = subprocess.run(cmd, capture_output=capture, text=True)
    if proc.returncode != 0:
        stderr = (proc.stderr or "").strip()
        stdout = (proc.stdout or "").strip()
        details = stderr or stdout or f"git exited {proc.returncode}"
        raise GitError(f"{_quote_cmd(cmd)}: {details}")
    return (proc.stdout or "").rstrip("\n")


def git_optional(repo: Path, args: list[str]) -> bool:
    cmd = ["git", "-C", str(repo)] + args
    proc = subprocess.run(cmd, capture_output=True, text=True)
    return proc.returncode == 0


def parse_series(branch: str | None, series: str | None, tag: str | None) -> Series:
    if series:
        m = SERIES_RE.match(series)
        if not m:
            raise ValueError(f"--series must look like '1.34' (got {series!r})")
        return Series(int(m["major"]), int(m["minor"]))

    if tag:
        m = STABLE_TAG_RE.match(tag)
        if not m:
            raise ValueError(f"--tag must look like 'v1.34.7' (got {tag!r})")
        return Series(int(m["major"]), int(m["minor"]))

    if branch:
        m = RELEASE_BRANCH_RE.match(branch)
        if not m:
            raise ValueError(
                f"--branch must look like 'release-1.34' (got {branch!r}); or pass --series 1.34"
            )
        return Series(int(m["major"]), int(m["minor"]))

    raise ValueError("Need --series, --tag, or --branch to determine series")


def series_from_branch(branch: str) -> Series | None:
    m = RELEASE_BRANCH_RE.match(branch)
    if not m:
        return None
    return Series(int(m["major"]), int(m["minor"]))



def ensure_remote(repo: Path, remote: str) -> None:
    if not git_optional(repo, ["remote", "get-url", remote]):
        remotes = run_git(repo, ["remote"]).splitlines()
        raise GitError(
            f"Remote {remote!r} not found. Existing remotes: {', '.join(remotes) or '(none)'}"
        )


def fetch_remote(repo: Path, remote: str) -> None:
    run_git(repo, ["fetch", remote, "--prune", "--tags"], capture=False)


def ensure_remote_branch(repo: Path, remote: str, branch: str) -> str:
    ref = f"{remote}/{branch}"
    sha = run_git(repo, ["rev-parse", "--verify", ref])
    return sha


def compute_next_tag(repo: Path, remote: str, branch: str, series: Series) -> str:
    merged = run_git(repo, ["tag", "--merged", f"{remote}/{branch}", "--list", series.glob])
    max_patch = -1
    stable_prefix = f"v{series.major}.{series.minor}."
    for line in merged.splitlines():
        tag = line.strip()
        if not tag.startswith(stable_prefix):
            continue
        m = STABLE_TAG_RE.match(tag)
        if not m:
            continue
        patch = int(m["patch"])
        max_patch = max(max_patch, patch)
    next_patch = 0 if max_patch < 0 else max_patch + 1
    return f"v{series.major}.{series.minor}.{next_patch}"


def ensure_tag_absent(repo: Path, tag: str) -> None:
    if git_optional(repo, ["show-ref", "--verify", "--quiet", f"refs/tags/{tag}"]):
        raise GitError(f"Tag {tag} already exists locally.")


def create_tag(repo: Path, tag: str, commit: str, *, message: str | None, sign: bool, lightweight: bool) -> None:
    if lightweight and sign:
        raise GitError("Cannot combine --lightweight and --sign.")

    if lightweight:
        run_git(repo, ["tag", tag, commit], capture=False)
        return

    msg = message or tag
    if sign:
        run_git(repo, ["tag", "-s", "-m", msg, tag, commit], capture=False)
    else:
        run_git(repo, ["tag", "-a", "-m", msg, tag, commit], capture=False)


def push_tag(repo: Path, remote: str, tag: str) -> None:
    run_git(repo, ["push", remote, tag], capture=False)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Create and optionally push the next vX.Y.Z tag for a release-X.Y branch."
    )
    parser.add_argument("--repo", default=".", help="Path to the target git repo (default: .)")
    parser.add_argument("--remote", default="upstream", help="Remote to fetch from / push to (default: upstream)")
    parser.add_argument("--branch", required=True, help="Release branch name (e.g. release-1.34)")
    parser.add_argument("--series", help="Override series (e.g. 1.34) if branch name does not match release-X.Y")
    parser.add_argument("--tag", help="Explicit tag to create (e.g. v1.34.7). If omitted, compute next tag.")
    parser.add_argument(
        "--create",
        action="store_true",
        help="Create the computed/explicit tag locally (default: plan only).",
    )
    parser.add_argument(
        "--push",
        action="store_true",
        help="Push the created tag to the remote (implies --create).",
    )
    parser.add_argument(
        "--no-fetch",
        action="store_true",
        help="Skip 'git fetch <remote> --prune --tags' (useful when offline).",
    )
    parser.add_argument(
        "--force-branch",
        action="store_true",
        help="No-op (kept for backward compatibility). Previously hard-reset local branch to <remote>/<branch>.",
    )
    parser.add_argument(
        "--message",
        help="Tag message for annotated/signed tags (default: the tag name).",
    )
    parser.add_argument("--sign", action="store_true", help="Create a signed tag (git tag -s).")
    parser.add_argument(
        "--lightweight",
        action="store_true",
        help="Create a lightweight tag instead of an annotated tag.",
    )
    args = parser.parse_args()

    repo = Path(args.repo).resolve()
    if not git_optional(repo, ["rev-parse", "--is-inside-work-tree"]):
        print(f"error: {repo} is not a git repo", file=sys.stderr)
        return 2

    try:
        ensure_remote(repo, args.remote)
        if args.no_fetch:
            print("note: --no-fetch used; remote refs and tags may be stale.", file=sys.stderr)
        else:
            fetch_remote(repo, args.remote)

        remote_sha = ensure_remote_branch(repo, args.remote, args.branch)
        series = parse_series(args.branch, args.series, args.tag)
        branch_series = series_from_branch(args.branch)
        if branch_series and branch_series != series:
            raise GitError(
                f"Series mismatch: branch {args.branch!r} implies {branch_series.major}.{branch_series.minor}, "
                f"but tag/series implies {series.major}.{series.minor}."
            )

        tag = args.tag or compute_next_tag(repo, args.remote, args.branch, series)
        if not STABLE_TAG_RE.match(tag):
            raise GitError(f"Refusing to create non-stable tag name {tag!r} (expected vX.Y.Z).")
        ensure_tag_absent(repo, tag)

        print(f"remote_branch: {args.remote}/{args.branch} @ {remote_sha}")
        print(f"series:        {series.major}.{series.minor}")
        print(f"next_tag:      {tag}")

        if not args.create and not args.push:
            print("")
            print("Plan only. Re-run with --create to create the tag, or --push to create + push it.")
            return 0

        create_tag(repo, tag, remote_sha, message=args.message, sign=args.sign, lightweight=args.lightweight)
        print(f"created_tag:   {tag} @ {remote_sha}")

        if args.push:
            push_tag(repo, args.remote, tag)
            print(f"pushed_tag:    {args.remote}/{tag}")

        return 0
    except (GitError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
