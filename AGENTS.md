# Development guidelines

## Before changing code

- Read [README.md](README.md), the documentation for the affected component, and the current Git diff. Preserve unrelated staged, unstaged and untracked work.
- Keep each change focused on one problem. Prefer clear functions and explicit control flow over new frameworks or speculative abstractions.
- Preserve public API, CLI and Helm compatibility unless the task calls for a behavior change; document intentional changes and migration steps.

## Implementation

- Follow nearby code conventions and run `gofmt` on changed Go files. Keep lifecycle orchestration, test assertions and fixtures separate where that makes the flow easier to read.
- Use the repository's Make targets for Python tooling and tests. Keep Python dependencies in `pyproject.toml` / `uv.lock`, and E2E tool versions and checksums in `tests/e2e/dependencies.json`.
- Keep tests independent of company infrastructure and credentials. Use isolated resources, clean up only resources owned by the test, and do not change global Docker context, default platform, PATH or kubeconfig.
- Never commit credentials or kubeconfig files. Keep temporary files, logs and generated evidence in ignored `artifacts/`.

## Validation

- Run checks relevant to the change before opening a PR; use [CI](.github/workflows/ci.yml) and the [E2E guide](tests/e2e/README.md) as the command references. Documentation-only changes need link and diff checks, not a full E2E run.
- Add regression tests for behavior changes and bug fixes. Assert observable behavior, including relevant failure and cleanup paths, rather than mirroring implementation details.
- For changes affecting the full build or deployment flow, run the relevant E2E scenarios. Tooling unit tests do not establish that the full E2E works. Distinguish emulated Mac results from native Linux results and state any checks that remain incomplete.

## Documentation and review

- Keep README concise; put detailed procedures in the relevant guide and link to them. Update documentation when commands or behavior change.
- Write each prose paragraph or list item's continuous text on one source line; use editor soft wrap instead of fixed-width hard wrap. Preserve code blocks, tables and intentional line breaks. Do not reformat unrelated existing documentation.
- Review the final diff for scope, secrets and generated files. Keep commits focused and describe the problem, resulting behavior, validation and remaining limitations in the PR.
