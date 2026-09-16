# buildctl-daemon API Guide

`buildctl-daemon` is an HTTP API for submitting Dockerfile build tasks. Callers
upload a Dockerfile context zip and specify the target image and image format;
the daemon drives BuildKit to build and push Nydus, OCI, or both image formats.

> For batch-building many Dockerfiles use the CLI, see
> [buildctl-batch.md](buildctl-batch.md). For dependency-install acceleration see
> [package-mirror.md](package-mirror.md).

## Part 1: API usage

### 1. Endpoints

Local development:

```bash
http://127.0.0.1:18080
```

In-cluster Service:

```text
http://buildkit-service-buildctl-daemon.buildkit-service.svc
```

Remote endpoint (only after adding TLS termination):

```text
https://build.example.com
```

The chart creates only a ClusterIP Service by default. It does not terminate
TLS. Use port-forwarding for local administration, or put an HTTPS
ingress/reverse proxy in front of the Service for remote access.

The examples below use:

```bash
TOKEN='<TOKEN>'   # the token configured via buildctlDaemon.auth.token
BASE='http://127.0.0.1:18080'
```

### 2. Authentication

When the daemon is started with `--auth-token`, every `/v1/*` request must
send:

```bash
-H "Authorization: Bearer $TOKEN"
```

`/healthz` requires no auth.

HTTP API:

| Method | Path | Purpose | Auth |
| --- | --- | --- | --- |
| `GET` | `/healthz` | Health check | No |
| `GET` | `/metrics` | Prometheus metrics | No |
| `POST` | `/v1/builds` | Create a build task | Yes |
| `GET` | `/v1/builds` | List build tasks | Yes |
| `GET` | `/v1/builds/<id>` | Get task status | Yes |
| `GET` | `/v1/builds/<id>/logs` | Get task logs | Yes |
| `DELETE` | `/v1/builds/<id>` | Cancel a task | Yes |
| `POST` | `/v1/builds/<id>/cancel` | Cancel (compat endpoint) | Yes |
| `HEAD` | `/v1/images?image=<ref>` | Check whether a registry manifest exists | Yes |

### 3. Health check

```bash
curl -sS "$BASE/healthz"
```

Expected response:

```json
{"status":"ok"}
```

### 4. Source zip requirements

The API accepts a zip containing a single Dockerfile context.

Create the fixture used by the examples from the readable Dockerfile in this
repository:

```bash
zip -j /tmp/buildctl-daemon-source.zip cmd/buildctl-daemon/test/Dockerfile
```

The default limits are a 512 MiB multipart request, 4 GiB total uncompressed
content, 100,000 archive entries, 100 retained tasks, and 8 GiB total work-dir
usage. The quickstart profile lowers these to 64 MiB, 512 MiB, 10,000 entries,
20 tasks, and 2 GiB. Archive limits return `413`, task admission returns `429`,
and exhausted work-dir capacity returns `507`. Multipart upload reads have a
five-minute deadline (two minutes in quickstart).

The Dockerfile may sit at the zip root:

```text
source.zip
└── Dockerfile
```

Or inside a single top-level directory:

```text
source.zip
└── context-dir/
    └── Dockerfile
```

`metadata.json` is optional; it can provide the default target image when the
request omits `image`:

```json
{
  "target": "registry.example.com/ns/repo:tag"
}
```

### 5. Target image precedence

1. multipart form field `image`
2. `metadata.json` field `target`

`image` wins. If neither is present the request fails.

### 6. Image formats

`image_type` selects the output:

| `image_type` | Behavior |
| --- | --- |
| `nydus` or empty | Build a Nydus image; a `_nydus_v3` suffix is appended to the tag if missing. |
| `oci` | Build an OCI image; a `_nydus_v3` tag suffix is stripped if present. |
| `both` | Build and push OCI from the uploaded context first, then build Nydus with `FROM <OCI digest-pinned ref>`; the task succeeds only when both succeed. |

