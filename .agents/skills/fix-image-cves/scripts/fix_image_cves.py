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
import time
from pathlib import Path
from typing import Any


STATE_FILE = Path(".git/fix-image-cves.json")
STATE_VERSION = 1
TRIVY_DB_STALE_SECONDS = 24 * 60 * 60


class CommandError(RuntimeError):
    pass


def err(message: str) -> None:
    print(f"[ERROR] {message}", file=sys.stderr)


def info(message: str) -> None:
    print(f"[INFO] {message}", file=sys.stderr)


def warn(message: str) -> None:
    print(f"[WARN] {message}", file=sys.stderr)


def quote_cmd(cmd: list[str]) -> str:
    return " ".join(shlex.quote(part) for part in cmd)


def print_cmd(cmd: list[str], *, cwd: Path | None = None) -> None:
    if cwd is None:
        print(f"+ {quote_cmd(cmd)}")
        return
    print(f"+ (cd {shlex.quote(str(cwd))} && {quote_cmd(cmd)})")


def run(
    cmd: list[str],
    *,
    cwd: Path,
    capture: bool = False,
    env: dict[str, str] | None = None,
) -> str:
    proc = subprocess.run(cmd, cwd=cwd, text=True, capture_output=capture, env=env)
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
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=cwd, text=True, capture_output=True, env=env)


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


def git_path(repo_root: Path) -> Path:
    return repo_root / STATE_FILE


def save_state(repo_root: Path, state: dict[str, Any]) -> None:
    path = git_path(repo_root)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def load_state(repo_root: Path) -> dict[str, Any]:
    path = git_path(repo_root)
    if not path.is_file():
        raise CommandError(
            f"No fix-image-cves state file found at {path}. Run scan first or use clean to reset state."
        )
    state = json.loads(path.read_text(encoding="utf-8"))
    if state.get("version") != STATE_VERSION:
        raise CommandError(f"Unsupported state version: {state.get('version')!r}")
    return state


def remove_state(repo_root: Path) -> bool:
    path = git_path(repo_root)
    if path.exists():
        path.unlink()
        return True
    return False


def relative_to_repo(repo_root: Path, path: Path) -> str:
    resolved = path.resolve()
    try:
        rel = resolved.relative_to(repo_root)
    except ValueError as exc:
        raise CommandError(f"{resolved} is outside repo root {repo_root}") from exc
    text = str(rel)
    return "." if text == "" else text


def abs_from_repo(repo_root: Path, rel_path: str) -> Path:
    if rel_path == ".":
        return repo_root
    return (repo_root / rel_path).resolve()


def detect_trivy_db_staleness() -> None:
    cache_root = Path(os.environ.get("TRIVY_CACHE_DIR", "~/.cache/trivy")).expanduser()
    candidates = (
        cache_root / "db" / "metadata.json",
        cache_root / "metadata.json",
    )
    for candidate in candidates:
        if candidate.is_file():
            age = time.time() - candidate.stat().st_mtime
            if age > TRIVY_DB_STALE_SECONDS:
                warn(f"Trivy DB metadata at {candidate} is older than 24h")
            return


def parse_fixed_version_candidates(fixed_version: str) -> list[str]:
    values = []
    for raw in fixed_version.split(","):
        candidate = raw.strip()
        if candidate:
            values.append(candidate)
    return values or [fixed_version.strip()]


def version_key(value: str) -> list[tuple[int, Any]]:
    key: list[tuple[int, Any]] = []
    for token in re.findall(r"\d+|[A-Za-z]+|[^A-Za-z\d]+", value):
        if token.isdigit():
            key.append((0, int(token)))
        else:
            key.append((1, token))
    return key


def highest_fixed_version(values: list[str]) -> str:
    candidates: list[str] = []
    for value in values:
        candidates.extend(parse_fixed_version_candidates(value))
    if not candidates:
        raise CommandError("No fixed versions available to compare")
    return max(candidates, key=version_key)


