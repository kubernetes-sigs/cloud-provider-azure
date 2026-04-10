---
name: fix-image-cves
description: Scan a built container image with Trivy, classify fixable Go-module and base-image CVEs, apply dependency and Dockerfile fixes, and verify the result with file checks and an optional image rescan. Use when the user wants to fix CVEs in a container image, scan for vulnerabilities, or mentions Trivy, CVE remediation, image security, or dependency vulnerabilities.
---

# Fix Image CVEs

## When To Use

Use this skill when you need to scan a built container image with Trivy,
identify fixable CVEs from embedded Go modules or the runtime base image, apply
the source changes in this repo, and verify that the planned fixes landed.

## Workflow

Replace `<SKILL_DIR>` with the path to this skill directory.

1. Scan the built image first. Pass the module root that owns the binary and
   the Dockerfile that owns the runtime `FROM` line:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py scan \
  <image> \
  --module-root <module-dir> \
  --dockerfile <Dockerfile>
```

2. Build the actionable plan from the saved scan state:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py plan
```

3. Review the plan before changing files. When base-image CVEs are in scope,
   decide the deterministic replacement image and digest yourself. Then apply
   the plan:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py apply \
  --base-image-target <image>@sha256:<digest>
```

Preview the apply step without changing files:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py apply --dry-run
```

For multiple Dockerfiles or stages, key each target explicitly:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py apply \
  --base-image-target Dockerfile:runtime=<image>@sha256:<digest>
```

4. Verify the file-level changes. The script checks the resolved Go module
   versions, `vendor/modules.txt`, Dockerfile `FROM` lines, multi-module
   `go mod verify`, and a diff scoped to the files recorded during `apply`:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py verify
```

5. After rebuilding the image outside the skill, verify the image-level result
   with a Trivy rescan:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py verify \
  --rescan \
  --image <rebuilt-image>
```

6. Remove the state file and any temporary artifacts once the task is done or
   if you want to reset the workflow:

```bash
python3 <SKILL_DIR>/scripts/fix_image_cves.py clean
```

## Notes

- The workflow is intentionally split into `scan`, `plan`, `apply`, `verify`,
  and `clean` so the agent can inspect the planned dependency and base-image
  changes before mutating files.
- `scan` runs `trivy image --detection-priority comprehensive --format json`
  and only records findings that include a non-empty `FixedVersion`.
- Findings are classified as:
  `GO_MODULE` for `Class="lang-pkgs"` and `Type="gobinary"`,
  `BASE_IMAGE` for `Class="os-pkgs"`,
  `OTHER` for everything else.
- `plan` groups multiple GO_MODULE CVEs by module path and chooses the highest
  `FixedVersion` across them. BASE_IMAGE findings are grouped into a
  recommendation that requires an explicit `--base-image-target`.
- `apply` updates Go modules in the selected module root, then mirrors the repo
  CI behavior by discovering all tracked `go.mod` files and running
  `go mod tidy` plus `go mod verify` in every module before `go mod vendor`.
- When vendoring is enabled at the repo root, `apply` also runs
  `hack/update-azure-vendor-licenses.sh` so `LICENSES/` stays in sync with
  `vendor/` and the resulting PR is self-contained.
- `verify` does not assume a clean worktree. It compares the current repo diff
  against the pre-existing changed files plus the files recorded during `apply`
  so unrelated user edits do not trigger false failures.
- The state file lives under `.git/fix-image-cves.json` intentionally so it
  stays untracked.
- `verify --rescan` expects the agent to rebuild the image first. The skill
  does not build or push images.
- `clean` removes the state file and cleans up any temporary helper artifacts
  left behind by vendor-license regeneration.
