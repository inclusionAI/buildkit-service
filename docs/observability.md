# Observability

BuildKit Service exposes Prometheus metrics, optional OpenTelemetry traces over OTLP, and JSON lifecycle logs. Monitoring backends are independent deployments: installing the product does not install Prometheus, an Operator, a Collector, or Grafana. Existing API routes and metric names remain compatible.

## Signals and ownership

| Signal | Producer | Meaning |
| --- | --- | --- |
| Build completions, duration, completed/cached steps | Native BuildKit `/metrics` | All clients of that worker, including direct gRPC callers and batch jobs |
| Active builds and cache record accounting | Optional `buildkit-observer` | Read-only BuildKit history and DiskUsage snapshots, independent of the HTTP daemon |
| Queue, execution slots, retries, cancellation, latency | `buildctl-daemon /metrics` | Work admitted by that daemon; these are not cluster-wide BuildKit admission limits |
| Cache files, allocated blocks, backing filesystem space/inodes | Cache sidecars | Per-volume measurements with freshness and scan health |
| CPU, working set, limits, node pressure | Kubelet/cAdvisor and cluster monitoring | Includes workload resource use; native BuildKit process RSS alone does not include all executor processes |
| Request, preparation, queue, solve and batch spans | OTel SDK and native BuildKit | W3C trace context propagated across asynchronous tasks, gRPC and buildctl subprocesses |
| Build lifecycle and native worker logs | stdout/stderr | JSON records carrying build/trace/span identifiers where available |

The image built by `chart/images/buildkit/Dockerfile` includes buildkitd, buildctl, buildctl-daemon, buildctl-batch, and buildkit-observer. Native metric availability must be checked against the deployed immutable image, after a real build and at least one complete scrape interval. A source tag or an immediate empty query does not establish what an image exports.

`buildkit-observer` reads one worker, with configurable polling and deadlines. It exposes `buildkit_worker_builds_active`, `buildkit_worker_cache_bytes{state="in_use|reclaimable"}`, `buildkit_worker_cache_records`, and observer health, error count, poll duration and last-success timestamp. Cache accounting follows `buildctl du`: positive record sizes are summed and records not in use are reclaimable. This is API accounting, not physical disk usage or a promise that GC immediately frees those bytes. Failed polls retain the previous sample, set `buildkit_worker_observer_up=0`, and leave last-success time unchanged. Always evaluate freshness alongside sampled gauges.

## Capacity and watermarks

Do not divide cache file size by a summed filesystem capacity to infer worker saturation. Several worker volumes may share a node filesystem, and `emptyDir.sizeLimit`, an explicit cache budget, a GC target, and filesystem capacity are different quantities.

| Metric | Contract |
| --- | --- |
| `buildkit_service_cache_used_bytes` | Logical file sizes, with hard links counted once |
| `buildkit_service_cache_allocated_bytes` | Allocated file blocks; sparse files can be much smaller than their logical sizes |
| `buildkit_service_cache_total_bytes` | Capacity of the backing filesystem, retained for compatibility; never a per-worker quota |
| `buildkit_service_cache_limit_bytes` | Explicit configured budget, omitted when unknown; exporting it does not enforce a quota |
| `buildkit_service_filesystem_available_bytes` | Actual space available on the backing filesystem |
| `buildkit_service_filesystem_free_inodes` | Available inode count |
| `buildkit_service_cache_scan_success` | Whether the latest requested scan completed successfully |
| `buildkit_service_cache_scan_timestamp_seconds` | Time of the latest successful directory scan |

Missing directories and incomplete scans produce an unhealthy measurement instead of a healthy empty cache. Directory scans are cached; choose their interval and CPU/memory budget for the real cache size. Use filesystem space/inodes and Kubernetes eviction/pressure metrics alongside API cache accounting. Do not sum shared filesystem capacity across Pods.

The legacy Python aggregator remains available for existing consumers, with expected/successful target counts and unhealthy status for partial failures. Use direct per-Pod discovery for new monitoring; do not route fleet counters through a load-balanced ClusterIP or discard worker identity before computing rates. Compute per-instance `rate()` before summing counters so worker restarts are handled independently.

## Kubernetes integration

For a Prometheus Operator deployment, enable the optional integration:

```yaml
observability:
  workerMetrics:
    enabled: true
  workerState:
    enabled: true
    interval: 30s
    timeout: 5s
  podMonitor:
    enabled: true
    labels:
      release: monitoring
  prometheusRule:
    enabled: true
    labels:
      release: monitoring
  dashboard:
    enabled: true
  tracing:
    enabled: true
    endpoint: https://otel-collector.monitoring.svc:4317
    protocol: grpc
    sampler: parentbased_traceidratio
    samplerArg: "0.1"
    caSecretName: otel-ca
    headersSecretName: otel-headers
buildkitdCacheMetrics:
  cacheLimitBytes: 966367641600 # Example explicit 900 GiB budget; match your own configuration.
```

The Operator and its CRDs must already exist when PodMonitor or PrometheusRule is enabled. Set labels and namespace selectors to match your monitoring installation. PodMonitors cover native workers, worker-state sidecars, cache sidecars, the HTTP daemon, package mirrors, Maven and registry mirrors. Endpoints remain distinct targets with Pod identity. The native worker debug listener also serves diagnostics; keep it on the trusted management network rather than exposing it through a public Service.

