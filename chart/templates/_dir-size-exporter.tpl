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
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("METRICS_PORT", "9300"))
PREFIX = os.environ.get("METRIC_PREFIX", "dir")
TTL = float(os.environ.get("DIR_STATS_TTL_SECONDS", "60"))

_CACHE = {}
_LOCK = threading.Lock()
_SCANNING = set()


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
    total = allocated = 0
    seen = set()
    errors = []
    def on_error(error):
        errors.append(error)
    for root, _dirs, names in os.walk(path, onerror=on_error):
        for n in names:
            try:
                st = os.lstat(os.path.join(root, n))
                if st.st_nlink > 1:
                    key = (st.st_dev, st.st_ino)
                    if key in seen:
                        continue
                    seen.add(key)
                total += st.st_size
                allocated += getattr(st, "st_blocks", 0) * 512
            except FileNotFoundError:
                # Cache GC may remove an entry while it is scanned.
                continue
            except OSError as error:
                errors.append(error)
    if errors:
        raise OSError("cache scan incomplete")
    return total, allocated


def scan(path):
    # Only a background sampler walks the directory. A slow scan must neither
    # block /metrics nor start another walk when Prometheus retries a scrape.
    with _LOCK:
        if path in _SCANNING:
            return
        _SCANNING.add(path)
    started = time.monotonic()
    sample = {"success": 0}
    try:
        if not os.path.isdir(path):
            raise FileNotFoundError(path)
        logical, allocated = used_bytes(path)
        sample.update(success=1, logical=logical, allocated=allocated,
                      timestamp=time.time())
    except OSError:
        pass
    finally:
        sample["duration"] = time.monotonic() - started
        with _LOCK:
            previous = _CACHE.get(path, {})
            if "timestamp" not in sample and "timestamp" in previous:
                sample["timestamp"] = previous["timestamp"]
            _CACHE[path] = sample
            _SCANNING.remove(path)


def start_scanners(stop=None):
    stop = stop if stop is not None else threading.Event()
    def sample_forever(path):
        while not stop.is_set():
            scan(path)
            # TTL is a pause after completion, so a walk taking longer than
            # the interval cannot cause overlapping scans or a tight loop.
            stop.wait(max(TTL, 1))
    threads = []
    for path in dict.fromkeys(path for _label, path in TARGETS):
        thread = threading.Thread(target=sample_forever, args=(path,), daemon=True)
        thread.start()
        threads.append(thread)
    return stop, threads


def total_bytes(path):
    try:
        st = os.statvfs(path)
        return st.f_blocks * st.f_frsize
    except OSError:
        return 0


def render():
    out = []
    descriptions = {
        "cache_used_bytes": "Logical file bytes under the watched directory, deduplicated by inode.",
        "cache_allocated_bytes": "Allocated file blocks under the watched directory, deduplicated by inode.",
        "cache_total_bytes": "Capacity of the backing filesystem; NOT a cache budget or per-pod quota.",
        "filesystem_available_bytes": "Backing filesystem bytes available to this process.",
        "filesystem_free_inodes": "Free inodes on the backing filesystem.",
        "cache_scan_success": "Whether the latest completed background scan succeeded; zero until the first success.",
        "cache_scan_timestamp_seconds": "Unix timestamp of the last successful directory scan.",
        "cache_scan_duration_seconds": "Duration of the latest completed background scan.",
        "cache_scan_in_progress": "Whether a background directory scan is in progress.",
        "cache_limit_bytes": "Explicit configured cache volume budget; absent when unknown.",
    }
    for name, help_text in descriptions.items():
        out.extend(["# HELP %s_%s %s" % (PREFIX, name, help_text), "# TYPE %s_%s gauge" % (PREFIX, name)])
    for label, path in TARGETS:
        with _LOCK:
            cached = _CACHE.get(path, {}).copy()
            scanning = path in _SCANNING
        values = {
            "cache_scan_success": cached.get("success", 0),
            "cache_scan_duration_seconds": cached.get("duration", 0),
            "cache_scan_in_progress": int(scanning),
        }
        if cached.get("success"):
            values["cache_used_bytes"] = cached["logical"]
            values["cache_allocated_bytes"] = cached["allocated"]
        if "timestamp" in cached:
            values["cache_scan_timestamp_seconds"] = cached["timestamp"]
        try:
            st = os.statvfs(path)
            values["cache_total_bytes"] = st.f_blocks * st.f_frsize
            values["filesystem_available_bytes"] = st.f_bavail * st.f_frsize
            values["filesystem_free_inodes"] = st.f_favail
        except OSError:
            # Unknown data is never represented as an empty, healthy cache.
            values = {"cache_scan_success": 0}
        limit = os.environ.get("CACHE_LIMIT_BYTES", "0")
        if int(limit) > 0:
            values["cache_limit_bytes"] = int(limit)
        safe_label = label.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
        for name, value in values.items():
            out.append('%s_%s{target="%s"} %s' % (PREFIX, name, safe_label, value))
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
    start_scanners()
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
{{- end -}}
