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
    def test_git_path_resolves_main_checkout_state(self) -> None:
        repo_root = Path("/repo")
        with mock.patch.object(
            fix_image_cves,
            "run",
            return_value=".git/fix-image-cves.json",
        ) as run:
            self.assertEqual(
                fix_image_cves.git_path(repo_root),
                Path("/repo/.git/fix-image-cves.json"),
            )

        run.assert_called_once_with(
            ["git", "rev-parse", "--git-path", "fix-image-cves.json"],
            cwd=repo_root,
            capture=True,
        )

    def test_git_path_preserves_linked_worktree_state(self) -> None:
        worktree_path = Path(
            "/repo/.git/worktrees/cloud-provider-azure1/fix-image-cves.json"
        )
        with mock.patch.object(
            fix_image_cves,
            "run",
            return_value=str(worktree_path),
        ):
            self.assertEqual(
                fix_image_cves.git_path(Path("/worktree")),
                worktree_path,
            )

    def test_git_path_rejects_empty_output(self) -> None:
        with mock.patch.object(fix_image_cves, "run", return_value=""):
            with self.assertRaisesRegex(
                fix_image_cves.CommandError,
                "empty CVE state path",
            ):
                fix_image_cves.git_path(Path("/repo"))

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
