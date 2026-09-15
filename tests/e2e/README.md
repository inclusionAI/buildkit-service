# Local kind E2E

This suite builds the service, apt-cacher-ng and Git cache from the current checkout, installs the product Helm chart into an isolated kind cluster, and uses controlled package/Git upstreams and a local registry. No company account, cloud registry credential or production cluster is used.

## Setup and entry points

Requirements: Docker with Linux containers, Git, Make and uv. Run `make setup` to prepare the repository-pinned Python environment. Allocate at least 4 CPUs, 6 GiB RAM and 10 GiB free disk; initial downloads and builds can need more disk. These are preflight minimums; peak resource use has not been benchmarked.

`make setup` creates the repository's `.venv` from `uv.lock`. The default interpreter is Python 3.12, selected by `.python-version`; uv can download it when needed. No manual virtual environment activation is required. Python scripts currently use only the standard library; add future Python tooling dependencies to `pyproject.toml` and commit the updated `uv.lock`.

All E2E Make targets use `uv run --locked`; local and CI callers should use the same targets. `make setup` prepares Python only, while `make e2e-setup` prepares kind, kubectl and Helm. Go build and test commands remain independent of Python setup. Both `.venv/` and `artifacts/` are excluded from Git and Docker build contexts.

```sh
make setup
make e2e-setup
make e2e
```

`make e2e-setup` invokes `scripts/setup-e2e.py`. That script only downloads, checks and caches kind, kubectl and Helm. Versions, download URLs and SHA256 checksums live in `tests/e2e/dependencies.json`; binaries and downloaded archives live in ignored `artifacts/e2e-tools/`. Repeating setup verifies and reuses the cache under an exclusive lock; cached symlinks are replaced without modifying their targets. It does not install system packages or change the system PATH.

`make e2e` also calls setup. The runner requires the cached tools for its own subprocesses (missing tools report `make e2e-setup`, without falling back to system versions), records the selected Docker endpoint and uses a private, anonymous Docker configuration. It never switches the global Docker context or kubeconfig. Every run owns its cluster, registry container and image tags; its kubeconfig, logs and state are under `artifacts/e2e/<run-id>/`.

For debugging, choose a run directory explicitly:

```sh
make e2e-build E2E_RUN_DIR=artifacts/e2e/debug
make e2e-up E2E_RUN_DIR=artifacts/e2e/debug
make e2e-test E2E_RUN_DIR=artifacts/e2e/debug
make e2e-logs E2E_RUN_DIR=artifacts/e2e/debug
make e2e-down E2E_RUN_DIR=artifacts/e2e/debug
```

The full entry point exports logs and cleans its resources on success or failure. `KEEP_CLUSTER=1 make e2e` retains resources for diagnosis and prints the cleanup command. Individual lifecycle commands retain resources for inspection; run `e2e-logs` and `e2e-down` after a failed manual step. Cleanup is idempotent, checks ownership, and preserves shared Docker networks and dependency layers.

## Copyable diagnostics

Run the following commands from the repository root. Replace the example directory with the `Run:` path printed by the runner. Read saved state and logs first; live queries only work while the cluster is retained. Failure messages print commands with the actual run directory, cached kubectl and explicit kubeconfig; generated Make commands also include the repository directory so they work from another working directory.

```sh
E2E_DIR=artifacts/e2e/debug
cat "$E2E_DIR/state.json"
ls "$E2E_DIR"/*.log

# Query only this run's cluster, with a bounded API request timeout.
artifacts/e2e-tools/bin/kubectl --kubeconfig "$E2E_DIR/kubeconfig" --request-timeout=10s get nodes -o wide
artifacts/e2e-tools/bin/kubectl --kubeconfig "$E2E_DIR/kubeconfig" --request-timeout=10s get pods -A -o wide
artifacts/e2e-tools/bin/kubectl --kubeconfig "$E2E_DIR/kubeconfig" --request-timeout=10s -n e2e get events --sort-by=.lastTimestamp

# Export diagnostics before removing this run's resources.
make e2e-logs E2E_RUN_DIR="$E2E_DIR"
make e2e-down E2E_RUN_DIR="$E2E_DIR"
```

