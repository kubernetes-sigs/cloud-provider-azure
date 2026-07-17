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

from contextlib import redirect_stdout
from io import StringIO
import os
import subprocess
import tempfile
import unittest
from unittest import mock
from pathlib import Path

import build_image


class BuildImageTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path("/repo")

    def test_ccm_uses_default_command(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
        )

        self.assertEqual(plan.cwd, self.repo)
        self.assertEqual(plan.cmd, ["make", "build-ccm-image"])
        self.assertEqual(
            build_image.format_command(plan),
            "IMAGE_TAG=dev IMAGE_REGISTRY=example.azurecr.io/cpa "
            "GOEXPERIMENT=nosystemcrypto ENABLE_GIT_COMMAND=false "
            "make build-ccm-image",
        )

    def test_aliases_use_expected_targets_directories_and_defaults(self) -> None:
        cases = [
            ("all", "image", self.repo, False),
            ("ccm", "build-ccm-image", self.repo, True),
            ("ccm-all", "build-all-ccm-images", self.repo, True),
            ("ccm-e2e", "build-ccm-e2e-test-image", self.repo, False),
            ("cnm", "build-node-image-linux", self.repo, True),
            ("cnm-all", "build-all-node-images", self.repo, False),
            ("cnm-linux", "build-node-image-linux", self.repo, True),
            ("cnm-windows", "build-node-image-windows", self.repo, False),
            ("cnm-windows-hpc", "build-node-image-windows-hpc", self.repo, False),
            (
                "hpp",
                "build-health-probe-proxy-image",
                self.repo / "health-probe-proxy",
                False,
            ),
            (
                "hpp-windows",
                "build-health-probe-proxy-image-windows",
                self.repo / "health-probe-proxy",
                False,
            ),
        ]

        for image, make_target, cwd, default_goexperiment in cases:
            with self.subTest(image=image):
                plan = build_image.build_plan(
                    image=image,
                    tag="dev",
                    registry="example.azurecr.io/cpa",
                    repo_root=self.repo,
                )

                self.assertEqual(plan.cwd, cwd)
                expected_cmd = ["make", make_target]
                if image in {"hpp", "hpp-windows"}:
                    expected_cmd.insert(1, "-B")
                self.assertEqual(plan.cmd, expected_cmd)
                self.assertEqual(plan.env["IMAGE_TAG"], "dev")
                self.assertEqual(plan.env["IMAGE_REGISTRY"], "example.azurecr.io/cpa")
                self.assertEqual(plan.env["ENABLE_GIT_COMMAND"], "false")
                if default_goexperiment:
                    self.assertEqual(plan.env["GOEXPERIMENT"], "nosystemcrypto")
                else:
                    self.assertNotIn("GOEXPERIMENT", plan.env)

    def test_can_set_and_unset_make_flags(self) -> None:
        plan = build_image.build_plan(
            image="cnm",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
            set_values=["ARCH=arm64"],
            unset_values=["GOEXPERIMENT"],
        )

        self.assertEqual(plan.cwd, self.repo)
        self.assertEqual(plan.cmd, ["make", "build-node-image-linux"])
        self.assertEqual(
            build_image.format_command(plan),
            "env -u GOEXPERIMENT "
            "IMAGE_TAG=dev IMAGE_REGISTRY=example.azurecr.io/cpa "
            "ENABLE_GIT_COMMAND=false ARCH=arm64 "
            "make build-node-image-linux",
        )

    def test_can_explicitly_set_goexperiment_for_non_default_alias(self) -> None:
        plan = build_image.build_plan(
            image="hpp",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
            set_values=["GOEXPERIMENT=nosystemcrypto"],
        )

        self.assertEqual(plan.env["GOEXPERIMENT"], "nosystemcrypto")

    def test_can_force_safe_local_amd64_output(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="local",
            repo_root=self.repo,
            set_values=["ARCH=amd64", "OUTPUT_TYPE=docker"],
            unset_values=["OUTPUT_FLAG", "BUILDX_EXTRA_FLAGS"],
            inherited_env={
                "ARCH": "arm64",
                "OUTPUT_TYPE": "registry",
                "OUTPUT_FLAG": "--output=type=registry",
                "BUILDX_EXTRA_FLAGS": "--push",
            },
        )

        self.assertEqual(plan.env["ARCH"], "amd64")
        self.assertEqual(plan.env["OUTPUT_TYPE"], "docker")
        self.assertIn("OUTPUT_FLAG", plan.unset_env)
        self.assertIn("BUILDX_EXTRA_FLAGS", plan.unset_env)
        self.assertNotIn("OUTPUT_FLAG", plan.env)
        self.assertNotIn("BUILDX_EXTRA_FLAGS", plan.env)

    def test_rejects_unknown_image_alias(self) -> None:
        with self.assertRaisesRegex(ValueError, "unknown image alias"):
            build_image.build_plan(
                image="acr",
                tag="dev",
                registry="example.azurecr.io/cpa",
                repo_root=self.repo,
            )

    def test_rejects_unsetting_required_image_inputs(self) -> None:
        for key in ("IMAGE_TAG", "IMAGE_REGISTRY"):
            with self.subTest(key=key):
                with self.assertRaisesRegex(ValueError, "required"):
                    build_image.build_plan(
                        image="ccm",
                        tag="dev",
                        registry="example.azurecr.io/cpa",
                        repo_root=self.repo,
                        unset_values=[key],
                    )

    def test_rejects_setting_required_image_inputs(self) -> None:
        for value in ("IMAGE_TAG=other", "IMAGE_REGISTRY=other"):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "--tag|--registry"):
                    build_image.build_plan(
                        image="ccm",
                        tag="dev",
                        registry="example.azurecr.io/cpa",
                        repo_root=self.repo,
                        set_values=[value],
                    )

    def test_rejects_setting_make_control_environment(self) -> None:
        for value in (
            "MAKEFLAGS=IMAGE_TAG=other",
            "MFLAGS=IMAGE_REGISTRY=other",
            "GNUMAKEFLAGS=IMAGE_TAG=other",
            "MAKEOVERRIDES=IMAGE_TAG=other",
            "MAKEFILES=/tmp/override.mk",
        ):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "reserved"):
                    build_image.build_plan(
                        image="ccm",
                        tag="dev",
                        registry="example.azurecr.io/cpa",
                        repo_root=self.repo,
                        set_values=[value],
                    )

    def test_rejects_set_unset_conflicts(self) -> None:
        with self.assertRaisesRegex(ValueError, "both --set and --unset"):
            build_image.build_plan(
                image="ccm",
                tag="dev",
                registry="example.azurecr.io/cpa",
                repo_root=self.repo,
                set_values=["ARCH=arm64"],
                unset_values=["ARCH"],
            )

    def test_main_dry_run_prints_command_without_running_make(self) -> None:
        stdout = StringIO()

        with mock.patch.object(
            build_image, "find_repo_root", return_value=self.repo
        ), mock.patch.object(build_image.subprocess, "run") as run_mock, redirect_stdout(
            stdout
        ):
            self.assertEqual(
                build_image.main(
                    [
                        "--image",
                        "ccm",
                        "--tag",
                        "dev",
                        "--registry",
                        "example.azurecr.io/cpa",
                        "--dry-run",
                    ]
                ),
                0,
            )

        run_mock.assert_not_called()
        self.assertEqual(
            stdout.getvalue(),
            "cwd: /repo\n"
            "command: IMAGE_TAG=dev IMAGE_REGISTRY=example.azurecr.io/cpa "
            "GOEXPERIMENT=nosystemcrypto ENABLE_GIT_COMMAND=false "
            "make build-ccm-image\n",
        )

    def test_dry_run_shows_inherited_make_control_cleanup(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
            inherited_env={"MAKEFLAGS": "IMAGE_TAG=other"},
        )

        self.assertEqual(
            build_image.format_command(plan),
            "env -u MAKEFLAGS "
            "IMAGE_TAG=dev IMAGE_REGISTRY=example.azurecr.io/cpa "
            "GOEXPERIMENT=nosystemcrypto ENABLE_GIT_COMMAND=false "
            "make build-ccm-image",
        )

    def test_main_can_target_an_explicit_checkout(self) -> None:
        stdout = StringIO()
        with tempfile.TemporaryDirectory() as temp_dir, mock.patch.object(
            build_image, "find_repo_root"
        ) as find_repo_root, mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, redirect_stdout(stdout):
            self.assertEqual(
                build_image.main(
                    [
                        "--image",
                        "ccm",
                        "--tag",
                        "dev",
                        "--registry",
                        "local",
                        "--repo",
                        temp_dir,
                        "--dry-run",
                    ]
                ),
                0,
            )

        find_repo_root.assert_not_called()
        run_mock.assert_not_called()
        self.assertIn(f"cwd: {Path(temp_dir).resolve()}\n", stdout.getvalue())

    def test_run_plan_prints_command_and_cleans_env(self) -> None:
        plan = build_image.build_plan(
            image="hpp",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
            unset_values=["ENABLE_GIT_COMMAND"],
        )
        completed = subprocess.CompletedProcess(args=plan.cmd, returncode=7)

        stdout = StringIO()
        with mock.patch.dict(
            os.environ,
            {"GOEXPERIMENT": "systemcrypto", "ENABLE_GIT_COMMAND": "true"},
            clear=True,
        ), mock.patch.object(
            build_image.subprocess, "run", return_value=completed
        ) as run_mock, redirect_stdout(stdout):
            self.assertEqual(build_image.run_plan(plan), 7)

        run_mock.assert_called_once()
        self.assertEqual(
            stdout.getvalue(),
            "cwd: /repo/health-probe-proxy\n"
            "command: env -u ENABLE_GIT_COMMAND -u GOEXPERIMENT "
            "IMAGE_TAG=dev IMAGE_REGISTRY=example.azurecr.io/cpa "
            "make -B build-health-probe-proxy-image\n",
        )
        _, kwargs = run_mock.call_args
        self.assertEqual(
            run_mock.call_args.args[0],
            ["make", "-B", "build-health-probe-proxy-image"],
        )
        self.assertEqual(kwargs["cwd"], self.repo / "health-probe-proxy")
        self.assertFalse(kwargs.get("shell", False))
        self.assertNotIn("GOEXPERIMENT", kwargs["env"])
        self.assertNotIn("ENABLE_GIT_COMMAND", kwargs["env"])
        self.assertEqual(kwargs["env"]["IMAGE_TAG"], "dev")
        self.assertEqual(kwargs["env"]["IMAGE_REGISTRY"], "example.azurecr.io/cpa")

    def test_run_plan_scrubs_inherited_make_control_environment(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="example.azurecr.io/cpa",
            repo_root=self.repo,
        )
        completed = subprocess.CompletedProcess(args=plan.cmd, returncode=0)

        with mock.patch.dict(
            os.environ,
            {
                "MAKEFLAGS": "IMAGE_TAG=other",
                "MFLAGS": "IMAGE_REGISTRY=other",
                "GNUMAKEFLAGS": "IMAGE_TAG=other",
                "MAKEOVERRIDES": "IMAGE_TAG=other",
                "MAKEFILES": "/tmp/override.mk",
                "PATH": "/usr/bin",
            },
            clear=True,
        ), mock.patch.object(
            build_image.subprocess, "run", return_value=completed
        ) as run_mock, redirect_stdout(StringIO()):
            self.assertEqual(build_image.run_plan(plan), 0)

        _, kwargs = run_mock.call_args
        for key in (
            "MAKEFLAGS",
            "MFLAGS",
            "GNUMAKEFLAGS",
            "MAKEOVERRIDES",
            "MAKEFILES",
        ):
            with self.subTest(key=key):
                self.assertNotIn(key, kwargs["env"])
        self.assertEqual(kwargs["env"]["PATH"], "/usr/bin")
        self.assertEqual(kwargs["env"]["IMAGE_TAG"], "dev")
        self.assertEqual(kwargs["env"]["IMAGE_REGISTRY"], "example.azurecr.io/cpa")

    def test_find_repo_root_uses_script_location_not_caller_git_state(self) -> None:
        script_path = (
            self.repo
            / ".agents"
            / "skills"
            / "build-images"
            / "scripts"
            / "build_image.py"
        )
        with tempfile.TemporaryDirectory() as temp_dir, mock.patch(
            "build_image.__file__",
            str(script_path),
        ):
            self.assertEqual(build_image.find_repo_root(Path(temp_dir)), self.repo)


if __name__ == "__main__":
    unittest.main()
