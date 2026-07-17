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
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path


ENV_KEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
REQUIRED_ENV = {"IMAGE_TAG", "IMAGE_REGISTRY"}
MANAGED_ENV = {"IMAGE_TAG", "IMAGE_REGISTRY", "ENABLE_GIT_COMMAND", "GOEXPERIMENT"}
MAKE_CONTROL_ENV = {
    "GNUMAKEFLAGS",
    "MAKEFILES",
    "MAKEFLAGS",
    "MAKEOVERRIDES",
    "MFLAGS",
}
GOEXPERIMENT_DEFAULT = "nosystemcrypto"
ENABLE_GIT_COMMAND_DEFAULT = "false"


@dataclass(frozen=True)
class ImageTarget:
    make_target: str
    subdir: str = "."
    default_goexperiment: bool = False
    force_rebuild: bool = False


@dataclass(frozen=True)
class BuildPlan:
    cwd: Path
    cmd: list[str]
    env: dict[str, str]
    unset_env: frozenset[str] = frozenset()


IMAGE_TARGETS = {
    "all": ImageTarget("image"),
    "ccm": ImageTarget("build-ccm-image", default_goexperiment=True),
    "ccm-all": ImageTarget("build-all-ccm-images", default_goexperiment=True),
    "ccm-e2e": ImageTarget("build-ccm-e2e-test-image"),
    "cnm": ImageTarget("build-node-image-linux", default_goexperiment=True),
    "cnm-all": ImageTarget("build-all-node-images"),
    "cnm-linux": ImageTarget("build-node-image-linux", default_goexperiment=True),
    "cnm-windows": ImageTarget("build-node-image-windows"),
    "cnm-windows-hpc": ImageTarget("build-node-image-windows-hpc"),
    "hpp": ImageTarget(
        "build-health-probe-proxy-image",
        "health-probe-proxy",
        force_rebuild=True,
    ),
    "hpp-windows": ImageTarget(
        "build-health-probe-proxy-image-windows",
        "health-probe-proxy",
        force_rebuild=True,
    ),
}


def validate_env_key(key: str) -> None:
    if not ENV_KEY_RE.match(key):
        raise ValueError(f"invalid flag name {key!r}")


def parse_set_value(value: str) -> tuple[str, str]:
    if "=" not in value:
        raise ValueError(f"--set expects KEY=VALUE, got {value!r}")
    key, env_value = value.split("=", 1)
    validate_env_key(key)
    if key == "IMAGE_TAG":
        raise ValueError("IMAGE_TAG must be provided with --tag, not --set")
    if key == "IMAGE_REGISTRY":
        raise ValueError("IMAGE_REGISTRY must be provided with --registry, not --set")
    if key in MAKE_CONTROL_ENV:
        raise ValueError(f"{key} is reserved and cannot be provided with --set")
    return key, env_value


def script_repo_root() -> Path:
    return Path(__file__).resolve().parents[4]


