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


STATE_FILE = Path(".git/cherry-pick-pr.json")
STATE_VERSION = 1
RELEASE_NOTE_RE = re.compile(
    r"(?s)(?:Release note\*\*:\s*(?:<!--[^<>]*-->\s*)?```(?:release-note)?|```release-note)(.+?)```"
)
DOCS_BLOCK_RE = re.compile(r"(?s)```docs\s*\n(.*?)```")
BRANCH_PREFIX_RE = re.compile(r"^\[(?P<branch>[^\]]+)\]\s+")


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
    capture: bool = False,
    dry_run: bool = False,
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


def run_result(
    cmd: list[str],
    *,
    cwd: Path,
    env: dict[str, str] | None = None,
    dry_run: bool = False,
) -> subprocess.CompletedProcess[str]:
    if dry_run:
        print_cmd(cmd)
        return subprocess.CompletedProcess(cmd, 0, "", "")
    return subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=True)


def run_optional(cmd: list[str], *, cwd: Path) -> bool:
    proc = subprocess.run(cmd, cwd=cwd, text=True, capture_output=True)
    return proc.returncode == 0


def ensure_command(name: str) -> None:
    if shutil.which(name) is None:
        raise CommandError(f"Required command not found in PATH: {name}")


def ensure_repo_root(repo_arg: str) -> Path:
    repo = Path(repo_arg).resolve()
    proc = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "--show-toplevel"],
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0 or not proc.stdout.strip():
        raise CommandError(f"{repo} is not a git checkout")
    return Path(proc.stdout.strip())


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
    raise CommandError(f"Could not derive owner/repo from remote {remote!r}")


def resolve_github_repo(repo_root: Path, remote: str, explicit_repo: str) -> str:
    if explicit_repo:
        return explicit_repo
    if not run_optional(["git", "-C", str(repo_root), "remote", "get-url", remote], cwd=repo_root):
        raise CommandError(f"Remote {remote!r} does not exist")
    return parse_remote_owner_repo(repo_root, remote)


def ensure_clean_worktree(repo_root: Path) -> None:
    status = run(["git", "-C", str(repo_root), "status", "--porcelain"], cwd=repo_root, capture=True)
    if status.strip():
        raise CommandError("Working tree is not clean. Commit or stash changes first.")


def ensure_remote_exists(repo_root: Path, remote: str) -> None:
    if not run_optional(["git", "-C", str(repo_root), "remote", "get-url", remote], cwd=repo_root):
        raise CommandError(f"Remote {remote!r} does not exist")


def fetch_remote_branch(repo_root: Path, remote: str, branch: str, *, dry_run: bool) -> None:
    run(["git", "-C", str(repo_root), "fetch", remote, branch], cwd=repo_root, dry_run=dry_run)


def remote_branch_sha(repo_root: Path, remote: str, branch: str) -> str:
    try:
        return run(["git", "-C", str(repo_root), "rev-parse", "--verify", f"{remote}/{branch}"], cwd=repo_root, capture=True)
    except CommandError as exc:
        raise CommandError(f"Remote branch {remote}/{branch} was not found") from exc


def current_branch(repo_root: Path) -> str:
    branch = run(["git", "-C", str(repo_root), "branch", "--show-current"], cwd=repo_root, capture=True)
    return branch.strip()


def local_branch_exists(repo_root: Path, branch: str) -> bool:
    return run_optional(
        ["git", "-C", str(repo_root), "show-ref", "--verify", "--quiet", f"refs/heads/{branch}"],
        cwd=repo_root,
    )


def git_path(repo_root: Path) -> Path:
    return repo_root / STATE_FILE


def save_state(repo_root: Path, state: dict[str, object]) -> None:
    path = git_path(repo_root)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def load_state(repo_root: Path) -> dict[str, object]:
    path = git_path(repo_root)
    if not path.is_file():
        raise CommandError("No cherry-pick state file found. Start with 'pick'.")
    state = json.loads(path.read_text(encoding="utf-8"))
    if state.get("version") != STATE_VERSION:
        raise CommandError(f"Unsupported state version: {state.get('version')!r}")
    return state


def remove_state(repo_root: Path) -> None:
    path = git_path(repo_root)
    if path.exists():
        path.unlink()


def ensure_no_state(repo_root: Path) -> None:
    path = git_path(repo_root)
    if path.exists():
        raise CommandError(
            f"Existing state file found at {path}. Run 'abort' before starting a new cherry-pick."
        )


