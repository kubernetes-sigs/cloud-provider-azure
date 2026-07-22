Baseline and final image results:

| Image | Baseline findings | Applied fixes | Final verification | Residual risks |
|---|---|---|---|---|
| CCM | <actionable count with Go-module/base-image split; unsupported-fixable count> | <fixes or None> | <remaining action and unsupported-fixable counts> | <count> |
| CNM | <actionable count with Go-module/base-image split; unsupported-fixable count> | <fixes or None> | <remaining action and unsupported-fixable counts> | <count> |
| health-probe-proxy | <actionable count with Go-module/base-image split; unsupported-fixable count> | <fixes or None> | <remaining action and unsupported-fixable counts> | <count> |

Validation:

- Module consistency: PASS - <result summary>
- Unit tests: PASS - `make test-unit`
- Diff check: PASS - no whitespace errors relative to `<input-branch>`
- Final image verification: PASS - all three images rebuilt and freshly scanned

Residual-risk details:

- CCM: <finding IDs and packages with no fixed version or Go toolchain finding, or None>
- CNM: <finding IDs and packages with no fixed version or Go toolchain finding, or None>
- health-probe-proxy: <finding IDs and packages with no fixed version or Go toolchain finding, or None>
