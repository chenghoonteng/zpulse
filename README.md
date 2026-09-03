# zpulse

A command-line monitor for ZFS pools. Early stage — it does a few things well and not much else yet.

![status](https://img.shields.io/badge/status-alpha-orange)
![license](https://img.shields.io/badge/license-MIT-blue)

## What it does

- **Pool overview** — health, capacity, and fragmentation for every pool on the host
- **Dataset table** — used/available space and compression ratio per dataset
- **Scrub status** — running scrubs with progress and estimated time remaining
- **Per-process I/O** — which processes are reading and writing which datasets, right now. Optional, off by default, and the one feature that needs more than read-only shell commands. See [Per-process I/O](#per-process-io)
- **Read-only** — zpulse never writes to your pools. It shells out to `zpool` and `zfs` with read-only commands and parses the output

That's the whole feature list right now.

## What it doesn't do (yet)

- Snapshots (listing, creating, pruning)
- ARC statistics
- Alerts / notifications on pool degradation
- Historical metrics — everything is live-only, nothing is stored
- Multi-host support
- Machine-readable output (JSON) for scripting

See the [roadmap](#roadmap) if you want to know what's coming first.

## Usage

```
usage: zpulse [command] [flags]

Commands:
  status    print one snapshot and exit (default)
  watch     redraw the snapshot on an interval until interrupted
  help      show this message

Flags:
  --pools tank,backup   only these pools (default: all)
  --top N               rows in the process I/O table; 0 turns it off
  --sample DURATION     process I/O sample window, status only (default 1s)
  --interval DURATION   redraw interval, watch only (default 10s)
  --no-datasets         skip the dataset table
  --color / --no-color  force colour on or off (default: on when a terminal)
```

`zpulse` on its own is `zpulse status`:

```
zpulse  2026-02-14 09:31:07  ·  all pools

POOLS
  NAME     HEALTH    ALLOC   SIZE  CAP  FRAG  USAGE
  tank     ONLINE    12.4T  21.8T  56%    8%  █████████·······
  backup   ONLINE     6.1T   7.2T  84%    2%  █████████████···
  scratch  DEGRADED   1.7T   1.8T  94%   31%  ███████████████·

SCRUBS
  POOL    PROGRESS            DONE  REMAINING
  tank    █████···········  32.14%   01:12:33

PROCESS I/O  1s sample
  PROCESS              PID  DATASET              READ       WRITE      IOPS
  postgres            1841  tank/db             0 B/s   94.2 MB/s   0r/812w
  qemu-system-x86_64  3320  tank/vm-images  12.8 MB/s  200.0 KB/s  331r/12w
  rsync               9077  backup/home      4.1 MB/s       0 B/s   104r/0w

DATASETS
  NAME             USED  AVAIL  COMPRESS
  tank            12.4T   9.4T     1.42x
  tank/db         4.10T   9.4T     1.02x
  tank/vm-images  8.31T   9.4T     2.11x
  backup/home      6.1T   1.1T     1.90x
```

Colour is on when stdout is a terminal and off when it is a pipe, so `zpulse status | mail -s "zfs" me@example.com` sends plain text. `NO_COLOR=1` and `--no-color` both turn it off explicitly.

`zpulse watch` keeps the same layout on screen and redraws it in place every 10 seconds. Ctrl-C quits.

`status` exits non-zero if a `zpool` or `zfs` command failed — the error is printed above the tables either way.

## Install

Requires Go 1.23+ and a host with ZFS installed. Tested on Ubuntu 24.04. The default build has no external dependencies — it's pure standard library.

```bash
git clone https://github.com/example/zpulse.git
cd zpulse
go build -o zpulse .
./zpulse
```

To include per-process I/O attribution, build with the `ebpf` tag instead. That build links `github.com/cilium/ebpf` (pure Go, no cgo) and embeds a pre-compiled BPF object, so it still needs nothing beyond the Go toolchain:

```bash
go build -tags ebpf -o zpulse .
```

zpulse needs permission to run `zpool status`, `zpool list`, and `zfs list`. On Linux this usually means running as a user with the `zfs` delegation set up, or as root.

## Configuration

There is one config file, `/etc/zpulse/config.toml`, and it's optional:

```toml
refresh_interval = 10   # seconds between redraws in `zpulse watch`
pools = []              # empty = show all pools
top_processes = 10      # rows in the process I/O table; 0 turns it off
```

Flags override the file: `--interval` overrides `refresh_interval`, `--pools` overrides `pools`, `--top` overrides `top_processes`.

## Per-process I/O

Everything else zpulse shows is aggregate. `zpool iostat` will tell you a pool is saturated and the dataset list will tell you which dataset is growing, but neither can tell you *which process* is responsible. This section fills that gap by counting bytes in the kernel, at the VFS layer, keyed by process and dataset.

It is deliberately opt-in, because it is the only part of zpulse that is not just parsing command output:

**Requirements**

- Linux, built with `-tags ebpf`
- Kernel 5.5+ with BTF (`CONFIG_DEBUG_INFO_BTF=y`; check `/sys/kernel/btf/vmlinux` exists). Ubuntu, Debian, Fedora and RHEL 9+ ship this by default
- `CAP_BPF` and `CAP_PERFMON`, or root. With systemd:

  ```ini
  AmbientCapabilities=CAP_BPF CAP_PERFMON
  CapabilityBoundingSet=CAP_BPF CAP_PERFMON
  ```

If any of that is missing, zpulse says so in place of the table and every other section carries on working. It never stops zpulse from running.

**Rates need two samples**

The kernel counters are cumulative, so a rate is the difference between two reads of them. `zpulse status` therefore takes a baseline, waits out `--sample` (one second by default), and reports the difference — a one-shot run is a second slower than it would otherwise be. `zpulse watch` samples in the background on the refresh interval, so its first frame says `sampling` and the numbers appear on the second.

**What it counts**

`read`, `write`, `pread`, `pwrite`, `readv`, `writev` and their variants, by hooking the generic `vfs_*` kernel functions and filtering on the ZFS superblock magic. Two things are *not* counted:

- **mmap'd I/O.** Writes to a mapped region are flushed by kernel writeback, not by the calling process, so there is no process to attribute them to at this layer.
- **`sendfile`/`splice`.** These take a different path through the VFS.

A file server or database using ordinary reads and writes is measured accurately. A workload that is mostly mmap will look quieter than it is.

**Why the VFS layer and not ZFS's own functions**

Hooking ZFS internals (`zpl_iter_read`, `arc_read`, `spa_sync`) would be less code and would catch the mmap path. But ZFS is an out-of-tree module with no stable internal ABI, and those symbols move between releases — the probe would silently stop matching after a ZFS upgrade. The `vfs_*` functions are in-tree and have been stable for years. The mmap gap is the price of that.

**Cost**

The programs run on every `read` and `write` on the host, not just ZFS ones, and leave immediately for other filesystems — roughly 50–100ns per syscall. On a host doing 100k syscalls/second that is well under 1% of one core. The counters live in a fixed 16k-entry kernel hash map; entries for exited processes are pruned as they go idle.

**Licensing**

`bpf/zfsio.c` is GPL-licensed, unlike the rest of zpulse, because the kernel helpers it calls are GPL-only. Only that file and the object built from it; the Go program that loads it is unaffected.

**Rebuilding the BPF object**

`zfsio_bpfel.o` is committed, so normal builds do not need a C toolchain. Regenerate it only after editing `bpf/zfsio.c`:

```bash
# needs clang 11+ and libbpf headers (Debian/Ubuntu: apt install clang libbpf-dev)
go generate -tags ebpf ./...
```

## Roadmap

Rough order of what's next:

1. Snapshot listing per dataset
2. `--json` output, so `zpulse status --json` can feed a monitoring agent
3. ARC stats — reading `/proc/spl/kstat/zfs/arcstats` directly rather than shelling out
4. Container attribution — the process table grouped by cgroup, so `postgres` in a container is named as such
5. Exit codes that reflect pool health, for use straight from cron

No timeline promises. Issues and PRs welcome — the codebase is small enough to read in an afternoon.

## License

MIT, except `bpf/zfsio.c`, which is GPL — see [Per-process I/O](#per-process-io).