def sanitize_dockerfile_stage(result: dict[str, Any]) -> str:
    return "runtime" if result.get("Class") == "os-pkgs" else "unknown"


def classify_result(result: dict[str, Any]) -> str:
    if result.get("Class") == "lang-pkgs" and result.get("Type") == "gobinary":
        return "GO_MODULE"
    if result.get("Class") == "os-pkgs":
        return "BASE_IMAGE"
    return "OTHER"


def collect_scan_metadata(payload: dict[str, Any]) -> dict[str, Any]:
    metadata = payload.get("Metadata") or {}
    image_config = metadata.get("ImageConfig") or {}
    os_info = metadata.get("OS") or {}
    repo_tags = payload.get("RepoTags") or []
    repo_digests = payload.get("RepoDigests") or []
    return {
        "repo_tags": repo_tags,
        "repo_digests": repo_digests,
        "os": os_info,
        "architecture": image_config.get("architecture") or image_config.get("Architecture"),
        "created": image_config.get("created") or image_config.get("Created"),
    }


def parse_scan_findings(
    payload: dict[str, Any],
    *,
    module_root: str,
    dockerfile: str,
) -> list[dict[str, Any]]:
    findings: list[dict[str, Any]] = []
    for result in payload.get("Results") or []:
        classification = classify_result(result)
        target = str(result.get("Target") or "")
        class_name = str(result.get("Class") or "")
        type_name = str(result.get("Type") or "")
        for vuln in result.get("Vulnerabilities") or []:
            fixed_version = str(vuln.get("FixedVersion") or "").strip()
            if not fixed_version:
                continue
            finding = {
                "category": classification,
                "class": class_name,
                "type": type_name,
                "target": target,
                "package": str(vuln.get("PkgName") or ""),
                "installed_version": str(vuln.get("InstalledVersion") or ""),
                "fixed_version": fixed_version,
                "id": str(vuln.get("VulnerabilityID") or ""),
                "title": str(vuln.get("Title") or ""),
                "severity": str(vuln.get("Severity") or ""),
                "module_root": module_root,
                "dockerfile": dockerfile,
                "stage": sanitize_dockerfile_stage(result),
            }
            findings.append(finding)
    return findings


def summarize_scan(findings: list[dict[str, Any]]) -> None:
    counts = {"GO_MODULE": 0, "BASE_IMAGE": 0, "OTHER": 0}
    for finding in findings:
        counts[finding["category"]] = counts.get(finding["category"], 0) + 1
    print("Scan Summary")
    print("============")
    print(f"GO_MODULE: {counts.get('GO_MODULE', 0)}")
    print(f"BASE_IMAGE: {counts.get('BASE_IMAGE', 0)}")
    print(f"OTHER: {counts.get('OTHER', 0)}")
    if not findings:
        print("")
        print("No fixable vulnerabilities with FixedVersion were found.")
        return
    print("")
    print("Findings")
    print("--------")
    for finding in findings:
        print(
            f"{finding['category']:10s} {finding['id']:18s} "
            f"{finding['package']} {finding['installed_version']} -> {finding['fixed_version']}"
        )


