{{/*
Cache-usage sidecar container for one directory, sharing the metric shape of
the registry mirror and buildkitd exporters. Authored at zero indentation;
callers render it with `nindent 8` under a `containers:` list.

Params:
  root        the top-level chart context ($ / .)
  config      the <backend>.cacheMetrics values map (port/portName/resources/...)
  prefix      METRIC_PREFIX value (for example "package_mirror")
  targets     TARGETS value, "label=path" pairs
  mountName   volume name holding the exported script
  mountPath   volume name holding the watched directory
  subPath     optional subPath when the watched volume is shared
  protocol    container port protocol (defaults to TCP)
*/}}
{{- define "buildkit-service.dirSizeExporterContainer" -}}
{{- $root := .root -}}
{{- $config := .config -}}
{{- $protocol := default "TCP" .protocol -}}
- name: cache-metrics
  image: {{ printf "%s:%s" $root.Values.packageMirror.metrics.image.repository $root.Values.packageMirror.metrics.image.tag }}
  imagePullPolicy: {{ $root.Values.packageMirror.metrics.image.pullPolicy }}
  securityContext:
    {{- toYaml $root.Values.packageMirror.securityContext | nindent 4 }}
  command:
    - python3
    - /etc/package-mirror/dir-size-exporter.py
  env:
    - name: METRICS_PORT
      value: {{ $config.port | quote }}
    - name: METRIC_PREFIX
      value: {{ .prefix | quote }}
    - name: TARGETS
      value: {{ .targets | quote }}
    - name: DIR_STATS_TTL_SECONDS
      value: {{ $config.dirStatsTtlSeconds | quote }}
  ports:
    - name: {{ $config.portName }}
      containerPort: {{ $config.port }}
      protocol: {{ $protocol }}
  livenessProbe:
    httpGet:
      path: /healthz
      port: {{ $config.portName }}
    initialDelaySeconds: 10
    periodSeconds: 30
    timeoutSeconds: 3
    failureThreshold: 3
    successThreshold: 1
  readinessProbe:
    httpGet:
      path: /healthz
      port: {{ $config.portName }}
    initialDelaySeconds: 5
    periodSeconds: 15
    timeoutSeconds: 3
    failureThreshold: 3
    successThreshold: 1
  resources:
    {{- toYaml ($config.resources | default $root.Values.packageMirror.metrics.resources) | nindent 4 }}
  volumeMounts:
    - name: {{ .mountName }}
      mountPath: {{ .mountPath }}
      {{- with .subPath }}
      subPath: {{ . }}
      {{- end }}
      readOnly: true
    - name: {{ include "buildkit-service.packageMirror.configVolumeName" $root }}
      mountPath: /etc/package-mirror/dir-size-exporter.py
      subPath: dir-size-exporter.py
      readOnly: true
{{- end -}}

{{- define "buildkit-service.dirSizeExporterScript" -}}
#!/usr/bin/env python3
"""Generic directory-size metrics exporter (stdlib only)."""
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("METRICS_PORT", "9300"))
PREFIX = os.environ.get("METRIC_PREFIX", "dir")
TTL = float(os.environ.get("DIR_STATS_TTL_SECONDS", "60"))

_CACHE = {}


def parse_targets():
    targets = []
    for item in os.environ.get("TARGETS", "").split(","):
        item = item.strip()
        if not item:
            continue
        if "=" in item:
            label, path = item.split("=", 1)
        else:
            path = item
            label = os.path.basename(item.rstrip("/")) or item
        targets.append((label.strip(), path.strip()))
    return targets


TARGETS = parse_targets()


def used_bytes(path):
    now = time.time()
    cached = _CACHE.get(path)
    if cached and now - cached[0] < TTL:
        return cached[1]
    total = 0
    for root, _dirs, names in os.walk(path):
        for n in names:
            try:
                total += os.lstat(os.path.join(root, n)).st_size
            except OSError:
                pass
    _CACHE[path] = (now, total)
    return total


def total_bytes(path):
    try:
        st = os.statvfs(path)
        return st.f_blocks * st.f_frsize
    except OSError:
        return 0


def render():
    out = []
    out.append("# HELP %s_cache_used_bytes Used bytes under the watched directory." % PREFIX)
    out.append("# TYPE %s_cache_used_bytes gauge" % PREFIX)
    out.append("# HELP %s_cache_total_bytes Total capacity of the filesystem backing the directory." % PREFIX)
    out.append("# TYPE %s_cache_total_bytes gauge" % PREFIX)
    for label, path in TARGETS:
        is_dir = os.path.isdir(path)
        used = used_bytes(path) if is_dir else 0
        total = total_bytes(path) if is_dir else 0
        out.append('%s_cache_used_bytes{target="%s"} %d' % (PREFIX, label, used))
        out.append('%s_cache_total_bytes{target="%s"} %d' % (PREFIX, label, total))
    return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/healthz"):
            self._respond(200, "text/plain", "ok\n")
            return
        if self.path.startswith("/metrics") or self.path == "/":
            try:
                body = render()
            except Exception as exc:
                self._respond(500, "text/plain", "error: %s\n" % exc)
                return
            self._respond(200, "text/plain; version=0.0.4", body)
            return
        self._respond(404, "text/plain", "not found\n")

    def _respond(self, code, ctype, body):
        data = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        pass


def main():
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
{{- end -}}