def gh_json(repo_root: Path, args: list[str]) -> object:
    output = run(["gh", *args], cwd=repo_root, capture=True)
    return json.loads(output)


def resolve_requester(repo_root: Path, explicit: str) -> str:
    if explicit:
        return explicit
    user = gh_json(repo_root, ["api", "user"])
    login = user.get("login")
    if not login:
        raise CommandError("Could not determine authenticated GitHub user")
    return str(login)


def fetch_pr_metadata(repo_root: Path, github_repo: str, number: int) -> dict[str, object]:
    payload = gh_json(
        repo_root,
        [
            "pr",
            "view",
            str(number),
            "--repo",
            github_repo,
            "--json",
            "number,title,body,state,mergedAt,mergeCommit,baseRefName,labels,url",
        ],
    )
    if payload.get("state") != "MERGED" or not payload.get("mergedAt"):
        raise CommandError(f"PR #{number} is not merged")
    merge_commit = payload.get("mergeCommit") or {}
    if not merge_commit or not merge_commit.get("oid"):
        raise CommandError(f"PR #{number} has no merge commit SHA")
    return payload


def select_kind_command(labels: list[dict[str, object]]) -> str:
    for label in labels:
        name = str(label.get("name") or "")
        if name.startswith("kind/"):
            return f"/kind {name.split('/', 1)[1]}"
    raise CommandError("Source PR does not have a kind/ label to carry into the cherry-pick PR")


def extract_release_note(body: str) -> str:
    match = RELEASE_NOTE_RE.search(body)
    if not match:
        return "NONE"
    note = match.group(1).strip()
    return note or "NONE"


def extract_docs_block(body: str) -> str:
    match = DOCS_BLOCK_RE.search(body)
    if not match:
        return ""
    return match.group(1).strip()


def strip_base_branch_prefix(title: str, base_branch: str) -> str:
    prefix = f"[{base_branch}] "
    if title.startswith(prefix):
        return title[len(prefix) :]
    return title


def detect_merge_parent_flag(repo_root: Path, sha: str) -> list[str]:
    parents = run(["git", "-C", str(repo_root), "rev-list", "--parents", "-1", sha], cwd=repo_root, capture=True).split()
    parent_count = max(len(parents) - 1, 0)
    if parent_count > 1:
        return ["-m", "1"]
    return []


def build_branch_name(pr_number: int, target_branch: str) -> str:
    return f"cherry-pick-{pr_number}-to-{target_branch}"


def find_existing_pr(
    repo_root: Path,
    target_repo: str,
    head_owner: str,
    branch_name: str,
    target_branch: str,
) -> dict[str, object] | None:
    output = gh_json(
        repo_root,
        [
            "pr",
            "list",
            "--repo",
            target_repo,
            "--head",
            f"{head_owner}:{branch_name}",
            "--base",
            target_branch,
            "--state",
            "open",
            "--json",
            "number,url",
        ],
    )
    if not output:
        return None
    return dict(output[0])


def conflicted_files(repo_root: Path) -> list[str]:
    output = run(
        ["git", "-C", str(repo_root), "diff", "--name-only", "--diff-filter=U"],
        cwd=repo_root,
        capture=True,
    )
    return [line for line in output.splitlines() if line.strip()]


def ensure_on_state_branch(repo_root: Path, state: dict[str, object]) -> None:
    expected = str(state["branch_name"])
    current = current_branch(repo_root)
    if current != expected:
        raise CommandError(f"Current branch is {current!r}; expected {expected!r}")


def build_pick_state(
    *,
    metadata: dict[str, object],
    github_repo: str,
    remote: str,
    push_remote: str,
    target_branch: str,
    target_sha: str,
    branch_name: str,
    head_owner: str,
    requester: str,
    chain: list[str],
    original_branch: str,
    kind_command: str,
) -> dict[str, object]:
    return {
        "version": STATE_VERSION,
        "source_pr_number": int(metadata["number"]),
        "source_pr_url": str(metadata["url"]),
        "source_title": str(metadata["title"]),
        "source_body": str(metadata.get("body") or ""),
        "source_base_branch": str(metadata["baseRefName"]),
        "source_merge_sha": str((metadata.get("mergeCommit") or {})["oid"]),
        "source_kind_command": kind_command,
        "github_repo": github_repo,
        "remote": remote,
        "push_remote": push_remote,
        "target_branch": target_branch,
        "target_base_sha": target_sha,
        "branch_name": branch_name,
        "head_owner": head_owner,
        "requester": requester,
        "chain": chain,
        "original_branch": original_branch,
        "cherry_pick_status": "planned",
        "conflict_files": [],
        "test_status": "not_run",
        "test_results": [],
        "pushed": False,
    }


