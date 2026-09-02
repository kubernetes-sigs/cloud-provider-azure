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
import subprocess
import unittest
from pathlib import Path
from unittest import mock

import sync_go_modules


class SyncGoModulesTest(unittest.TestCase):
    def test_read_only_git_disables_optional_locks_without_mutating_parent_env(self) -> None:
        repo = Path("/repo")
        with mock.patch.dict(
            os.environ,
            {"GIT_OPTIONAL_LOCKS": "1", "KEEP_ME": "yes"},
            clear=True,
        ):
            with mock.patch.object(
                sync_go_modules,
                "run",
                return_value="ok",
            ) as run:
                self.assertEqual(
                    sync_go_modules.read_only_git(repo, ["status", "--short"]),
                    "ok",
                )

            child_env = run.call_args.kwargs["env"]
            self.assertEqual(child_env["GIT_OPTIONAL_LOCKS"], "0")
            self.assertEqual(child_env["KEEP_ME"], "yes")
            self.assertEqual(os.environ["GIT_OPTIONAL_LOCKS"], "1")

        run.assert_called_once_with(
            ["git", "-C", str(repo), "status", "--short"],
            cwd=repo,
            env=child_env,
            capture=True,
        )

    def test_repo_queries_use_read_only_git(self) -> None:
        repo = Path("/repo")
        with mock.patch.object(
            sync_go_modules,
            "read_only_git",
            side_effect=[
                str(repo),
                " M go.mod",
                "go.mod\nnested/go.mod",
                " go.mod | 2 +-",
            ],
        ) as read_only_git:
            self.assertEqual(sync_go_modules.resolve_repo_root(repo), repo)
            self.assertEqual(sync_go_modules.status_short(repo), " M go.mod")
            self.assertEqual(
                sync_go_modules.discover_go_modules(repo),
                [".", "nested"],
            )
            with mock.patch("sys.stderr", new=io.StringIO()) as stderr:
                sync_go_modules.print_diff_stat(repo)
                self.assertIn("go.mod | 2 +-", stderr.getvalue())

        self.assertEqual(
            read_only_git.call_args_list,
            [
                mock.call(repo, ["rev-parse", "--show-toplevel"]),
                mock.call(repo, ["status", "--short"]),
                mock.call(repo, ["ls-files", "go.mod", "**/go.mod"]),
                mock.call(repo, ["diff", "--stat"]),
            ],
        )

    def test_check_clean_disables_optional_git_locks(self) -> None:
        repo = Path("/repo")
        with mock.patch.dict(
            os.environ,
            {"GIT_OPTIONAL_LOCKS": "1", "KEEP_ME": "yes"},
            clear=True,
        ):
            with mock.patch.object(
                sync_go_modules.subprocess,
                "run",
                return_value=subprocess.CompletedProcess([], 0),
            ) as subprocess_run:
                sync_go_modules.check_clean(repo)

            child_env = subprocess_run.call_args.kwargs["env"]
            self.assertEqual(child_env["GIT_OPTIONAL_LOCKS"], "0")
            self.assertEqual(child_env["KEEP_ME"], "yes")
            self.assertEqual(os.environ["GIT_OPTIONAL_LOCKS"], "1")

        subprocess_run.assert_called_once_with(
            ["git", "-C", str(repo), "diff", "--quiet"],
            cwd=repo,
            env=child_env,
        )

    def test_check_clean_preserves_dirty_result(self) -> None:
        repo = Path("/repo")
        with mock.patch.object(
            sync_go_modules.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 1),
        ):
            with self.assertRaisesRegex(
                sync_go_modules.CommandError,
                "Repository has changes after sync",
            ):
                sync_go_modules.check_clean(repo)

    def test_run_logged_preserves_non_git_environment(self) -> None:
        repo = Path("/repo")
        env = {"GOTOOLCHAIN": "local", "KEEP_ME": "yes"}
        with mock.patch.object(sync_go_modules, "run") as run:
            with mock.patch("sys.stdout", new=io.StringIO()):
                sync_go_modules.run_logged(
                    ["go", "mod", "verify"],
                    cwd=repo,
                    env=env,
                    dry_run=False,
                )

        run.assert_called_once_with(
            ["go", "mod", "verify"],
            cwd=repo,
            env=env,
        )
        self.assertNotIn("GIT_OPTIONAL_LOCKS", env)


if __name__ == "__main__":
    unittest.main()
