# zpulse

A command-line monitor for ZFS pools. Early stage — it does a few things well and not much else yet.

![status](https://img.shields.io/badge/status-alpha-orange)
![license](https://img.shields.io/badge/license-MIT-blue)

## What it does

- **Pool overview** — health, capacity, and fragmentation for every pool on the host
- **Dataset table** — used/available space and compression ratio per dataset
- **Scrub status** — running scrubs with progress and estimated time remaining
- **Per-process I/O** — which processes are reading and writing which datasets, right now. Included in the release build, and the one feature that needs more than read-only shell commands: it wants `CAP_BPF`/`CAP_PERFMON`, and says so instead of it if it doesn't have them. See [Per-process I/O](#per-process-io)
- **JSON output** — `--json` prints the same snapshot as one object for a monitoring agent, or one object per interval in `watch`. See [JSON output](#json-output)
- **Read-only** — zpulse never writes to your pools. It shells out to `zpool` and `zfs` with read-only commands and parses the output

That's the whole feature list right now.

## What it doesn't do (yet)

- Snapshots (listing, creating, pruning)
- ARC statistics
- Alerts / notifications on pool degradation
- Historical metrics — everything is live-only, nothing is stored
- Multi-host support

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
  --json                machine-readable output instead of tables
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

## JSON output

`--json` replaces the tables with one JSON object, for a monitoring agent or a script:

```bash
zpulse status --json | jq '.pools[] | select(.capacity_percent > 80) | .name'
```

```json
{
  "time": "2026-02-14T09:31:07+01:00",
  "pools": [
    {
      "name": "tank",
      "health": "ONLINE",
      "size": "21.8T",
      "size_bytes": 23969353485516,
      "alloc": "12.4T",
      "alloc_bytes": 13633944184422,
      "capacity": "56%",
      "capacity_percent": 56,
      "fragmentation": "8%",
      "fragmentation_percent": 8
    }
  ],
  "scrubs": [
    {
      "pool": "tank",
      "percent_done": 32.14,
      "time_remaining": "01:12:33",
      "time_remaining_seconds": 4353
    }
  ],
  "datasets": [
    {
      "name": "tank/db",
      "used": "4.10T",
      "used_bytes": 4507997673881,
      "avail": "9.4T",
      "avail_bytes": 10335409301094,
      "compress_ratio": 1.02
    }
  ],
  "process_io": {
    "available": true,
    "window_seconds": 1,
    "processes": [
      {
        "pid": 1841,
        "command": "postgres",
        "dataset": "tank/db",
        "read_bytes_per_sec": 0,
        "write_bytes_per_sec": 98784247,
        "read_iops": 0,
        "write_iops": 812
      }
    ]
  },
  "pools_filter": []
}
```

Things worth knowing about the schema:

- **Every ZFS value appears twice**: the string ZFS printed (`"size": "21.8T"`) and a number derived from it (`"size_bytes"`). The numbers carry ZFS's own precision — three significant digits on sizes — so they are right to within a rounding step, not to the byte. Percentages and the per-process counters are exact.
- **A derived field is absent, not zero, when it isn't known.** A pool whose `fragmentation` is `"-"` has no `fragmentation_percent` key at all, so a `0` in the output always means a measured zero.
- **Lists are always lists.** `pools`, `scrubs`, `datasets` and `processes` are `[]` when empty, never `null`.
- **`error`** appears only when a `zpool` or `zfs` command failed; the exit code is non-zero in that case too, and whatever did succeed is still in the object.
- **`process_io.available`** is `false` when attribution is off or unavailable, with the reason in `process_io.error` — a missing `CAP_BPF`, a build without the `ebpf` tag, or `--top 0`. That is not a failed run and does not affect the exit code.

`zpulse watch --json` streams instead: one compact object per line, per interval (newline-delimited JSON), with no cursor tricks even on a terminal.

```bash
zpulse watch --json --interval 30s | while read -r line; do ...; done
```

The first frame has `"warming_up": true` on `process_io`: process rates are differences between two samples, so the first one is only a baseline and its empty table means "not yet", not "nothing happened".

## Install

Released binaries and the `.deb` are built with `-tags ebpf`, so per-process I/O is included. The `.deb` also grants the binary the two capabilities that feature needs, so an apt install works out of the box; a tarball needs one `setcap`, see [Per-process I/O](#per-process-io).

To build from source you need Go 1.23+ and a host with ZFS. Tested on Ubuntu 24.04.

```bash
git clone https://github.com/example/zpulse.git
cd zpulse
go build -tags ebpf -o zpulse .
./zpulse
```

The `ebpf` tag is what the release build uses. It links `github.com/cilium/ebpf` (pure Go, no cgo) and embeds a pre-compiled BPF object, so it needs nothing beyond the Go toolchain and the binary stays static.

Dropping the tag builds against the standard library alone, with no third-party dependencies at all. Everything works except the process table, which says it was left out:

```bash
go build -o zpulse .
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

It sits behind a build tag because it is the only part of zpulse that is not just parsing command output. Released binaries are built with that tag; a plain `go build` leaves it out.

**Requirements**

- Linux, built with `-tags ebpf` (the release build; see [Install](#install))
- Kernel 5.5+ with BTF (`CONFIG_DEBUG_INFO_BTF=y`; check `/sys/kernel/btf/vmlinux` exists). Ubuntu, Debian, Fedora and RHEL 9+ ship this by default
- `CAP_BPF` and `CAP_PERFMON`, or root. Those two were split out of `CAP_SYS_ADMIN` in Linux 5.8 — on a 5.5–5.7 kernel it is root or `CAP_SYS_ADMIN`.

  The `.deb` grants these on install with `setcap cap_bpf,cap_perfmon+ep /usr/bin/zpulse`, so an apt install needs nothing further. For a binary you built or unpacked yourself, run that same command once:

  ```bash
  sudo setcap cap_bpf,cap_perfmon+ep ./zpulse
  ```

  Or, if you run it from a systemd unit:

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
2. ARC stats — reading `/proc/spl/kstat/zfs/arcstats` directly rather than shelling out
3. Container attribution — the process table grouped by cgroup, so `postgres` in a container is named as such
4. Exit codes that reflect pool health, for use straight from cron

No timeline promises. Issues and PRs welcome — the codebase is small enough to read in an afternoon.

## License

MIT, except `bpf/zfsio.c`, which is GPL — see [Per-process I/O](#per-process-io).