def emit(output_mode: str, payload: dict[str, object], lines: list[str]) -> None:
    if output_mode == "json":
        print(json.dumps(payload, indent=2, sort_keys=True))
        return
    for line in lines:
        print(line)


def command_pick(args: argparse.Namespace, repo_root: Path) -> int:
    ensure_command("git")
    ensure_command("gh")
    ensure_remote_exists(repo_root, args.remote)
    ensure_remote_exists(repo_root, args.push_remote)
    ensure_no_state(repo_root)
    if not args.dry_run:
        ensure_clean_worktree(repo_root)

    github_repo = resolve_github_repo(repo_root, args.remote, args.github_repo)
    requester = resolve_requester(repo_root, args.requester)
    head_owner = parse_remote_owner_repo(repo_root, args.push_remote).split("/", 1)[0]
    metadata = fetch_pr_metadata(repo_root, github_repo, args.pr)
    source_base_branch = str(metadata["baseRefName"])
    if args.target_branch == source_base_branch:
        raise CommandError(
            f"Target branch {args.target_branch!r} matches the source PR base branch and is not a cherry-pick target"
        )
    kind_command = select_kind_command(list(metadata.get("labels") or []))

    fetch_remote_branch(repo_root, args.remote, args.target_branch, dry_run=args.dry_run)
    target_sha = remote_branch_sha(repo_root, args.remote, args.target_branch)
    branch_name = build_branch_name(args.pr, args.target_branch)

    if local_branch_exists(repo_root, branch_name):
        raise CommandError(f"Local branch {branch_name!r} already exists")
    existing_pr = find_existing_pr(repo_root, github_repo, head_owner, branch_name, args.target_branch)
    if existing_pr:
        raise CommandError(
            f"Existing cherry-pick PR already open: #{existing_pr['number']} {existing_pr['url']}"
        )

    parent_flag = detect_merge_parent_flag(repo_root, str((metadata["mergeCommit"])["oid"]))
    original = current_branch(repo_root)
    state = build_pick_state(
        metadata=metadata,
        github_repo=github_repo,
        remote=args.remote,
        push_remote=args.push_remote,
        target_branch=args.target_branch,
        target_sha=target_sha,
        branch_name=branch_name,
        head_owner=head_owner,
        requester=requester,
        chain=list(args.chain or []),
        original_branch=original,
        kind_command=kind_command,
    )

    payload = {
        "action": "pick",
        "branch_name": branch_name,
        "github_repo": github_repo,
        "merge_sha": state["source_merge_sha"],
        "mode": "dry-run" if args.dry_run else "apply",
        "requester": requester,
        "target_base_sha": target_sha,
        "target_branch": args.target_branch,
        "use_mainline": bool(parent_flag),
    }
    lines = [
        f"PR:            #{args.pr}",
        f"Target branch: {args.target_branch}",
        f"Base ref:      {args.remote}/{args.target_branch} @ {target_sha}",
        f"Branch name:   {branch_name}",
        f"Requester:     {requester}",
        f"Merge mode:    {'merge commit (-m 1)' if parent_flag else 'single-parent commit'}",
    ]

    if args.dry_run:
        emit(args.output, payload, lines)
        return 0

    save_state(repo_root, state)
    branch_created = False
    try:
        run(
            ["git", "-C", str(repo_root), "checkout", "-b", branch_name, f"{args.remote}/{args.target_branch}"],
            cwd=repo_root,
        )
        branch_created = True
        cherry_pick_cmd = ["git", "-C", str(repo_root), "cherry-pick", *parent_flag, str(state["source_merge_sha"])]
        proc = run_result(cherry_pick_cmd, cwd=repo_root)
        stdout = (proc.stdout or "").strip()
        stderr = (proc.stderr or "").strip()
        if stdout:
            print(stdout)
        if stderr:
            print(stderr, file=sys.stderr)
        if proc.returncode == 0:
            state["cherry_pick_status"] = "applied"
            state["test_status"] = "not_run"
            save_state(repo_root, state)
            emit(
                args.output,
                {**payload, "status": "applied"},
                lines + ["Cherry-pick applied cleanly"],
            )
            return 0

        conflicts = conflicted_files(repo_root)
        state["cherry_pick_status"] = "conflicted"
        state["conflict_files"] = conflicts
        save_state(repo_root, state)
        emit(
            args.output,
            {**payload, "status": "conflicted", "conflict_files": conflicts},
            lines + ["Cherry-pick has conflicts", *[f"  - {path}" for path in conflicts]],
        )
        return 3
    except Exception:
        if not branch_created:
            remove_state(repo_root)
        raise