`compression=gzip` is accepted as an alias for `image_type=oci`.

### 7. Creating a task asynchronously

`mode` is optional. Without it, the legacy behavior is preserved: the task
enters the default `default_mode` and the target image is not rewritten. Only
modes explicitly configured with `routingEnabled: true` in the chart perform
registry routing.

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/tmp/buildctl-daemon-source.zip \
  -F image=localhost:5000/node:latest \
  -F image_type=nydus \
  "$BASE/v1/builds"
```

Response status: `202 Accepted`.

Example response:

```json
{
  "id": "53279eedf80c35ed386d24a31b7f9ddc",
  "status": "queued",
  "image": "localhost:5000/node:latest",
  "mode": "default_mode",
  "routed_target": "localhost:5000/node:latest",
  "image_type": "nydus",
  "retry": 3,
  "retry_interval_seconds": 10,
  "created_at": "2026-06-17T09:00:00Z"
}
```

The pushed Nydus image is:

```text
localhost:5000/node:latest_nydus_v3
```

A task can opt into a routing-enabled mode explicitly:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/tmp/buildctl-daemon-source.zip \
  -F image=logical-registry.example.com/mirror/harbor:example_0123456789ab \
  -F image_type=nydus \
  -F mode=routing_mode \
  "$BASE/v1/builds"
```

`image` in the response always keeps the caller-provided logical reference;
`routed_target` is the actually used base reference after routing. The caller
remains responsible for tag naming conventions; the daemon only selects the
registry prefix. For Nydus, the push target still gets the `_nydus_v3` suffix.
Routing is decided before enqueueing; retries never re-route.

`image_type=both` runs a two-phase build against the same `routed_target`:

```text
uploaded context -> routed_target (OCI)
FROM routed_target@<OCI manifest digest> -> routed_target_nydus_v3 (Nydus)
```

The second phase uses a daemon-generated single-line Dockerfile and does not
inherit the original Dockerfile's target or build args. If the OCI build or
push fails, the task fails immediately without building Nydus; a Nydus retry
never rebuilds an already-succeeded OCI image.

### 8. Creating a task with metadata.json

If the zip contains `metadata.json`, `image` may be omitted:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@source.zip \
  -F image_type=nydus \
  "$BASE/v1/builds"
```

If both are provided, `image` overrides `metadata.json.target`.

### 9. Synchronous creation

With `sync=true`, the request waits for a terminal state and returns the same
JSON as `GET /v1/builds/<id>`. This mode does not stream logs.

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/tmp/buildctl-daemon-source.zip \
  -F image=localhost:5000/node:latest \
  -F image_type=nydus \
  -F sync=true \
  "$BASE/v1/builds"
```

Response status on completion: `200 OK`.

Task state and logs are retained until `--keep-ttl` expires. If the client of
a `sync=true` request disconnects while queued or running, the daemon cancels
the task's context.

### 10. Getting a task

```bash
curl -sS \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/builds/<id>"
```

`200 OK` while retained; `404 Not Found` after `--keep-ttl` cleanup.

Terminal-state example:

```json
{
  "id": "53279eedf80c35ed386d24a31b7f9ddc",
  "status": "succeeded",
  "image": "localhost:5000/node:latest",
  "mode": "default_mode",
  "routed_target": "localhost:5000/node:latest",
  "image_type": "nydus",
  "buildkitd_addr": "tcp://127.0.0.1:9094",
  "node_ip": "127.0.0.1",
  "retry": 3,
  "retry_interval_seconds": 10,
  "created_at": "2026-06-17T09:00:00Z",
  "started_at": "2026-06-17T09:00:01Z",
  "finished_at": "2026-06-17T09:00:10Z"
}
```

Status responses never include `log_path`, inline logs, or
`exporter_response`.

### 11. Listing tasks

```bash
curl -sS \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/builds"
```

Response status: `200 OK`.

