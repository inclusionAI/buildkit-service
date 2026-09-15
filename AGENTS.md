# Development guidelines

## Start here

Read the current diff and the relevant guide; preserve existing work.

- [README](README.md): project overview and build commands.
- [Local development](deploy/local/README.md): source rebuilds, environment lifecycle and recovery.
- [E2E guide](tests/e2e/README.md) and [CI](.github/workflows/ci.yml): validation commands and coverage.

## Changes

- Keep changes focused, follow nearby conventions and preserve compatibility unless the task requires otherwise. Prefer clear functions over new frameworks.
- Use Make entry points. Keep static local configuration in `deploy/local/`, Python dependencies in `pyproject.toml` / `uv.lock`, and tool versions/checksums in `tests/e2e/dependencies.json`.
- Operate only on the intended environment; preserve its data during routine recovery and do not change global Docker or Kubernetes settings. Keep credentials and generated evidence out of Git, under ignored `artifacts/`.

## Verification and handoff

- Run checks appropriate to the change and add regression tests for meaningful failure cases. Verify deployed behavior for build/deployment changes; distinguish emulated amd64 from native results.
- Review the final diff, update affected documentation, and report results and remaining limitations. Keep Markdown paragraphs on single source lines and avoid unrelated formatting changes.