def command_continue(args: argparse.Namespace, repo_root: Path) -> int:
    state = load_state(repo_root)
    ensure_on_state_branch(repo_root, state)
    if state.get("cherry_pick_status") != "conflicted":
        raise CommandError("Cherry-pick is not in conflicted state")
    conflicts = conflicted_files(repo_root)
    if conflicts:
        raise CommandError(f"Unresolved conflicts remain: {', '.join(conflicts)}")

    env = os.environ.copy()
    env["GIT_EDITOR"] = "true"
    run(["git", "-C", str(repo_root), "cherry-pick", "--continue"], cwd=repo_root, env=env)
    state["cherry_pick_status"] = "applied"
    state["conflict_files"] = []
    state["test_status"] = "not_run"
    state["test_results"] = []
    save_state(repo_root, state)
    emit(
        args.output,
        {"action": "continue", "status": "applied", "branch_name": state["branch_name"]},
        ["Cherry-pick continued successfully"],
    )
    return 0


def nearest_go_mod(repo_root: Path, file_path: Path) -> Path | None:
    candidate = file_path.parent
    while True:
        if (candidate / "go.mod").is_file():
            return candidate
        if candidate == repo_root:
            break
        candidate = candidate.parent
    return repo_root if (repo_root / "go.mod").is_file() else None


def gather_test_targets(repo_root: Path, base_sha: str) -> tuple[list[dict[str, object]], bool]:
    diff_names = run(
        ["git", "-C", str(repo_root), "diff", "--name-only", f"{base_sha}..HEAD"],
        cwd=repo_root,
        capture=True,
    )
    diff_status = run(
        ["git", "-C", str(repo_root), "diff", "--name-status", f"{base_sha}..HEAD"],
        cwd=repo_root,
        capture=True,
    )
    added_files = any(line.startswith("A\t") for line in diff_status.splitlines())

    groups: dict[Path, dict[str, object]] = {}
    for raw in diff_names.splitlines():
        if not raw.endswith(".go"):
            continue
        full_path = repo_root / raw
        module_root = nearest_go_mod(repo_root, full_path)
        if module_root is None:
            continue
        group = groups.setdefault(module_root, {"module_root": module_root, "packages": set(), "all": False})
        package_dir = full_path.parent
        if not package_dir.exists():
            group["all"] = True
            continue
        relative = package_dir.relative_to(module_root)
        package = "." if str(relative) == "." else f"./{relative.as_posix()}"
        group["packages"].add(package)

    targets = []
    for module_root, group in sorted(groups.items(), key=lambda item: str(item[0])):
        if group["all"]:
            packages = ["./..."]
        else:
            packages = sorted(group["packages"])
        targets.append(
            {
                "module_root": str(module_root),
                "packages": packages,
            }
        )
    return targets, added_files


def classify_test_failure(output: str) -> str:
    lowered = output.lower()
    if "[build failed]" in lowered or "[setup failed]" in lowered or "build failed" in lowered:
        return "build_failed"
    return "failed"