def build_plan(scan: dict[str, Any]) -> dict[str, Any]:
    findings = scan.get("findings") or []
    go_groups: dict[str, dict[str, Any]] = {}
    base_groups: dict[tuple[str, str], dict[str, Any]] = {}
    other_findings: list[dict[str, Any]] = []
    evidence: list[dict[str, Any]] = []

    for finding in findings:
        category = finding["category"]
        if category == "GO_MODULE":
            package = finding["package"]
            group = go_groups.setdefault(
                package,
                {
                    "category": "GO_MODULE",
                    "module": package,
                    "module_root": finding["module_root"],
                    "target": finding["target"],
                    "installed_versions": set(),
                    "fixed_versions": [],
                    "cves": [],
                },
            )
            group["installed_versions"].add(finding["installed_version"])
            group["fixed_versions"].append(finding["fixed_version"])
            group["cves"].append(finding["id"])
            evidence.append(
                {
                    "category": "GO_MODULE",
                    "id": finding["id"],
                    "package": package,
                    "fixed_version": finding["fixed_version"],
                    "target_file": str(Path(finding["module_root"]) / "go.mod"),
                }
            )
        elif category == "BASE_IMAGE":
            key = (finding["dockerfile"], finding["stage"])
            group = base_groups.setdefault(
                key,
                {
                    "category": "BASE_IMAGE",
                    "dockerfile": finding["dockerfile"],
                    "stage": finding["stage"],
                    "packages": [],
                    "cves": [],
                },
            )
            group["packages"].append(
                {
                    "package": finding["package"],
                    "installed_version": finding["installed_version"],
                    "fixed_version": finding["fixed_version"],
                    "id": finding["id"],
                    "target": finding["target"],
                    "type": finding["type"],
                }
            )
            group["cves"].append(finding["id"])
            evidence.append(
                {
                    "category": "BASE_IMAGE",
                    "id": finding["id"],
                    "package": finding["package"],
                    "fixed_version": finding["fixed_version"],
                    "target_file": finding["dockerfile"],
                }
            )
        else:
            other_findings.append(finding)

    go_actions = []
    for package, group in sorted(go_groups.items()):
        fixed_version = highest_fixed_version(group["fixed_versions"])
        go_actions.append(
            {
                "module": package,
                "module_root": group["module_root"],
                "target": group["target"],
                "installed_versions": sorted(group["installed_versions"]),
                "fixed_version": fixed_version,
                "cves": sorted(set(group["cves"])),
            }
        )

    base_actions = []
    for (dockerfile, stage), group in sorted(base_groups.items()):
        packages = sorted(group["packages"], key=lambda item: (item["package"], item["id"]))
        base_actions.append(
            {
                "dockerfile": dockerfile,
                "stage": stage,
                "packages": packages,
                "cves": sorted(set(group["cves"])),
                "requires_base_image_target": True,
            }
        )

    return {
        "go_module_actions": go_actions,
        "base_image_actions": base_actions,
        "other_findings": other_findings,
        "evidence": evidence,
        "planned_vulnerability_keys": sorted(
            {
                vuln_key(finding)
                for finding in findings
                if finding["category"] in {"GO_MODULE", "BASE_IMAGE"}
            }
        ),
    }


def summarize_plan(plan: dict[str, Any]) -> None:
    go_actions = plan.get("go_module_actions") or []
    base_actions = plan.get("base_image_actions") or []
    other_findings = plan.get("other_findings") or []

    print("Plan Summary")
    print("============")
    print(f"Go module actions: {len(go_actions)}")
    print(f"Base image actions: {len(base_actions)}")
    print(f"Report-only findings: {len(other_findings)}")
    if go_actions:
        print("")
        print("Go Module Bumps")
        print("----------------")
        for action in go_actions:
            print(
                f"{action['module']} -> {action['fixed_version']} "
                f"(module root: {action['module_root']})"
            )
    if base_actions:
        print("")
        print("Base Image Recommendations")
        print("--------------------------")
        for action in base_actions:
            print(
                f"{action['dockerfile']} [{action['stage']}] "
                f"packages={len(action['packages'])} requires --base-image-target"
            )


def git_status_paths(repo_root: Path) -> set[str]:
    output = run(["git", "-C", str(repo_root), "status", "--porcelain"], cwd=repo_root, capture=True)
    paths: set[str] = set()
    for line in output.splitlines():
        if len(line) < 4:
            continue
        entry = line[3:]
        if " -> " in entry:
            entry = entry.split(" -> ", 1)[1]
        paths.add(entry)
    return paths


def path_is_under(path: str, prefix: str) -> bool:
    if prefix in {"", "."}:
        return True
    return path == prefix or path.startswith(prefix.rstrip("/") + "/")


