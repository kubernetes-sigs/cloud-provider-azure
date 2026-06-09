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

"""Fetch Prow e2e pipeline artifacts from GCS.

Accepts a Prow URL (any form), downloads key
artifacts, discovers cluster topology, and outputs a manifest JSON.

Usage:
    python3 fetch_artifacts.py "<PROW_URL>" [--output-dir <dir>]

Examples:
    python3 fetch_artifacts.py "https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/cloud-provider-azure-ccm-windows-capz/2056042249866711040"
"""

import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from html.parser import HTMLParser

GCS_BUCKET = "kubernetes-ci-logs"
GCS_BASE = f"https://storage.googleapis.com/{GCS_BUCKET}"
GCSWEB_BASE = f"https://gcsweb.k8s.io/gcs/{GCS_BUCKET}"
ARTIFACTS_DIR_NAME = "_artifacts"

# Key files to download (relative to the build root).
KEY_ARTIFACTS = [
    "build-log.txt",
    "started.json",
    "finished.json",
    "prowjob.json",
]


def _resolve_directory_url(job_name: str, build_id: str) -> str | None:
    """Resolve a pr-logs/directory/ symlink to the real GCS prefix.

    Prow stores a small text file at
    ``pr-logs/directory/{JOB}/{BUILD}.txt`` whose content is the
    ``gs://`` path to the actual build artifacts.  Fetch that file and
    return the GCS prefix (the part after ``gs://kubernetes-ci-logs/``),
    or *None* if the redirect file cannot be fetched.
    """
    redirect_url = f"{GCS_BASE}/pr-logs/directory/{job_name}/{build_id}.txt"
    body = fetch_text(redirect_url)
    if not body:
        return None
    gs_path = body.strip()
    prefix = f"gs://{GCS_BUCKET}/"
    if gs_path.startswith(prefix):
        return gs_path[len(prefix):]
    return None


def parse_url(url: str) -> tuple[str, str, str]:
    """Extract (job_name, build_id, gcs_path_prefix) from any Prow/GCS URL form.

    For periodic/postsubmit jobs the prefix is 'logs/JOB/BUILD'.
    For presubmit (PR) jobs the prefix is 'pr-logs/pull/ORG_REPO/PR/JOB/BUILD'.
    For directory symlinks the prefix is resolved via a .txt redirect file.
    """
    # PR log patterns (presubmit jobs).
    pr_patterns = [
        r"prow\.k8s\.io/view/gs/kubernetes-ci-logs/(pr-logs/pull/[^/]+/\d+/([^/]+)/(\d+))",
        r"gcsweb\.k8s\.io/gcs/kubernetes-ci-logs/(pr-logs/pull/[^/]+/\d+/([^/]+)/(\d+))",
        r"storage\.googleapis\.com/kubernetes-ci-logs/(pr-logs/pull/[^/]+/\d+/([^/]+)/(\d+))",
    ]
    for pattern in pr_patterns:
        m = re.search(pattern, url)
        if m:
            return m.group(2), m.group(3), m.group(1)

    # Periodic/postsubmit patterns.
    patterns = [
        r"prow\.k8s\.io/view/gs/kubernetes-ci-logs/(logs/([^/]+)/(\d+))",
        r"gcsweb\.k8s\.io/gcs/kubernetes-ci-logs/(logs/([^/]+)/(\d+))",
        r"storage\.googleapis\.com/kubernetes-ci-logs/(logs/([^/]+)/(\d+))",
    ]
    for pattern in patterns:
        m = re.search(pattern, url)
        if m:
            return m.group(2), m.group(3), m.group(1)

    # Directory symlink patterns (pr-logs/directory/JOB/BUILD).
    dir_patterns = [
        r"prow\.k8s\.io/view/gs/kubernetes-ci-logs/pr-logs/directory/([^/]+)/(\d+)",
        r"gcsweb\.k8s\.io/gcs/kubernetes-ci-logs/pr-logs/directory/([^/]+)/(\d+)",
        r"storage\.googleapis\.com/kubernetes-ci-logs/pr-logs/directory/([^/]+)/(\d+)",
    ]
    for pattern in dir_patterns:
        m = re.search(pattern, url)
        if m:
            job_name, build_id = m.group(1), m.group(2)
            resolved = _resolve_directory_url(job_name, build_id)
            if resolved:
                return job_name, build_id, resolved
            raise ValueError(
                f"Could not resolve directory symlink for {job_name}/{build_id}.\n"
                f"The redirect file at pr-logs/directory/{job_name}/{build_id}.txt "
                "was not found or could not be fetched."
            )

    raise ValueError(
        f"Cannot parse Prow URL: {url}\n"
        "Expected one of:\n"
        "  https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/JOB_NAME/BUILD_ID\n"
        "  https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/ORG_REPO/PR/JOB_NAME/BUILD_ID\n"
        "  https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/directory/JOB_NAME/BUILD_ID\n"
        "  https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/...\n"
        "  https://storage.googleapis.com/kubernetes-ci-logs/..."
    )