| Failure stage | Start with | Next step |
| --- | --- | --- |
| `build.*` | The named `pull-*.log` or `build-*.log` and matching `*.stderr.log` | Correct the reported download/build issue, then retry `make e2e-build E2E_RUN_DIR="$E2E_DIR"` if the run has not been cleaned. |
| `up.kind` | `kind-create.log`, `kind-create.stderr.log`, then exported `kind-logs/` | Inspect the kubelet/containerd errors even if the API never became available. Export logs, clean up, and start a new run. |
| `up.images` / `up.helm` | `kind-load.*log`, `helm-install.*log`, Pod status and events | Inspect image loading, scheduling and container startup failures before cleanup. |
| `test.*` | The reported command log, `package-tests*`, `build-round-*` and Pod logs | Identify the failed assertion; use a new run for another complete scenario pass because caches and result databases are stateful. |
| `down` | `cleanup_errors` in `state.json` and the reported command log | Resolve the reported error and retry `e2e-down`. Ownership mismatches require inspection; do not bypass them with a global prune. |

The full `make e2e` command normally cleans up after exporting diagnostics. To retain a new run for live inspection, start it with `KEEP_CLUSTER=1 make e2e`; clean it explicitly when finished. Do not run lifecycle mutations concurrently against the same run directory.

Keep one-off investigation commands and their results in that run's ignored artifacts directory, for example `investigation.md`. Record the failing stage, exact command, exit status and conclusion without credentials. Promote reusable procedures into this guide and required test steps into the Python runner/scenarios. Static container inputs belong in `fixtures/`; add a fixture Shell script only when a substantial fixed container-side procedure benefits from reuse. Make remains the lifecycle command entry point.

## Code map and lifecycle

- `run.py`: preflight, image builds, registry, kind, image loading, DNS, fixtures, Helm, diagnostics and ownership-checked cleanup.
- `scenarios.py`: first build/push/pull, a distinct second build with RUN cache assertions, worker recovery, mirror recovery and deployed image identity.
- `verify.py`: package/Git/registry cache assertions, executed in the client Pod.
- `fixture.py`: controlled upstream HTTP service and request counters.
- `fixtures/`: static Kubernetes/kind JSON and Dockerfiles. The runner fills image references, registry address and per-round Pod names directly.
- `values.json`: the small Helm deployment; `dependencies.json`: the only source of tool versions/checksums and image digests.
- `../../scripts/setup-e2e.py`: download, verify and cache tools only.

`up()` shows the startup order directly: registry → kind → containerd registry routing → image loading → DNS → upstream fixtures → Helm. `Scenarios.verify()` shows the assertions in execution order. Neither uses a plugin or template framework.

`state.json` records the last completed `phase`, current `action`/`stage`, `status`, and any failure. Phases advance through `preflight → built → up → tested`; successful cleanup ends at `cleaned` and preserves the test result.

| Action | Starting state | Repeat/failure behavior |
| --- | --- | --- |
| `e2e` | New directory | Always export logs, then clean up unless `KEEP_CLUSTER=1`. |
| `e2e-build` | New, preflight or built | A failed or interrupted build can be retried in place; failed preflight or cleanup requires resolving the reported error first. |
| `e2e-up` | Successful build | Partial startup requires logs, cleanup and a new run. |
| `e2e-test` | Fresh successful startup | Run once: cache counters and result databases are stateful. Use a fresh run to repeat. |
| `e2e-logs` | Any initialized run | Continue collecting after individual errors; incomplete required diagnostics fail the command without skipping full-run cleanup. |
| `e2e-down` | Any initialized run | Retry failed cleanup; a cleaned run is a no-op. |