def discover_go_modules(repo_root: Path) -> list[str]:
    output = run(["git", "-C", str(repo_root), "ls-files", "go.mod", "**/go.mod"], cwd=repo_root, capture=True)
    modules = []
    for mod_path in output.splitlines():
        directory = str(Path(mod_path).parent)
        modules.append("." if directory == "." else directory)
    unique = sorted(set(modules))
    if not unique:
        raise CommandError("No go.mod files found via git ls-files")
    return unique


def has_vendor_tree(repo_root: Path) -> bool:
    return (repo_root / "vendor").is_dir() and (repo_root / "vendor" / "modules.txt").exists()


def cleanup_vendor_license_artifacts(repo_root: Path) -> None:
    paths = (
        repo_root / "third_party",
        repo_root / "hack" / "lib",
        repo_root / "hack" / "update-vendor-licenses.sh",
    )
    for path in paths:
        if path.is_dir():
            shutil.rmtree(path, ignore_errors=True)
        elif path.exists():
            path.unlink()


def parse_base_image_targets(values: list[str]) -> dict[str, str]:
    targets: dict[str, str] = {}
    for value in values:
        if "=" in value:
            key, image = value.split("=", 1)
            key = key.strip()
            image = image.strip()
        else:
            key = "default"
            image = value.strip()
        if not key or not image:
            raise CommandError(f"Invalid --base-image-target value: {value!r}")
        targets[key] = image
    return targets


def select_base_image_target(
    action: dict[str, Any],
    targets: dict[str, str],
    *,
    allow_default: bool,
) -> str | None:
    if not targets:
        return None
    dockerfile = action["dockerfile"]
    stage = action["stage"]
    keys = [f"{dockerfile}:{stage}", stage, dockerfile]
    if allow_default:
        keys.append("default")
    for key in keys:
        if key in targets:
            return targets[key]
    return None


FROM_RE = re.compile(r"^(?P<prefix>\s*FROM(?:\s+--platform=[^\s]+)?\s+)(?P<image>\S+)(?P<suffix>.*)$")


def replace_dockerfile_from(repo_root: Path, dockerfile: str, stage: str, target_image: str) -> None:
    path = abs_from_repo(repo_root, dockerfile)
    lines = path.read_text(encoding="utf-8").splitlines()
    from_indexes = [index for index, line in enumerate(lines) if FROM_RE.match(line)]
    if not from_indexes:
        raise CommandError(f"No FROM line found in {dockerfile}")
    if stage == "runtime":
        index = from_indexes[-1]
    else:
        index = from_indexes[0]
    match = FROM_RE.match(lines[index])
    if not match:
        raise CommandError(f"Could not parse FROM line in {dockerfile}: {lines[index]!r}")
    lines[index] = f"{match.group('prefix')}{target_image}{match.group('suffix')}"
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def parse_vendor_modules(repo_root: Path) -> dict[str, str]:
    path = repo_root / "vendor" / "modules.txt"
    if not path.is_file():
        return {}
    modules: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.startswith("# "):
            continue
        parts = line.split()
        if len(parts) >= 3 and parts[2] != "=>":
            modules[parts[1]] = parts[2]
    return modules


def go_list_module_version(module_dir: Path, module: str) -> str:
    fmt = "{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}"
    output = run(
        ["go", "list", "-m", "-f", fmt, module],
        cwd=module_dir,
        capture=True,
    )
    return output.strip()


def normalize_path_set(values: list[str]) -> set[str]:
    return {value for value in values if value}


def find_unexpected_changes(
    current_paths: set[str],
    *,
    preexisting_paths: set[str],
    recorded_paths: set[str],
) -> list[str]:
    unexpected = []
    for path in sorted(current_paths):
        if path in preexisting_paths:
            continue
        if path in recorded_paths:
            continue
        unexpected.append(path)
    return unexpected


def vuln_key(finding: dict[str, Any]) -> str:
    return "::".join(
        [
            finding.get("category", ""),
            finding.get("id", ""),
            finding.get("package", ""),
        ]
    )


