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
  and records both fixable findings and findings without a `FixedVersion`.
  `plan` preserves findings without a fixed version as residual risks and never
  turns them into apply actions.
- Findings are classified as:
  `GO_MODULE` for `Class="lang-pkgs"` and `Type="gobinary"`, except for the
  `stdlib` and `toolchain` pseudo-packages,
  `GO_TOOLCHAIN` for the `stdlib` and `toolchain` Go runtime pseudo-packages,
  `BASE_IMAGE` for `Class="os-pkgs"`,
  `OTHER` for everything else.
- GO_TOOLCHAIN findings are report-only because they require upgrading the Go
  compiler or pinned builder image and rebuilding the affected image. They are
  never passed to `go mod edit`, `go list -m`, or module verification.
  `apply`, `verify`, and rescan also filter these pseudo-packages defensively
  when reading a plan saved by an older version of the skill.
- OTHER findings with a non-empty `FixedVersion` are unsupported fixable
  findings. They require manual remediation and must not be treated as a clean
  verification result.
- `plan` groups multiple GO_MODULE CVEs by module path, chooses the highest
  `FixedVersion` as the minimum required version, and adds Go's required `v`
  prefix when Trivy reports a digit-leading version. BASE_IMAGE findings are
  grouped into a recommendation that requires an explicit
  `--base-image-target`.
- `apply` stages every unresolved Go module minimum for the same module root in
  one `go mod edit` command, then lets `go mod tidy` resolve the complete module
  graph using Minimal Version Selection. This avoids treating command-line
  versions as conflicting exact `go get` requests. It then mirrors the repo CI
  behavior by discovering all tracked `go.mod` files and running `go mod tidy`
  plus `go mod verify` in every module before `go mod vendor`.
- When vendoring is enabled at the repo root, `apply` also runs
  `hack/update-azure-vendor-licenses.sh` so `LICENSES/` stays in sync with
  `vendor/` and the resulting PR is self-contained. The upstream license
  generator runs `go list -m all`, which can add otherwise-unused checksums to
  the root `go.sum`, so `apply` reruns root `go mod tidy` and `go mod verify`
  after license generation. This keeps the recorded changes clean under the
  repository's `go-mod-consistency` workflow.
- `verify` accepts resolved and vendored Go module versions that are equal to or
  higher than each planned minimum. It does not assume a clean worktree and
  compares the current repo diff against the pre-existing changed files plus
  the files recorded during `apply` so unrelated user edits do not trigger
  false failures.
- `apply` refuses planned Go modules with `replace` directives, and `verify`
  fails resolved or vendored replacements. A replacement module's version does
  not prove that the original module meets its CVE fix threshold; update or
  remove the replacement explicitly before using this workflow.
- The state file is resolved with
  `git rev-parse --git-path fix-image-cves.json` so it stays untracked and each
  linked worktree gets isolated scan, plan, apply, and verification state.
- `verify --rescan` expects the agent to rebuild the image first. The skill
  does not build or push images.
- `clean` removes the state file and cleans up any temporary helper artifacts
  left behind by vendor-license regeneration.
- `apply` and `verify` set `GOTOOLCHAIN=local` for all Go subprocess calls to
  match CI behavior (`actions/setup-go` uses `GOTOOLCHAIN=local`). This
  prevents toolchain auto-download from producing extra `go.sum` entries that
  fail the Go Module Consistency check. The skill requires a locally installed
  Go version that satisfies the repo's `go` directive; `apply` performs a
  preflight check and fails early with a clear message if the local Go is too
  old.