Task state is currently kept in the daemon process. The chart deploys a single
`buildctl-daemon` replica by default, so list/get/logs through the public
LoadBalancer always hit the same instance. `queued` means the task is waiting
for a global concurrency slot or a buildkit worker slot — including
build-level retries, worker failover, and re-queueing between multi-format
steps; while waiting, stale `buildkitd_addr` / `node_ip` values are cleared.
`running` means the task has been assigned a `buildkitd_addr` and the BuildKit
solve has started. Terminal tasks keep the last worker address and are cleaned
up when `--keep-ttl` expires.

### 12. Reading task logs

```bash
curl -sS \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/builds/<id>/logs?tail_bytes=20000"
```

`tail_bytes` is optional; the daemon defaults to `--max-log-bytes` and rejects
values above that configured limit.

Logs are stored on the daemon's local disk and deleted together with the task
at `--keep-ttl` cleanup.

### 13. Cancelling a task

```bash
curl -sS -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/builds/<id>"
```

Equivalent endpoint:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/builds/<id>/cancel"
```

Returns `202 Accepted` when the task exists; `404 Not Found` after cleanup.

### 14. Checking image existence

Query the target registry for a manifest with a full image reference:

```bash
curl -fsSI -G \
  -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'image=registry.example.com/ns/example:latest' \
  "$BASE/v1/images"
```

The endpoint only checks the manifest — no blob pulls, no BuildKit worker
involvement. The target registry host must appear in the chart's top-level
`registries[].host` or the daemon's `routingTargets`; other hosts return
`403 Forbidden` so the endpoint cannot be used to probe the daemon's network.
The daemon reads `/root/.docker/config.json` and completes Registry V2
Basic/Bearer challenges; the Docker config is reloaded per request, so updated
Secret mounts take effect without a restart. The chart reuses the Docker
config Secret referenced by `imagePullSecrets.name` by default; set
`buildctlDaemon.registryAuthSecretName` explicitly when `imagePullSecrets` is
a list.

Manifest, redirect, and Bearer token requests are all constrained by
per-registry host rules. The chart trusts each `registries` entry's `host` and
`remoteUrl` host; if a registry's `WWW-Authenticate` realm uses a separate
domain, add `tokenHosts` to that entry:

```yaml
registries:
  - name: example
    host: registry.example.com
    remoteUrl: https://registry.example.com
    tokenHosts:
      - auth.example.com
```

Routing targets do not need to be duplicated in `registries`: entries in
`routingTargets` automatically join the HEAD allowlist and reuse the Docker
config credentials from `registryAuthSecretName`. Shared auth domains are
listed once:

```yaml
buildctlDaemon:
  routingTargets:
    - registry-a.example.com/mirror
    - registry-b.example.com/mirror
  registryTokenHosts:
    - auth.example.com
```

Do not add untrusted or unrelated token hosts. Docker Hub is preconfigured
with `registry-1.docker.io` and `auth.docker.io`.

`image` must include a full registry hostname and repository. A missing tag
defaults to `latest`. `repository@sha256:...` digest references are rejected
(a generic resolver could misreport an existing blob as a manifest), as are
implicit Docker Hub references like `ubuntu:22.04` or `namespace/repo:tag`,
and `https://` schemes.

After rotating the API token Secret, restart the Deployment so the daemon reads
the new file at startup:

```bash
kubectl -n buildkit-service rollout restart \
  deployment/buildkit-service-buildctl-daemon
```

Response statuses:

| Status | Meaning |
| --- | --- |
| `200 OK` | Manifest exists. Headers include the registry's `Docker-Content-Digest`, `Content-Type`, `Content-Length`. |
| `400 Bad Request` | Missing `image` or invalid reference. |
| `401 Unauthorized` | Missing/incorrect daemon Bearer token. |
| `403 Forbidden` | Registry host not in the allowlist. |
| `404 Not Found` | Registry definitively reports the manifest missing. |
| `429 Too Many Requests` | `--registry-check-concurrency` reached. |
| `502 Bad Gateway` | Registry DNS/network/TLS/auth/throttling/server failure. |
| `503 Service Unavailable` | Image checker not configured; should not occur in normal deployments. |
| `504 Gateway Timeout` | Whole check exceeded `--registry-check-timeout`. |

