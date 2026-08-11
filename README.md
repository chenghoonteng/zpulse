# zpulse

A web dashboard for ZFS pools. Early stage — it does a few things well and not much else yet.

![status](https://img.shields.io/badge/status-alpha-orange)
![license](https://img.shields.io/badge/license-MIT-blue)

## What it does

- **Pool overview** — health, capacity, and fragmentation for every pool on the host, refreshed every 10 seconds
- **Dataset browser** — list datasets with used/available space and compression ratio
- **Scrub status** — see running scrubs with progress and estimated time remaining
- **Read-only** — zpulse never writes to your pools. It shells out to `zpool` and `zfs` with read-only commands and parses the output

That's the whole feature list right now.

## What it doesn't do (yet)

- Snapshots (listing, creating, pruning)
- ARC statistics and cache hit graphs
- Alerts / notifications on pool degradation
- Historical metrics — everything is live-only, nothing is stored
- Multi-host support
- Authentication — bind it to localhost or put it behind a reverse proxy

See the [roadmap](#roadmap) if you want to know what's coming first.

## Screenshot

```
┌─────────────────────────────────────────────┐
│ tank        ONLINE   12.4T / 21.8T   frag 8%│
│ backup      ONLINE    3.1T /  7.2T   frag 2%│
│ scratch   DEGRADED  801G  /  1.8T   frag 31%│
└─────────────────────────────────────────────┘
```

(Real screenshots coming once the UI stops changing every week.)

## Install

Requires Go 1.21+ and a host with ZFS installed. Tested on Ubuntu 24.04. No external dependencies — it's pure standard library.

```bash
git clone https://github.com/example/zpulse.git
cd zpulse
go build -o zpulse .
./zpulse serve --port 8090
```

Open `http://localhost:8090`.

The dashboard is server-rendered and reloads itself on the refresh interval — no JavaScript required.

The process needs permission to run `zpool status`, `zpool list`, and `zfs list`. On Linux this usually means running as a user with the `zfs` delegation set up, or as root.

## Configuration

There is one config file, `/etc/zpulse/config.toml`, and it's optional:

```toml
port = 8090
refresh_interval = 10   # seconds
pools = []              # empty = show all pools
```

The `--port` flag overrides the `port` value from the config file. Everything else is configured through the file.

## Roadmap

Rough order of what's next:

1. Snapshot listing per dataset
2. ARC stats page
3. Email/webhook alerts on state changes
4. SQLite-backed history with capacity trend graphs

No timeline promises. Issues and PRs welcome — the codebase is small enough to read in an afternoon.

## License

MIT
