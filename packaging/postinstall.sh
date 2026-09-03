#!/bin/sh
# Grant zpulse the capabilities its per-process I/O attribution needs.
#
# Everything else zpulse does is read-only shell-outs to zpool and zfs, which
# need no privileges at all. Loading the eBPF programs needs CAP_BPF, and
# attaching them to the vfs_* probes needs CAP_PERFMON. Those two are a far
# smaller grant than setuid root, and are the only reason this script exists.
#
# A failure here is deliberately not fatal. zpulse reports a missing capability
# in place of the process table and every other section carries on working, so
# installing on a filesystem without xattr support, or inside a container that
# forbids the grant, should degrade the feature rather than fail the install.
set -e

binary=/usr/bin/zpulse

if ! command -v setcap >/dev/null 2>&1; then
	echo "zpulse: setcap not found; per-process I/O will need root." >&2
	exit 0
fi

# cap_bpf and cap_perfmon were split out of cap_sys_admin in Linux 5.8 and need
# libcap 2.33+ to name. On anything older setcap fails here and zpulse falls
# back to saying so at runtime.
if ! setcap cap_bpf,cap_perfmon+ep "$binary" 2>/dev/null; then
	echo "zpulse: could not grant cap_bpf,cap_perfmon on $binary." >&2
	echo "        Per-process I/O will need root until you run:" >&2
	echo "          setcap cap_bpf,cap_perfmon+ep $binary" >&2
fi

exit 0