def command_test(args: argparse.Namespace, repo_root: Path) -> int:
    state = load_state(repo_root)
    ensure_on_state_branch(repo_root, state)
    if state.get("cherry_pick_status") != "applied":
        raise CommandError("Cherry-pick must be applied before running tests")

    targets, added_files = gather_test_targets(repo_root, str(state["target_base_sha"]))
    results: list[dict[str, object]] = []
    overall = "passed"

    for target in targets:
        module_root = Path(str(target["module_root"]))
        packages = list(target["packages"])
        cmd = ["go", "test", "-v", "-count=1", *packages]
        proc = run_result(cmd, cwd=module_root)
        output = ((proc.stdout or "") + (proc.stderr or "")).strip()
        if output:
            print(output)
        result = {
            "command": quote_cmd(cmd),
            "module_root": str(module_root),
            "packages": packages,
            "returncode": proc.returncode,
            "status": "passed" if proc.returncode == 0 else classify_test_failure(output),
        }
        results.append(result)
        if proc.returncode != 0:
            overall = "build_failed" if result["status"] == "build_failed" else "failed"

    if added_files:
        proc = run_result(["make", "test-check"], cwd=repo_root)
        output = ((proc.stdout or "") + (proc.stderr or "")).strip()
        if output:
            print(output)
        result = {
            "command": "make test-check",
            "module_root": str(repo_root),
            "packages": [],
            "returncode": proc.returncode,
            "status": "passed" if proc.returncode == 0 else "failed",
        }
        results.append(result)
        if proc.returncode != 0 and overall == "passed":
            overall = "failed"

    state["test_status"] = overall
    state["test_results"] = results
    save_state(repo_root, state)

    payload = {
        "action": "test",
        "status": overall,
        "results": results,
    }
    lines = [f"Test status: {overall}"]
    for result in results:
        lines.append(f"{result['status']}: {result['command']}")

    emit(args.output, payload, lines)
    return 0 if overall == "passed" else 2


def replace_section(template: str, heading: str, next_heading: str, content: str) -> str:
    pattern = re.compile(rf"(?s)(^{re.escape(heading)}\n).*?(?=^{re.escape(next_heading)}\n)", re.MULTILINE)
    if not pattern.search(template):
        raise CommandError(f"Required template anchor missing: {heading}")
    return pattern.sub(rf"\1{content}\n\n", template, count=1)


def replace_line(template: str, old: str, new: str) -> str:
    if old not in template:
        raise CommandError(f"Required template anchor missing: {old}")
    return template.replace(old, new, 1)


def replace_fenced_block(template: str, fence: str, content: str) -> str:
    pattern = re.compile(rf"(?s)(```{re.escape(fence)}\n).*?(```)")
    if not pattern.search(template):
        raise CommandError(f"Required template anchor missing: ```{fence}")
    replacement = rf"\1{content.rstrip()}\n\2" if content else rf"\1\2"
    return pattern.sub(replacement, template, count=1)


def build_pr_body(repo_root: Path, state: dict[str, object]) -> str:
    template_path = repo_root / ".github" / "PULL_REQUEST_TEMPLATE.md"
    template = template_path.read_text(encoding="utf-8")
    required = [
        "#### What type of PR is this?",
        "#### What this PR does / why we need it:",
        "Fixes #",
        "```release-note",
        "```docs",
    ]
    for anchor in required:
        if anchor not in template:
            raise CommandError(f"Required template anchor missing: {anchor}")

    description = (
        f"Cherry-picks #{state['source_pr_number']} onto `{state['target_branch']}`.\n\n"
        f"Source PR: {state['source_pr_url']}\n"
        f"Source commit: `{state['source_merge_sha']}`"
    )
    template = replace_section(
        template,
        "#### What type of PR is this?",
        "#### What this PR does / why we need it:",
        str(state["source_kind_command"]),
    )
    template = replace_section(
        template,
        "#### What this PR does / why we need it:",
        "#### Which issue(s) this PR fixes:",
        description,
    )
    template = replace_section(
        template,
        "#### Special notes for your reviewer:",
        "#### Does this PR introduce a user-facing change?",
        "",
    )
    template = replace_line(template, "Fixes #", "Fixes #")
    template = replace_fenced_block(template, "release-note", extract_release_note(str(state["source_body"])))
    template = replace_fenced_block(template, "docs", extract_docs_block(str(state["source_body"])))

    extras = [f"/assign {state['requester']}"]
    chain = list(state.get("chain") or [])
    if chain:
        extras.append(f"/cherrypick {' '.join(chain)}")
    return template.rstrip() + "\n\n" + "\n".join(extras) + "\n"