def verify_go_module_actions(repo_root: Path, plan: dict[str, Any], results: dict[str, Any]) -> bool:
    ok = True
    vendor_versions = parse_vendor_modules(repo_root)
    for action in plan.get("go_module_actions") or []:
        module_dir = abs_from_repo(repo_root, action["module_root"])
        module = action["module"]
        expected = action["fixed_version"]
        actual = go_list_module_version(module_dir, module)
        item = {
            "module": module,
            "module_root": action["module_root"],
            "expected_version": expected,
            "resolved_version": actual,
            "go_list_ok": actual == expected,
        }
        if has_vendor_tree(repo_root) and action["module_root"] == ".":
            item["vendor_version"] = vendor_versions.get(module, "")
            item["vendor_ok"] = item["vendor_version"] == expected
        results.setdefault("go_module_checks", []).append(item)
        if not item["go_list_ok"] or ("vendor_ok" in item and not item["vendor_ok"]):
            ok = False
    return ok


def verify_dockerfile_actions(repo_root: Path, apply_state: dict[str, Any], results: dict[str, Any]) -> bool:
    ok = True
    for applied in apply_state.get("base_image_updates") or []:
        dockerfile = applied["dockerfile"]
        path = abs_from_repo(repo_root, dockerfile)
        text = path.read_text(encoding="utf-8")
        target = applied["target_image"]
        passed = target in text
        results.setdefault("dockerfile_checks", []).append(
            {
                "dockerfile": dockerfile,
                "stage": applied["stage"],
                "target_image": target,
                "passed": passed,
            }
        )
        if not passed:
            ok = False
    return ok


def verify_module_consistency(repo_root: Path, results: dict[str, Any]) -> bool:
    ok = True
    for module in discover_go_modules(repo_root):
        module_dir = abs_from_repo(repo_root, module)
        proc = run_result(["go", "mod", "verify"], cwd=module_dir)
        module_result = {
            "module": module,
            "passed": proc.returncode == 0,
            "stdout": (proc.stdout or "").strip(),
            "stderr": (proc.stderr or "").strip(),
        }
        results.setdefault("module_verify", []).append(module_result)
        if proc.returncode != 0:
            ok = False
    return ok


def run_rescan(
    repo_root: Path,
    *,
    image: str,
    module_root: str,
    dockerfile: str,
    planned_keys: set[str],
    results: dict[str, Any],
) -> bool:
    ensure_command("trivy")
    detect_trivy_db_staleness()
    output = run(
        ["trivy", "image", "--detection-priority", "comprehensive", "--format", "json", image],
        cwd=repo_root,
        capture=True,
    )
    payload = json.loads(output)
    findings = parse_scan_findings(payload, module_root=module_root, dockerfile=dockerfile)
    remaining = sorted(planned_keys.intersection({vuln_key(finding) for finding in findings}))
    results["rescan"] = {
        "image": image,
        "remaining_vulnerability_keys": remaining,
        "resolved": len(remaining) == 0,
    }
    return len(remaining) == 0


def command_scan(args: argparse.Namespace) -> int:
    ensure_command("trivy")
    repo_root = ensure_repo_root(args.repo)
    module_root_path = (repo_root / args.module_root).resolve()
    dockerfile_path = (repo_root / args.dockerfile).resolve()
    module_root = relative_to_repo(repo_root, module_root_path)
    dockerfile = relative_to_repo(repo_root, dockerfile_path)
    detect_trivy_db_staleness()
    output = run(
        ["trivy", "image", "--detection-priority", "comprehensive", "--format", "json", args.image],
        cwd=repo_root,
        capture=True,
    )
    payload = json.loads(output)
    findings = parse_scan_findings(payload, module_root=module_root, dockerfile=dockerfile)
    state = load_state(repo_root) if git_path(repo_root).exists() else {"version": STATE_VERSION}
    state["version"] = STATE_VERSION
    state["scan"] = {
        "image": args.image,
        "module_root": module_root,
        "dockerfile": dockerfile,
        "collected_at": int(time.time()),
        "metadata": collect_scan_metadata(payload),
        "findings": findings,
    }
    state.pop("plan", None)
    state.pop("apply", None)
    state.pop("verify", None)
    save_state(repo_root, state)
    summarize_scan(findings)
    return 0


