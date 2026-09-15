#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

common_args=(--set buildctlDaemon.auth.token=ci-lint-placeholder)

fail() {
  echo "package-mirror maven render test: $*" >&2
  exit 1
}

assert_contains() {
  local needle="$1"
  local file="$2"
  grep -Fq -- "$needle" "$file" || fail "expected '$needle' in $file"
}

assert_absent() {
  local needle="$1"
  local file="$2"
  if grep -Fq -- "$needle" "$file"; then
    fail "did not expect '$needle' in $file"
  fi
}

helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.maven.persistence.enabled=true \
  --show-only templates/package-mirror-maven-deployment.yaml > "$tmp_dir/deployment.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-maven-service.yaml > "$tmp_dir/service.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-maven-configmap.yaml > "$tmp_dir/configmap.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.maven.persistence.enabled=true \
  --show-only templates/package-mirror-pvc.yaml > "$tmp_dir/pvc.yaml"

# The shared-configuration file is the whole point of the backend: proxied
# artifact storage must be ON (Reposilite defaults it off), the metadata TTL must
# be a bounded value rather than 0 (0 refetches every request, which is the
# upstream traffic we are removing), and the resolution cache must be sized for
# a broad upstream such as Maven Central.
assert_contains '"store": true' "$tmp_dir/configmap.yaml"
assert_contains '"metadataMaxAge": 600' "$tmp_dir/configmap.yaml"
assert_contains '"resolutionCacheMaxEntries": 2048' "$tmp_dir/configmap.yaml"
assert_contains '"reference": "https://repo.maven.apache.org/maven2/"' "$tmp_dir/configmap.yaml"
assert_contains '"type": "fs"' "$tmp_dir/configmap.yaml"
assert_contains '"maven"' "$tmp_dir/configmap.yaml"

# Reposilite is driven by startup parameters plus a read-only ConfigMap link;
# no database bootstrap or management token is involved.
assert_contains '--shared-configuration=/etc/package-mirror/configuration.shared.json' "$tmp_dir/deployment.yaml"
assert_contains 'dzikoysk/reposilite:3.6.3' "$tmp_dir/deployment.yaml"
assert_contains 'mountPath: /app/data' "$tmp_dir/deployment.yaml"
assert_contains 'readOnly: true' "$tmp_dir/deployment.yaml"
assert_contains 'claimName: package-mirror-maven' "$tmp_dir/deployment.yaml"
assert_contains 'name: maven' "$tmp_dir/service.yaml"
assert_contains 'targetPort: maven' "$tmp_dir/service.yaml"
assert_contains 'kind: PersistentVolumeClaim' "$tmp_dir/pvc.yaml"

# Repository data must not be reachable by the generic cache-GC janitor, which
# only understands plain file caches and would corrupt Reposilite's data dir.
assert_absent "CACHE_GC_TARGETS" "$tmp_dir/deployment.yaml"

# Disabling the backend removes every maven object.
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.maven.enabled=false > "$tmp_dir/disabled.yaml"
assert_absent "package-mirror-maven" "$tmp_dir/disabled.yaml"
assert_absent "dzikoysk/reposilite" "$tmp_dir/disabled.yaml"

# Persistence is off by default (matching the other backends), falling back to
# an emptyDir bounded by mavenSizeLimit; enabling it switches to a PVC.
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-maven-deployment.yaml > "$tmp_dir/emptydir.yaml"
assert_contains "emptyDir:" "$tmp_dir/emptydir.yaml"
assert_contains "sizeLimit: 100Gi" "$tmp_dir/emptydir.yaml"
assert_absent "persistentVolumeClaim" "$tmp_dir/emptydir.yaml"

# (rendered in full: with persistence off the PVC template emits no document,
# which --show-only rejects)
helm template package-mirror-test "$chart_dir" "${common_args[@]}" > "$tmp_dir/no-pvc.yaml"
assert_absent "kind: PersistentVolumeClaim" "$tmp_dir/no-pvc.yaml"

# A quota is a repository storage provider property, not an env var.
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set-string packageMirror.maven.env.quota=80Gi \
  --show-only templates/package-mirror-maven-configmap.yaml > "$tmp_dir/quota.yaml"
assert_contains '"quota": "80Gi"' "$tmp_dir/quota.yaml"