`HEAD` responses have no body; scripts can branch on the curl exit code:

```bash
if curl -fsS -o /dev/null -I -G \
  -H "Authorization: Bearer $TOKEN" \
  --data-urlencode "image=$IMAGE" \
  "$BASE/v1/images"; then
  echo "image exists"
else
  echo "image missing or registry check failed"
fi
```

To distinguish "missing" from upstream failure, read the HTTP status code
instead of the exit code alone.

### 15. Optional form fields

| Field | Description |
| --- | --- |
| `file` | Required. The source zip. |
| `image` | Optional when `metadata.json.target` exists; takes precedence over metadata. |
| `image_type` | `nydus`, `oci`, or `both`. Default `nydus`. |
| `mode` | Optional build queue name; defaults to `default_mode`. Unknown modes return `400`. |
| `compression` | Alias; `gzip` means OCI. |
| `sync` | `true` waits for completion and returns the terminal JSON. |
| `target` | Dockerfile target stage. |
| `timeout_seconds` | Per-task timeout in seconds. |
| `retry` | Build-level retries, non-negative; default `3`, `0` disables. |
| `retry-interval` | Seconds between build-level retries; default `10`, `0` retries immediately. |
| `no_cache` | `true` sets frontend `no-cache`. |
| `build_arg.NAME` | Docker build arg, e.g. `-F build_arg.VERSION=1.2.3`. |
| `build_arg:NAME` | Alternative build-arg syntax. |

`retry` covers build-level retries: after a BuildKit solve fails with a
recoverable infrastructure error (registry 5xx, dropped connections, ...), the
daemon resubmits the same target. Dockerfile parse errors and `RUN` commands
that exit non-zero are not retried — rerunning identical input cannot fix
them. The daemon also keeps its internal worker-address failover: on
retryable network errors (`no route to host`, `connection refused`,
`Unavailable`), it refreshes worker addresses and switches workers first; only
if the build still fails does the next `retry`-controlled attempt start.

### 16. Verifying the pushed manifest

After a Nydus task succeeds:

```bash
curl -fsS http://localhost:5000/v2/node/manifests/latest_nydus_v3 \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  -o /tmp/node-latest-nydus-v3-manifest.json

wc -c /tmp/node-latest-nydus-v3-manifest.json
```

### 17. Notes

#### Dockerfile heredoc compatibility

buildctl-daemon and buildctl-batch preprocess Dockerfiles before scheduling builds. For compatibility with frontends that do not recognize legacy shell heredocs, a standalone file overwrite is converted to a native Dockerfile `COPY` heredoc:

```dockerfile
RUN cat > /path/file << 'EOF'
file contents
EOF
# Becomes:
COPY <<'EOF' /path/file
file contents
EOF
```

Supported overwrite forms are `RUN cat > /path/file <<EOF` and `RUN cat <<EOF > /path/file`, including quoted delimiters, `<<-` tab stripping, instruction case variations and Dockerfile line continuations. Targets must be literal paths: shell variables, substitutions and globs cannot retain their meaning in `COPY`. Delimiter quoting and body contents are preserved. The scheduling key remains the hash of the original source context, calculated before rewriting.

Heredocs combined with command chains (`&&`, `;`), pipes, append redirects (`>>`), multiple redirects, or commands such as `tee` are rejected with the original `RUN` line number and an actionable error. The HTTP API returns **400 before scheduling**; batch preparation fails before submitting a build. No partially rewritten Dockerfile is written on failure.

For example, split a chained file creation into separate instructions:

```dockerfile
# Unsupported: RUN mkdir -p /opt/demo && cat > /opt/demo/app.conf <<'EOF'
RUN mkdir -p /opt/demo
COPY <<'EOF' /opt/demo/app.conf
listen_port=5140
max_message_size=1024
EOF
RUN chmod 644 /opt/demo/app.conf
```