def command_plan(args: argparse.Namespace) -> int:
    repo_root = ensure_repo_root(args.repo)
    state = load_state(repo_root)
    scan = state.get("scan")
    if not scan:
        raise CommandError("State file has no scan results. Run scan first.")
    plan = build_plan(scan)
    plan["generated_at"] = int(time.time())
    state["plan"] = plan
    state.pop("apply", None)
    state.pop("verify", None)
    save_state(repo_root, state)
    summarize_plan(plan)
    return 0


def protect_apply_targets(
    preexisting_paths: set[str],
    *,
    module_roots: set[str],
    dockerfiles: set[str],
    vendor_enabled: bool,
) -> None:
    blocked: list[str] = []
    for path in preexisting_paths:
        if path in dockerfiles:
            blocked.append(path)
            continue
        for module_root in module_roots:
            if path in {f"{module_root}/go.mod", f"{module_root}/go.sum"} or (
                module_root == "." and path in {"go.mod", "go.sum"}
            ):
                blocked.append(path)
                break
        if vendor_enabled and (path_is_under(path, "vendor") or path_is_under(path, "LICENSES")):
            blocked.append(path)
    if blocked:
        joined = ", ".join(sorted(set(blocked)))
        raise CommandError(
            "Refusing to apply while target dependency files are already dirty: "
            f"{joined}. Clean or stash those paths first."
        )


def command_apply(args: argparse.Namespace) -> int:
    repo_root = ensure_repo_root(args.repo)
    ensure_command("go")
    state = load_state(repo_root)
    plan = state.get("plan")
    scan = state.get("scan")
    if not plan or not scan:
        raise CommandError("State file must contain scan and plan results. Run scan and plan first.")

    go_actions = plan.get("go_module_actions") or []
    base_actions = plan.get("base_image_actions") or []
    targets = parse_base_image_targets(args.base_image_target or [])
    preexisting_paths = git_status_paths(repo_root)
    module_roots = {action["module_root"] for action in go_actions}
    dockerfiles = {action["dockerfile"] for action in base_actions}
    vendor_enabled = bool(go_actions) and has_vendor_tree(repo_root)
    protect_apply_targets(
        preexisting_paths,
        module_roots=module_roots,
        dockerfiles=dockerfiles,
        vendor_enabled=vendor_enabled,
    )

    go_commands: list[dict[str, Any]] = []
    for action in go_actions:
        module_dir = abs_from_repo(repo_root, action["module_root"])
        cmd = ["go", "get", f"{action['module']}@{action['fixed_version']}"]
        if args.dry_run:
            print_cmd(cmd, cwd=module_dir)
        else:
            info(f"Running {quote_cmd(cmd)} in {action['module_root']}")
            run(cmd, cwd=module_dir)
        go_commands.append({"cwd": action["module_root"], "cmd": cmd})

    ran_module_consistency = False
    if go_actions:
        for module in discover_go_modules(repo_root):
            module_dir = abs_from_repo(repo_root, module)
            if args.dry_run:
                print_cmd(["go", "mod", "tidy"], cwd=module_dir)
                print_cmd(["go", "mod", "verify"], cwd=module_dir)
            else:
                run(["go", "mod", "tidy"], cwd=module_dir)
                run(["go", "mod", "verify"], cwd=module_dir)
            go_commands.append({"cwd": module, "cmd": ["go", "mod", "tidy"]})
            go_commands.append({"cwd": module, "cmd": ["go", "mod", "verify"]})
        ran_module_consistency = True
        if vendor_enabled:
            if args.dry_run:
                print_cmd(["go", "mod", "vendor"], cwd=repo_root)
            else:
                run(["go", "mod", "vendor"], cwd=repo_root)
            go_commands.append({"cwd": ".", "cmd": ["go", "mod", "vendor"]})
            license_script = repo_root / "hack" / "update-azure-vendor-licenses.sh"
            if license_script.is_file():
                if args.dry_run:
                    print_cmd(["bash", str(license_script)], cwd=repo_root)
                else:
                    try:
                        run(["bash", str(license_script)], cwd=repo_root)
                    except CommandError:
                        cleanup_vendor_license_artifacts(repo_root)
                        raise
                    cleanup_vendor_license_artifacts(repo_root)
                go_commands.append({"cwd": ".", "cmd": ["bash", "hack/update-azure-vendor-licenses.sh"]})

    base_image_updates: list[dict[str, Any]] = []
    allow_default_base_target = len(base_actions) <= 1
    for action in base_actions:
        selected = select_base_image_target(
            action,
            targets,
            allow_default=allow_default_base_target,
        )
        if selected is None:
            continue
        if args.dry_run:
            print(f"+ update {action['dockerfile']} [{action['stage']}] FROM -> {selected}")
        else:
            replace_dockerfile_from(repo_root, action["dockerfile"], action["stage"], selected)
        base_image_updates.append(
            {
                "dockerfile": action["dockerfile"],
                "stage": action["stage"],
                "target_image": selected,
            }
        )

    if args.dry_run:
        print("Apply Summary")
        print("=============")
        print("Dry run only. No files changed.")
        if base_actions and not base_image_updates:
            print("Base image actions were planned but no --base-image-target was provided.")
        return 0

    post_paths = git_status_paths(repo_root)
    modified_files = sorted(post_paths - preexisting_paths)
    state["apply"] = {
        "applied_at": int(time.time()),
        "preexisting_changed_files": sorted(preexisting_paths),
        "modified_files": modified_files,
        "ran_module_consistency": ran_module_consistency,
        "go_commands": go_commands,
        "base_image_updates": base_image_updates,
        "temp_paths": [],
    }
    state.pop("verify", None)
    save_state(repo_root, state)

    print("Apply Summary")
    print("=============")
    print(f"Modified files recorded: {len(modified_files)}")
    if base_actions and not base_image_updates:
        print("Base image actions were planned but no --base-image-target was provided.")
    return 0


