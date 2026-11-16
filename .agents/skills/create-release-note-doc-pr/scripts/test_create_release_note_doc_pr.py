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

from contextlib import ExitStack, redirect_stdout
import csv
from io import StringIO
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import create_release_note_doc_pr as doc_pr


TAG = "v0.6.0"
NOTE = """---
title: v0.6.0
linkTitle: v0.6.0
date: 2020-09-01
description: >
  Cloud Provider Azure v0.6.0
---
Full Changelog: v0.5.0..v0.6.0

## Changes by Kind

- Preserve release-note content, including Unicode: →.

---
url: this is body text, not front matter
"""
URL_LINE = 'url: "/blog/v0.6.0/"\n'


class ReleaseNoteURLTest(unittest.TestCase):
    def setUp(self) -> None:
        temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(temp_dir.cleanup)
        self.repo = Path(temp_dir.name)
        self.site_file = self.repo / "content/en/blog/releases/v0.6.0.md"
        self.site_file.parent.mkdir(parents=True)

    def write_note(self, content: str = NOTE) -> None:
        self.site_file.write_text(content, encoding="utf-8")

    def test_adds_date_free_url_without_changing_metadata_or_body(self) -> None:
        self.write_note()

        doc_pr.set_release_note_url(self.site_file, TAG)

        self.assertEqual(
            self.site_file.read_text(encoding="utf-8"),
            NOTE.replace("---\nFull Changelog:", URL_LINE + "---\nFull Changelog:"),
        )
        doc_pr.validate_generated_site_file(self.site_file)

    def test_replaces_existing_url_and_preserves_aliases(self) -> None:
        for key in ("url", '"url"', "'url'"):
            for old_url in (
                "/blog/2020/09/01/v0.6.0/",
                '"/blog/2020/09/01/v0.6.0/"',
                "'/blog/v0.6.0/'",
            ):
                with self.subTest(key=key, old_url=old_url):
                    old_line = f"{key}: {old_url}\n"
                    original = NOTE.replace(
                        "date: 2020-09-01\n",
                        f"date: 2020-09-01\n{old_line}"
                        "aliases:\n  - /previous-release-note/\n",
                    )
                    self.write_note(original)

                    doc_pr.set_release_note_url(self.site_file, TAG)

                    self.assertEqual(
                        self.site_file.read_text(encoding="utf-8"),
                        original.replace(old_line, URL_LINE),
                    )

    def test_preserves_blank_lines_after_single_line_url(self) -> None:
        old_line = "url: /blog/old-address/\n"
        for blank_lines in ("\n", "  \n", "\n \t\n"):
            with self.subTest(blank_lines=blank_lines):
                original = NOTE.replace(
                    "date: 2020-09-01\n",
                    f"date: 2020-09-01\n{old_line}{blank_lines}",
                )
                self.write_note(original)

                doc_pr.set_release_note_url(self.site_file, TAG)

                self.assertEqual(
                    self.site_file.read_text(encoding="utf-8"),
                    original.replace(old_line, URL_LINE),
                )

    def test_uses_requested_tag(self) -> None:
        self.write_note(NOTE.replace(TAG, "v1.35.7"))

        doc_pr.set_release_note_url(self.site_file, "v1.35.7")

        self.assertIn(
            'url: "/blog/v1.35.7/"\n',
            self.site_file.read_text(encoding="utf-8"),
        )

    def test_normalization_is_idempotent(self) -> None:
        self.write_note()
        doc_pr.set_release_note_url(self.site_file, TAG)

        with mock.patch.object(Path, "write_text") as write:
            doc_pr.set_release_note_url(self.site_file, TAG)

        write.assert_not_called()

    def test_dry_run_does_not_read_or_write_the_note(self) -> None:
        stdout = StringIO()
        with redirect_stdout(stdout):
            doc_pr.set_release_note_url(self.site_file, TAG, dry_run=True)

        self.assertFalse(self.site_file.exists())
        self.assertIn("/blog/v0.6.0/", stdout.getvalue())

    def test_rejects_unsupported_front_matter_without_writing(self) -> None:
        cases = [
            ("## Changes\n", "Expected YAML front matter"),
            ("---\ntitle: v0.6.0\n## Changes\n", "Unterminated YAML front matter"),
            ("---\nurl: /one/\nurl: /two/\n---\n## Changes\n", "Multiple URL fields"),
            ('---\n"url": /one/\nurl: /two/\n---\n## Changes\n', "Multiple URL fields"),
            ("---\n'url': /one/\nurl: /two/\n---\n## Changes\n", "Multiple URL fields"),
            ("---\nurl: >\n  /old/\n---\n## Changes\n", "single-line URL"),
            ("---\nurl: >\n\n  /old/\n---\n## Changes\n", "single-line URL"),
            ('---\n"url": >\n\n  /old/\n---\n## Changes\n', "single-line URL"),
            ("---\nurl: |\n  \n  /old/\n---\n## Changes\n", "single-line URL"),
            ("---\nurl:\n\n  /old/\n---\n## Changes\n", "single-line URL"),
            ("---\nurl: /old/\n\n  continued\n---\n## Changes\n", "single-line URL"),
        ]
        for content, error in cases:
            with self.subTest(content=content):
                self.write_note(content)

                with self.assertRaisesRegex(doc_pr.CommandError, error):
                    doc_pr.set_release_note_url(self.site_file, TAG)

                self.assertEqual(self.site_file.read_text(encoding="utf-8"), content)

    def run_main(self, *flags: str) -> tuple[list[list[str]], list[str]]:
        commands: list[list[str]] = []
        staged_content: list[str] = []

        def run(cmd: list[str], **kwargs: object) -> str:
            commands.append(cmd)
            if kwargs.get("dry_run"):
                return ""
            if cmd[:2] == ["git", "rev-parse"]:
                return "master"
            if cmd[0] == "./hack/generate-release-note.sh":
                # Simulate the documentation branch's existing generator, which
                # emits a date but no explicit URL.
                self.write_note()
            if cmd[:2] == ["git", "add"]:
                staged_content.append(self.site_file.read_text(encoding="utf-8"))
            return ""

        with ExitStack() as stack:
            for name, result in {
                "ensure_command": None,
                "ensure_repo_root": self.repo,
                "ensure_clean_worktree": None,
                "remote_exists": True,
                "resolve_base_remote": "upstream",
                "resolve_target_repo": "kubernetes-sigs/cloud-provider-azure",
                "resolve_head_owner": "contributor",
                "load_github_env": {},
                "get_existing_pr": "",
            }.items():
                stack.enter_context(mock.patch.object(doc_pr, name, return_value=result))
            stack.enter_context(mock.patch.object(doc_pr, "run", side_effect=run))
            stack.enter_context(
                mock.patch.object(
                    doc_pr.subprocess,
                    "run",
                    return_value=subprocess.CompletedProcess([], 1),
                )
            )
            stack.enter_context(
                mock.patch.object(doc_pr.sys, "argv", ["doc-pr", "--tag", TAG, *flags])
            )
            stack.enter_context(redirect_stdout(StringIO()))
            self.assertEqual(doc_pr.main(), 0)

        return commands, staged_content

    def test_main_normalizes_generated_note_before_staging(self) -> None:
        commands, staged_content = self.run_main()

        self.assertTrue(any(cmd[0] == "./hack/generate-release-note.sh" for cmd in commands))
        self.assertEqual(len(staged_content), 1)
        self.assertIn(URL_LINE, staged_content[0])
        self.assertIn("date: 2020-09-01\n", staged_content[0])

    def test_main_normalizes_note_with_no_generate(self) -> None:
        self.write_note()

        commands, staged_content = self.run_main("--no-generate")

        self.assertFalse(any(cmd[0] == "./hack/generate-release-note.sh" for cmd in commands))
        self.assertEqual(len(staged_content), 1)
        self.assertIn(URL_LINE, staged_content[0])

    def test_main_dry_run_does_not_change_existing_note(self) -> None:
        self.write_note()

        _, staged_content = self.run_main("--dry-run")

        self.assertEqual(self.site_file.read_text(encoding="utf-8"), NOTE)
        self.assertEqual(staged_content, [])

    def test_main_rejects_empty_sections_before_normalizing_or_staging(self) -> None:
        original = "---\ntitle: v0.6.0\n---\nFull Changelog: v0.5.0..v0.6.0\n"
        self.write_note(original)

        with self.assertRaisesRegex(doc_pr.CommandError, "lacks release note sections"):
            self.run_main("--no-generate")

        self.assertEqual(self.site_file.read_text(encoding="utf-8"), original)

    @unittest.skipUnless(shutil.which("hugo"), "Hugo is required for permalink integration")
    def test_hugo_overrides_dated_permalink_and_keeps_base_path(self) -> None:
        self.write_note()
        doc_pr.set_release_note_url(self.site_file, TAG)
        for base_url in (
            "https://cloud-provider-azure.sigs.k8s.io/",
            "https://kubernetes-sigs.github.io/cloud-provider-azure/",
        ):
            with self.subTest(base_url=base_url):
                (self.repo / "hugo.toml").write_text(
                    f'baseURL = "{base_url}"\n'
                    'contentDir = "content/en"\n'
                    '[permalinks]\nblog = "/:section/:year/:month/:day/:slug/"\n',
                    encoding="utf-8",
                )
                result = subprocess.run(
                    ["hugo", "list", "all", "--source", str(self.repo)],
                    check=True,
                    text=True,
                    capture_output=True,
                )
                rows = list(csv.DictReader(StringIO(result.stdout)))
                page = next(row for row in rows if row["title"] == TAG)
                self.assertEqual(page["permalink"], base_url + "blog/v0.6.0/")
                self.assertTrue(page["date"].startswith("2020-09-01"))


if __name__ == "__main__":
    unittest.main()
