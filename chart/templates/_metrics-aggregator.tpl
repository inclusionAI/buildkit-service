{{/*
Self-contained metrics aggregator (stdlib-only Python). Scrapes each component
endpoint and re-exposes a single unified /metrics in the buildkit-service
namespace. Config is a JSON file mounted at CONFIG_PATH describing scrape jobs.
Only single { } braces are used so Helm leaves the body untouched.
*/}}
{{- define "buildkit-service.metricsAggregatorScript" -}}
#!/usr/bin/env python3
"""Self-contained metrics aggregator for buildkit-service."""
import json
import os
import socket
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CONFIG_PATH = os.environ.get("CONFIG_PATH", "/etc/metrics-aggregator/targets.json")
PORT = int(os.environ.get("METRICS_PORT", "9090"))
TTL = float(os.environ.get("SCRAPE_TTL_SECONDS", "15"))
TIMEOUT = float(os.environ.get("SCRAPE_TIMEOUT_SECONDS", "5"))

_cache = {"ts": 0.0, "body": ""}


def http_get(url):
    with urllib.request.urlopen(url, timeout=TIMEOUT) as r:
        return r.read().decode("utf-8", "replace")


def resolve(host):
    ips = set()
    try:
        for info in socket.getaddrinfo(host, None):
            ips.add(info[4][0])
    except OSError:
        pass
    return sorted(ips)


def _split_labels(s):
    out, cur, q = [], "", False
    for ch in s:
        if ch == '"':
            q = not q
            cur += ch
        elif ch == "," and not q:
            out.append(cur)
            cur = ""
        else:
            cur += ch
    if cur:
        out.append(cur)
    return out


def parse_prom(text):
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        try:
            if "{" in line:
                name, rest = line.split("{", 1)
                labels_str, val = rest.rsplit("}", 1)
                labels = {}
                for part in _split_labels(labels_str):
                    if "=" in part:
                        k, v = part.split("=", 1)
                        labels[k.strip()] = v.strip().strip('"')
                value = float(val.strip().split()[0])
            else:
                name, val = line.split(None, 1)
                labels = {}
                value = float(val.strip().split()[0])
        except (ValueError, IndexError):
            continue
        yield name.strip(), labels, value


def job_dirsize_sum(job, out, up):
    prefix = job["prefix"]
    used = 0.0
    total = 0.0
    ok = 0
    succeeded = 0
    ips = resolve(job["dns"]) or [job["dns"]]
    for ip in ips:
        url = "http://%s:%d%s" % (ip, job["port"], job.get("path", "/metrics"))
        try:
            text = http_get(url)
            succeeded += 1
        except Exception:
            continue
        for name, _labels, value in parse_prom(text):
            if name == prefix + "_cache_used_bytes":
                used += value
            elif name == prefix + "_cache_total_bytes":
                total += value
    out.append("# HELP %s_cache_used_bytes Aggregated cache used bytes." % prefix)
    out.append("# TYPE %s_cache_used_bytes gauge" % prefix)
    out.append("%s_cache_used_bytes %d" % (prefix, int(used)))
    out.append("# HELP %s_cache_total_bytes Aggregated cache total capacity." % prefix)
    out.append("# TYPE %s_cache_total_bytes gauge" % prefix)
    out.append("%s_cache_total_bytes %d" % (prefix, int(total)))
    up[job.get("name", prefix)] = int(succeeded == len(ips))
    target = job.get("name", prefix)
    out.append('metrics_aggregator_targets_expected{target="%s"} %d' % (target, len(ips)))
    out.append('metrics_aggregator_targets_scraped{target="%s"} %d' % (target, succeeded))


def job_passthrough(job, out, up):
    match = job["match_prefix"]
    ok = 0
    try:
        text = http_get(job["url"])
        ok = 1
    except Exception:
        text = ""
    for line in text.splitlines():
        s = line.strip()
        if not s:
            continue
        if s.startswith("#"):
            parts = s.split()
            if len(parts) >= 3 and parts[2].startswith(match):
                out.append(s)
        elif s.split("{")[0].split()[0].startswith(match):
            out.append(s)
    up[job["name"]] = ok