New Dockerfiles should prefer native `COPY <<'EOF' /path`. Quoting the delimiter prevents frontend expansion of `${...}` and preserves shell variables in scripts literally. Use separate `RUN chmod` / `RUN chown` instructions where required; splitting shell chains automatically could change their conditional execution, variables, redirections or permissions.

Native `COPY`/`ADD` heredocs and explicit `RUN <<EOF` scripts are passed through, with their bodies treated as data rather than additional Dockerfile instructions. Native `RUN` heredoc support still depends on the selected frontend. Quoted strings, JSON exec arguments (including instructions with flags), here-strings (`<<<`) and arithmetic shifts are not treated as legacy heredocs. Quoted shell programs such as `RUN sh -c '...'` are not recursively rewritten; express multiline file contents with native Dockerfile heredocs instead.

## Part 2: Deployment and implementation details

### 1. Running locally

```bash
go run ./cmd/buildctl-daemon \
  --listen 127.0.0.1:18080 \
  --buildkitd-addr tcp://127.0.0.1:9094 \
  --work-dir /tmp/buildctl-daemon-api-test \
  --keep-ttl 2h \
  --rlimit-nofile 1048576 \
  --pprof-listen 127.0.0.1:6060
```

Common flags:

| Flag | Description |
| --- | --- |
| `--listen` | HTTP API listen address. |
| `--buildkitd-addr` | Comma-separated buildkitd addresses; scheme-less entries are treated as `tcp://...`. |
| `--tlscacert` | CA certificate for BuildKit connections. |
| `--tlscert` | Client certificate for mTLS BuildKit connections. |
| `--tlskey` | Client private key for mTLS BuildKit connections. |
| `--tlsdir` | Directory containing `ca.pem`, `cert.pem`, and `key.pem`. |
| `--tlsservername` | TLS server name for BuildKit certificate verification. |
| `--auth-token` | Bearer token for `/v1/*`; empty disables API auth. Avoid this on shared hosts because argv is observable. |
| `--auth-token-file` | Read the Bearer token from a file; mutually exclusive with `--auth-token`. The Helm chart uses this mode. |
| `--work-dir` | Local scratch dir for uploaded zips, extracted contexts, and build logs. |
| `--default-mode` | Mode used when a request omits `mode`; default `default_mode`. |
| `--modes-json` | Mode config JSON; each entry has `concurrency` and `routingEnabled`. Empty keeps the legacy scheduler. |
| `--routing-targets-json` | JSON array of registry prefixes for routing-enabled modes; requires 2-32 non-empty, deduplicated entries. |
| `--keep-ttl` | How long terminal task state and logs are retained, e.g. `2h`. |
| `--max-log-bytes` | Maximum bytes stored per build log and returned by `/logs`; additional output is discarded. |
| `--max-request-bytes` | Maximum multipart build request size; default 512 MiB. |
| `--max-extracted-bytes` | Maximum total uncompressed build-context size; default 4 GiB. |
| `--max-archive-files` | Maximum number of zip entries; default 100,000. |
| `--max-retained-tasks` | Maximum queued, running, and retained tasks; default 100. |
| `--max-work-dir-bytes` | Total work-dir byte budget; default 8 GiB. |
| `--upload-read-timeout` | Deadline for reading one multipart upload body; default `5m`. |
| `--addr-concurrency` | Concurrent BuildKit solves per buildkitd address; excess requests queue in the daemon. |
| `--max-concurrency` | Global cap on concurrently building tasks (regardless of address count); excess stays `queued`; `0` disables. |
| `--registry-check-timeout` | Timeout for `HEAD /v1/images` registry checks; default `30s`. |
| `--registry-check-host-rules` | Semicolon-separated `registry=network-host\|token-host` rules; the chart merges `registries`, `routingTargets`, and `registryTokenHosts`. Empty makes all image checks return `403`. |
| `--registry-check-concurrency` | Max concurrent registry HEADs; default `32`, `429` at the cap. |
| `--rlimit-nofile` | Raise process `RLIMIT_NOFILE` at startup; needs adequate privileges. |
| `--pprof-listen` | Optional pprof listen address; also via `BUILDCTL_DAEMON_PPROF_SERVER`. |

