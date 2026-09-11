---
name: fix-image-cves
description: Scan a built container image with Trivy, classify fixable Go-module and base-image CVEs, apply dependency and Dockerfile fixes, and verify the result with file checks and an optional image rescan. Use when the user wants to fix CVEs in a container image, scan for vulnerabilities, or mentions Trivy, CVE remediation, image security, or dependency vulnerabilities.
---

# Fix Image CVEs

Use `scripts/fix_image_cves.py` for dependency selection, source updates, and
verification. The helper implements the lowest-fixed-version policy and manages
module, vendor, and license updates; it does not build or push images.

Run from the repository root, replacing `<SKILL_DIR>` with this skill directory.
When running elsewhere, add `--repo <worktree>` to each command.

## Workflow

1. Scan the built image, specifying the owning module and runtime Dockerfile,
   then inspect the plan:

   ```bash
   python3 <SKILL_DIR>/scripts/fix_image_cves.py scan <image> \
     --module-root <module-dir> --dockerfile <Dockerfile>
   python3 <SKILL_DIR>/scripts/fix_image_cves.py plan
   ```

2. Review the plan before changing files and choose any required image targets:

   - Select a locally installed Go version satisfying the repository and planned
     Go directives; do not rely on automatic toolchain downloads.
   - For a Go directive bump, update older builders for every affected Dockerfile
     to a stable Go version at least as new as the target. Preserve the builder's
     registry, repository, and OS variant. Include both `Dockerfile:builder` and
     `cloud-node-manager.Dockerfile:builder` when the root module needs new builders.
   - For runtime CVEs, select a fixed base image. Verify target-platform support
     and the actual digest for every image target; these are agent decisions.

3. Preview and apply. Append one
   `--base-image-target <Dockerfile>:<stage>=<image>@sha256:<digest>` per chosen
   target to both commands; use `builder` or `runtime` for the stage. Omit the
   option when no image change is needed.

   ```bash
   python3 <SKILL_DIR>/scripts/fix_image_cves.py apply --dry-run
   python3 <SKILL_DIR>/scripts/fix_image_cves.py apply
   ```

4. Verify the source changes:

   ```bash
   python3 <SKILL_DIR>/scripts/fix_image_cves.py verify
   ```

5. Rebuild the image outside this helper, then rescan that rebuilt image:

   ```bash
   python3 <SKILL_DIR>/scripts/fix_image_cves.py verify \
     --rescan --image <rebuilt-image>
   ```

6. Record results before starting another scan. Clean up after success or an
   intentional workflow reset:

   ```bash
   python3 <SKILL_DIR>/scripts/fix_image_cves.py clean
   ```

## Failures and Reporting

- If the helper stops, preserve its evidence and any partial changes, report the
  cause, and resolve it before retrying. Do not edit saved state to bypass checks.
- Report Go toolchain findings (`stdlib`/`toolchain`) and findings without a fixed
  version as residual risks; the helper does not auto-fix them. Unsupported
  fixable findings require manual remediation and must not be reported as clean.
- Distinguish file checks from rebuilt-image verification. A rescan verifies the
  planned CVEs, not that the entire image is free of vulnerabilities.
