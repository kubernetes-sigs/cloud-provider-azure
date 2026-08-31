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

from contextlib import redirect_stderr, redirect_stdout
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

    def podman_plan(self) -> build_image.BuildPlan:
        return build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="local",
            repo_root=self.repo,
            set_values=["CONTAINER_CLI=/opt/podman/bin/podman"],
        )

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
        ), mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, redirect_stdout(
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

    def test_main_dry_run_with_retry_does_not_run_or_change_command(self) -> None:
        stdout = StringIO()

        with mock.patch.object(
            build_image, "find_repo_root", return_value=self.repo
        ), mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
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
                        "local",
                        "--set",
                        "CONTAINER_CLI=/opt/podman/bin/podman",
                        "--retry-transient-runtime-errors",
                        "--dry-run",
                    ]
                ),
                0,
            )

        run_mock.assert_not_called()
        sleep_mock.assert_not_called()
        self.assertEqual(
            stdout.getvalue(),
            "cwd: /repo\n"
            "command: IMAGE_TAG=dev IMAGE_REGISTRY=local "
            "GOEXPERIMENT=nosystemcrypto ENABLE_GIT_COMMAND=false "
            "CONTAINER_CLI=/opt/podman/bin/podman make build-ccm-image\n",
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
        ) as run_mock, redirect_stdout(
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
        ) as run_mock, redirect_stdout(
            stdout
        ):
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

    def test_classifies_only_transient_podman_registry_failures(self) -> None:
        positive_cases = [
            "pinging container registry gcr.io: Temporary failure in name resolution",
            "pulling image quay.io/example: i/o timeout",
            "reading manifest latest in registry: connection timed out",
            "fetching blob sha256:abc: connection reset by peer",
            "initializing source docker://example/image: TLS handshake timeout",
            "copying system image: unexpected HTTP status: 429 Too Many Requests",
            "pinging container registry: status code 503 Service Unavailable",
        ]
        negative_cases = [
            "compile: connection reset by peer",
            "RUN go mod download: i/o timeout",
            "pinging container registry: status code 401 Unauthorized",
            "reading manifest: status code 404 Not Found",
            "copying blob: no space left on device",
            "initializing source docker://example/image: manifest unknown",
            "context deadline exceeded",
            "pinging container registry: i/o timeout\n"
            + "\n".join(["go build: compilation failed"] * 51),
        ]

        for output in positive_cases:
            with self.subTest(output=output):
                self.assertTrue(
                    build_image.is_transient_podman_registry_failure(output)
                )
        for output in negative_cases:
            with self.subTest(output=output):
                self.assertFalse(
                    build_image.is_transient_podman_registry_failure(output)
                )

    def test_run_plan_captures_only_failure_tail_and_replays_stderr(self) -> None:
        plan = self.podman_plan()
        lines = [f"stderr line {index}\n" for index in range(55)]
        process = mock.Mock()
        process.stderr = iter(lines)
        process.wait.return_value = 7
        stderr = StringIO()

        with mock.patch.object(
            build_image.subprocess, "Popen", return_value=process
        ) as popen_mock, redirect_stderr(stderr):
            result = build_image.run_plan_capturing_stderr(plan, {"PATH": "/bin"})

        self.assertEqual(result.returncode, 7)
        self.assertEqual(
            result.stderr_tail,
            "".join(lines[-build_image.FAILURE_TAIL_LINES :]),
        )
        self.assertEqual(stderr.getvalue(), "".join(lines))
        _, kwargs = popen_mock.call_args
        self.assertNotIn("stdout", kwargs)
        self.assertEqual(kwargs["stderr"], subprocess.PIPE)
        self.assertTrue(kwargs["text"])

    def test_podman_transient_failure_retries_identical_build_once(self) -> None:
        plan = self.podman_plan()
        first = build_image.BuildAttempt(
            returncode=125,
            stderr_tail=(
                "pinging container registry gcr.io: "
                "Temporary failure in name resolution\n"
            ),
        )
        health = subprocess.CompletedProcess(
            args=["/opt/podman/bin/podman", "info"],
            returncode=0,
            stderr="",
        )
        second = build_image.BuildAttempt(returncode=0, stderr_tail="")

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", side_effect=[first, second]
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run", return_value=health
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 0
            )

        self.assertEqual(build_mock.call_count, 2)
        run_mock.assert_called_once()
        sleep_mock.assert_called_once_with(build_image.RETRY_DELAY_SECONDS)
        first_call, second_call = build_mock.call_args_list
        self.assertEqual(first_call.args, second_call.args)
        self.assertEqual(first_call.kwargs, second_call.kwargs)
        self.assertEqual(run_mock.call_args.args[0], ["/opt/podman/bin/podman", "info"])

    def test_podman_failed_retry_returns_second_failure_without_third_attempt(
        self,
    ) -> None:
        plan = self.podman_plan()
        transient = (
            "initializing source docker://gcr.io/example: " "connection reset by peer\n"
        )
        first = build_image.BuildAttempt(returncode=125, stderr_tail=transient)
        health = subprocess.CompletedProcess(
            args=["/opt/podman/bin/podman", "info"], returncode=0, stderr=""
        )
        second = build_image.BuildAttempt(returncode=2, stderr_tail=transient)

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", side_effect=[first, second]
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run", return_value=health
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ), redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 2
            )

        self.assertEqual(build_mock.call_count, 2)
        run_mock.assert_called_once()

    def test_podman_nontransient_failure_is_not_retried(self) -> None:
        plan = self.podman_plan()
        failed = build_image.BuildAttempt(
            returncode=2,
            stderr_tail="go build: compilation failed\n",
        )

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", return_value=failed
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 2
            )

        build_mock.assert_called_once()
        run_mock.assert_not_called()
        sleep_mock.assert_not_called()

    def test_interrupted_podman_build_is_not_retried(self) -> None:
        plan = self.podman_plan()

        for returncode in (-2, 130, 137, 143):
            with self.subTest(returncode=returncode):
                interrupted = build_image.BuildAttempt(
                    returncode=returncode,
                    stderr_tail="pulling image: TLS handshake timeout\n",
                )
                with mock.patch.object(
                    build_image,
                    "run_plan_capturing_stderr",
                    return_value=interrupted,
                ) as build_mock, mock.patch.object(
                    build_image.subprocess, "run"
                ) as run_mock, mock.patch.object(
                    build_image.time, "sleep"
                ) as sleep_mock, redirect_stdout(
                    StringIO()
                ), redirect_stderr(
                    StringIO()
                ):
                    self.assertEqual(
                        build_image.run_plan(plan, retry_transient_runtime_errors=True),
                        returncode,
                    )

                build_mock.assert_called_once()
                run_mock.assert_not_called()
                sleep_mock.assert_not_called()

    def test_podman_success_with_transient_warning_is_not_retried(self) -> None:
        plan = self.podman_plan()
        succeeded = build_image.BuildAttempt(
            returncode=0,
            stderr_tail=(
                "pulling image recovered after "
                "Temporary failure in name resolution\n"
            ),
        )

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", return_value=succeeded
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 0
            )

        build_mock.assert_called_once()
        run_mock.assert_not_called()
        sleep_mock.assert_not_called()

    def test_podman_health_check_failure_prevents_retry(self) -> None:
        plan = self.podman_plan()
        first = build_image.BuildAttempt(
            returncode=125,
            stderr_tail="pulling image: TLS handshake timeout\n",
        )
        health = subprocess.CompletedProcess(
            args=["/opt/podman/bin/podman", "info"],
            returncode=1,
            stderr="machine unavailable\n",
        )

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", return_value=first
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run", return_value=health
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ), redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 125
            )

        build_mock.assert_called_once()
        run_mock.assert_called_once()

    def test_podman_health_check_timeout_prevents_retry(self) -> None:
        plan = self.podman_plan()
        first = build_image.BuildAttempt(
            returncode=125,
            stderr_tail="pulling image: TLS handshake timeout\n",
        )

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", return_value=first
        ) as build_mock, mock.patch.object(
            build_image.subprocess,
            "run",
            side_effect=subprocess.TimeoutExpired("podman info", 10),
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ), redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 125
            )

        build_mock.assert_called_once()
        run_mock.assert_called_once()

    def test_transient_podman_failure_without_retry_option_runs_once(self) -> None:
        plan = self.podman_plan()
        failed = subprocess.CompletedProcess(args=plan.cmd, returncode=125)

        with mock.patch.object(
            build_image.subprocess, "run", return_value=failed
        ) as run_mock, mock.patch.object(
            build_image, "run_plan_capturing_stderr"
        ) as build_mock, redirect_stdout(
            StringIO()
        ):
            self.assertEqual(build_image.run_plan(plan), 125)

        run_mock.assert_called_once()
        build_mock.assert_not_called()

    def test_retry_option_requires_explicit_supported_runtime(self) -> None:
        cases = [[], ["CONTAINER_CLI=/usr/local/bin/nerdctl"]]

        for set_values in cases:
            with self.subTest(set_values=set_values):
                stderr = StringIO()
                argv = [
                    "--image",
                    "ccm",
                    "--tag",
                    "dev",
                    "--registry",
                    "local",
                    "--retry-transient-runtime-errors",
                    "--dry-run",
                ]
                for value in set_values:
                    argv.extend(["--set", value])

                with mock.patch.object(
                    build_image, "find_repo_root", return_value=self.repo
                ), mock.patch.object(
                    build_image.subprocess, "run"
                ) as run_mock, redirect_stderr(
                    stderr
                ):
                    self.assertEqual(build_image.main(argv), 2)

                run_mock.assert_not_called()
                self.assertIn("requires --set CONTAINER_CLI", stderr.getvalue())

    def test_docker_buildx_setup_race_retries_once(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="local",
            repo_root=self.repo,
            set_values=["CONTAINER_CLI=/usr/local/bin/docker"],
        )
        first = build_image.BuildAttempt(
            returncode=1,
            stderr_tail=(
                'ERROR: existing instance for "img-builder" but no append mode, '
                "specify the node name to make changes\n"
            ),
        )
        second = build_image.BuildAttempt(returncode=0, stderr_tail="")

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", side_effect=[first, second]
        ) as build_mock, mock.patch.object(
            build_image.subprocess, "run"
        ) as run_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 0
            )

        self.assertEqual(build_mock.call_count, 2)
        run_mock.assert_not_called()
        sleep_mock.assert_called_once_with(build_image.RETRY_DELAY_SECONDS)

    def test_docker_non_buildx_failure_is_not_retried(self) -> None:
        plan = build_image.build_plan(
            image="ccm",
            tag="dev",
            registry="local",
            repo_root=self.repo,
            set_values=["CONTAINER_CLI=/usr/local/bin/docker"],
        )
        failed = build_image.BuildAttempt(
            returncode=1,
            stderr_tail=(
                "pinging container registry gcr.io: "
                "Temporary failure in name resolution\n"
            ),
        )

        with mock.patch.object(
            build_image, "run_plan_capturing_stderr", return_value=failed
        ) as build_mock, mock.patch.object(
            build_image.time, "sleep"
        ) as sleep_mock, redirect_stdout(
            StringIO()
        ), redirect_stderr(
            StringIO()
        ):
            self.assertEqual(
                build_image.run_plan(plan, retry_transient_runtime_errors=True), 1
            )

        build_mock.assert_called_once()
        sleep_mock.assert_not_called()

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
        ) as run_mock, redirect_stdout(
            StringIO()
        ):
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
