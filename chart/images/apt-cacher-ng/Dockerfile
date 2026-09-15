# apt-cacher-ng for package-mirror (apt / yum / apk cache backend).
#
# Replaces sameersbn/apt-cacher-ng:3.7.4-20220421, whose bundled 3.7.4 build
# crashes under concurrency ("*** buffer overflow detected ***", SIGSEGV 139).
# Debian trixie ships apt-cacher-ng 3.7.5+ with those crash fixes.
#
# Build & push:
#   docker build -t <registry>/<namespace>/apt-cacher-ng:3.7.5 chart/images/apt-cacher-ng
#   docker push <registry>/<namespace>/apt-cacher-ng:3.7.5
# Then set aptYum.image.repository/tag in values.yaml accordingly.
ARG BASE_IMAGE=debian:trixie-slim
FROM ${BASE_IMAGE}

RUN apt-get update \
    && apt-get install -y --no-install-recommends apt-cacher-ng ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/cache/apt-cacher-ng /var/log/apt-cacher-ng \
    && chown -R apt-cacher-ng:apt-cacher-ng /var/cache/apt-cacher-ng /var/log/apt-cacher-ng

COPY entrypoint.sh /usr/local/bin/apt-cacher-ng-entrypoint

RUN chmod +x /usr/local/bin/apt-cacher-ng-entrypoint

EXPOSE 3142

# ForeGround=1 keeps apt-cacher-ng as PID 1; every other setting comes from
# /etc/apt-cacher-ng/*.conf (the chart mounts zz-package-mirror.conf there).
CMD ["/usr/local/bin/apt-cacher-ng-entrypoint"]