def command_create_pr(args: argparse.Namespace, repo_root: Path) -> int:
    state = load_state(repo_root)
    ensure_on_state_branch(repo_root, state)
    if state.get("cherry_pick_status") != "applied":
        raise CommandError("Cherry-pick is not ready for PR creation")
    if state.get("test_status") != "passed":
        raise CommandError("Tests have not recorded a successful run")

    title = f"[{state['target_branch']}] {strip_base_branch_prefix(str(state['source_title']), str(state['source_base_branch']))}"
    body = build_pr_body(repo_root, state)

    with tempfile.NamedTemporaryFile("w", delete=False, encoding="utf-8") as handle:
        handle.write(body)
        body_file = handle.name

    try:
        run(["git", "-C", str(repo_root), "push", "-u", str(state["push_remote"]), str(state["branch_name"])], cwd=repo_root)
        state["pushed"] = True
        save_state(repo_root, state)
        create_cmd = [
            "gh",
            "pr",
            "create",
            "--repo",
            str(state["github_repo"]),
            "--base",
            str(state["target_branch"]),
            "--head",
            f"{state['head_owner']}:{state['branch_name']}",
            "--title",
            title,
            "--body-file",
            body_file,
        ]
        pr_url = run(create_cmd, cwd=repo_root, capture=True).strip()
    finally:
        Path(body_file).unlink(missing_ok=True)

    emit(
        args.output,
        {"action": "create-pr", "status": "created", "pr_url": pr_url, "title": title},
        [f"Created PR: {pr_url}"],
    )
    remove_state(repo_root)
    return 0


def command_abort(args: argparse.Namespace, repo_root: Path) -> int:
    state = load_state(repo_root)
    branch_name = str(state["branch_name"])
    original_branch = str(state.get("original_branch") or "")
    pushed = bool(state.get("pushed"))

    if run_optional(["git", "-C", str(repo_root), "rev-parse", "--verify", "CHERRY_PICK_HEAD"], cwd=repo_root):
        run(["git", "-C", str(repo_root), "cherry-pick", "--abort"], cwd=repo_root)

    current = current_branch(repo_root)
    if original_branch and current == branch_name and original_branch != branch_name:
        run(["git", "-C", str(repo_root), "checkout", original_branch], cwd=repo_root)

    if not pushed and local_branch_exists(repo_root, branch_name):
        if current_branch(repo_root) == branch_name and original_branch and original_branch != branch_name:
            run(["git", "-C", str(repo_root), "checkout", original_branch], cwd=repo_root)
        if current_branch(repo_root) != branch_name:
            run(["git", "-C", str(repo_root), "branch", "-D", branch_name], cwd=repo_root)

    remove_state(repo_root)
    emit(
        args.output,
        {"action": "abort", "status": "aborted", "branch_name": branch_name},
        ["Cherry-pick state cleared"],
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Cherry-pick a merged PR onto a release branch.")
    parser.add_argument("--repo", default=".", help="Path to the target git repo (default: .)")
    parser.add_argument("--output", choices=("text", "json"), default="text", help="Output format")

    subparsers = parser.add_subparsers(dest="command", required=True)

    pick = subparsers.add_parser("pick", help="Validate and apply a cherry-pick")
    pick.add_argument("--pr", type=int, required=True, help="Source PR number")
    pick.add_argument("--target-branch", required=True, help="Target branch to cherry-pick onto")
    pick.add_argument("--remote", default="upstream", help="Remote hosting the target branch")
    pick.add_argument("--push-remote", default="origin", help="Remote used to push the cherry-pick branch")
    pick.add_argument("--github-repo", default="", help="Override target GitHub repo as owner/repo")
    pick.add_argument("--requester", default="", help="Override requester login for /assign")
    pick.add_argument("--chain", nargs="*", default=[], help="Optional chained cherry-pick targets")
    pick.add_argument("--dry-run", action="store_true", help="Validate and print the plan without mutating git")
    pick.set_defaults(func=command_pick)

    continue_cmd = subparsers.add_parser("continue", help="Continue a conflicted cherry-pick")
    continue_cmd.set_defaults(func=command_continue)

    test = subparsers.add_parser("test", help="Run targeted validation for the cherry-pick")
    test.set_defaults(func=command_test)

    create_pr = subparsers.add_parser("create-pr", help="Push the branch and create the PR")
    create_pr.set_defaults(func=command_create_pr)

    abort = subparsers.add_parser("abort", help="Abort and clean up the current cherry-pick state")
    abort.set_defaults(func=command_abort)

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    try:
        repo_root = ensure_repo_root(args.repo)
        return int(args.func(args, repo_root))
    except CommandError as exc:
        err(str(exc))
        return 1


if __name__ == "__main__":
    sys.exit(main())