def job_registry(job, out, up):
    name = job["name"]
    used = 0.0
    total = 0.0
    cache_ok = 0
    # Per-pod emptyDir caches differ per replica, so sum them. A shared PVC makes
    # every replica report the same data, so take the first replica's view.
    cache_agg = job.get("cache_agg", "sum")
    ips = resolve(job["cache_dns"]) or [job["cache_dns"]]
    for ip in ips:
        url = "http://%s:%d/metrics" % (ip, job.get("cache_port", 9300))
        try:
            text = http_get(url)
        except Exception:
            continue
        cache_ok = 1
        for mname, _labels, value in parse_prom(text):
            if mname == "registry_mirror_cache_used_bytes":
                used += value
            elif mname == "registry_mirror_cache_total_bytes":
                total += value
        if cache_agg == "first":
            break
    out.append('registry_mirror_cache_used_bytes{mirror="%s"} %d' % (name, int(used)))
    out.append('registry_mirror_cache_total_bytes{mirror="%s"} %d' % (name, int(total)))

    req_total = 0.0
    req_failed = 0.0
    req_ok = 0
    dips = resolve(job["debug_dns"]) or [job["debug_dns"]]
    for ip in dips:
        url = "http://%s:%d/metrics" % (ip, job.get("debug_port", 5001))
        try:
            text = http_get(url)
            req_ok = 1
        except Exception:
            continue
        for mname, labels, value in parse_prom(text):
            if mname == "registry_http_requests_total":
                req_total += value
                if labels.get("code", "").startswith("5"):
                    req_failed += value
    out.append('registry_mirror_requests_total{mirror="%s"} %d' % (name, int(req_total)))
    out.append('registry_mirror_requests_failed_total{mirror="%s"} %d' % (name, int(req_failed)))
    up["registry-cache-" + name] = cache_ok
    up["registry-requests-" + name] = req_ok


def render():
    with open(CONFIG_PATH) as f:
        cfg = json.load(f)
    out = []
    up = {}
    reg_seen = False
    for job in cfg.get("jobs", []):
        kind = job.get("kind")
        if kind == "dirsize_sum":
            job_dirsize_sum(job, out, up)
        elif kind == "passthrough":
            job_passthrough(job, out, up)
        elif kind == "registry":
            if not reg_seen:
                out.append("# HELP registry_mirror_cache_used_bytes Registry mirror cache used bytes.")
                out.append("# TYPE registry_mirror_cache_used_bytes gauge")
                out.append("# HELP registry_mirror_cache_total_bytes Registry mirror cache total capacity.")
                out.append("# TYPE registry_mirror_cache_total_bytes gauge")
                out.append("# HELP registry_mirror_requests_total Registry mirror requests served.")
                out.append("# TYPE registry_mirror_requests_total counter")
                out.append("# HELP registry_mirror_requests_failed_total Registry mirror 5xx responses.")
                out.append("# TYPE registry_mirror_requests_failed_total counter")
                reg_seen = True
            job_registry(job, out, up)
    out.append("# HELP metrics_aggregator_scrape_up Whether each scrape target responded (1=up).")
    out.append("# TYPE metrics_aggregator_scrape_up gauge")
    for target, ok in sorted(up.items()):
        out.append('metrics_aggregator_scrape_up{target="%s"} %d' % (target, ok))
    return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/healthz"):
            self._respond(200, "text/plain", "ok\n")
            return
        if self.path.startswith("/metrics") or self.path == "/":
            now = time.time()
            if now - _cache["ts"] >= TTL or not _cache["body"]:
                try:
                    _cache["body"] = render()
                    _cache["ts"] = now
                except Exception as exc:
                    self._respond(500, "text/plain", "error: %s\n" % exc)
                    return
            self._respond(200, "text/plain; version=0.0.4", _cache["body"])
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