def command_verify(args: argparse.Namespace) -> int:
    repo_root = ensure_repo_root(args.repo)
    state = load_state(repo_root)
    plan = state.get("plan")
    scan = state.get("scan")
    apply_state = state.get("apply")
    if not plan or not scan or not apply_state:
        raise CommandError("State file must contain scan, plan, and apply results. Run scan, plan, and apply first.")

    results: dict[str, Any] = {"checked_at": int(time.time())}
    ok = True
    if not verify_go_module_actions(repo_root, plan, results):
        ok = False
    if not verify_dockerfile_actions(repo_root, apply_state, results):
        ok = False
    if apply_state.get("ran_module_consistency") and not verify_module_consistency(repo_root, results):
        ok = False

    current_paths = git_status_paths(repo_root)
    preexisting = normalize_path_set(apply_state.get("preexisting_changed_files") or [])
    modified = normalize_path_set(apply_state.get("modified_files") or [])
    unexpected = find_unexpected_changes(current_paths, preexisting_paths=preexisting, recorded_paths=modified)
    results["scoped_diff"] = {
        "preexisting_changed_files": sorted(preexisting),
        "recorded_modified_files": sorted(modified),
        "unexpected_changed_files": unexpected,
        "passed": len(unexpected) == 0,
    }
    if unexpected:
        ok = False

    if args.rescan:
        if not args.image:
            raise CommandError("--image is required when using --rescan")
        planned_keys = set(plan.get("planned_vulnerability_keys") or [])
        if not run_rescan(
            repo_root,
            image=args.image,
            module_root=scan["module_root"],
            dockerfile=scan["dockerfile"],
            planned_keys=planned_keys,
            results=results,
        ):
            ok = False

    state["verify"] = results
    save_state(repo_root, state)

    print("Verify Summary")
    print("==============")
    for item in results.get("go_module_checks", []):
        vendor_note = ""
        if "vendor_ok" in item:
            vendor_note = f", vendor_ok={item['vendor_ok']}"
        print(
            f"{item['module']} expected={item['expected_version']} "
            f"resolved={item['resolved_version']} go_list_ok={item['go_list_ok']}{vendor_note}"
        )
    if results.get("dockerfile_checks"):
        for item in results["dockerfile_checks"]:
            print(
                f"{item['dockerfile']} [{item['stage']}] target={item['target_image']} passed={item['passed']}"
            )
    print(f"Scoped diff passed: {results['scoped_diff']['passed']}")
    if "rescan" in results:
        print(f"Rescan resolved: {results['rescan']['resolved']}")
    return 0 if ok else 1


