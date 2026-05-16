# Code Conventions

## General Standards

- Follow the project's existing patterns.
- Match import style and naming conventions.
- Keep each function or class focused on one responsibility.
- Prefer boring, simple solutions over clever ones.
- Avoid speculative abstraction.
- Keep changes small and testable.

## Repository Conventions

- **Constants**: all constants live in `pkg/consts/`; do not scatter magic strings.
- **Azure API calls**: go through `pkg/azclient/` wrappers; never call the Azure SDK directly.
- **Mocks**: use existing `azure_mock_*.go` generated mocks and `azure_fakes.go` manual fakes.
- **Tests**: table-driven tests are preferred; match the style in the file being edited.
- **Repository pattern**: `azure_*_repo.go` files abstract data access; follow this pattern.

## Error Handling

- Return clear errors with useful context.
- Use `fmt.Errorf` with `%w` when wrapping errors.
- Avoid exposing sensitive information in error messages.
- Prefer graceful degradation where the caller can recover.
- Do not log an error immediately before returning it unless the log is intentionally
  diagnostic and follows the logging guidance.