Each run snapshots `dependencies.json`; build, startup and test commands reject dependency drift, while logs and cleanup remain available. Make passes `E2E_RUN_DIR` through the environment to preserve literal path characters.

An interrupted action remains `running` until cleanup, except that image builds can be retried. Lifecycle failures report the stage, artifact directory and applicable Make commands. Commands save stdout in `*.log` and stderr in `*.stderr.log`, so diagnostic text cannot corrupt JSON parsing. Timeouts and interrupts terminate the command’s own process group before cleanup. `failure.json` records the first failure, including preflight errors; `diagnostic_errors` and `cleanup_errors` record secondary failures. The original failure remains the reported error if log export or cleanup also fails. Image tags are removed only when their recorded image ID (or run label for an interrupted build) still matches; shared networks and downloaded layers remain.

## Platform and dependency contract

The current profile explicitly targets **linux/amd64** with the production Nydus-enabled BuildKit base. On Apple Silicon / OrbStack this is **emulated**; on Linux amd64 CI it is native. The runner records both daemon and target architectures. kind nodes and their containerd runtime use the Docker daemon's native architecture; service, mirror, client and build images remain amd64. The platform environment is scoped to child processes. Native arm64 OCI compatibility is a separate future profile, not a prerequisite.

Image dependencies are pinned by platform manifest digest in `dependencies.json`, with upstream source and license references. Product Dockerfiles accept pinned base-image overrides without changing their default production base. Debian and Alpine package repositories and Go downloads are used during image preparation; package versions and source-file hashes are recorded. Pinning an image alone does not freeze packages installed later from a distribution repository.

If your network requires a proxy during image preparation, set `E2E_BUILD_PROXY` to an HTTP proxy reachable from Docker build containers. This optional transport is scoped to image builds; it is not a test dependency.

Core test traffic stays within the test environment. Synthetic `.deb` / `.apk` bodies exercise HTTP caching, not package installation. Python wheels, npm metadata/tarballs, Git repositories and registry layers are controlled fixtures.

## Assertions

- Source-built service images deploy through the product chart.
- Debian/Alpine metadata refreshes; package payloads reuse upstream cache entries.
- pip/npm/Git and registry layer checks use structured upstream request counters.
- The source-built `buildctl-batch` submits real builds and exports successful results. Each round uses a distinct tag/result database to prevent task skips.
- Build steps access the mirror through the BuildKit CNI network.
- The repeated build reuses RUN cache; fresh Pods pull and execute the output.
- Before cache assertions, the client waits for mirror Service TCP routes with a bounded deadline, without fetching cacheable data. Pod Ready and Service routing converge asynchronously after replacement.
- A replaced worker resumes builds; a replaced main mirror refetches cold cache entries and serves builds again. Default emptyDir storage does not preserve deleted Pod caches.

HTTP API, the shell wrapper, Nydus snapshotter execution, PVC persistence and production-scale performance are not implied by these checks. The existing `chart/test-package-mirror-remap.py` remains a smaller Docker regression with a separate responsibility.

Run `make e2e-unit` for fast ownership/cleanup checks. Core E2E results should be reported separately for Mac emulation and native Linux execution.

## Compatibility evidence and current validation status

The fully emulated amd64 kind/node attempt on Apple Silicon reached containerd but failed to create Kubernetes control-plane sandboxes with `seccomp is not supported`. The control-plane Pods explicitly requested `RuntimeDefault`. The suite therefore keeps kind/containerd native while retaining production Nydus BuildKit and amd64 workload images. This does not validate native arm64 OCI workloads or seccomp under amd64 emulation.

On 2026-09-15, the native-node / amd64-workload full entry passed on Apple Silicon / OrbStack, including build, push, pull, cache assertions, worker/mirror recovery, log export and cleanup. All 26 tooling, lifecycle and failure-path unit tests passed. Linux amd64 CI is configured to run the same `make e2e` entry natively, but its execution remains unverified; Mac emulation is not native Linux evidence.
