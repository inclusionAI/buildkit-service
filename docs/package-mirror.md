# Package Mirror

The package mirror is an in-cluster package cache that accelerates pip, npm,
apt, yum/dnf, apk, git, and Docker/BuildKit `FROM` image pulls.

> For the build service itself, see [buildctl-batch.md](buildctl-batch.md) (batch
> CLI) and [buildctl-daemon.md](buildctl-daemon.md) (HTTP API).

Use the minimal setup below in your images. All Services live in the
`package-mirror` namespace and listen on port 80.

The chart ships an opt-in ingress NetworkPolicy
(`packageMirror.networkPolicy.enabled`). When enabled it allows clients from
the package-mirror namespace itself and `buildkit-service`; add other trusted
client namespaces through `packageMirror.networkPolicy.allowedNamespaces`
before enabling it, or cross-namespace clients lose cache access.

The cache Services are unauthenticated plain HTTP endpoints intended only for
a trusted cluster network. Do not expose them publicly. Use Kubernetes
NetworkPolicy (or your CNI's equivalent) to restrict clients, and use HTTPS for
all external upstreams where the selected cache protocol supports it.

| Type | Address |
|------|---------|
| pip | `http://pip.package-mirror.svc.cluster.local/index/` |
| npm | `http://npm.package-mirror.svc.cluster.local/` |
| apt / yum / dnf | `http://apt-yum.package-mirror.svc.cluster.local` |
| apk | `http://apk.package-mirror.svc.cluster.local/alpine` |
| git | `http://git.package-mirror.svc.cluster.local` |
| Docker registry | Configured transparently on the BuildKit side; Dockerfiles keep their original `FROM` lines |
| metrics | `http://metrics.package-mirror.svc.cluster.local/metrics` |

## Minimal Dockerfile setup

Add this block to your image. The script writes generic client configs and only
configures package managers that actually exist in the base image. apt and apk
can fall back at the config level; pip, npm, and git use command-level fallbacks
(see per-tool usage below).

```dockerfile
RUN set -eux; \
    if [ "$(id -u)" != 0 ]; then echo "package mirror setup must run as root before USER switch" >&2; exit 1; fi; \
    PIP=pip.package-mirror.svc.cluster.local; \
    NPM=npm.package-mirror.svc.cluster.local; \
    AY=apt-yum.package-mirror.svc.cluster.local; \
    APK=apk.package-mirror.svc.cluster.local; \
    APT_DEBIAN=http://deb.debian.org/debian; \
    APT_DEBIAN_SECURITY=http://security.debian.org/debian-security; \
    APT_UBUNTU=http://archive.ubuntu.com/ubuntu; \
    if command -v pip >/dev/null 2>&1 || command -v pip3 >/dev/null 2>&1; then \
      printf '[global]\nindex-url = http://%s/index/\ntrusted-host = %s\ntimeout = 30\nretries = 3\n' "$PIP" "$PIP" > /etc/pip.conf; \
    fi; \
    if command -v npm >/dev/null 2>&1; then \
      mkdir -p /root; \
      printf 'registry=http://%s/\nfetch-retries=3\nfetch-timeout=60000\nfetch-retry-maxtimeout=30000\n' "$NPM" > /etc/npmrc; \
      printf 'registry=http://%s/\nfetch-retries=3\nfetch-timeout=60000\nfetch-retry-maxtimeout=30000\n' "$NPM" > /root/.npmrc; \
    fi; \
    if command -v apt-get >/dev/null 2>&1; then \
      mkdir -p /etc/apt/apt.conf.d /usr/local/bin; \
      APT_DEFAULT="$APT_DEBIAN"; \
      if [ -r /etc/os-release ]; then . /etc/os-release; [ "${ID:-}" = ubuntu ] && APT_DEFAULT="$APT_UBUNTU" || true; fi; \
      for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do \
        [ -f "$f" ] || continue; \
        for h in "$AY" "$AY:80" apt-yum.package-mirror.svc apt-yum.package-mirror.svc:80 apt-yum.package-mirror apt-yum.package-mirror:80; do \
          sed -i \
            -e "s#http://$h/debian-security#${APT_DEBIAN_SECURITY}#g" \
            -e "s#https://$h/debian-security#${APT_DEBIAN_SECURITY}#g" \
            -e "s#http://$h/debian#${APT_DEBIAN}#g" \
            -e "s#https://$h/debian#${APT_DEBIAN}#g" \
            -e "s#http://$h/ubuntu#${APT_UBUNTU}#g" \
            -e "s#https://$h/ubuntu#${APT_UBUNTU}#g" \
            -e "s#http://$h/ #${APT_DEFAULT} #g" \
            -e "s#https://$h/ #${APT_DEFAULT} #g" \
            -e "s#http://$h #${APT_DEFAULT} #g" \
            -e "s#https://$h #${APT_DEFAULT} #g" \
            -e "s#http://$h/\$#${APT_DEFAULT}#g" \
            -e "s#https://$h/\$#${APT_DEFAULT}#g" \
            -e "s#http://$h\$#${APT_DEFAULT}#g" \
            -e "s#https://$h\$#${APT_DEFAULT}#g" \
            "$f"; \
        done; \
      done; \
      printf '#!/bin/sh\nH=%s\nif command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1 && timeout 2 bash -c "</dev/tcp/$H/80" 2>/dev/null; then\n  echo "http://$H"\nelse\n  echo DIRECT\nfi\n' "$AY" > /usr/local/bin/apt-proxy-detect; \
      chmod +x /usr/local/bin/apt-proxy-detect; \
      printf 'Acquire::http::Proxy-Auto-Detect "/usr/local/bin/apt-proxy-detect";\n' > /etc/apt/apt.conf.d/01proxy; \
      printf 'Acquire::http::Timeout "30";\nAcquire::Retries "3";\n' > /etc/apt/apt.conf.d/80retries; \
    fi; \
    for f in /etc/dnf/dnf.conf /etc/yum.conf; do \
      [ -f "$f" ] && printf '\ntimeout=30\nretries=3\n' >> "$f" || true; \
    done; \
    if [ -f /etc/alpine-release ]; then \
      av=$(cut -d. -f1,2 /etc/alpine-release); \
      case "$av" in [0-9]*) v=v$av ;; *) v=$av ;; esac; \
      printf 'http://%s/alpine/%s/main\nhttp://%s/alpine/%s/community\nhttp://dl-cdn.alpinelinux.org/alpine/%s/main\nhttp://dl-cdn.alpinelinux.org/alpine/%s/community\n' "$APK" "$v" "$APK" "$v" "$v" "$v" > /etc/apk/repositories; \
    fi
```

This script requires root; place it before any `USER` switch in the Dockerfile.

The apt section uses the recommended **proxy mode**: only
`Acquire::http::Proxy-Auto-Detect` is configured, and
`/etc/apt/sources.list` / `/etc/apt/sources.list.d/*.sources` keep pointing at
real upstreams. The script scans both classic `.list` and deb822 `.sources`
files; if an earlier layer already rewrote sources to
`apt-yum.package-mirror.svc.cluster.local` (with or without `:80`), it restores
the real upstream to avoid self-referential requests. `apt-proxy-detect` runs
with `/bin/sh`; if the image lacks `bash` or `timeout` it returns `DIRECT`
instead of breaking `apt-get update`.

Default restore targets: Debian `http://deb.debian.org/debian`, Debian Security
`http://security.debian.org/debian-security`, Ubuntu
`http://archive.ubuntu.com/ubuntu`. If your environment requires a fixed
corporate upstream mirror, change `APT_DEBIAN` / `APT_DEBIAN_SECURITY` /
`APT_UBUNTU` at the top of the script. Do NOT additionally rewrite sources to
`http://apt-yum.package-mirror.svc.cluster.local/debian` or `.../ubuntu`.

Without this restoration, apt requests can become self-referential ("access
apt-yum through the apt-yum proxy"), typically failing with
`500 Domain Not Found`, `TooManyRequests`, or
`Remote end closed connection`.

yum/dnf fallback requires per-repo multi-`baseurl` configuration that a generic
snippet cannot infer reliably; see the yum/dnf section below.

Environment-variable style is also supported:

```dockerfile
ENV PIP_INDEX_URL=http://pip.package-mirror.svc.cluster.local/index/ \
    PIP_TRUSTED_HOST=pip.package-mirror.svc.cluster.local \
    PIP_DEFAULT_TIMEOUT=30 \
    PIP_RETRIES=3 \
    NPM_CONFIG_REGISTRY=http://npm.package-mirror.svc.cluster.local/ \
    NPM_CONFIG_FETCH_RETRIES=3 \
    NPM_CONFIG_FETCH_TIMEOUT=60000
```

## Per-tool usage

### pip

```bash
pip install \
  --index-url http://pip.package-mirror.svc.cluster.local/index/ \
  --trusted-host pip.package-mirror.svc.cluster.local \
  --timeout 30 --retries 3 \
  requests || pip install \
  --index-url https://pypi.org/simple/ \
  --timeout 30 --retries 3 \
  requests
```

Do not use public PyPI as `extra-index-url` for fallback: pip considers all
configured indexes together, so a higher-version public package can shadow an
internal package with the same name. The command-level fallback above queries
one index at a time.

### npm

```bash
npm install \
  --registry http://npm.package-mirror.svc.cluster.local/ \
  --fetch-retries 3 \
  --fetch-timeout 60000 \
  lodash || npm install \
  --registry https://registry.npmjs.org/ \
  --fetch-retries 3 \
  --fetch-timeout 60000 \
  lodash
```

Or as global config:

```bash
npm config set registry http://npm.package-mirror.svc.cluster.local/
npm config set fetch-retries 3
npm config set fetch-timeout 60000
```

npm supports a single registry only; use the `cmd || fallback` pattern above
for fallback.

### apt

Recommended proxy mode: keep real upstreams in sources and use
`Proxy-Auto-Detect` to fall back to direct access when the mirror is
unreachable.

```bash
cat > /usr/local/bin/apt-proxy-detect <<'EOF'
#!/bin/bash
H=apt-yum.package-mirror.svc.cluster.local
if timeout 2 bash -c "</dev/tcp/$H/80" 2>/dev/null; then
  echo "http://$H"
else
  echo DIRECT
fi
EOF
chmod +x /usr/local/bin/apt-proxy-detect

echo 'Acquire::http::Proxy-Auto-Detect "/usr/local/bin/apt-proxy-detect";' \
  > /etc/apt/apt.conf.d/01proxy

cat > /etc/apt/apt.conf.d/80retries <<'EOF'
Acquire::http::Timeout "30";
Acquire::Retries "3";
EOF

apt-get update
```

Never rewrite sources to `apt-yum.package-mirror.svc.cluster.local` and
configure `Acquire::http::Proxy` / `Proxy-Auto-Detect` at the same time — pick
one. `apt-yum` is an HTTP proxy for Debian/Ubuntu upstreams; it is not itself a
Debian repository rooted at `/debian` or `/ubuntu`. Use proxy mode for those
distributions.

Wrong:

```text
# Do not combine these
deb http://apt-yum.package-mirror.svc.cluster.local/debian bookworm main
Acquire::http::Proxy "http://apt-yum.package-mirror.svc.cluster.local";
```

Right:

```text
# Sources keep the real upstream; only the proxy is configured
deb http://deb.debian.org/debian bookworm main
Acquire::http::Proxy-Auto-Detect "/usr/local/bin/apt-proxy-detect";
```

### yum / dnf

```ini
# In the repo section; the second baseurl is the fallback upstream
baseurl=http://apt-yum.package-mirror.svc.cluster.local/centos/$releasever/os/$basearch/
        http://mirror.centos.org/centos/$releasever/os/$basearch/
timeout=30
retries=3
```

yum/dnf `proxy=` accepts a single proxy only; use multiple `baseurl` entries
for fallback.

### apk

```bash
v=v$(cut -d. -f1,2 /etc/alpine-release)
cat > /etc/apk/repositories <<EOF
http://apk.package-mirror.svc.cluster.local/alpine/$v/main
http://apk.package-mirror.svc.cluster.local/alpine/$v/community
http://dl-cdn.alpinelinux.org/alpine/$v/main
http://dl-cdn.alpinelinux.org/alpine/$v/community
EOF
apk update
```

### git

Use the mirror URL explicitly:

```bash
git clone http://git.package-mirror.svc.cluster.local/cache/github.com/opencontainers/runc.git
```

To refresh upstream refs on later `fetch`es, switch the remote to `/fresh/`:

```bash
git -C runc remote set-url origin \
  http://git.package-mirror.svc.cluster.local/fresh/github.com/opencontainers/runc.git
git -C runc fetch --prune --tags
```

Or rewrite only GitHub transparently:

```bash
git config --global \
  url."http://git.package-mirror.svc.cluster.local/github.com/".insteadOf \
  "https://github.com/"
```

Transparent rewriting does not fall back automatically; use a command-level
fallback:

```bash
git_clone_with_fallback() {
  mirror_host=${GIT_MIRROR_HOST:-git.package-mirror.svc.cluster.local}
  url=$1; shift
  case "$url" in http://*|https://*) ;; *) git clone "$url" "$@"; return $? ;; esac
  rest=${url#*://}; host=${rest%%/*}; path=${rest#*/}
  case "$path" in *.git|*.git/*) ;; *) git clone "$url" "$@"; return $? ;; esac
  git clone "http://$mirror_host/cache/$host/$path" "$@" || git clone "$url" "$@"
}

git_clone_with_fallback https://github.com/opencontainers/runc.git /src/runc
```

### Docker registry / BuildKit `FROM`

Docker/BuildKit `FROM` pulls are mirrored via the buildkitd registry mirror
config. Dockerfiles keep their original form:

```dockerfile
FROM ubuntu:22.04
FROM registry.example.com/ns/image:tag
```

BuildKit prefers the matching in-cluster registry cache and falls back to the
origin registry when the cache/mirror is unavailable.

## Fallback when the mirror is unavailable

| Type | Recommendation |
|------|----------------|
| pip | Command-level: install from the mirror, then retry with `--index-url https://pypi.org/simple/`. Do not combine private and public indexes with `extra-index-url`. |
| npm | Command-level: `npm install || npm install --registry https://registry.npmjs.org/`. |
| apt | Proxy mode with `Proxy-Auto-Detect` returning `DIRECT` when unreachable. |
| yum / dnf | List both mirror and upstream in `baseurl`. |
| apk | List both mirror and upstream in `/etc/apk/repositories`. |
| git | Use `git_clone_with_fallback` above. |
| BuildKit | Put the origin registry after the package-mirror entry in the `mirrors` list. |

## Metrics

```bash
curl http://metrics.package-mirror.svc.cluster.local/metrics
```

Common metrics:

| Metric | Meaning |
|--------|---------|
| `package_mirror_backend_up{backend}` | Backend health, 1 = up. |
| `package_mirror_cache_files{backend}` | Cached file count. |
| `package_mirror_cache_used_bytes{backend}` | Cache bytes used. |
| `package_mirror_cache_total_bytes{backend}` | Filesystem capacity of the cache. |
| `package_mirror_apt_cache_hits` / `_misses` | apt request-level hit/miss counts (parsed from acng access log). |
| `package_mirror_apt_bytes_out_total` / `_in_total` | Bytes served to clients / fetched from upstream; byte hit ratio = 1 - in/out. |

## Deployment internals

The package mirror is deployed by `chart/` (shared with buildkit-service,
gated by `packageMirror.enabled`) as three kinds of workloads:

- Main Deployment `package-mirror`: pip, npm, apt/yum/apk, metrics containers.
- Standalone Deployment `package-mirror-git`: git-cache (cache growth / OOM
  cannot take down other backends).
- One StatefulSet per registry mirror (for example
  `package-mirror-registry-dockerhub`; its Service is `registry-dockerhub`),
  derived from the top-level `registries` section of `chart/values.yaml`.

Every backend gets its own Service (own DNS name, port 80).

The chart can create `PodDisruptionBudget`s (`maxUnavailable: 1`) for the main
Deployment, git-cache, and each registry mirror, plus soft hostname-level
`topologySpreadConstraints`. Disable via
`packageMirror.podDisruptionBudget.enabled` and
`packageMirror.topologySpread.enabled`.

The main Deployment hard-avoids nodes running git-cache pods, while git-cache
soft-avoids the main Deployment and other git-cache replicas — disk I/O
isolation in the normal case without permanent Pending during node shortage.

## Components and ports

| Cache | Component | Container port | Service | Service port | Default upstream |
|-------|-----------|----------------|---------|--------------|------------------|
| pip | [proxpi](https://github.com/EpicWink/proxpi) | 5000 | `pip` | 80 | `https://pypi.org/simple/` |
| npm | [verdaccio](https://verdaccio.org/) | 4873 | `npm` | 80 | `https://registry.npmjs.org/` |
| apt + yum | [apt-cacher-ng](https://www.unix-ag.uni-kl.de/~bloch/acng/) (self-built 3.7.5 image) | 3142 | `apt-yum` | 80 | `http://mirrors.edge.kernel.org` |
| apk | Same container as apt/yum | 3142 | `apk` | 80 | `http://dl-cdn.alpinelinux.org/alpine` |
| git | Self-built git-cache (`git clone --mirror` + `git-http-backend`) | 8080 | `git` | 80 | Any valid host (first URL segment) |
| registry | Docker Distribution pull-through cache | 5000 | `registry-<name>` | 80 | Configured in the `registries` section |
| metrics | Python exporter | 9090 | `metrics` | 80 | Local cache dirs and health checks |

apt, yum/dnf, and apk share one apt-cacher-ng container (port 3142): apk is
plain HTTP fetching of static files (`APKINDEX.tar.gz` + `.apk`), cached via
`Remap-alpine` and file-pattern extensions, no extra component needed.

git-cache is not a plain HTTP proxy. Git smart HTTP packs are generated by
`git-upload-pack`, so generic HTTP caches (nginx/squid/varnish) cannot cache
them effectively; the chart uses a small self-built image that maintains local
bare mirrors and serves clone/fetch with `git-http-backend`.

Git upstreams that resolve to loopback, link-local, private, multicast, or
reserved addresses are rejected by default. Set
`packageMirror.git.env.allowPrivateUpstreams=true` only when intentionally
mirroring trusted private Git servers and after restricting client namespaces.

> apt-cacher-ng uses a self-built image
> ([chart/images/apt-cacher-ng/Dockerfile](../chart/images/apt-cacher-ng/Dockerfile),
> Debian trixie + acng 3.7.5): the older `sameersbn/apt-cacher-ng:3.7.4`
> crashes under concurrency (`buffer overflow detected`, exit 139), causing
> sporadic apt timeouts.

> git-cache is also a self-built image
> ([chart/images/git-cache/Dockerfile](../chart/images/git-cache/Dockerfile),
> `python:3.12-alpine` + `git` + a lightweight Python server). Off-the-shelf
> nginx/squid/varnish images cannot maintain pull-through bare mirrors and are
> not drop-in replacements.

> Note: apt-cacher-ng internally uses `select(2)` / `fd_set`; do not blindly
> raise `nofile`. Kubernetes/containerd may default to `1048576`, letting the
> process obtain fds above `FD_SETSIZE(1024)` and crash with
> `bit out of range 0 - FD_SETSIZE on fd_set`. The image entrypoint applies
> `ulimit -n 1024` and the chart caps concurrency via `MaxConThreads: 96` /
> `MaxStandbyConThreads: 32` (active connection threads, not a 96-client
> limit). If acng rejects connections without crashing, raise
> `packageMirror.aptYum.env.maxConThreads` gradually; never set
> `packageMirror.aptYum.env.nofileLimit` above 1024 unless acng no longer uses
> `select(2)`.

## Component choices

- pip → proxpi: proxies the PyPI Simple API and caches package files; lightest
  deployment.
- npm → verdaccio: transparent uplink proxy cache, anonymous read-only,
  publishing disabled, zero extra dependencies.
- apt/yum → apt-cacher-ng: native Debian/Ubuntu and RPM (CentOS/EPEL/Fedora)
  support; one instance serves both apt and yum/dnf.
- git → self-built git-cache: generic HTTP caches cannot cache smart HTTP
  packs; local bare mirrors + `git-http-backend` are required.
- registry → Docker Distribution: pull-through cache for BuildKit `FROM`
  pulls.

## Deployment

For a small evaluation, install package-mirror as its own Helm release with the
quickstart values (separate from buildkit-service; see
[buildctl-batch.md](buildctl-batch.md)):

The apt-cacher-ng and git-cache images are published by the repository release
workflow. Verify both tags before installation:

```bash
docker manifest inspect \
  ghcr.io/inclusionai/buildkit-service/apt-cacher-ng:latest >/dev/null
docker manifest inspect \
  ghcr.io/inclusionai/buildkit-service/git-cache:latest >/dev/null
```

For a fork or before its first release, build and push both images, then pass
their repositories to Helm:

```bash
HELPER_PREFIX='registry.example.com/myorg'
docker buildx build --platform linux/amd64 --push \
  -t "$HELPER_PREFIX/apt-cacher-ng:dev" chart/images/apt-cacher-ng
docker buildx build --platform linux/amd64 --push \
  -t "$HELPER_PREFIX/git-cache:dev" chart/images/git-cache
```

When using those fork images, append these flags to the Helm command below:

```bash
--set-string packageMirror.aptYum.image.repository="$HELPER_PREFIX/apt-cacher-ng" \
--set-string packageMirror.aptYum.image.tag=dev \
--set-string packageMirror.git.image.repository="$HELPER_PREFIX/git-cache" \
--set-string packageMirror.git.image.tag=dev
```

```bash
helm upgrade --install package-mirror ./chart \
  --namespace package-mirror \
  --create-namespace \
  --values ./chart/values-quickstart.yaml \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.namespaceOverride=package-mirror
```

The chart defaults use 20 main replicas, while the quickstart profile overrides
the main, git, and registry workloads to one replica each. Both profiles use
node-local `emptyDir` caches, no dedicated-node affinity, and no PVC. The
quickstart profile additionally tightens cache and API limits. Configure
autoscaling, node-pool scheduling, and persistence explicitly for production.

### Main Deployment autoscaling and repair

Autoscaling is available only for the main `package-mirror` Deployment, which
contains the pip, npm, and apt-yum/apk backends plus their metrics and cache-GC
sidecars. It does not target the standalone git Deployment or any registry
StatefulSet, so their replica counts and storage lifecycle remain unchanged.
The HPA is disabled by default and can be enabled in a production values file:

```yaml
packageMirror:
  autoscaling:
    enabled: true
    minReplicas: 20
    maxReplicas: 40
```

The default HPA uses `autoscaling/v2` container resource metrics. Kubernetes
calculates all six recommendations and uses the largest desired replica count:

| Container | CPU target | Memory target | Request / limit |
|-----------|------------|---------------|-----------------|
| pip | 65% of request | `1536Mi` | `1 CPU` / `2Gi` |
| npm | 65% of request | `6Gi` | `3 CPU` / `8Gi` |
| apt-yum | 65% of request | `3Gi` | `3 CPU` / `4Gi` |

Memory uses absolute `AverageValue` targets so pressure in one backend is not
diluted by the other containers. The targets are 75% of their memory limits,
leaving headroom before OOM. Scale-up can add the larger of 100% or four Pods
per minute. Scale-down waits for a 30-minute stable window and then removes at
most 25% every five minutes, limiting cold-cache churn. The cluster must expose
CPU and memory through the Resource Metrics API (normally Metrics Server or the
managed-cluster equivalent); otherwise the HPA reports unknown metrics and does
not scale.

All backend `resources.requests`, `resources.limits`, and
`autoscaling.metrics.<backend>` values are configurable in an environment
values file. HPA memory targets are independent Kubernetes quantities rather
than a calculated percentage: when changing a backend memory limit, update its
`averageValue` at the same time and normally keep it around 70–75% of the new
limit.

The main Pod has explicit requests equal to limits for pip, npm, apt-yum,
metrics, and cache-GC. Its default scheduling footprint is approximately
`7.35 CPU`, `14.375Gi` memory, and `272Gi` ephemeral storage per Pod. This gives
the scheduler, HPA, and node autoscaler one consistent capacity model and makes
the default main Pod `Guaranteed` QoS. Quickstart overrides the three backend
requests and limits together to keep its smaller profile valid.

Self-repair remains Kubernetes-native: readiness removes an unhealthy backend
Pod from all four main Services, liveness restarts a stuck container, and the
Deployment replaces a failed Pod. Startup probes on pip, npm, and apt-yum allow
up to five minutes for cache checks or initialization before liveness begins;
this prevents slow startup from entering a restart loop. OOM-killed containers
are restarted by kubelet, while the memory HPA is intended to add capacity
before the limit is reached. Enable the existing PDB and topology spread
settings separately when the cluster has enough failure-domain capacity.

Node autoscaling is configured on the cluster or cloud node pool, not by this
chart. The Pod-side inputs are included: concrete resource requests and the
main-only `cluster-autoscaler.kubernetes.io/safe-to-evict: "true"` annotation.
If the main workload uses a dedicated autoscaled pool, select and tolerate it
without moving git or registry Pods:

```yaml
packageMirror:
  deployment:
    nodeSelector:
      workload: package-mirror
    tolerations:
      - key: workload
        operator: Equal
        value: package-mirror
        effect: NoSchedule
```

The HPA creates additional Pods; Pending Pods with satisfiable scheduling
constraints are then the signal for the cluster autoscaler to add nodes. The
node pool must have enough CPU, memory, and especially local ephemeral storage
for the per-Pod footprint, its labels and taints must match the values above,
and its configured maximum must accommodate 40 main Pods. The four main
Services retain `ClientIP` affinity with a 900-second timeout to improve cache
locality while allowing clients to move away from replaced or scaled-down Pods.

`packageMirror.pip.env.cacheSize` must be an integer byte count (no `1GiB`
strings). The default `17179869184` is 16 GiB; keep it well below the
`cache-pip` emptyDir sizeLimit (default 30Gi) because proxpi evicts lazily —
equality guarantees kubelet evicting the whole pod.

For a small single-replica installation, one RWO PVC can persist the main
pip/npm/apt caches and an embedded git cache. Disable standalone git so every
container using the global claim stays in the same Pod:

```bash
helm upgrade --install package-mirror ./chart \
  --namespace package-mirror \
  --create-namespace \
  --values ./chart/values-quickstart.yaml \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.persistence.enabled=true \
  --set packageMirror.git.standalone=false \
  --set packageMirror.persistence.storageClassName=your-storage-class
```

Or reuse an existing PVC:

```bash
helm upgrade --install package-mirror ./chart \
  --namespace package-mirror \
  --create-namespace \
  --values ./chart/values-quickstart.yaml \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.persistence.enabled=true \
  --set packageMirror.git.standalone=false \
  --set packageMirror.persistence.existingClaim=your-existing-pvc
```

For multiple main replicas, use an RWX claim that supports concurrent access,
or keep their caches on independent emptyDirs. Configure standalone git
persistence separately through `packageMirror.git.persistence.*`.

`pod has unbound immediate PersistentVolumeClaims` usually means there is no
default StorageClass, the StorageClass cannot bind for the node pool, or the
default StorageClass is immediate-binding. Start with the default emptyDir; if
you need persistence, prefer a WaitForFirstConsumer StorageClass or bind to a
pre-created PVC.

Registry persistence is intentionally per replica. Enabling it creates a
StatefulSet `volumeClaimTemplate`, so each mirror Pod receives an independent
RWO claim. This avoids requiring RWX storage, but caches are partitioned across
replicas. Size and scale them by disk/network throughput and expected hit rate:

```bash
kubectl get sc

helm upgrade --install package-mirror ./chart \
  --namespace package-mirror \
  --create-namespace \
  --values ./chart/values-quickstart.yaml \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.registry.persistence.enabled=true \
  --set packageMirror.registry.persistence.storageClassName=your-storage-class \
  --set packageMirror.registry.persistence.size=100Gi
```

Registry `volumeClaimTemplates` do not support `existingClaim`. The git cache
does support an existing RWX claim through
`packageMirror.git.persistence.existingClaim`; use it only when the selected
filesystem supports safe concurrent access from all git-cache replicas.

After changing Registry proxy credentials in an existing Secret, increment
`packageMirror.registry.configRevision` to roll the mirror Pods without exposing
a credential-derived checksum in Deployment metadata.

The global `packageMirror.persistence.enabled=true` remains supported (one
shared PVC with subPaths for pip, npm, and apt-yum; embedded git also uses it
when standalone mode is disabled). Per-backend `registry.persistence` and
`git.persistence` are recommended for production. Set global persistence to
false to keep pip/npm/apt-yum on independent local emptyDirs.

The chart sets the Registry StatefulSet claim retention policy to `Delete` for
both deletion and scale-down. Kubernetes therefore deletes the corresponding
PVCs when Helm removes the StatefulSet or a replica is scaled away. Whether the
underlying volume is also deleted depends on the StorageClass reclaim policy.
Inspect the claims and StorageClass before destructive changes:

```bash
kubectl -n package-mirror get pvc
kubectl get storageclass
```

## Cache directories and disk isolation

In-container cache paths:

| Backend | Path | emptyDir volume | Default sizeLimit |
|---------|------|-----------------|-------------------|
| pip | `/var/cache/proxpi` | `cache-pip` | `2Gi` |
| npm | `/verdaccio/storage` | `cache-npm` | `2Gi` |
| apt/yum/apk | `/var/cache/apt-cacher-ng` | `cache-apt-yum` | `2Gi` |
| git | `/var/cache/git-mirror` | `cache-git` | `2Gi` |
| registry (per mirror) | `/var/lib/registry` | `cache-registry` | `2Gi` |
| metrics | `/cache/*` (read-only) | same as above | no writes |

With `packageMirror.persistence.enabled=false`, pip/npm/apt-yum use independent
emptyDirs on the node's local disk. Registry uses emptyDir only when
`packageMirror.registry.persistence.enabled=false`; otherwise it uses its
per-replica claim. The node-local paths are
(`/var/lib/kubelet/pods/<pod-uid>/volumes/kubernetes.io~empty-dir/<volume>`).
Note that `/var`, kubelet, and containerd typically share one node disk, so
package-mirror caches, container writable layers, and image layers compete for
the same device.

Impact of a full disk / reached quota:

- **pip/proxpi**: `PROXPI_CACHE_SIZE` bounds proxpi's own file cache; if the
  emptyDir or node disk fills up, new cache writes fail and requests degrade to
  pass-through or 5xx. Existing cached content usually stays readable.
- **npm/verdaccio**: tarball/metadata writes fail once `/verdaccio/storage` is
  full; npm installs may 5xx and verdaccio can go not-ready under error/memory
  pressure.
- **apt/yum/apk (apt-cacher-ng)**: a full cache disk causes
  `Cannot create cache files`, partial files, `File has unexpected size`, and
  metadata failures — the failure mode most likely to break clients. Keep the
  sizeLimit and client retries.
- **git-cache**: size-based LRU GC (`maxDiskBytes` high-water, default 1Gi)
  normally prevents fill-up; if the disk still fills, new mirror creation and
  refreshes fail while existing bare mirrors stay readable — clients need the
  upstream fallback.
- **registry (Docker Distribution)**: the pull-through cache is cleaned by the
  native proxy TTL scheduler. A full cache disk evicts the whole pod; BuildKit
  `FROM` pulls fall back to the next mirror/origin or fail. Never run generic
  file-deletion scripts against a live `/var/lib/registry` — deleting blobs
  under Distribution desynchronizes metadata and files.
- **Node-level disk pressure**: even with per-emptyDir sizeLimits, the sum of
  pods, image layers, and containerd snapshots can fill `/var` and trigger
  DiskPressure evictions for the whole node.
- **Quota rule**: an emptyDir sizeLimit should be ≤ the container's
  `ephemeral-storage` limit, or container-level eviction fires first. The chart
  keeps them aligned.

## Client access

Per-backend Services (namespace `package-mirror`):

- pip: `pip.package-mirror.svc.cluster.local`
- npm: `npm.package-mirror.svc.cluster.local`
- apt/yum: `apt-yum.package-mirror.svc.cluster.local`
- apk: `apk.package-mirror.svc.cluster.local`
- git: `git.package-mirror.svc.cluster.local`
- metrics: `metrics.package-mirror.svc.cluster.local` (`/metrics`)

All on port 80.

## Configuration overrides

- pip upstream: `--set packageMirror.pip.env.indexUrl=https://pypi.org/simple/`
- npm upstream: `--set packageMirror.npm.env.upstreamUrl=https://registry.npmjs.org/`
- apt upstream: `--set packageMirror.aptYum.env.aptMirror=http://mirrors.edge.kernel.org`
- yum upstream: `--set packageMirror.aptYum.env.centosMirror=...` / `--set packageMirror.aptYum.env.epelMirror=...`
- apk upstream: `--set packageMirror.aptYum.env.alpineMirror=http://dl-cdn.alpinelinux.org/alpine`
- git refresh TTL: `--set packageMirror.git.env.cacheTtlSeconds=300`
- git max active requests per pod: `--set packageMirror.git.env.maxActiveRequests=8`
- git busy-wait timeout: `--set packageMirror.git.env.busyTimeoutSeconds=30`
- git upstream command timeout: `--set packageMirror.git.env.gitTimeoutSeconds=7200` (first-time mirror clones of huge repos can exceed 30 minutes)
- git max concurrent first-time clones: `--set packageMirror.git.env.maxConcurrentClones=2`
- git GC high-water: `--set packageMirror.git.env.maxDiskBytes=128849018880` (default 120Gi; LRU-evicts down to 80%, keep headroom below the emptyDir `gitSizeLimit`)
- registry mirror upstreams/auth: edit the top-level `registries` section of `chart/values.yaml`
- registry pull-through TTL: `--set packageMirror.registry.env.ttl=1h`
- PDB: `--set packageMirror.podDisruptionBudget.enabled=true --set packageMirror.podDisruptionBudget.maxUnavailable=1`
- Topology spread: `--set packageMirror.topologySpread.enabled=true --set packageMirror.topologySpread.whenUnsatisfiable=ScheduleAnyway`

Full verdaccio, apt-cacher-ng, and registry configs are rendered by the chart
into ConfigMaps and mounted read-only.

## Operational constraints

- Scale each backend based on measured CPU, memory, disk, and upstream latency.
  Replicas do not share warm caches;
  `sessionAffinity: ClientIP` keeps a client on one replica to avoid
  cross-replica cache inconsistency.
- pip/proxpi runs a single gunicorn worker with 8 threads (default 2 CPU /
  4Gi). Slow upstream reads can saturate the threads and briefly time out
  `/health`; liveness is relaxed to ~5 minutes of consecutive failures while
  readiness stays sensitive. Scale threads first — do not switch to multiple
  worker processes (proxpi's index/file cache bookkeeping is in-process).
- apt/yum content is only cached when the upstream is HTTP; HTTPS is tunnelled
  via CONNECT and passes through uncached. Default remap upstreams are all
  HTTP.
- apt clients should use **proxy mode** (real sources + proxy config). Combining
  rewritten sources with a proxy produces self-referential requests, visible in
  acng logs as `apt-yum.../debian/... [HTTP error, code: 500]` and client-side
  as `500 Domain Not Found` / `TooManyRequests` / `Remote end closed
  connection`.
- The chart disables caching of self-referential requests via
  `DontCacheRequested` as a server-side backstop; clients must still use proxy
  mode.
- The chart also disables caching of index metadata (`InRelease` / `Release` /
  `Release.gpg` / `dists/*/(Packages|Sources|Contents|Translation-*)` /
  `repodata/*` / `APKINDEX.tar.gz`) to avoid stale-metadata races that surface
  as signature/hash/candidate/HTTP 503/truncated-transfer errors; large package
  files (`.deb` / `.rpm` / `.apk`) are still cached.
- acng `nofile` is capped at 1024 by the image entrypoint, paired with
  `MaxConThreads: 96`. Do not raise nofile; if connections are rejected while
  the process is stable, raise `aptYum.env.maxConThreads` gradually.
- git-cache accepts any valid host
  (`http://git.package-mirror.svc.cluster.local/<host>/<owner>/<repo>.git`). It
  validates host and repo path against traversal, but it can reach arbitrary
  external Git hosts — restrict egress via cluster network policy in
  production.
- git-cache defaults to 4 active requests, 1 concurrent first-time clone, and a
  1Gi memory limit. For large repositories, raise memory/cache limits, lower
  concurrency, or add replicas
  instead of only raising memory.
- For private package publishing, per-team indexes, or auth, switch pip to
  devpi or enable verdaccio authentication/publishing separately.

## Docker regression test

Run `python3 chart/test-package-mirror-remap.py` from the repository root with
Docker and Python 3.12+. Helm is used locally when available, otherwise a public
Helm container renders the chart. The test builds apt-cacher-ng from this
checkout, starts a local changing HTTP upstream that rejects duplicate Host
headers, checks metadata freshness and
`.deb` / `.apk` cache reuse, then removes its containers, network and image tag.
Initial image builds/downloads require public network access; no Kubernetes
cluster, cloud registry credentials or external test harness is required.

To run the same checks against an older chart, use `--chart-dir PATH`. For a
negative-control comparison that also runs the current chart, use
`--baseline-ref REF`; it requires the old chart to reproduce both metadata
failures before checking the current chart.