def build_plan(
    *,
    image: str,
    tag: str,
    registry: str,
    repo_root: Path,
    set_values: list[str] | None = None,
    unset_values: list[str] | None = None,
    inherited_env: Mapping[str, str] | None = None,
) -> BuildPlan:
    image_key = image.lower()
    if image_key not in IMAGE_TARGETS:
        aliases = ", ".join(sorted(IMAGE_TARGETS))
        raise ValueError(f"unknown image alias {image!r}; expected one of: {aliases}")
    if not tag:
        raise ValueError("--tag must not be empty")
    if not registry:
        raise ValueError("--registry must not be empty")
    target = IMAGE_TARGETS[image_key]

    unset_env = set[str]()
    for key in unset_values or []:
        validate_env_key(key)
        if key in REQUIRED_ENV:
            raise ValueError(f"{key} is required and cannot be unset")
        unset_env.add(key)

    set_entries = [parse_set_value(value) for value in set_values or []]
    set_keys = {key for key, _ in set_entries}
    conflicts = set_keys.intersection(unset_env)
    if conflicts:
        keys = ", ".join(sorted(conflicts))
        raise ValueError(f"{keys} cannot be passed with both --set and --unset")

    env = {
        "IMAGE_TAG": tag,
        "IMAGE_REGISTRY": registry,
    }
    if target.default_goexperiment:
        env["GOEXPERIMENT"] = GOEXPERIMENT_DEFAULT
    env["ENABLE_GIT_COMMAND"] = ENABLE_GIT_COMMAND_DEFAULT

    for key in unset_env:
        env.pop(key, None)

    for key, env_value in set_entries:
        env[key] = env_value

    for key in REQUIRED_ENV:
        if not env.get(key):
            raise ValueError(f"{key} is required and cannot be empty")

    unset_managed_env = {
        key
        for key in MANAGED_ENV
        if key not in env and key not in set_keys and key not in REQUIRED_ENV
    }
    unset_env.update(unset_managed_env)
    if inherited_env is not None:
        unset_env.update(key for key in MAKE_CONTROL_ENV if key in inherited_env)
    cmd = ["make"]
    if target.force_rebuild:
        cmd.append("-B")
    cmd.append(target.make_target)
    return BuildPlan(
        cwd=repo_root / target.subdir,
        cmd=cmd,
        env=env,
        unset_env=frozenset(unset_env),
    )


def quote_env_assignment(key: str, value: str) -> str:
    return f"{key}={shlex.quote(value)}"


def format_command(plan: BuildPlan) -> str:
    unset_parts: list[str] = []
    if plan.unset_env:
        unset_parts.append("env")
        for key in sorted(plan.unset_env):
            unset_parts.extend(["-u", shlex.quote(key)])
    env_parts = [quote_env_assignment(key, value) for key, value in plan.env.items()]
    cmd_parts = [shlex.quote(part) for part in plan.cmd]
    return " ".join([*unset_parts, *env_parts, *cmd_parts])


def format_dry_run(plan: BuildPlan) -> str:
    return f"cwd: {plan.cwd}\ncommand: {format_command(plan)}"


def find_repo_root(start: Path) -> Path:
    _ = start
    return script_repo_root()


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build cloud-provider-azure images with explicit tag and registry inputs.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--image", required=True, choices=sorted(IMAGE_TARGETS))
    parser.add_argument("--tag", required=True, help="IMAGE_TAG value to use")
    parser.add_argument("--registry", required=True, help="IMAGE_REGISTRY value to use")
    parser.add_argument(
        "--repo",
        help="Target cloud-provider-azure checkout (default: checkout containing this skill)",
    )
    parser.add_argument(
        "--set",
        dest="set_values",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="add or override an environment/make variable",
    )
    parser.add_argument(
        "--unset",
        dest="unset_values",
        action="append",
        default=[],
        metavar="KEY",
        help="remove a default environment/make variable",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the resolved working directory and command without building",
    )
    return parser.parse_args(argv)


def run_plan(plan: BuildPlan) -> int:
    env = os.environ.copy()
    for key in MAKE_CONTROL_ENV:
        env.pop(key, None)
    for key in plan.unset_env:
        env.pop(key, None)
    env.update(plan.env)
    print(format_dry_run(plan))
    return subprocess.run(plan.cmd, cwd=plan.cwd, env=env, check=False).returncode


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        if args.repo is None:
            repo_root = find_repo_root(Path.cwd())
        else:
            repo_root = Path(args.repo).resolve()
            if not repo_root.is_dir():
                raise ValueError(f"--repo is not a directory: {repo_root}")
        plan = build_plan(
            image=args.image,
            tag=args.tag,
            registry=args.registry,
            repo_root=repo_root,
            set_values=args.set_values,
            unset_values=args.unset_values,
            inherited_env=os.environ,
        )
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    if args.dry_run:
        print(format_dry_run(plan))
        return 0
    return run_plan(plan)


if __name__ == "__main__":
    sys.exit(main())
