# BuildKit Service

Distributed image build and package mirror services for agentic infrastructure.It batch-builds Dockerfiles into OCI / [Nydus](https://github.com/dragonflyoss/nydus) images, pushes them to any OCI registry, and ships an in-cluster package cache (pip / npm / apt / yum / apk / git / container registry) that dramatically speeds up dependency installation during builds.

## Features

- **Batch builds**: `buildctl-batch` CLI drives large Dockerfile build sets across a horizontally scalable pool of buildkitd workers, with retries, failure export, and consistent-hash worker scheduling.
- **HTTP build API**: `buildctl-daemon` accepts a single Dockerfile context zip per request, supports OCI / Nydus / dual-format output, FIFO build queues (modes), and rendezvous-hash registry routing for sharding pushes across multiple registries.
- **Package mirror**: in-cluster caches for pip, npm, apt/yum/dnf, apk, git, and pull-through container registry mirrors — with graceful fallback to upstreams.
- **Nydus support**: built on [Nydus](https://github.com/dragonflyoss/nydus), enabling lazy-pulling container images.
- **Cloud neutral**: plain Kubernetes + Helm; works on any cloud or on-premises cluster. Any OCI registry (Docker Hub, ghcr.io, Harbor, ACR, ECR, GAR, ...) can be a build target or mirror upstream.
- **P2P preheating**: optional [Dragonfly](https://d7y.io) image preheating ([docs/preheat.md](docs/preheat.md)).

## Architecture

```text
                 ┌────────────────────────────────────────────────┐
   source.zip    │ Kubernetes                                     │
  ──────────────▶│  buildctl-daemon (HTTP API)                    │
   curl / CLI    │        │ schedule (context-hash affinity)      │
                 │        ▼                                       │
                 │  buildkitd worker pool (Deployment, N pods)    │
                 │        │ FROM pulls        │ push              │
                 │        ▼                   ▼                   │
                 │  package-mirror      target registry           │
                 │  (pip/npm/apt/yum/                             │
                 │   apk/git/registry)                            │
                 └────────────────────────────────────────────────┘
```

## Quick start

### Prerequisites

- Kubernetes with support for privileged Pods (required by buildkitd)
- Helm 3, `kubectl`, `curl`, `zip`, and `openssl`
- Cluster egress to GHCR, Docker Hub, and the target registry
- About 4 CPU, 8 GiB memory, and 20 GiB ephemeral storage for this evaluation

The chart defaults to one replica per workload, node-local caches, no PVC, and no dedicated-node selectors. [chart/values-quickstart.yaml](chart/values-quickstart.yaml) adds tighter API and cache limits for a small development cluster. Size and isolate production workers using the guidance in [docs/buildctl-batch.md](docs/buildctl-batch.md#recommended-node-pools).

The default service image is published by the release workflow. Verify that it is available before installing:

```bash
docker manifest inspect \
  ghcr.io/inclusionai/buildkit-service/buildctl-daemon:latest >/dev/null
```

For a fork or before its first release, build and push the image to a registry your cluster can pull from, then set `SERVICE_IMAGE`:

```bash
SERVICE_IMAGE='registry.example.com/myorg/buildctl-daemon:dev'
docker buildx build --platform linux/amd64 \
  -f chart/images/buildkit/Dockerfile \
  --tag "$SERVICE_IMAGE" --push .
```

Otherwise use the published default:

```bash
SERVICE_IMAGE='ghcr.io/inclusionai/buildkit-service/buildctl-daemon:latest'
```

Install the build service without the optional package mirrors:

```bash
TOKEN="$(openssl rand -hex 32)"

helm upgrade --install buildkit-service ./chart \
  --namespace buildkit-service --create-namespace \
  --values ./chart/values-quickstart.yaml \
  --set packageMirror.enabled=false \
  --set-string buildctlDaemon.image="$SERVICE_IMAGE" \
  --set-string buildctlDaemon.auth.token="$TOKEN"

kubectl -n buildkit-service rollout status deployment/buildkit-service
kubectl -n buildkit-service rollout status \
  deployment/buildkit-service-buildctl-daemon
```

Keep this port-forward running in a second terminal. The API is intentionally ClusterIP-only by default:

```bash
kubectl -n buildkit-service port-forward \
  service/buildkit-service-buildctl-daemon 18080:80
```

Create a minimal context and push an OCI image to a unique, anonymous [ttl.sh](https://ttl.sh) repository (the image expires after one hour):

```bash
mkdir -p /tmp/buildkit-service-quickstart
printf 'FROM alpine:3.20\nRUN echo "buildkit-service is ready"\n' \
  > /tmp/buildkit-service-quickstart/Dockerfile
(cd /tmp/buildkit-service-quickstart && zip -q /tmp/source.zip Dockerfile)

IMAGE="ttl.sh/buildkit-service-$(openssl rand -hex 6):1h"
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@/tmp/source.zip \
  -F image="$IMAGE" \
  -F image_type=oci \
  -F sync=true \
  http://127.0.0.1:18080/v1/builds
```

Production targets should use an authenticated OCI registry and immutable image tags. Install the package mirrors separately when needed; see [docs/package-mirror.md](docs/package-mirror.md).

For a private target registry, create a Docker config Secret instead of putting credentials in Helm values or shell arguments:

```bash
kubectl -n buildkit-service create secret generic registry-auth \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$HOME/.docker/config.json"

helm upgrade buildkit-service ./chart \
  --namespace buildkit-service \
  --reuse-values \
  --set imagePullSecrets.create=false \
  --set-json 'imagePullSecrets=[{"name":"registry-auth"}]' \
  --set-string buildctlDaemon.registryAuthSecretName=registry-auth
```

The referenced Docker config controls target push and manifest-check credentials. Keep service-image pull credentials in the same config or provide another compatible pull Secret.

## Documentation

| I want to... | Document |
| --- | --- |
| Batch-build / convert many Dockerfiles (CLI) | [docs/buildctl-batch.md](docs/buildctl-batch.md) |
| Submit single builds over HTTP | [docs/buildctl-daemon.md](docs/buildctl-daemon.md) |
| Speed up pip / npm / apt / yum / apk / git in Dockerfiles | [docs/package-mirror.md](docs/package-mirror.md) |
| Preheat images into a Dragonfly P2P cluster | [docs/preheat.md](docs/preheat.md) |

## Repository layout

```text
├── scripts/
│   ├── buildctl-batch.sh  # kubectl wrapper: run batch builds from your laptop
│   └── preheat.sh         # Dragonfly image preheating helper
├── cmd/buildctl-batch/    # batch build CLI (runner side)
├── cmd/buildctl-daemon/   # build HTTP API daemon
├── chart/                 # Helm chart: buildkit-service + package-mirror workloads
│   └── images/            # self-built helper images (apt-cacher-ng, git-cache)
├── docs/                  # component documentation
└── pkg/                   # shared libraries (Dockerfile preprocessing, ...)
```

## Building from source

Requires Go 1.23 or newer. The batch CLI requires Linux, a C compiler and LMDB development files (`liblmdb-dev` on Debian/Ubuntu); its result store is unavailable without CGO. Cross-compiling it requires a matching Linux C toolchain and LMDB library. On macOS, build and run the container image or use the local E2E suite. Docker with Buildx is required to build the container images.

```bash
make all      # builds bin/buildctl-batch and bin/buildctl-daemon
make test     # go test ./...
docker build -f chart/images/buildkit/Dockerfile -t buildkit-service:dev .
```

Release workflows publish `buildctl-daemon`, `apt-cacher-ng`, `git-cache`, and `preheat` images to `ghcr.io/<repository-owner>/buildkit-service/`.

## Local end-to-end tests

Requires Docker with Linux containers, Git, Make and [uv](https://docs.astral.sh/uv/getting-started/installation/).

```sh
make setup       # prepare the Python environment
make e2e-unit    # test E2E tooling without starting Docker or kind
make e2e         # prepare tools and run the full kind suite
```

The suite builds current sources and deploys them into an isolated kind cluster with a local registry. It targets `linux/amd64`: Apple Silicon uses emulation; Linux amd64 runs natively. See the [E2E guide](tests/e2e/README.md) for resource requirements, lifecycle commands, diagnostics and coverage boundaries.

## Security

`buildctl-daemon` accepts source archives and controls image builds. Keep it on a trusted network. The chart disables its public LoadBalancer by default; use port-forwarding for development or place an HTTPS ingress/reverse proxy in front of it for remote access. Never send the Bearer token over public plain HTTP, and never expose the pprof port publicly.

## License

[Apache License 2.0](LICENSE)
