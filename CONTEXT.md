# Cloud Provider Azure Image CVE Remediation Context

This glossary defines the project-specific language used by default-branch and
release-branch image security automation.

## Language

**CVE verification image**:
A disposable container image built from a supported target branch to detect
fixable vulnerabilities and validate their remediation. It is not published
and is removed after the verification workflow finishes.
_Avoid_: Release image, published image

**CVE remediation target branch**:
The input branch whose images are verified and whose remediation PR receives
the changes. It is exactly `master` or matches
`^release-[0-9]+\.[0-9]+$`.
_Avoid_: Release branch when referring to both supported branch types

**CVE verification set**:
The Linux CCM, CNM, and health-probe-proxy CVE verification images. All three
must be scanned after remediation before verification is complete.
_Avoid_: Representative image, sample image

**CVE verification complete**:
The state in which final scans of the CVE verification set contain neither
actionable nor unsupported non-toolchain fixable vulnerabilities. Findings
without a fixed version and Go toolchain findings remain documented as residual
risks.
_Avoid_: CVE-free, vulnerability-free

**Release artifact**:
A platform-specific binary attached to a GitHub Release. CVE verification
images are not release artifacts.
_Avoid_: Release image