def command_clean(args: argparse.Namespace) -> int:
    repo_root = ensure_repo_root(args.repo)
    removed_paths: list[str] = []
    if git_path(repo_root).exists():
        state = load_state(repo_root)
        for raw_path in state.get("apply", {}).get("temp_paths", []):
            path = Path(raw_path)
            if not path.is_absolute():
                path = repo_root / raw_path
            path = path.resolve()
            safe_prefixes = (repo_root.resolve(), Path("/tmp"))
            if not any(str(path).startswith(str(prefix)) for prefix in safe_prefixes):
                warn(f"Skipping cleanup of unsafe path outside repo/tmp: {path}")
                continue
            if path.is_dir():
                shutil.rmtree(path, ignore_errors=True)
                removed_paths.append(str(path))
            elif path.exists():
                path.unlink()
                removed_paths.append(str(path))
    cleanup_vendor_license_artifacts(repo_root)
    if remove_state(repo_root):
        removed_paths.append(str(git_path(repo_root)))
    print("Clean Summary")
    print("=============")
    if removed_paths:
        for path in removed_paths:
            print(path)
    else:
        print("Nothing to remove.")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Scan images with Trivy, plan dependency/base-image fixes, apply them, and verify the result."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    scan = subparsers.add_parser("scan", help="Scan an image with Trivy and persist fixable findings")
    scan.add_argument("image", help="Container image reference to scan")
    scan.add_argument("--repo", default=".", help="Path to the git repo (default: .)")
    scan.add_argument("--module-root", default=".", help="Module root for GO_MODULE findings (default: repo root)")
    scan.add_argument("--dockerfile", default="Dockerfile", help="Dockerfile to associate with BASE_IMAGE findings")
    scan.set_defaults(func=command_scan)

    plan = subparsers.add_parser("plan", help="Build an actionable plan from the last scan results")
    plan.add_argument("--repo", default=".", help="Path to the git repo (default: .)")
    plan.set_defaults(func=command_plan)

    apply = subparsers.add_parser("apply", help="Apply the planned dependency and Dockerfile updates")
    apply.add_argument("--repo", default=".", help="Path to the git repo (default: .)")
    apply.add_argument(
        "--base-image-target",
        action="append",
        default=[],
        metavar="[dockerfile:stage=]IMAGE@sha256:...",
        help="Deterministic base image target. Keys are matched by dockerfile:stage, stage, dockerfile, or default.",
    )
    apply.add_argument("--dry-run", action="store_true", help="Print the planned apply commands without changing files")
    apply.set_defaults(func=command_apply)

    verify = subparsers.add_parser("verify", help="Verify planned fixes at file level and optionally rescan the rebuilt image")
    verify.add_argument("--repo", default=".", help="Path to the git repo (default: .)")
    verify.add_argument("--rescan", action="store_true", help="Rescan the rebuilt image with Trivy")
    verify.add_argument("--image", help="Rebuilt container image reference to rescan when using --rescan")
    verify.set_defaults(func=command_verify)

    clean = subparsers.add_parser("clean", help="Remove state and temporary artifacts")
    clean.add_argument("--repo", default=".", help="Path to the git repo (default: .)")
    clean.set_defaults(func=command_clean)

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        return args.func(args)
    except CommandError as exc:
        err(str(exc))
        return 2


if __name__ == "__main__":
    sys.exit(main())