# The emptyDir sizeLimit must not exceed the container's ephemeral-storage
# limit, or container-level eviction fires before the volume is full (the
# "quota rule" in docs/package-mirror.md). Maven artifacts are large, so this
# pairing is easy to get wrong when only one side is tuned. The default profile
# and the quickstart profile are both checked.
check_maven_size_pairing() {
  local values_file="$1"
  local size_limit ephemeral_limit
  size_limit="$(sed -n 's/^ *mavenSizeLimit: *//p' "$values_file" | head -1)"
  # The limits value, not requests: the container is evicted on its limit.
  ephemeral_limit="$(awk '
    /^  maven:/ { inmaven=1; next }
    /^  [a-zA-Z]/ { inmaven=0 }
    inmaven && /^    resources:/ { inres=1; next }
    inmaven && inres && /^    [a-zA-Z]/ { inres=0 }
    inres && /^      limits:/ { inlim=1; next }
    inres && inlim && /^      [a-zA-Z]/ { inlim=0 }
    inlim && /ephemeral-storage:/ { print $2; exit }
  ' "$values_file")"
  [[ -n "$size_limit" && -n "$ephemeral_limit" ]] \
    || fail "could not read maven sizeLimit/ephemeral-storage from $values_file"
  to_bytes() {
    local v="${1%\"}"; v="${v%\"}"
    case "$v" in
      *Gi) echo $(( ${v%Gi} * 1024 * 1024 * 1024 )) ;;
      *Mi) echo $(( ${v%Mi} * 1024 * 1024 )) ;;
      *) fail "unexpected size unit in '$1' ($values_file)" ;;
    esac
  }
  (( $(to_bytes "$size_limit") <= $(to_bytes "$ephemeral_limit") )) \
    || fail "mavenSizeLimit ($size_limit) exceeds maven ephemeral-storage limit ($ephemeral_limit) in $values_file"
}

check_maven_size_pairing "$chart_dir/values.yaml"
check_maven_size_pairing "$chart_dir/values-quickstart.yaml"

# The shared configuration is mounted with subPath, so Helm updating the
# ConfigMap alone does NOT reach a running container. Without a checksum in the
# Pod template, `helm upgrade` with new configuration leaves Reposilite serving
# the old file. Each configurable repository setting must therefore change the
# rendered Pod template.
render_maven_deployment() {
  helm template package-mirror-test "$chart_dir" "${common_args[@]}" "$@" \
    --show-only templates/package-mirror-maven-deployment.yaml
}

render_maven_deployment > "$tmp_dir/base-deployment.yaml"
assert_contains "checksum/config:" "$tmp_dir/base-deployment.yaml"

# Numbers, strings and booleans each use the --set form Helm parses to the
# matching type (--set-string would turn false into the truthy string "false").
for setting in \
  "packageMirror.maven.metadataMaxAge=9999" \
  "packageMirror.maven.resolutionCacheMaxEntries=512"; do
  render_maven_deployment --set "$setting" > "$tmp_dir/changed.yaml"
  if diff -q "$tmp_dir/base-deployment.yaml" "$tmp_dir/changed.yaml" >/dev/null; then
    fail "changing $setting does not change the maven Pod template, so the Pod will not roll"
  fi
done

for setting in \
  "packageMirror.maven.upstreamUrl=https://alt.example.com/m2/" \
  "packageMirror.maven.repositoryId=internal"; do
  render_maven_deployment --set-string "$setting" > "$tmp_dir/changed.yaml"
  if diff -q "$tmp_dir/base-deployment.yaml" "$tmp_dir/changed.yaml" >/dev/null; then
    fail "changing $setting does not change the maven Pod template, so the Pod will not roll"
  fi
done

render_maven_deployment --set packageMirror.maven.store=false > "$tmp_dir/changed.yaml"
if diff -q "$tmp_dir/base-deployment.yaml" "$tmp_dir/changed.yaml" >/dev/null; then
  fail "disabling packageMirror.maven.store does not change the maven Pod template, so the Pod will not roll"
fi

# Helm renders numeric 0 as empty, so `default` would silently fall back to the
# shared replica count and scaling this backend to zero would start Pods.
render_rendered_replicas() {
  render_maven_deployment "$@" | sed -n 's/^  replicas: //p' | head -1
}
[[ "$(render_rendered_replicas)" == "1" ]] \
  || fail "expected the default maven replica count to be 1, got '$(render_rendered_replicas)'"
[[ "$(render_rendered_replicas --set packageMirror.maven.replicaCount=0)" == "0" ]] \
  || fail "an explicit maven replicaCount=0 must render replicas: 0, got '$(render_rendered_replicas --set packageMirror.maven.replicaCount=0)'"
[[ "$(render_rendered_replicas --set packageMirror.maven.replicaCount=3)" == "3" ]] \
  || fail "maven replicaCount=3 must render replicas: 3"

# A ReadWriteOnce cache volume cannot attach to a replacement Pod while the
# outgoing one holds it, so the surging default strategy must not be used when
# persistence is on. Overriding explicitly stays possible.
render_rendered_strategy() {
  render_maven_deployment "$@" | sed -n '/^  strategy:/,/^  selector:/p' | sed -n 's/^    type: //p' | head -1
}
[[ "$(render_rendered_strategy --set packageMirror.maven.persistence.enabled=true)" == "Recreate" ]] \
  || fail "persistence-enabled maven must default to the Recreate strategy"
[[ "$(render_rendered_strategy --set packageMirror.maven.persistence.enabled=false)" == "RollingUpdate" ]] \
  || fail "non-persistent maven must keep the shared RollingUpdate strategy"
[[ "$(render_rendered_strategy --set packageMirror.maven.persistence.enabled=true --set packageMirror.maven.strategy.type=RollingUpdate)" == "RollingUpdate" ]] \
  || fail "an explicit maven.strategy must override the topology-aware default"

echo "package-mirror maven render test: PASS"