For a TLS/mTLS BuildKit endpoint, create a Secret with `ca.pem`, `cert.pem`, and
`key.pem`, then enable the chart integration:

```bash
kubectl -n buildkit-service create secret generic buildkit-client-tls \
  --from-file=ca.pem \
  --from-file=cert.pem \
  --from-file=key.pem

helm upgrade buildkit-service ./chart \
  --namespace buildkit-service --reuse-values \
  --set buildctlDaemon.buildkitTLS.enabled=true \
  --set-string buildctlDaemon.buildkitTLS.secretName=buildkit-client-tls \
  --set-string buildctlDaemon.buildkitTLS.serverName=buildkitd.example.com
```

Restart the Deployment after rotating files in an existing TLS Secret.

### 2. Network access via the chart

The safe development path is the default ClusterIP plus port-forwarding:

```bash
kubectl -n buildkit-service port-forward \
  service/buildkit-service-buildctl-daemon 18080:80
```

The chart can also create a LoadBalancer Service, but it does not provide TLS:

```yaml
buildctlDaemon:
  publicService:
    enabled: true
    name: buildctl-daemon-public
    type: LoadBalancer
    port: 80
    targetPort: http
    # Add cloud-specific annotations as needed, for example:
    #   AWS:           service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
    #   Alibaba Cloud: service.beta.kubernetes.io/alibaba-cloud-loadbalancer-type: "nlb"
    annotations: {}
```

Use this only with an internal/private load balancer, or behind infrastructure
that terminates HTTPS before traffic reaches the Service. Never send the Bearer
token or source archives over public plain HTTP.

Find the load balancer address:

```bash
kubectl -n buildkit-service get svc buildctl-daemon-public
```

Call the HTTPS endpoint exposed by your TLS terminator:

```bash
curl -sS \
  -H "Authorization: Bearer $TOKEN" \
  https://build.example.com/v1/builds
```

### 3. pprof debugging

Enable at startup:

```bash
buildctl-daemon \
  --pprof-listen 0.0.0.0:6060
```

Or via environment variable:

```bash
BUILDCTL_DAEMON_PPROF_SERVER=0.0.0.0:6060 buildctl-daemon ...
```

In Kubernetes, port-forward the internal Service first:

```bash
kubectl -n buildkit-service port-forward svc/buildkit-service-buildctl-daemon 6060:6060
```

Common commands:

```bash
curl -sS http://127.0.0.1:6060/debug/pprof/goroutine?debug=2 > goroutine.txt
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
```

Never expose pprof through the public LoadBalancer Service.

### 4. High-concurrency configuration

The chart defaults to 5 concurrent solves per buildkitd address and 600 running
builds globally (the standalone CLI default for `--max-concurrency` is 0,
meaning disabled). The quickstart profile tightens these limits for small
clusters:

```yaml
buildctlDaemon:
  replicas: 1
  addrConcurrency: 5
  maxConcurrency: 600
  maxRequestBytes: 1073741824
  maxExtractedBytes: 8589934592
  maxArchiveFiles: 200000
  maxRetainedTasks: 2000
  maxWorkDirBytes: 429496729600
  uploadReadTimeout: 10m
  rlimitNoFile: 1048576
  resources:
    requests:
      cpu: "8"
      memory: 16Gi
      ephemeral-storage: 200Gi
    limits:
      cpu: "16"
      memory: 32Gi
      ephemeral-storage: 500Gi
```

`addrConcurrency` controls concurrent BuildKit solves per buildkitd address;
extra sync HTTP clients wait inside the daemon. Setting it too high can
overwhelm BuildKit sessions or registry backends.

