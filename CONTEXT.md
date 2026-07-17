# Cloud Provider Azure Release Context

This glossary defines the project-specific language used by release and
pre-release security automation.

## Language

**CVE verification image**:
A disposable container image built to detect fixable vulnerabilities and
validate their remediation before a release. It is not published and is
removed after the verification workflow finishes.
_Avoid_: Release image, published image

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
