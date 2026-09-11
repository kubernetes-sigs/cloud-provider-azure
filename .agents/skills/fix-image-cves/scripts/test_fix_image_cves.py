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

import io
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path
from unittest import mock

import fix_image_cves


def module_info(
    version: str,
    *,
    replacement_path: str = "",
    replacement_version: str = "",
) -> dict[str, str]:
    return {
        "path": "golang.org/x/sys",
        "version": version,
        "replacement_path": replacement_path,
        "replacement_version": replacement_version,
    }


class FixImageCVEsTest(unittest.TestCase):
    def test_read_only_git_disables_optional_locks_without_mutating_parent_env(self) -> None:
        repo_root = Path("/repo")
        with mock.patch.dict(
            os.environ,
            {"GIT_OPTIONAL_LOCKS": "1", "KEEP_ME": "yes"},
            clear=True,
        ):
            with mock.patch.object(
                fix_image_cves,
                "run",
                return_value="ok",
            ) as run:
                self.assertEqual(
                    fix_image_cves.read_only_git(repo_root, ["status", "--short"]),
                    "ok",
                )

            child_env = run.call_args.kwargs["env"]
            self.assertEqual(child_env["GIT_OPTIONAL_LOCKS"], "0")
            self.assertEqual(child_env["KEEP_ME"], "yes")
            self.assertEqual(os.environ["GIT_OPTIONAL_LOCKS"], "1")

        run.assert_called_once_with(
            ["git", "-C", str(repo_root), "status", "--short"],
            cwd=repo_root,
            capture=True,
            env=child_env,
        )

    def test_ensure_repo_root_uses_read_only_git(self) -> None:
        repo = Path("/repo")
        with mock.patch.object(Path, "is_dir", return_value=True):
            with mock.patch.object(
                fix_image_cves,
                "read_only_git",
                return_value=str(repo),
            ) as read_only_git:
                self.assertEqual(fix_image_cves.ensure_repo_root(str(repo)), repo)

        read_only_git.assert_called_once_with(
            repo,
            ["rev-parse", "--show-toplevel"],
        )

    def test_ensure_repo_root_rejects_missing_path_before_running_git(self) -> None:
        missing_repo = Path("/definitely-missing-fix-image-cves-repo")
        with mock.patch.object(
            fix_image_cves,
            "read_only_git",
        ) as read_only_git:
            with self.assertRaisesRegex(
                fix_image_cves.CommandError,
                "is not a git checkout",
            ):
                fix_image_cves.ensure_repo_root(str(missing_repo))

        read_only_git.assert_not_called()

    def test_git_path_resolves_main_checkout_state(self) -> None:
        repo_root = Path("/repo")
        with mock.patch.object(
            fix_image_cves,
            "read_only_git",
            return_value=".git/fix-image-cves.json",
        ) as read_only_git:
            self.assertEqual(
                fix_image_cves.git_path(repo_root),
                Path("/repo/.git/fix-image-cves.json"),
            )

        read_only_git.assert_called_once_with(
            repo_root,
            ["rev-parse", "--git-path", "fix-image-cves.json"],
        )

    def test_git_path_preserves_linked_worktree_state(self) -> None:
        worktree_path = Path(
            "/repo/.git/worktrees/cloud-provider-azure1/fix-image-cves.json"
        )
        with mock.patch.object(
            fix_image_cves,
            "read_only_git",
            return_value=str(worktree_path),
        ):
            self.assertEqual(
                fix_image_cves.git_path(Path("/worktree")),
                worktree_path,
            )

    def test_git_path_rejects_empty_output(self) -> None:
        with mock.patch.object(fix_image_cves, "read_only_git", return_value=""):
            with self.assertRaisesRegex(
                fix_image_cves.CommandError,
                "empty CVE state path",
            ):
                fix_image_cves.git_path(Path("/repo"))

    def test_status_and_module_discovery_use_read_only_git(self) -> None:
        repo_root = Path("/repo")
        with mock.patch.object(
            fix_image_cves,
            "read_only_git",
            side_effect=[" M tracked.txt\n?? untracked.txt", "go.mod\nnested/go.mod"],
        ) as read_only_git:
            self.assertEqual(
                fix_image_cves.git_status_paths(repo_root),
                {"tracked.txt", "untracked.txt"},
            )
            self.assertEqual(
                fix_image_cves.discover_go_modules(repo_root),
                [".", "nested"],
            )

        self.assertEqual(
            read_only_git.call_args_list,
            [
                mock.call(repo_root, ["status", "--porcelain"]),
                mock.call(repo_root, ["ls-files", "go.mod", "**/go.mod"]),
            ],
        )

    def test_classify_result_treats_go_runtime_packages_as_toolchain(self) -> None:
        result = {"Class": "lang-pkgs", "Type": "gobinary"}

        for package in ("stdlib", "toolchain"):
            with self.subTest(package=package):
                self.assertEqual(
                    fix_image_cves.classify_result(result, package),
                    "GO_TOOLCHAIN",
                )

        self.assertEqual(
            fix_image_cves.classify_result(result, "golang.org/x/net"),
            "GO_MODULE",
        )

    def test_mixed_go_scan_keeps_toolchain_findings_report_only(self) -> None:
        findings = fix_image_cves.parse_scan_findings(
            {
                "Results": [
                    {
                        "Class": "lang-pkgs",
                        "Type": "gobinary",
                        "Target": "cloud-controller-manager",
                        "Vulnerabilities": [
                            {
                                "PkgName": "stdlib",
                                "InstalledVersion": "v1.24.5",
                                "FixedVersion": "1.23.12, 1.24.6",
                                "VulnerabilityID": "CVE-2026-0001",
                            },
                            {
                                "PkgName": "golang.org/x/net",
                                "InstalledVersion": "v0.49.0",
                                "FixedVersion": "0.55.0",
                                "VulnerabilityID": "CVE-2026-0002",
                            },
                        ],
                    }
                ]
            },
            module_root=".",
            dockerfile="Dockerfile",
        )

        plan = fix_image_cves.build_plan({"findings": findings})

        self.assertEqual(
            [action["module"] for action in plan["go_module_actions"]],
            ["golang.org/x/net"],
        )
        self.assertEqual(
            [(finding["category"], finding["package"]) for finding in plan["other_findings"]],
            [("GO_TOOLCHAIN", "stdlib")],
        )
        self.assertEqual(
            plan["planned_vulnerability_keys"],
            ["GO_MODULE::CVE-2026-0002::golang.org/x/net"],
        )

    def test_plan_preserves_unfixable_and_unsupported_findings(self) -> None:
        findings = fix_image_cves.parse_scan_findings(
            {
                "Results": [
                    {
                        "Class": "os-pkgs",
                        "Type": "debian",
                        "Target": "runtime",
                        "Vulnerabilities": [
                            {
                                "PkgName": "libc6",
                                "InstalledVersion": "2.36-9",
                                "FixedVersion": "",
                                "VulnerabilityID": "CVE-2026-0004",
                            }
                        ],
                    },
                    {
                        "Class": "custom",
                        "Type": "application",
                        "Target": "config",
                        "Vulnerabilities": [
                            {
                                "PkgName": "example-package",
                                "InstalledVersion": "v1.0.0",
                                "FixedVersion": "v1.0.1",
                                "VulnerabilityID": "CVE-2026-0005",
                            }
                        ],
                    },
                ]
            },
            module_root=".",
            dockerfile="Dockerfile",
        )

        plan = fix_image_cves.build_plan({"findings": findings})

        self.assertEqual(
            [finding["id"] for finding in plan["unfixable_findings"]],
            ["CVE-2026-0004"],
        )
        self.assertEqual(
            [finding["id"] for finding in plan["other_findings"]],
            ["CVE-2026-0005"],
        )
        self.assertEqual(
            [
                finding["id"]
                for finding in fix_image_cves.unsupported_fixable_findings(plan)
            ],
            ["CVE-2026-0005"],
        )

    def test_summaries_report_findings_without_fixed_versions(self) -> None:
        finding = {
            "category": "BASE_IMAGE",
            "package": "libc6",
            "installed_version": "2.36-9",
            "fixed_version": "",
            "id": "CVE-2026-0004",
        }
        scan_output = io.StringIO()
        plan_output = io.StringIO()

        with mock.patch("sys.stdout", scan_output):
            fix_image_cves.summarize_scan([finding])
        with mock.patch("sys.stdout", plan_output):
            fix_image_cves.summarize_plan(
                {
                    "go_module_actions": [],
                    "base_image_actions": [],
                    "other_findings": [],
                    "unfixable_findings": [finding],
                }
            )

        self.assertIn("NO_FIXED_VERSION: 1", scan_output.getvalue())
        self.assertIn("<no fixed version>", scan_output.getvalue())
        self.assertIn(
            "Findings Without a Fixed Version (Residual Risk)",
            plan_output.getvalue(),
        )

    def test_build_plan_reclassifies_toolchain_from_older_scan_state(self) -> None:
        finding = {
            "category": "GO_MODULE",
            "package": "toolchain",
            "module_root": ".",
            "target": "cloud-controller-manager",
            "installed_version": "v1.24.5",
            "fixed_version": "1.24.6",
            "id": "CVE-2026-0003",
        }

        plan = fix_image_cves.build_plan({"findings": [finding]})

        self.assertEqual(plan["go_module_actions"], [])
        self.assertEqual(plan["planned_vulnerability_keys"], [])
        self.assertEqual(plan["other_findings"][0]["category"], "GO_TOOLCHAIN")

    def test_toolchain_findings_do_not_require_a_newer_module_fix(self) -> None:
        for category in ("GO_MODULE", "GO_TOOLCHAIN"):
            for package in ("stdlib", "toolchain"):
                with self.subTest(category=category, package=package):
                    finding = {
                        "category": category,
                        "package": package,
                        "module_root": ".",
                        "target": "cloud-controller-manager",
                        "installed_version": "v1.24.6",
                        "fixed_version": "1.23.12, 1.24.6",
                        "id": "CVE-2026-0003",
                    }
                    with mock.patch.object(fix_image_cves, "lowest_fixed_version") as select:
                        plan = fix_image_cves.build_plan({"findings": [finding]})

                    select.assert_not_called()
                    self.assertEqual(plan["go_module_actions"], [])
                    self.assertEqual(plan["planned_vulnerability_keys"], [])
                    self.assertEqual(plan["other_findings"][0]["category"], "GO_TOOLCHAIN")

    def test_apply_ignores_toolchain_action_from_older_plan_state(self) -> None:
        state = {
            "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
            "plan": {
                "go_module_actions": [
                    {
                        "module": "stdlib",
                        "module_root": ".",
                        "fixed_version": "v1.24.6",
                    }
                ],
                "base_image_actions": [],
            },
        }
        args = Namespace(repo=".", base_image_target=[], dry_run=True)

        with mock.patch.object(
            fix_image_cves, "ensure_repo_root", return_value=Path("/repo")
        ), mock.patch.object(
            fix_image_cves, "load_state", return_value=state
        ), mock.patch.object(
            fix_image_cves, "git_status_paths", return_value=set()
        ), mock.patch.object(
            fix_image_cves, "go_list_module_info"
        ) as go_list, mock.patch.object(
            fix_image_cves, "ensure_command"
        ) as ensure_command, mock.patch.object(
            fix_image_cves, "discover_go_modules"
        ) as discover_modules, mock.patch("sys.stdout", io.StringIO()):
            self.assertEqual(fix_image_cves.command_apply(args), 0)

        go_list.assert_not_called()
        ensure_command.assert_not_called()
        discover_modules.assert_not_called()

    def test_apply_builder_targets_are_recorded_and_verified(self) -> None:
        builder = "mcr.microsoft.com/oss/go/microsoft/golang:1.26.0-bookworm@sha256:" + "a" * 64
        runtime = "gcr.io/distroless/base@sha256:" + "b" * 64
        original = "FROM --platform=linux/amd64 golang:1.25.11 AS builder\nFROM distroless:old\n"
        dockerfiles = ["Dockerfile", "cloud-node-manager.Dockerfile"]
        for dry_run in (True, False):
            with self.subTest(dry_run=dry_run), tempfile.TemporaryDirectory() as temp_dir:
                repo = Path(temp_dir)
                for name in dockerfiles:
                    (repo / name).write_text(original, encoding="utf-8")
                state = {
                    "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
                    "plan": {
                        "go_directive_actions": [{"module_root": ".", "target_version": "1.26.0"}],
                        "base_image_actions": [{"dockerfile": "Dockerfile", "stage": "runtime"}],
                    },
                }
                args = fix_image_cves.build_parser().parse_args(
                    ["apply", "--repo", str(repo), "--base-image-target", runtime]
                    + [arg for name in dockerfiles for arg in
                       ("--base-image-target", f"{name}:builder={builder}")]
                    + (["--dry-run"] if dry_run else [])
                )
                with mock.patch.object(fix_image_cves, "ensure_repo_root", return_value=repo), \
                     mock.patch.object(fix_image_cves, "load_state", return_value=state), \
                     mock.patch.object(fix_image_cves, "save_state") as save_state, \
                     mock.patch.object(fix_image_cves, "git_status_paths",
                                       side_effect=[set(), set(dockerfiles) | {"go.mod"}]), \
                     mock.patch.object(fix_image_cves, "run") as run, \
                     mock.patch("sys.stdout", io.StringIO()):
                    self.assertEqual(fix_image_cves.command_apply(args), 0)
                self.assertEqual(len(state["plan"]["base_image_actions"]), 1)
                if dry_run:
                    save_state.assert_not_called()
                    run.assert_not_called()
                    for name in dockerfiles:
                        self.assertEqual((repo / name).read_text(), original)
                else:
                    applied = save_state.call_args.args[1]["apply"]
                    self.assertEqual(set(applied["modified_files"]), set(dockerfiles) | {"go.mod"})
                    results = {}
                    self.assertTrue(fix_image_cves.verify_dockerfile_actions(repo, applied, results))
                    self.assertEqual(len(results["dockerfile_checks"]), 3)
                    for name in dockerfiles:
                        lines = (repo / name).read_text().splitlines()
                        self.assertEqual(lines[0], f"FROM --platform=linux/amd64 {builder} AS builder")
                        self.assertEqual(lines[-1], f"FROM {runtime}" if name == "Dockerfile" else "FROM distroless:old")

    def test_apply_rejects_invalid_builder_targets_before_mutation(self) -> None:
        pinned = "golang:1.26.0@sha256:" + "a" * 64
        original = "FROM golang:1.25.11 AS builder\nFROM distroless:old\n"
        directive = {"module_root": ".", "target_version": "1.26.0"}
        for directives, text, target, dirty, error in [
            ([], original, pinned, set(), "planned Go directive"),
            ([directive], "FROM distroless:old\n", pinned, set(), "named builder stage"),
            ([directive], original, "golang:1.26.0", set(), "sha256 digest"),
            ([directive], original, pinned, {"Dockerfile"}, "already dirty"),
        ]:
            with self.subTest(error=error), tempfile.TemporaryDirectory() as temp_dir:
                repo = Path(temp_dir)
                (repo / "Dockerfile").write_text(text, encoding="utf-8")
                state = {"scan": {"module_root": "."}, "plan": {"go_directive_actions": directives}}
                args = Namespace(repo=str(repo), base_image_target=[f"Dockerfile:builder={target}"], dry_run=False)
                with mock.patch.object(fix_image_cves, "ensure_repo_root", return_value=repo), \
                     mock.patch.object(fix_image_cves, "load_state", return_value=state), \
                     mock.patch.object(fix_image_cves, "git_status_paths", return_value=dirty), \
                     mock.patch.object(fix_image_cves, "run") as run, \
                     mock.patch.object(fix_image_cves, "save_state") as save_state:
                    with self.assertRaisesRegex(fix_image_cves.CommandError, error):
                        fix_image_cves.command_apply(args)
                run.assert_not_called()
                save_state.assert_not_called()
                self.assertEqual((repo / "Dockerfile").read_text(), text)

    def test_apply_retidies_root_after_vendor_license_update(self) -> None:
        state = {
            "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
            "plan": {
                "go_module_actions": [
                    {
                        "module": "golang.org/x/net",
                        "module_root": ".",
                        "fixed_version": "v0.56.0",
                    }
                ],
                "go_directive_actions": [],
                "base_image_actions": [],
            },
        }
        args = Namespace(repo=".", base_image_target=[], dry_run=False)

        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            (repo_root / "vendor").mkdir()
            (repo_root / "vendor" / "modules.txt").write_text("", encoding="utf-8")
            (repo_root / "hack").mkdir()
            (repo_root / "hack" / "update-azure-vendor-licenses.sh").write_text(
                "#!/bin/bash\n",
                encoding="utf-8",
            )

            with mock.patch.object(
                fix_image_cves, "ensure_repo_root", return_value=repo_root
            ), mock.patch.object(
                fix_image_cves, "load_state", return_value=state
            ), mock.patch.object(
                fix_image_cves, "save_state"
            ) as save_state, mock.patch.object(
                fix_image_cves,
                "git_status_paths",
                side_effect=[set(), {"go.mod", "go.sum"}],
            ), mock.patch.object(
                fix_image_cves,
                "discover_go_modules",
                return_value=[".", "health-probe-proxy"],
            ), mock.patch.object(
                fix_image_cves,
                "go_list_module_info",
                return_value=module_info("v0.55.0"),
            ), mock.patch.object(
                fix_image_cves, "ensure_command"
            ), mock.patch.object(
                fix_image_cves, "_check_local_go_version"
            ), mock.patch.object(
                fix_image_cves, "cleanup_vendor_license_artifacts"
            ), mock.patch.object(
                fix_image_cves, "run", return_value=""
            ) as run, mock.patch("sys.stdout", io.StringIO()):
                self.assertEqual(fix_image_cves.command_apply(args), 0)

        commands = [call.args[0] for call in run.call_args_list]
        license_index = commands.index(
            ["bash", str(repo_root / "hack" / "update-azure-vendor-licenses.sh")]
        )
        root_tidy_indices = [
            index
            for index, command in enumerate(commands)
            if command == ["go", "mod", "tidy"]
            and run.call_args_list[index].kwargs["cwd"] == repo_root
        ]
        root_verify_indices = [
            index
            for index, command in enumerate(commands)
            if command == ["go", "mod", "verify"]
            and run.call_args_list[index].kwargs["cwd"] == repo_root
        ]

        self.assertEqual(len(root_tidy_indices), 2)
        self.assertEqual(len(root_verify_indices), 2)
        self.assertGreater(root_tidy_indices[-1], license_index)
        self.assertGreater(root_verify_indices[-1], root_tidy_indices[-1])
        applied = save_state.call_args.args[1]["apply"]
        self.assertEqual(applied["modified_files"], ["go.mod", "go.sum"])

    def test_apply_rejects_old_local_go_before_source_mutation(self) -> None:
        state = {
            "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
            "plan": {
                "go_module_actions": [
                    {
                        "module": "golang.org/x/crypto",
                        "module_root": ".",
                        "fixed_version": "v0.56.0",
                    }
                ],
                "go_directive_actions": [
                    {
                        "module_root": ".",
                        "current_version": "1.25.0",
                        "target_version": "1.26.0",
                        "required_by": ["golang.org/x/crypto@v0.56.0"],
                    }
                ],
                "base_image_actions": [],
            },
        }
        args = Namespace(repo=".", base_image_target=[], dry_run=False)

        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            (repo_root / "go.mod").write_text(
                "module example.com/root\n\ngo 1.25.0\n",
                encoding="utf-8",
            )
            go_version = subprocess.CompletedProcess(
                ["go", "version"],
                0,
                stdout="go version go1.25.9 darwin/arm64\n",
                stderr="",
            )
            with mock.patch.object(
                fix_image_cves, "ensure_repo_root", return_value=repo_root
            ), mock.patch.object(
                fix_image_cves, "load_state", return_value=state
            ), mock.patch.object(
                fix_image_cves, "git_status_paths", return_value=set()
            ), mock.patch.object(
                fix_image_cves, "discover_go_modules", return_value=["."]
            ), mock.patch.object(
                fix_image_cves, "ensure_command"
            ), mock.patch.object(
                fix_image_cves.subprocess, "run", return_value=go_version
            ), mock.patch.object(
                fix_image_cves, "go_list_module_info"
            ) as go_list, mock.patch.object(
                fix_image_cves, "run"
            ) as run:
                with self.assertRaisesRegex(
                    fix_image_cves.CommandError,
                    r"Local Go version 1\.25\.9.*1\.26\.0",
                ):
                    fix_image_cves.command_apply(args)

        go_list.assert_not_called()
        run.assert_not_called()

    def test_apply_bumps_go_directive_before_dependency_requirements(self) -> None:
        state = {
            "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
            "plan": {
                "go_module_actions": [
                    {
                        "module": "golang.org/x/crypto",
                        "module_root": ".",
                        "fixed_version": "v0.56.0",
                    }
                ],
                "go_directive_actions": [
                    {
                        "module_root": ".",
                        "current_version": "1.25.0",
                        "target_version": "1.26.0",
                        "required_by": ["golang.org/x/crypto@v0.56.0"],
                    }
                ],
                "base_image_actions": [],
            },
        }
        args = Namespace(repo=".", base_image_target=[], dry_run=False)

        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            with mock.patch.object(
                fix_image_cves, "ensure_repo_root", return_value=repo_root
            ), mock.patch.object(
                fix_image_cves, "load_state", return_value=state
            ), mock.patch.object(
                fix_image_cves, "save_state"
            ) as save_state, mock.patch.object(
                fix_image_cves,
                "git_status_paths",
                side_effect=[set(), {"go.mod", "go.sum"}],
            ), mock.patch.object(
                fix_image_cves, "discover_go_modules", return_value=[]
            ), mock.patch.object(
                fix_image_cves,
                "go_list_module_info",
                return_value=module_info("v0.55.0"),
            ), mock.patch.object(
                fix_image_cves, "ensure_command"
            ), mock.patch.object(
                fix_image_cves, "_check_local_go_version"
            ), mock.patch.object(
                fix_image_cves, "run", return_value=""
            ) as run, mock.patch(
                "sys.stdout", io.StringIO()
            ):
                self.assertEqual(fix_image_cves.command_apply(args), 0)

        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(
            commands[:2],
            [
                ["go", "mod", "edit", "-go=1.26.0"],
                ["go", "mod", "edit", "-require=golang.org/x/crypto@v0.56.0"],
            ],
        )
        self.assertEqual(save_state.call_args.args[1]["apply"]["go_commands"][:2], [
            {"cwd": ".", "cmd": commands[0]},
            {"cwd": ".", "cmd": commands[1]},
        ])

    def test_apply_enriches_older_plan_before_dry_run(self) -> None:
        state = {
            "scan": {"module_root": ".", "dockerfile": "Dockerfile"},
            "plan": {
                "go_module_actions": [
                    {
                        "module": "golang.org/x/crypto",
                        "module_root": ".",
                        "fixed_version": "v0.56.0",
                    }
                ],
                "base_image_actions": [],
            },
        }
        directive_action = {
            "module_root": ".",
            "current_version": "1.25.0",
            "target_version": "1.26.0",
            "required_by": ["golang.org/x/crypto@v0.56.0"],
        }
        args = Namespace(repo=".", base_image_target=[], dry_run=True)
        output = io.StringIO()

        with mock.patch.object(
            fix_image_cves, "ensure_repo_root", return_value=Path("/repo")
        ), mock.patch.object(
            fix_image_cves, "load_state", return_value=state
        ), mock.patch.object(
            fix_image_cves,
            "build_go_directive_actions",
            return_value=[directive_action],
        ) as build_directives, mock.patch.object(
            fix_image_cves, "save_state"
        ) as save_state, mock.patch.object(
            fix_image_cves, "git_status_paths", return_value=set()
        ), mock.patch.object(
            fix_image_cves,
            "go_list_module_info",
            return_value=module_info("v0.55.0"),
        ), mock.patch.object(
            fix_image_cves, "discover_go_modules", return_value=[]
        ), mock.patch.object(
            fix_image_cves, "ensure_command"
        ), mock.patch.object(
            fix_image_cves, "_check_local_go_version"
        ), mock.patch(
            "sys.stdout", output
        ):
            self.assertEqual(fix_image_cves.command_apply(args), 0)

        build_directives.assert_called_once()
        self.assertEqual(save_state.call_args_list[0].args[1]["plan"]["go_directive_actions"], [
            directive_action
        ])
        self.assertIn("go mod edit -go=1.26.0", output.getvalue())

    def test_verify_ignores_toolchain_action_from_older_plan_state(self) -> None:
        plan = {
            "go_module_actions": [
                {
                    "module": "toolchain",
                    "module_root": ".",
                    "fixed_version": "v1.24.6",
                }
            ]
        }
        results = {}

        with mock.patch.object(fix_image_cves, "go_list_module_info") as go_list, mock.patch.object(
            fix_image_cves, "parse_vendor_modules", return_value={}
        ):
            self.assertTrue(
                fix_image_cves.verify_go_module_actions(Path("/repo"), plan, results)
            )

        go_list.assert_not_called()

    def test_rescan_ignores_toolchain_key_from_older_plan_state(self) -> None:
        plan = {
            "planned_vulnerability_keys": [
                "GO_MODULE::CVE-2026-0001::stdlib",
                "GO_MODULE::CVE-2026-0002::golang.org/x/net",
                "BASE_IMAGE::CVE-2026-0003::libc6",
            ]
        }

        self.assertEqual(
            fix_image_cves.actionable_vulnerability_keys(plan),
            {
                "GO_MODULE::CVE-2026-0002::golang.org/x/net",
                "BASE_IMAGE::CVE-2026-0003::libc6",
            },
        )

    def test_summarize_plan_explains_manual_go_toolchain_remediation(self) -> None:
        plan = {
            "go_module_actions": [],
            "base_image_actions": [],
            "other_findings": [
                {
                    "category": "GO_TOOLCHAIN",
                    "package": "stdlib",
                    "installed_version": "v1.24.5",
                    "fixed_version": "1.23.12, 1.24.6",
                    "id": "CVE-2026-0001",
                }
            ],
        }

        output = io.StringIO()
        with mock.patch("sys.stdout", output):
            fix_image_cves.summarize_plan(plan)

        summary = output.getvalue()
        self.assertIn("Go Toolchain Findings (Manual Action Required)", summary)
        self.assertIn("Upgrade the Go build toolchain or pinned builder image", summary)
        self.assertIn("then rebuild the image", summary)

    def test_highest_fixed_version_normalizes_and_uses_go_semver_order(self) -> None:
        cases = [
            (["0.55.0"], "v0.55.0"),
            (["v0.55.0"], "v0.55.0"),
            (["0.54.0, 0.55.0"], "v0.55.0"),
            (["v1.2.3-rc.1", "v1.2.3"], "v1.2.3"),
            (
                [
                    "v0.0.0-20240101000000-aaaaaaaaaaaa",
                    "v0.0.0-20250101000000-bbbbbbbbbbbb",
                ],
                "v0.0.0-20250101000000-bbbbbbbbbbbb",
            ),
        ]

        for versions, expected in cases:
            with self.subTest(versions=versions):
                self.assertEqual(fix_image_cves.highest_fixed_version(versions), expected)

    def test_lowest_fixed_version_normalizes_and_uses_go_semver_order(self) -> None:
        cases = [
            ("0.55.0", "v0.49.0", "v0.55.0"),
            ("v1.10.0, 1.9.9", "1.9.0", "v1.9.9"),
            (" , v0.55.0, 0.54.0,, 0.54.0, ", "v0.49.0", "v0.54.0"),
            ("v1.2.3-rc.2, v1.2.3, v1.2.3-rc.1", "v1.2.2", "v1.2.3-rc.1"),
            ("v1.2.3-rc.1, v1.2.3, v1.2.3-rc.2", "v1.2.3-rc.1", "v1.2.3-rc.2"),
            (
                "v0.0.0-20250101000000-bbbbbbbbbbbb, v0.0.0-20240101000000-aaaaaaaaaaaa",
                "v0.0.0-20230101000000-cccccccccccc",
                "v0.0.0-20240101000000-aaaaaaaaaaaa",
            ),
            ("2.0.1+incompatible, 2.1.0+incompatible", "v2.0.0+incompatible", "v2.0.1+incompatible"),
        ]
        for fixed, installed, expected in cases:
            with self.subTest(fixed=fixed, installed=installed):
                self.assertEqual(fix_image_cves.lowest_fixed_version(fixed, installed), expected)

    def test_lowest_fixed_version_rejects_invalid_versions(self) -> None:
        cases = [
            ("v1.2.3", ""),
            ("v1.2.3", "(devel)"),
            ("", "v1.2.0"),
            (", ,", "v1.2.0"),
            ("not-a-version", "v1.2.0"),
            ("v1.2.3, not-a-version", "v1.2.0"),
        ]
        for fixed, installed in cases:
            with self.subTest(fixed=fixed, installed=installed):
                with self.assertRaisesRegex(fix_image_cves.CommandError, "Invalid .* Go version"):
                    fix_image_cves.lowest_fixed_version(fixed, installed)

    def test_lowest_fixed_version_rejects_candidates_at_or_below_installed(self) -> None:
        for fixed in ("v1.8.7", "v1.9.0", "v1.8.7, v1.9.0", "v1.9.0+build"):
            with self.subTest(fixed=fixed):
                with self.assertRaisesRegex(fix_image_cves.CommandError, "No fixed version newer than"):
                    fix_image_cves.lowest_fixed_version(fixed, "v1.9.0")

    def test_build_plan_persists_canonical_go_version(self) -> None:
        plan = fix_image_cves.build_plan(
            {
                "findings": [
                    {
                        "category": "GO_MODULE",
                        "package": "golang.org/x/net",
                        "module_root": "health-probe-proxy",
                        "target": "health-probe-proxy",
                        "installed_version": "v0.49.0",
                        "fixed_version": "0.55.0",
                        "id": "CVE-2026-0001",
                    }
                ]
            }
        )

        self.assertEqual(plan["go_module_actions"][0]["fixed_version"], "v0.55.0")

    def test_build_plan_uses_lowest_upgrade_for_each_finding(self) -> None:
        cases = [
            ([("v1.8.0", "1.9.3, 1.8.7")], "v1.8.7"),
            (
                [("v1.8.0", "1.8.7, 1.9.3"), ("v1.8.0", "1.8.8, 1.9.4")],
                "v1.8.8",
            ),
            ([("v1.9.0", "1.8.7, 1.9.3")], "v1.9.3"),
            (
                [("v1.8.0", "1.8.7, 1.9.3"), ("v1.9.0", "1.8.8, 1.9.4")],
                "v1.9.4",
            ),
            ([("v0.49.0", "0.54.0"), ("v0.49.0", "0.55.0")], "v0.55.0"),
        ]
        for versions, expected in cases:
            with self.subTest(versions=versions):
                findings = [
                    {
                        "category": "GO_MODULE",
                        "package": "example.com/dependency",
                        "module_root": ".",
                        "target": "cloud-controller-manager",
                        "installed_version": installed,
                        "fixed_version": fixed,
                        "id": f"CVE-2026-{index:04d}",
                    }
                    for index, (installed, fixed) in enumerate(versions)
                ]
                for ordered in (findings, list(reversed(findings))):
                    plan = fix_image_cves.build_plan({"findings": ordered})
                    actions = plan["go_module_actions"]
                    self.assertEqual(len(actions), 1)
                    self.assertEqual(actions[0]["fixed_version"], expected)
                    self.assertEqual(actions[0]["cves"], sorted(f["id"] for f in findings))
                    self.assertEqual(
                        plan["planned_vulnerability_keys"],
                        sorted(fix_image_cves.vuln_key(f) for f in findings),
                    )
                    self.assertEqual(
                        fix_image_cves.build_go_requirement_commands(
                            actions,
                            {(".", "example.com/dependency"): versions[-1][0]},
                        ),
                        [{
                            "cwd": ".",
                            "cmd": [
                                "go", "mod", "edit",
                                f"-require=example.com/dependency@{expected}",
                            ],
                        }],
                    )

    def test_cross_branch_plan_still_requires_all_cves_to_disappear_on_rescan(self) -> None:
        findings = [
            {
                "category": "GO_MODULE",
                "package": "example.com/dependency",
                "module_root": ".",
                "target": "cloud-controller-manager",
                "installed_version": "v1.8.0",
                "fixed_version": fixed,
                "id": f"CVE-2026-{index:04d}",
            }
            for index, fixed in enumerate(("v1.8.7", "v1.9.4"))
        ]
        plan = fix_image_cves.build_plan({"findings": findings})
        self.assertEqual(plan["go_module_actions"][0]["fixed_version"], "v1.9.4")
        # A numerically higher release need not include another branch's fix.
        payload = {"Results": [{
            "Class": "lang-pkgs",
            "Type": "gobinary",
            "Target": "cloud-controller-manager",
            "Vulnerabilities": [{
                "PkgName": "example.com/dependency",
                "InstalledVersion": "v1.9.4",
                "FixedVersion": "v1.8.7",
                "VulnerabilityID": findings[0]["id"],
            }],
        }]}
        results = {}
        with mock.patch.object(
            fix_image_cves, "ensure_command"
        ), mock.patch.object(
            fix_image_cves, "detect_trivy_db_staleness"
        ), mock.patch.object(fix_image_cves, "run", return_value=json.dumps(payload)):
            self.assertFalse(fix_image_cves.run_rescan(
                Path("/repo"), image="local/test:rebuilt", module_root=".",
                dockerfile="Dockerfile",
                planned_keys=fix_image_cves.actionable_vulnerability_keys(plan),
                results=results,
            ))

        self.assertEqual(
            results["rescan"]["remaining_vulnerability_keys"],
            [fix_image_cves.vuln_key(findings[0])],
        )

    def test_build_plan_rejects_finding_without_a_newer_fix(self) -> None:
        finding = {
            "category": "GO_MODULE",
            "package": "example.com/dependency",
            "module_root": ".",
            "target": "cloud-controller-manager",
            "installed_version": "v1.9.0",
            "fixed_version": "1.8.7, 1.9.0",
            "id": "CVE-2026-0001",
        }
        with self.assertRaisesRegex(
            fix_image_cves.CommandError,
            "example.com/dependency.*CVE-2026-0001.*newer than",
        ):
            fix_image_cves.build_plan({"findings": [finding]})

    def test_command_plan_stops_before_saving_unusable_version_metadata(self) -> None:
        state = {
            "scan": {
                "findings": [{
                    "category": "GO_MODULE",
                    "package": "example.com/dependency",
                    "module_root": ".",
                    "target": "cloud-controller-manager",
                    "installed_version": "(devel)",
                    "fixed_version": "v1.2.3",
                    "id": "CVE-2026-0001",
                }],
            },
        }
        with mock.patch.object(
            fix_image_cves, "ensure_repo_root", return_value=Path("/repo")
        ), mock.patch.object(
            fix_image_cves, "load_state", return_value=state
        ), mock.patch.object(
            fix_image_cves, "build_go_directive_actions"
        ) as directives, mock.patch.object(
            fix_image_cves, "save_state"
        ) as save_state:
            with self.assertRaisesRegex(
                fix_image_cves.CommandError, "example.com/dependency.*CVE-2026-0001.*Invalid installed"
            ):
                fix_image_cves.command_plan(Namespace(repo="."))

        directives.assert_not_called()
        save_state.assert_not_called()

    def test_selected_lowest_fix_drives_go_directive_requirement(self) -> None:
        plan = fix_image_cves.build_plan({
            "findings": [{
                "category": "GO_MODULE",
                "package": "example.com/dependency",
                "module_root": ".",
                "target": "cloud-controller-manager",
                "installed_version": "v1.8.0",
                "fixed_version": "v1.8.7, v1.9.3",
                "id": "CVE-2026-0001",
            }],
        })
        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            (repo_root / "go.mod").write_text(
                "module example.com/main\n\ngo 1.25.0\n", encoding="utf-8"
            )
            with mock.patch.object(
                fix_image_cves,
                "target_module_go_directive",
                side_effect=lambda module, version: {"v1.8.7": "1.25.0", "v1.9.3": "1.26.0"}[version],
            ) as target_directive:
                self.assertEqual(
                    fix_image_cves.build_go_directive_actions(repo_root, plan["go_module_actions"]),
                    [],
                )

        target_directive.assert_called_once_with("example.com/dependency", "v1.8.7")

    def test_target_module_go_directive_reads_go_mod_without_repo_context(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            go_mod = Path(temp_dir) / "x-crypto-v0.56.0.mod"
            go_mod.write_text(
                "module golang.org/x/crypto\n\ngo 1.26.0\n",
                encoding="utf-8",
            )
            payload = json.dumps(
                {
                    "Path": "golang.org/x/crypto",
                    "Version": "v0.56.0",
                    "GoMod": str(go_mod),
                }
            )
            with mock.patch.object(
                fix_image_cves,
                "run",
                return_value=payload,
            ) as run:
                self.assertEqual(
                    fix_image_cves.target_module_go_directive(
                        "golang.org/x/crypto",
                        "v0.56.0",
                    ),
                    "1.26.0",
                )

        call = run.call_args
        self.assertEqual(
            call.args[0],
            ["go", "list", "-m", "-json", "golang.org/x/crypto@v0.56.0"],
        )
        self.assertNotEqual(call.kwargs["cwd"], Path.cwd())
        self.assertEqual(call.kwargs["env"]["GOTOOLCHAIN"], "local")
        self.assertEqual(call.kwargs["env"]["GO111MODULE"], "on")
        self.assertEqual(call.kwargs["env"]["GOWORK"], "off")
        self.assertEqual(call.kwargs["env"]["PATH"], os.environ["PATH"])

    def test_build_go_directive_actions_groups_by_root_and_uses_highest_requirement(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            (repo_root / "go.mod").write_text(
                "module example.com/root\n\ngo 1.25.0\n",
                encoding="utf-8",
            )
            nested = repo_root / "health-probe-proxy"
            nested.mkdir()
            (nested / "go.mod").write_text(
                "module example.com/hpp\n\ngo 1.26.0\n",
                encoding="utf-8",
            )
            actions = [
                {
                    "module": "golang.org/x/crypto",
                    "module_root": ".",
                    "fixed_version": "v0.56.0",
                },
                {
                    "module": "golang.org/x/net",
                    "module_root": ".",
                    "fixed_version": "v0.58.0",
                },
                {
                    "module": "golang.org/x/sys",
                    "module_root": "health-probe-proxy",
                    "fixed_version": "v0.46.0",
                },
            ]
            with mock.patch.object(
                fix_image_cves,
                "target_module_go_directive",
                side_effect=["1.26.0", "1.27.0", "1.26.0"],
            ):
                directive_actions = fix_image_cves.build_go_directive_actions(
                    repo_root,
                    actions,
                )

        self.assertEqual(
            directive_actions,
            [
                {
                    "module_root": ".",
                    "current_version": "1.25.0",
                    "target_version": "1.27.0",
                    "required_by": [
                        "golang.org/x/crypto@v0.56.0",
                        "golang.org/x/net@v0.58.0",
                    ],
                }
            ],
        )

    def test_command_plan_persists_and_displays_go_directive_action(self) -> None:
        state = {
            "scan": {
                "findings": [
                    {
                        "category": "GO_MODULE",
                        "package": "golang.org/x/crypto",
                        "module_root": ".",
                        "target": "cloud-controller-manager",
                        "installed_version": "v0.55.0",
                        "fixed_version": "v0.56.0",
                        "id": "CVE-2026-0001",
                    }
                ]
            }
        }
        directive_action = {
            "module_root": ".",
            "current_version": "1.25.0",
            "target_version": "1.26.0",
            "required_by": ["golang.org/x/crypto@v0.56.0"],
        }
        output = io.StringIO()
        with mock.patch.object(
            fix_image_cves, "ensure_repo_root", return_value=Path("/repo")
        ), mock.patch.object(
            fix_image_cves, "load_state", return_value=state
        ), mock.patch.object(
            fix_image_cves,
            "build_go_directive_actions",
            return_value=[directive_action],
        ), mock.patch.object(
            fix_image_cves, "ensure_command"
        ), mock.patch.object(
            fix_image_cves, "save_state"
        ) as save_state, mock.patch(
            "sys.stdout", output
        ):
            self.assertEqual(fix_image_cves.command_plan(Namespace(repo=".")), 0)

        saved_plan = save_state.call_args.args[1]["plan"]
        self.assertEqual(saved_plan["go_directive_actions"], [directive_action])
        self.assertIn("Go Directive Bumps", output.getvalue())
        self.assertIn("./go.mod 1.25.0 -> 1.26.0", output.getvalue())

    def test_build_go_requirement_commands_batches_and_sorts_by_module_root(self) -> None:
        actions = [
            {
                "module": "golang.org/x/sys",
                "module_root": "health-probe-proxy",
                "fixed_version": "0.44.0",
            },
            {
                "module": "golang.org/x/net",
                "module_root": "health-probe-proxy",
                "fixed_version": "v0.55.0",
            },
            {
                "module": "golang.org/x/text",
                "module_root": "tests",
                "fixed_version": "0.37.0",
            },
            {
                "module": "golang.org/x/sync",
                "module_root": "tests",
                "fixed_version": "0.20.0",
            },
        ]
        resolved_versions = {
            ("health-probe-proxy", "golang.org/x/net"): "v0.49.0",
            ("health-probe-proxy", "golang.org/x/sys"): "v0.40.0",
            ("tests", "golang.org/x/text"): "v0.36.0",
            ("tests", "golang.org/x/sync"): "v0.21.0",
        }

        self.assertEqual(
            fix_image_cves.build_go_requirement_commands(actions, resolved_versions),
            [
                {
                    "cwd": "health-probe-proxy",
                    "cmd": [
                        "go",
                        "mod",
                        "edit",
                        "-require=golang.org/x/net@v0.55.0",
                        "-require=golang.org/x/sys@v0.44.0",
                    ],
                },
                {
                    "cwd": "tests",
                    "cmd": [
                        "go",
                        "mod",
                        "edit",
                        "-require=golang.org/x/text@v0.37.0",
                    ],
                },
            ],
        )

    @unittest.skipUnless(shutil.which("go"), "go is required for the MVS integration test")
    def test_go_requirement_commands_allow_mvs_to_raise_transitive_minimum(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            (root / "net55").mkdir()
            (root / "sys44").mkdir()
            (root / "sys45").mkdir()
            (root / "go.mod").write_text(
                """module example.com/main

go 1.20

require (
	example.com/net v0.49.0
	example.com/sys v0.40.0
)

replace example.com/net v0.55.0 => ./net55
replace example.com/sys v0.44.0 => ./sys44
replace example.com/sys v0.45.0 => ./sys45
""",
                encoding="utf-8",
            )
            (root / "main.go").write_text(
                'package main\n\nimport _ "example.com/net"\n\nfunc main() {}\n',
                encoding="utf-8",
            )
            (root / "net55" / "go.mod").write_text(
                "module example.com/net\n\ngo 1.20\n\nrequire example.com/sys v0.45.0\n",
                encoding="utf-8",
            )
            (root / "net55" / "net.go").write_text(
                'package net\n\nimport _ "example.com/sys"\n',
                encoding="utf-8",
            )
            for directory in ("sys44", "sys45"):
                (root / directory / "go.mod").write_text(
                    "module example.com/sys\n\ngo 1.20\n",
                    encoding="utf-8",
                )
                (root / directory / "sys.go").write_text("package sys\n", encoding="utf-8")

            actions = [
                {
                    "module": "example.com/net",
                    "module_root": ".",
                    "fixed_version": "v0.55.0",
                },
                {
                    "module": "example.com/sys",
                    "module_root": ".",
                    "fixed_version": "v0.44.0",
                },
            ]
            commands = fix_image_cves.build_go_requirement_commands(
                actions,
                {
                    (".", "example.com/net"): "v0.49.0",
                    (".", "example.com/sys"): "v0.40.0",
                },
            )
            env = {
                **os.environ,
                "GOCACHE": str(root / "gocache"),
                "GOMODCACHE": str(root / "gomodcache"),
                "GOPROXY": "off",
                "GOSUMDB": "off",
                "GOTOOLCHAIN": "local",
            }
            env.pop("GOROOT", None)

            subprocess.run(commands[0]["cmd"], cwd=root, env=env, check=True, capture_output=True)
            subprocess.run(
                ["go", "mod", "tidy"], cwd=root, env=env, check=True, capture_output=True
            )
            resolved = subprocess.run(
                ["go", "list", "-m", "-f", "{{.Path}} {{.Version}}", "example.com/net", "example.com/sys"],
                cwd=root,
                env=env,
                check=True,
                capture_output=True,
                text=True,
            )

            self.assertEqual(
                resolved.stdout.splitlines(),
                ["example.com/net v0.55.0", "example.com/sys v0.45.0"],
            )

    def test_version_at_least_handles_equal_higher_lower_and_missing(self) -> None:
        cases = [
            ("v0.44.0", "0.44.0", True),
            ("v0.45.0", "v0.44.0", True),
            ("v0.43.0", "v0.44.0", False),
            ("", "v0.44.0", False),
            ("v1.2.3", "v1.2.3-rc.1", True),
        ]

        for actual, minimum, expected in cases:
            with self.subTest(actual=actual, minimum=minimum):
                self.assertEqual(
                    fix_image_cves.version_at_least(actual, minimum),
                    expected,
                )

    def test_verify_go_directive_accepts_equal_or_higher_and_rejects_lower(self) -> None:
        plan = {
            "go_directive_actions": [
                {
                    "module_root": ".",
                    "current_version": "1.25.0",
                    "target_version": "1.26.0",
                    "required_by": ["golang.org/x/crypto@v0.56.0"],
                }
            ]
        }
        for actual, expected in (
            ("1.26.0", True),
            ("1.27.0", True),
            ("1.25.9", False),
        ):
            with self.subTest(actual=actual), tempfile.TemporaryDirectory() as temp_dir:
                repo_root = Path(temp_dir)
                (repo_root / "go.mod").write_text(
                    f"module example.com/root\n\ngo {actual}\n",
                    encoding="utf-8",
                )
                results = {}
                self.assertEqual(
                    fix_image_cves.verify_go_directive_actions(
                        repo_root,
                        plan,
                        results,
                    ),
                    expected,
                )
                self.assertEqual(
                    results["go_directive_checks"][0]["passed"],
                    expected,
                )

    def test_verify_accepts_transitive_upgrade_above_minimum(self) -> None:
        plan = {
            "go_module_actions": [
                {
                    "module": "golang.org/x/net",
                    "module_root": "health-probe-proxy",
                    "fixed_version": "v0.55.0",
                },
                {
                    "module": "golang.org/x/sys",
                    "module_root": "health-probe-proxy",
                    "fixed_version": "0.44.0",
                },
            ]
        }
        results = {}

        with mock.patch.object(
            fix_image_cves,
            "go_list_module_info",
            side_effect=[module_info("v0.55.0"), module_info("v0.45.0")],
        ), mock.patch.object(
            fix_image_cves, "has_vendor_tree", return_value=False
        ), mock.patch.object(
            fix_image_cves, "parse_vendor_modules", return_value={}
        ):
            passed = fix_image_cves.verify_go_module_actions(Path("/repo"), plan, results)

        self.assertTrue(passed)
        self.assertEqual(results["go_module_checks"][1]["expected_version"], "v0.44.0")
        self.assertTrue(results["go_module_checks"][1]["go_list_ok"])

    def test_verify_rejects_lower_or_missing_resolved_version(self) -> None:
        plan = {
            "go_module_actions": [
                {
                    "module": "golang.org/x/sys",
                    "module_root": "health-probe-proxy",
                    "fixed_version": "v0.44.0",
                }
            ]
        }

        for actual in ("v0.43.0", ""):
            with self.subTest(actual=actual), mock.patch.object(
                fix_image_cves, "go_list_module_info", return_value=module_info(actual)
            ), mock.patch.object(
                fix_image_cves, "has_vendor_tree", return_value=False
            ), mock.patch.object(
                fix_image_cves, "parse_vendor_modules", return_value={}
            ):
                results = {}
                self.assertFalse(
                    fix_image_cves.verify_go_module_actions(Path("/repo"), plan, results)
                )

    def test_verify_checks_vendor_version_as_minimum(self) -> None:
        plan = {
            "go_module_actions": [
                {
                    "module": "golang.org/x/sys",
                    "module_root": ".",
                    "fixed_version": "v0.44.0",
                }
            ]
        }

        for vendor_version, expected in (
            ("v0.44.0", True),
            ("v0.45.0", True),
            ("v0.43.0", False),
            ("", False),
        ):
            with self.subTest(vendor_version=vendor_version), mock.patch.object(
                fix_image_cves, "go_list_module_info", return_value=module_info("v0.45.0")
            ), mock.patch.object(
                fix_image_cves, "has_vendor_tree", return_value=True
            ), mock.patch.object(
                fix_image_cves,
                "parse_vendor_modules",
                return_value={"golang.org/x/sys": module_info(vendor_version)},
            ):
                results = {}
                self.assertEqual(
                    fix_image_cves.verify_go_module_actions(Path("/repo"), plan, results),
                    expected,
                )

    def test_verify_rejects_module_and_vendor_replacements(self) -> None:
        plan = {
            "go_module_actions": [
                {
                    "module": "golang.org/x/sys",
                    "module_root": ".",
                    "fixed_version": "v0.44.0",
                }
            ]
        }
        replacement = module_info(
            "v0.44.0",
            replacement_path="example.com/fork/sys",
            replacement_version="v9.0.0",
        )

        cases = [
            (replacement, module_info("v0.45.0")),
            (module_info("v0.45.0"), replacement),
        ]
        for resolved_info, vendor_info in cases:
            with self.subTest(
                resolved_replacement=resolved_info["replacement_path"],
                vendor_replacement=vendor_info["replacement_path"],
            ), mock.patch.object(
                fix_image_cves, "go_list_module_info", return_value=resolved_info
            ), mock.patch.object(
                fix_image_cves, "has_vendor_tree", return_value=True
            ), mock.patch.object(
                fix_image_cves,
                "parse_vendor_modules",
                return_value={"golang.org/x/sys": vendor_info},
            ):
                results = {}
                self.assertFalse(
                    fix_image_cves.verify_go_module_actions(Path("/repo"), plan, results)
                )

    def test_apply_preflight_rejects_module_replacement(self) -> None:
        replacement = module_info(
            "v0.44.0",
            replacement_path="example.com/fork/sys",
            replacement_version="v9.0.0",
        )

        with self.assertRaisesRegex(
            fix_image_cves.CommandError,
            "replaced by example.com/fork/sys@v9.0.0",
        ):
            fix_image_cves.require_unreplaced_module("golang.org/x/sys", replacement)

    def test_go_list_module_info_preserves_replacement_identity(self) -> None:
        payload = """{
  "Path": "golang.org/x/sys",
  "Version": "v0.44.0",
  "Replace": {
    "Path": "example.com/fork/sys",
    "Version": "v9.0.0"
  }
}"""
        with mock.patch.object(fix_image_cves, "run", return_value=payload):
            resolved = fix_image_cves.go_list_module_info(Path("/repo"), "golang.org/x/sys")

        self.assertEqual(
            resolved,
            {
                "path": "golang.org/x/sys",
                "version": "v0.44.0",
                "replacement_path": "example.com/fork/sys",
                "replacement_version": "v9.0.0",
            },
        )

    def test_parse_vendor_modules_preserves_replacement_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            repo_root = Path(temp_dir)
            vendor = repo_root / "vendor"
            vendor.mkdir()
            (vendor / "modules.txt").write_text(
                "# golang.org/x/sys v0.44.0 => example.com/fork/sys v9.0.0\n",
                encoding="utf-8",
            )

            modules = fix_image_cves.parse_vendor_modules(repo_root)

        self.assertEqual(
            modules["golang.org/x/sys"],
            {
                "path": "golang.org/x/sys",
                "version": "v0.44.0",
                "replacement_path": "example.com/fork/sys",
                "replacement_version": "v9.0.0",
            },
        )


if __name__ == "__main__":
    unittest.main()