def gcs_url(gcs_prefix: str, path: str = "") -> str:
    """Build a raw GCS URL for a build artifact."""
    base = f"{GCS_BASE}/{gcs_prefix}"
    if path:
        return f"{base}/{path}"
    return base


def gcsweb_url(gcs_prefix: str, path: str = "") -> str:
    """Build a gcsweb URL for directory listing."""
    base = f"{GCSWEB_BASE}/{gcs_prefix}"
    if path:
        return f"{base}/{path}"
    return f"{base}/"


def download_file(url: str, dest: str) -> None:
    """Download a URL to a local file. Raises on failure."""
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    try:
        req = urllib.request.Request(
            url, headers={"User-Agent": "debug-e2e-pipeline/1.0"}
        )
        with urllib.request.urlopen(req, timeout=60) as resp, open(dest, "wb") as f:
            while True:
                chunk = resp.read(65536)
                if not chunk:
                    break
                f.write(chunk)
    except (urllib.error.HTTPError, urllib.error.URLError, OSError) as e:
        raise RuntimeError(f"Failed to download {url}: {e}") from e


def fetch_text(url: str) -> str | None:
    """Fetch a URL and return its text content, or None on failure."""
    try:
        req = urllib.request.Request(
            url, headers={"User-Agent": "debug-e2e-pipeline/1.0"}
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.read().decode("utf-8", errors="replace")
    except (urllib.error.HTTPError, urllib.error.URLError, OSError):
        return None


class GCSWebLinkParser(HTMLParser):
    """Parse gcsweb HTML directory listings to extract child paths."""

    def __init__(self):
        super().__init__()
        self.links: list[str] = []

    def handle_starttag(self, tag, attrs):
        if tag == "a":
            for name, value in attrs:
                if name == "href" and value:
                    self.links.append(value)


def list_gcs_directory(gcs_path_prefix: str, prefix: str) -> list[str]:
    """List entries in a GCS directory via gcsweb HTML scraping.

    Returns a list of relative names (files and dirs with trailing /).
    Handles both /gcs/ relative links (directories) and full
    storage.googleapis.com links (files).
    """
    url = gcsweb_url(gcs_path_prefix, prefix)
    html = fetch_text(url)
    if not html:
        return []

    parser = GCSWebLinkParser()
    parser.feed(html)

    entries = []
    # gcsweb uses two link formats:
    #   directories: /gcs/{bucket}/{gcs_path_prefix}/{prefix}{child}/
    #   files: https://storage.googleapis.com/{bucket}/{gcs_path_prefix}/{prefix}{child}
    gcs_link_prefix = f"/gcs/{GCS_BUCKET}/{gcs_path_prefix}/{prefix}"
    storage_prefix = (
        f"https://storage.googleapis.com/{GCS_BUCKET}/{gcs_path_prefix}/{prefix}"
    )

    for link in parser.links:
        relative = ""
        if link.startswith(gcs_link_prefix) and link != gcs_link_prefix:
            relative = link[len(gcs_link_prefix) :]
        elif link.startswith(storage_prefix) and link != storage_prefix:
            relative = link[len(storage_prefix) :]
        else:
            continue

        # Strip query params.
        if "?" in relative:
            relative = relative.split("?")[0]
        if relative and relative != "../":
            entries.append(relative)
    return entries


def discover_clusters(gcs_prefix: str) -> list[dict]:
    """Discover cluster names and machine names from the artifacts directory."""
    clusters = []
    cluster_entries = list_gcs_directory(gcs_prefix, "artifacts/clusters/")
    for cluster_dir in cluster_entries:
        cluster_name = cluster_dir.rstrip("/")
        if not cluster_name:
            continue
        machines = []
        machine_entries = list_gcs_directory(
            gcs_prefix, f"artifacts/clusters/{cluster_name}/machines/"
        )
        for machine_dir in machine_entries:
            machine_name = machine_dir.rstrip("/")
            if machine_name:
                machines.append(machine_name)
        clusters.append({"name": cluster_name, "machines": machines})
    return clusters


def list_available_artifacts(
    gcs_prefix: str,
    max_depth: int = 6,
) -> list[str]:
    """Recursively list all available artifact paths in GCS.

    Walks the artifacts/ directory tree so the agent can see everything
    that's available to fetch on demand (kubelet.log, CCM logs,
    kube-system pod logs, activity logs, etc.).
    """
    available: list[str] = []

    def _walk(prefix: str, depth: int) -> None:
        if depth > max_depth:
            return
        entries = list_gcs_directory(gcs_prefix, prefix)
        for entry in entries:
            full = f"{prefix}{entry}"
            if entry.endswith("/"):
                _walk(full, depth + 1)
            else:
                available.append(full)

    _walk("artifacts/", 0)
    return available


def discover_junit_files(gcs_prefix: str) -> list[str]:
    """Find JUnit XML files in the artifacts directory.

    Uses both directory listing and probing common filenames since gcsweb
    HTML listings may not show files alongside directories.
    """
    junit_files = []

    # Try directory listing first.
    entries = list_gcs_directory(gcs_prefix, "artifacts/")
    for entry in entries:
        name = entry.rstrip("/")
        if name.startswith("junit") and name.endswith(".xml"):
            junit_files.append(f"artifacts/{name}")

    # Probe common JUnit filenames if listing didn't find any.
    if not junit_files:
        for i in range(1, 6):  # junit_01.xml through junit_05.xml
            path = f"artifacts/junit_{i:02d}.xml"
            url = gcs_url(gcs_prefix, path)
            try:
                req = urllib.request.Request(
                    url, method="HEAD", headers={"User-Agent": "debug-e2e-pipeline/1.0"}
                )
                with urllib.request.urlopen(req, timeout=10):
                    junit_files.append(path)
            except (urllib.error.HTTPError, urllib.error.URLError, OSError):
                break  # Stop probing after first miss.

    return junit_files


def load_job_metadata(output_dir: str) -> dict:
    """Load started.json and finished.json to extract job metadata."""
    meta: dict = {}
    started_path = os.path.join(output_dir, "started.json")
    if os.path.exists(started_path):
        with open(started_path) as f:
            meta["started"] = json.load(f)

    finished_path = os.path.join(output_dir, "finished.json")
    if os.path.exists(finished_path):
        with open(finished_path) as f:
            meta["finished"] = json.load(f)

    prowjob_path = os.path.join(output_dir, "prowjob.json")
    if os.path.exists(prowjob_path):
        with open(prowjob_path) as f:
            meta["prowjob"] = json.load(f)

    return meta


def _find_repo_root() -> str | None:
    """Find the git repository root, or None if not in a repo."""
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        pass
    return None


def _parse_junit(junit_paths: list[str]) -> dict:
    """Parse JUnit XML files and extract test results.

    Returns a dict with:
      test_counts: {total, passed, failed, skipped}
      tests: {passed: [name, ...], failed: [{name, duration, message}], skipped: [name, ...]}
    """
    passed: list[str] = []
    failed: list[dict] = []
    skipped: list[str] = []
    total_from_suite = 0
    disabled_from_suite = 0

    for path in junit_paths:
        try:
            tree = ET.parse(path)
            root = tree.getroot()
        except (ET.ParseError, OSError) as e:
            print(f"  Warning: failed to parse JUnit {path}: {e}", file=sys.stderr)
            continue

        # Collect suite-level counts.
        for ts in root.iter("testsuite"):
            try:
                total_from_suite += int(ts.get("tests", 0))
                disabled_from_suite += int(ts.get("disabled", 0))
            except ValueError:
                pass

        for tc in root.iter("testcase"):
            name = tc.get("name", "unknown")
            failure = tc.find("failure")
            error = tc.find("error")
            skip = tc.find("skipped")

            if failure is not None or error is not None:
                elem = failure if failure is not None else error
                failed.append(
                    {
                        "name": name,
                        "duration": tc.get("time", ""),
                        "message": (elem.get("message", "") or "")[:500],
                    }
                )
            elif skip is not None:
                skipped.append(name)
            else:
                passed.append(name)

    total = len(passed) + len(failed) + len(skipped)
    # If the suite reports more disabled than we found as skipped, trust the suite.
    if disabled_from_suite > len(skipped):
        skip_count = disabled_from_suite
    else:
        skip_count = len(skipped)
    if total_from_suite > total:
        total = total_from_suite

    return {
        "test_counts": {
            "total": total,
            "passed": len(passed),
            "failed": len(failed),
            "skipped": skip_count,
        },
        "tests": {
            "passed": sorted(passed),
            "failed": failed,
            "skipped": sorted(skipped),
        },
    }


def _extract_build_log_summary(build_log_path: str | None) -> dict:
    """Extract factual information from the build log.

    Extracts:
      ginkgo_summary: raw Ginkgo result line + parsed counts
      ginkgo_result: "Test Suite Failed" / "Test Suite Passed" / null
      node_info: raw 'kubectl get nodes -o wide' table block
    """
    summary: dict = {
        "ginkgo_summary": None,
        "ginkgo_result": None,
        "node_info": None,
    }
    if not build_log_path or not os.path.exists(build_log_path):
        return summary

    try:
        with open(build_log_path, "r", errors="replace") as f:
            lines = f.readlines()
    except OSError:
        return summary

    # Strip ANSI escape codes for matching.
    ansi_re = re.compile(r"\x1b\[[0-9;]*m")

    # Extract Ginkgo summary: "Ran N of M Specs in T seconds"
    # and "FAIL! -- X Passed | Y Failed | Z Pending | W Skipped"
    # or  "SUCCESS! -- X Passed | Y Failed | ..."
    for line in lines:
        stripped = ansi_re.sub("", line).strip()
        if re.match(r"Ran \d+ of \d+ Specs in", stripped):
            summary["ginkgo_summary"] = stripped
        if re.match(r"(FAIL!|SUCCESS!) -- \d+ Passed", stripped):
            summary["ginkgo_summary"] = stripped

    # Extract Ginkgo result.
    for line in lines:
        stripped = ansi_re.sub("", line).strip()
        if stripped == "Test Suite Failed":
            summary["ginkgo_result"] = "Test Suite Failed"
        elif stripped == "Test Suite Passed":
            summary["ginkgo_result"] = "Test Suite Passed"

    # Extract node table: find the "NAME  STATUS  ROLES" header line
    # and capture until the next blank line or non-table line.
    node_lines: list[str] = []
    capturing = False
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("NAME") and "STATUS" in stripped and "ROLES" in stripped:
            capturing = True
            node_lines = [stripped]
            continue
        if capturing:
            # Node table rows contain Ready or NotReady as the status column.
            if stripped and ("Ready" in stripped or "NotReady" in stripped):
                node_lines.append(stripped)
            else:
                capturing = False
    if node_lines:
        summary["node_info"] = "\n".join(node_lines)

    return summary


def _extract_prowjob_summary(job_meta: dict) -> dict:
    """Extract a concise summary from prowjob.json for quick reference.

    Pulls out the entrypoint script, cloned repos, env vars, and labels
    so the agent can immediately see what the job does without parsing
    the full prowjob JSON.
    """
    summary: dict = {}

    # Repos from started.json.
    started = job_meta.get("started", {})
    summary["repos"] = started.get("repos", {})
    summary["repo_commit"] = started.get("repo-commit", "")

    # Timing.
    summary["start_time"] = started.get("timestamp", "")
    finished = job_meta.get("finished", {})
    summary["finish_time"] = finished.get("timestamp", "")
    if summary["start_time"] and summary["finish_time"]:
        try:
            duration_s = int(summary["finish_time"]) - int(summary["start_time"])
            mins, secs = divmod(duration_s, 60)
            summary["duration"] = f"{mins}m{secs}s"
        except (ValueError, TypeError):
            pass

    # Extract from prowjob spec.
    prowjob = job_meta.get("prowjob", {})
    spec = prowjob.get("spec", {})

    # Entrypoint command (the script the job runs).
    containers = spec.get("pod_spec", {}).get("containers", [])
    if not containers:
        containers = spec.get("containers", [])
    if containers:
        c = containers[0]
        summary["command"] = c.get("command", [])
        summary["args"] = c.get("args", [])
        # Extract env vars (skip secrets).
        env_vars = {}
        for e in c.get("env", []):
            name = e.get("name", "")
            value = e.get("value", "")
            if value and "secret" not in name.lower():
                env_vars[name] = value
        summary["env"] = env_vars

    # Extra refs (additional repos cloned).
    extra_refs = spec.get("extra_refs", [])
    summary["extra_refs"] = [
        {
            "org": r.get("org", ""),
            "repo": r.get("repo", ""),
            "base_ref": r.get("base_ref", ""),
        }
        for r in extra_refs
    ]

    # Labels (contain preset info).
    labels = prowjob.get("metadata", {}).get("labels", {})
    preset_labels = {k: v for k, v in labels.items() if k.startswith("preset-")}
    summary["presets"] = preset_labels

    return summary


def main():
    parser = argparse.ArgumentParser(
        description="Fetch Prow e2e pipeline artifacts from GCS."
    )
    parser.add_argument(
        "url",
        help="Prow URL (any form: prow.k8s.io, gcsweb, or storage.googleapis.com)",
    )
    parser.add_argument(
        "--output-dir",
        default=None,
        help="Directory for downloaded artifacts (default: temp dir)",
    )
    parser.add_argument(
        "--fetch",
        nargs="+",
        metavar="PATTERN",
        help="Download artifacts matching glob pattern(s) from available_artifacts "
        '(e.g. "*/controllers/*/manager.log" "*/MachinePool/*.yaml")',
    )
    args = parser.parse_args()

    # Parse URL.
    try:
        job_name, build_id, gcs_prefix = parse_url(args.url)
    except ValueError as e:
        print(str(e), file=sys.stderr)
        sys.exit(1)

    # Set up output directory.
    if args.output_dir:
        output_dir = os.path.abspath(args.output_dir)
    else:
        repo_root = _find_repo_root() or os.getcwd()
        output_dir = os.path.join(
            repo_root, ARTIFACTS_DIR_NAME, "e2e-debug", job_name, build_id
        )
    os.makedirs(output_dir, exist_ok=True)

    print(f"Job: {job_name}", file=sys.stderr)
    print(f"Build: {build_id}", file=sys.stderr)
    print(f"GCS prefix: {gcs_prefix}", file=sys.stderr)
    print(f"Output: {output_dir}", file=sys.stderr)
    print(f"GCS: {gcs_url(gcs_prefix)}", file=sys.stderr)
    print(file=sys.stderr)

    # If --fetch is used and manifest already exists, reuse it for the
    # available_artifacts list and only download the matched artifacts.
    manifest_path = os.path.join(output_dir, "manifest.json")
    if args.fetch and os.path.exists(manifest_path):
        with open(manifest_path) as f:
            existing_manifest = json.load(f)
        available_artifacts = existing_manifest.get("available_artifacts", [])
        matched = []
        for art in available_artifacts:
            if any(fnmatch.fnmatch(art, pat) for pat in args.fetch):
                matched.append(art)
        print(
            f"  {len(matched)} artifact(s) matched from existing manifest",
            file=sys.stderr,
        )
        fetch_count = 0
        for art in matched:
            dest = os.path.join(output_dir, art)
            url = gcs_url(gcs_prefix, art)
            print(f"  Downloading {art}...", file=sys.stderr)
            download_file(url, dest)
            fetch_count += 1
        print(file=sys.stderr)
        print(
            f"Done. Downloaded {fetch_count} artifact(s) to {output_dir}",
            file=sys.stderr,
        )
        return

    # Download key artifacts.
    downloaded_files: dict[str, str] = {}
    for artifact in KEY_ARTIFACTS:
        dest = os.path.join(output_dir, artifact)
        url = gcs_url(gcs_prefix, artifact)
        print(f"  Downloading {artifact}...", file=sys.stderr)
        download_file(url, dest)
        downloaded_files[artifact] = dest

    # Discover and download JUnit files.
    print("  Discovering JUnit files...", file=sys.stderr)
    junit_paths = discover_junit_files(gcs_prefix)
    junit_local: list[str] = []
    for junit_path in junit_paths:
        dest = os.path.join(output_dir, junit_path)
        url = gcs_url(gcs_prefix, junit_path)
        print(f"  Downloading {junit_path}...", file=sys.stderr)
        download_file(url, dest)
        junit_local.append(dest)

    # Discover cluster topology.
    print("  Discovering cluster topology...", file=sys.stderr)
    clusters = discover_clusters(gcs_prefix)

    # List all available artifacts (table of contents for on-demand fetch).
    print("  Listing available artifacts...", file=sys.stderr)
    available_artifacts = list_available_artifacts(gcs_prefix)
    print(f"  Found {len(available_artifacts)} available artifact(s)", file=sys.stderr)

    # Load job metadata.
    job_meta = load_job_metadata(output_dir)

    # Build job status summary.
    status = "UNKNOWN"
    if "finished" in job_meta:
        result = job_meta["finished"].get("result", "UNKNOWN")
        status = result

    # Extract prowjob summary for quick reference.
    job_summary = _extract_prowjob_summary(job_meta)

    # Parse JUnit results.
    print("  Parsing JUnit results...", file=sys.stderr)
    junit_results = _parse_junit(junit_local)

    # Extract build log summary.
    print("  Extracting build log summary...", file=sys.stderr)
    build_log_summary = _extract_build_log_summary(
        downloaded_files.get("build-log.txt")
    )

    # Build manifest.
    manifest = {
        "job_name": job_name,
        "build_id": build_id,
        "status": status,
        "gcs_base_url": gcs_url(gcs_prefix),
        "prow_url": f"https://prow.k8s.io/view/gs/{GCS_BUCKET}/{gcs_prefix}",
        "output_dir": output_dir,
        "job_summary": job_summary,
        "test_counts": junit_results["test_counts"],
        "tests": junit_results["tests"],
        "build_log_summary": build_log_summary,
        "files": downloaded_files,
        "junit_files": junit_local,
        "clusters": clusters,
        "available_artifacts": available_artifacts,
    }

    # Write full manifest to disk.
    manifest_path = os.path.join(output_dir, "manifest.json")
    with open(manifest_path, "w") as f:
        json.dump(manifest, f, indent=2)

    # Print concise summary to stdout (not the full manifest).
    summary_output = {
        "job_name": job_name,
        "build_id": build_id,
        "status": status,
        "duration": job_summary.get("duration", ""),
        "prow_url": manifest["prow_url"],
        "output_dir": output_dir,
        "test_counts": junit_results["test_counts"],
        "failed_tests": junit_results["tests"]["failed"],
        "build_log_summary": build_log_summary,
        "manifest_path": manifest_path,
    }
    print(json.dumps(summary_output, indent=2))

    # Download additional artifacts matching --fetch patterns.
    fetch_count = 0
    if args.fetch and available_artifacts:
        matched = []
        for art in available_artifacts:
            if any(fnmatch.fnmatch(art, pat) for pat in args.fetch):
                matched.append(art)
        print(
            f"  {len(matched)} artifact(s) matched --fetch pattern(s)",
            file=sys.stderr,
        )
        for art in matched:
            dest = os.path.join(output_dir, art)
            url = gcs_url(gcs_prefix, art)
            print(f"  Downloading {art}...", file=sys.stderr)
            download_file(url, dest)
            fetch_count += 1

    file_count = sum(1 for v in downloaded_files.values()) + len(junit_local)
    print(file=sys.stderr)
    parts = [f"{file_count} artifact(s)"]
    if fetch_count:
        parts.append(f"{fetch_count} fetched artifact(s)")
    print(
        f"Done. Downloaded {' + '.join(parts)} to {output_dir}",
        file=sys.stderr,
    )


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
