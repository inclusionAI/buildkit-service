#!/bin/sh
# Delegate controllers only inside this bounded, private worker cgroup namespace.
set -eu
IFS= read -r task_membership < /proc/self/cgroup
[ "$task_membership" = '0::/' ] || { echo 'Expected a fresh private cgroup v2 namespace' >&2; exit 1; }
IFS= read -r task_limit < /sys/fs/cgroup/memory.max
[ "$task_limit" = "$OBS_WORKER_MEMORY_BYTES" ] || { echo 'Worker cgroup memory limit does not match this local profile' >&2; exit 1; }
# The namespace root must contain no processes before domain controllers can
# be delegated. All future buildkitd/runc children inherit the daemon leaf.
mkdir -p /sys/fs/cgroup/daemon
printf '%s\n' "$$" > /sys/fs/cgroup/daemon/cgroup.procs
while IFS= read -r task_pid; do
    [ -z "$task_pid" ] || { echo 'Unexpected process in worker cgroup root' >&2; exit 1; }
done < /sys/fs/cgroup/cgroup.procs
printf '+cpu +memory +pids\n' > /sys/fs/cgroup/cgroup.subtree_control
exec buildkitd "$@"