To configure independent FIFO queues (for example an interactive queue and a
routed production queue):

```yaml
buildctlDaemon:
  defaultMode: default_mode
  modes:
    default_mode:
      concurrency: 200
      routingEnabled: false
    routing_mode:
      concurrency: 400
      routingEnabled: true
  routingTargets:
    - registry-a.example.com/mirror
    - registry-b.example.com/mirror
    - registry-c.example.com/mirror
```

Within one mode, tasks are scheduled in enqueue order; modes use independent
worker pools. When `maxConcurrency > 0`, it must be at least the sum of all
mode `concurrency` values (600 in this example, matching the chart default),
or the daemon refuses to start. Tasks share the underlying BuildKit
address pool, so `addrConcurrency × number of buildkitd addresses` must leave
enough physical slots for each mode.

Registry routing uses Rendezvous/HRW hashing. The daemon sorts all regular
files in the uploaded context by relative POSIX path and hashes
`relative_path + NUL + file_content + NUL` in order, producing the full
64-character hex `context_sha256`. Each registry prefix scores
`SHA256(registry_prefix + NUL + context_sha256)`; the highest 256-bit score
wins. Pool order does not affect the outcome; adding a registry moves only
about `1/(N+1)` of contexts to the new entry. No live load probing, fallback,
or multi-replica pushes.

Registry prefixes are trimmed of whitespace and trailing `/`. Empty entries,
duplicates, or pool sizes outside 2-32 make the daemon refuse to start. When a
target is rewritten, the routing decision is written into the task log before
BuildKit submission so it can be independently recomputed from the context
digest.

#### 4.1 Long-lived canary station

The chart also ships a disabled-by-default `buildctlDaemonCanary`. It creates
an independent Deployment, ClusterIP Service, and optional public
LoadBalancer, all labelled `app.kubernetes.io/component:
buildctl-daemon-canary`; the production Services keep selecting only
`buildctl-daemon`, so canary pods never receive production traffic.

The canary reuses the token and registry-auth Secrets from an installed
production release. Verify that the production token Secret exists, then render
and apply only the two canary templates:

```bash
kubectl -n buildkit-service get secret \
  buildkit-service-buildctl-daemon-token

IMAGE='ghcr.io/example/buildkit-service/buildctl-daemon:candidate'
helm template buildkit-service ./chart \
  --set buildctlDaemon.enabled=false \
  --set buildctlDaemonCanary.enabled=true \
  --set-string buildctlDaemonCanary.image="$IMAGE" \
  --show-only templates/buildctl-daemon-canary-deployment.yaml \
  --show-only templates/buildctl-daemon-canary-service.yaml \
  | kubectl -n buildkit-service apply -f -

kubectl -n buildkit-service rollout status \
  deployment/buildkit-service-buildctl-daemon-canary
```

Submit an isolated OCI smoke build through a local port-forward:

```bash
zip -j /tmp/buildctl-daemon-canary.zip \
  cmd/buildctl-daemon/test/smoke/Dockerfile
kubectl -n buildkit-service port-forward \
  service/buildkit-service-buildctl-daemon-canary 18081:80

SMOKE_IMAGE="ttl.sh/buildkit-service-canary-$(openssl rand -hex 6):1h"
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/tmp/buildctl-daemon-canary.zip \
  -F image="$SMOKE_IMAGE" \
  -F image_type=oci \
  -F sync=true \
  http://127.0.0.1:18081/v1/builds
```

Keep canary concurrency low (for example per-mode `1`, global `2`). The canary
still uses the real BuildKit cluster, so test tasks consume some build and
registry resources; it is not an isolated capacity environment.

Use reachable registries in tests: `localhost:5000` is reachable in the local
integration setup, `localhost:50000` is not.

### 5. State and log retention

After a task reaches a terminal state, its state and logs are retained for
`--keep-ttl`. A background sweep then deletes the in-memory record and the
local `workDir` entry, after which task and log queries return
`404 Not Found`.
