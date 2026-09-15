# Local source development

Run the current source in a dedicated kind cluster with BuildKit, package mirrors and a persistent registry. Workloads use the production Nydus BuildKit base and `linux/amd64`: emulated on Apple Silicon, native on Linux amd64. No cloud account or E2E fixture is required.

## Prepare and develop

Requirements: Make, uv, Linux Docker with buildx and amd64 execution support, at least 6 GiB RAM, free disk space and port `127.0.0.1:15000`. Initial builds and public package mirrors require internet access.

Run from the repository root:

```sh
make dev-setup    # cache the pinned kind, kubectl and Helm tools
make dev-up       # build missing images, deploy and wait for readiness
# Edit product source, then:
make dev-rebuild  # rebuild source images and update deployments
make dev-status
make dev-logs
make dev-down    # stop applications, retain data
make dev-up      # resume
```

Use `dev-rebuild` after source or Dockerfile changes; `dev-up` reuses existing images and applies current configuration. Rebuilds briefly interrupt services. Verify the changed behavior after deployment; Ready Pods alone do not prove a functional fix. Run `make dev-unit` for runner regression tests and the relevant checks from [CI](../../.github/workflows/ci.yml). Use the separate [E2E suite](../../tests/e2e/README.md) for controlled build/cache/recovery assertions. Agent working rules are in [AGENTS.md](../../AGENTS.md).

Static configuration lives here; orchestration is in `scripts/dev.py`. Runtime files stay in ignored `artifacts/dev/`. Tool setup reuses `scripts/setup-e2e.py` and `tests/e2e/dependencies.json`. Commands do not change global PATH, Docker context, platform or kubeconfig. If required, `DEV_BUILD_PROXY` supplies a container-reachable proxy for image builds only.

## Recover a failed or interrupted command

Read the reported stage and `artifacts/dev/commands.log`, then run `make dev-status` and `make dev-logs`. Logs are retained in separate diagnostic directories. Only one dev command may run at a time; do not operate Helm separately while initialization is active.

After correcting the cause, retry `dev-up` or `dev-rebuild`. Failed builds retain the previous image generation. A pending Helm operation is recovered using Helm rollback to its last deployed revision (or the recorded first install), followed by upgrade with current configuration. An owned stopped node is started in place. Missing nodes, ownership mismatches and incompatible storage changes require inspection; recovery does not uninstall the release or delete PVC data.

## Data and destructive reset

`dev-down` stops this environment's deployments and keeps the kind node, images, registry data and PVCs. BuildKit and mirror cache policies may still evict entries. PVC data survives application rebuilds and restarts, but not deletion of the kind node.

To deliberately delete the cluster **and all its PVC data**, copy the exact name from `artifacts/dev/state.json`:

```sh
make dev-reset DEV_RESET_CONFIRM=bks-dev-REPLACE_WITH_EXACT_ID
```

Reset retains logs and built images. Use `dev-down` for routine stopping. Local PVCs use RWO; migrating an existing incompatible bound claim is a separate operation.

## Access

```sh
artifacts/e2e-tools/bin/kubectl --kubeconfig artifacts/dev/kubeconfig -n buildkit-dev get pods,pvc
# Keep this command running to connect a local buildctl client:
artifacts/e2e-tools/bin/kubectl --kubeconfig artifacts/dev/kubeconfig -n buildkit-dev port-forward service/buildkit-service 9094:9094
```

BuildKit uses TCP port 9094. The registry is available at `http://127.0.0.1:15000` on the host; builds and Pods use `registry.buildkit-dev.svc.cluster.local:5000`. Push build outputs using that in-cluster name. Registry routing for containerd is configured only inside this environment's node.
