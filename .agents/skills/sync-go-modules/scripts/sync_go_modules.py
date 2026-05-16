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
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path


GO_DIRECTIVE_RE = re.compile(r"^go\s+(?P<version>\d+(?:\.\d+){1,2})\s*$")
GO_VERSION_RE = re.compile(r"\bgo(?P<version>\d+(?:\.\d+){1,2})\b")


class CommandError(RuntimeError):
    pass


def quote_cmd(cmd: list[str]) -> str:
    return " ".join(shlex.quote(part) for part in cmd)


def print_cmd(cmd: list[str], *, cwd: Path) -> None:
    print(f"+ (cd {shlex.quote(str(cwd))} && {quote_cmd(cmd)})")


def run(
    cmd: list[str],
    *,
    cwd: Path,
    env: dict[str, str] | None = None,
    capture: bool = False,
) -> str:
    proc = subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=capture)
    if proc.returncode != 0:
        stderr = (proc.stderr or "").strip()
        stdout = (proc.stdout or "").strip()
        details = stderr or stdout or f"command exited {proc.returncode}"
        raise CommandError(f"{quote_cmd(cmd)}: {details}")
    return (proc.stdout or "").rstrip("\n")


def run_logged(
    cmd: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
    dry_run: bool,
) -> None:
    print_cmd(cmd, cwd=cwd)
    if dry_run:
        return
    run(cmd, cwd=cwd, env=env)


def git(repo: Path, args: list[str], *, capture: bool = True) -> str:
    return run(["git", "-C", str(repo), *args], cwd=repo, capture=capture)


def resolve_repo_root(path: Path) -> Path:
    root = run(
        ["git", "-C", str(path), "rev-parse", "--show-toplevel"],
        cwd=path,
        capture=True,
    )
    return Path(root).resolve()


def status_short(repo: Path) -> str:
    return git(repo, ["status", "--short"])


def discover_go_modules(repo: Path) -> list[str]:
    out = git(repo, ["ls-files", "go.mod", "**/go.mod"])
    go_mods = [line for line in out.splitlines() if line]
    if not go_mods:
        raise CommandError("No tracked go.mod files were found")

    modules: set[str] = set()
    for mod in go_mods:
        if mod == "go.mod":
            modules.add(".")
        elif mod.endswith("/go.mod"):
            modules.add(str(Path(mod).parent))
        else:
            raise CommandError(f"Unexpected go.mod path from git ls-files: {mod}")
    return sorted(modules)


def parse_version(version: str) -> tuple[int, int, int]:
    parts = [int(part) for part in version.split(".")]
    while len(parts) < 3:
        parts.append(0)
    return tuple(parts[:3])


def highest_go_directive(repo: Path, modules: list[str]) -> str:
    highest = "0.0.0"
    for module in modules:
        mod_file = repo / module / "go.mod"
        for line in mod_file.read_text(encoding="utf-8").splitlines():
            match = GO_DIRECTIVE_RE.match(line)
            if match and parse_version(match["version"]) > parse_version(highest):
                highest = match["version"]
    if highest == "0.0.0":
        raise CommandError("No go directive found in tracked go.mod files")
    return highest


def installed_go_version(go_binary: str, repo: Path, env: dict[str, str]) -> str:
    output = run([go_binary, "version"], cwd=repo, env=env, capture=True)
    match = GO_VERSION_RE.search(output)
    if not match:
        raise CommandError(f"Could not parse Go version from: {output}")
    return match["version"]


def check_go_version(
    go_binary: str,
    repo: Path,
    modules: list[str],
    env: dict[str, str],
    *,
    dry_run: bool,
) -> None:
    required = highest_go_directive(repo, modules)
    if dry_run:
        print(f"[INFO] Highest go directive is {required}", file=sys.stderr)
        return

    installed = installed_go_version(go_binary, repo, env)
    if parse_version(installed) < parse_version(required):
        raise CommandError(
            f"Local Go {installed} is older than the highest repo go directive "
            f"{required}. Install or select Go {required}+ before syncing modules."
        )
    print(f"[INFO] Using Go {installed}; highest go directive is {required}", file=sys.stderr)


def print_diff_stat(repo: Path) -> None:
    stat = git(repo, ["diff", "--stat"])
    if not stat:
        print("[INFO] No repository diff after sync", file=sys.stderr)
        return
    print("[INFO] Repository diff after sync:", file=sys.stderr)
    print(stat, file=sys.stderr)


def check_clean(repo: Path) -> None:
    proc = subprocess.run(["git", "-C", str(repo), "diff", "--quiet"], cwd=repo)
    if proc.returncode == 0:
        return
    if proc.returncode == 1:
        raise CommandError("Repository has changes after sync; commit the generated diff to satisfy CI")
    raise CommandError(f"git diff --quiet exited {proc.returncode}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Synchronize all tracked Go modules and refresh the root vendor tree.",
    )
    parser.add_argument("--repo", default=".", help="Path inside the repository")
    parser.add_argument("--go", default="go", help="Go binary to execute")
    parser.add_argument(
        "--allow-dirty",
        action="store_true",
        help="Proceed even when the worktree already has uncommitted changes",
    )
    parser.add_argument(
        "--skip-vendor",
        action="store_true",
        help="Do not run go mod vendor in the root module",
    )
    parser.add_argument(
        "--update-vendor-licenses",
        action="store_true",
        help="Run make update-vendor-licenses after refreshing vendor/",
    )
    parser.add_argument(
        "--check-clean",
        action="store_true",
        help="Fail if the sync leaves any git diff, matching the CI clean-check step",
    )
    parser.add_argument("--dry-run", action="store_true", help="Print commands without running them")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    repo = resolve_repo_root(Path(args.repo).resolve())
    env = {**os.environ, "GOTOOLCHAIN": "local"}

    dirty = status_short(repo)
    if dirty and not args.allow_dirty:
        print(dirty, file=sys.stderr)
        raise CommandError(
            "Worktree has uncommitted changes. Inspect them first and rerun with "
            "--allow-dirty only if they are in scope for this module sync."
        )

    modules = discover_go_modules(repo)
    print("[INFO] Discovered modules:", file=sys.stderr)
    for module in modules:
        print(f"[INFO]   {module}", file=sys.stderr)

    check_go_version(args.go, repo, modules, env, dry_run=args.dry_run)

    for module in modules:
        module_dir = repo / module
        run_logged([args.go, "mod", "tidy"], cwd=module_dir, env=env, dry_run=args.dry_run)
        run_logged([args.go, "mod", "verify"], cwd=module_dir, env=env, dry_run=args.dry_run)

    if not args.skip_vendor:
        run_logged([args.go, "mod", "vendor"], cwd=repo, env=env, dry_run=args.dry_run)

    if args.update_vendor_licenses:
        run_logged(["make", "update-vendor-licenses"], cwd=repo, env=env, dry_run=args.dry_run)

    if not args.dry_run:
        print_diff_stat(repo)
        if args.check_clean:
            check_clean(repo)

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except CommandError as exc:
        print(f"[ERROR] {exc}", file=sys.stderr)
        raise SystemExit(1)