Tracing uses standard `OTEL_*` variables. The header Secret contains a `headers` key in the SDK's comma-separated header format; the CA Secret contains `ca.crt`. Values reference Secrets rather than placing credentials in arguments or Helm values. For mTLS BuildKit workers, configure `observability.workerState.tlsSecretName` with `ca.pem`, `cert.pem`, `key.pem`, and the appropriate `tlsServerName`.

Before using per-Pod resource alerts in a new runtime, verify that RUN executor cgroups are descendants of the worker cgroup. `buildkitdConfig.worker.oci.defaultCgroupParent` passes through an explicitly chosen executor parent when the deployment requires one; its correct path depends on the runtime and cgroup namespace, so the chart does not guess a host path.

The product chart supplies integration resources, not node monitoring. Use the cluster's kubelet/cAdvisor, kube-state-metrics and node exporter for container limits, OOM/restarts, CPU throttling, node pressure and filesystem/inode alerts. The local profile below uses standalone cAdvisor for the equivalent Docker container metrics.

Standalone service and batch binaries enable tracing only when `OTEL_TRACES_EXPORTER=otlp` or an OTLP endpoint is configured. `OTEL_TRACES_EXPORTER=none` disables export. Both `grpc` and `http/protobuf` are supported; standard sampler, exporter TLS, headers and batch-processor environment settings are honored. Set these variables on the batch runner container when launching Kubernetes batch jobs.

Async builds retain the caller's trace context without inheriting request cancellation; synchronous builds retain the existing disconnect-cancellation behavior. The API returns an optional `trace_id`. Lifecycle records include `build_id`, `trace_id`, `span_id`, mode, result, worker and bounded error classification. Metrics use route templates and bounded outcome labels; source contents, build arguments, build IDs, arbitrary error text and image names are not metric labels. Trace export uses a bounded asynchronous queue and a five-second shutdown deadline, so Collector failure does not block image builds. Sampling and bounded buffers can drop traces; telemetry is not a durable build audit database.

## Local open-source stack

Requirements: a Linux amd64 host using cgroup v2, with local Linux amd64 Docker, Compose and Buildx, at least 20 GiB free on the selected data filesystem, and enough Docker image-store space for pinned dependencies. No cloud registry or Kubernetes credentials are used. The stack includes two source-built workers, the source-built HTTP API and worker observers, a local OCI registry, Prometheus, Alertmanager, Grafana, an OTel Collector, Tempo, Loki and cAdvisor. Images and tools are pinned in `tests/e2e/dependencies.json`; native source image IDs and source file hashes are recorded for each run.

```sh
make setup e2e-setup
make observability-unit
make observability          # source build, deploy, and end-to-end assertions
make observability-status  # owned resources and loopback endpoints
make observability-logs
make observability-down    # stop owned services; preserve data
```

`OBS_RUN_DIR` optionally selects an exclusive persistent data directory, for example `OBS_RUN_DIR=/data/buildkit-observability make observability`. The default is ignored `artifacts/observability/`. Reuse the same value for later lifecycle commands. All published ports bind to `127.0.0.1` and Docker allocates them dynamically. Data volumes bind below the chosen directory. The runner stores its own Docker configuration, never switches global Docker/Kubernetes contexts, and validates resource ownership and source-built image identity. `up` applies configuration changes using content hashes and recreates affected owned services while preserving volumes. It does not rebuild source images; use `observability-build` after source changes, then `observability-up`.

The local worker entrypoint delegates CPU, memory and PID controllers inside each private, explicitly bounded worker cgroup namespace. It moves only its own PID into a daemon leaf before starting BuildKit, so nested RUN cgroups can start and their resources remain charged to the owning worker. It rejects unexpected namespace/limit layouts and does not modify host-wide cgroup or Docker configuration.

The loopback Grafana dashboard is provisioned automatically with Prometheus, Tempo and Loki data sources. The profile permits anonymous Viewer access for local inspection. Alertmanager records local alert delivery without sending external notifications. Standard production alerts include a two-minute target-down threshold; the E2E intentionally waits for a real alert to fire and then resolve.

Validation checks real HTTP and batch CLI builds, OCI registry manifests, LMDB export, failed RUN instructions, queue watermarks and idempotent cancellation, W3C caller-to-worker trace propagation, native build metrics, API cache accounting, RUN memory attribution, CPU/memory and filesystem measurements, worker failover, alert delivery/resolution, Collector outage with successful builds and telemetry recovery, correlated Loki logs, retained Prometheus history, and the provisioned Grafana dashboard and log-to-trace link. Restart checks rediscover ephemeral host ports before querying the backend. Observer snapshots are sampled; cumulative completed-build metrics come from native BuildKit. Failed attempts retain separate reports. This suite is functional validation, not a throughput, long-soak, cross-cloud, or HK deployment certification.

Raw evidence and credentials stay under the runtime directory: `api-token` and `docker/` are private, `builds/` retains source manifests, `tests/<id>/` retains each attempt's report and selected backend responses, and `state.json` records resources and endpoints. `report.json` is the latest successful summary. Do not publish the whole directory. `down` retains service data and diagnostic evidence; deletion is a separate explicit operation.
