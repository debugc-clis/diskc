# diskc

Find what is filling your Linux disk, how fast it is growing, and which process is writing it.

`diskc` is a read-only, SRE-focused CLI that combines the usual `df`, `df -i`, `du`, `find`, `lsof`, `ps`, and `/proc` investigation into one workflow.

## Quick Start

Build and run from source:

```bash
go build -o diskc ./cmd/diskc
./diskc
```

The default command scans `/`, samples files and device activity for three seconds, and reports byte pressure, inode pressure, largest files/directories, fastest-growing files, estimated time-to-full, writable processes, and Linux storage diagnostics. Add `--deleted` to include deleted-open file detection.

`diskc` is read-only and does not require `sudo` or root privileges. Linux `/proc` permissions may limit process details for other users.

## Example Output

```text
$ ./diskc
Disk pressure: WARNING  92.4% used (38.0 GB free)
Inodes: 41.0% used  |  /
Trend: +2.4 GB/hour
Estimated filesystem full: ~35 minutes

Largest files
   74.8 GB  /var/log/payment/app.log  (log)  ↑ 18.2 MB/s  [1 writer(s)]
   42.1 GB  /tmp/upload-883120.bin   (temporary/cache)  ↑ 11.8 MB/s

Active writers
/var/log/payment/app.log
└─ PID 21819  payment-api
   ├─ exe      /opt/payment/bin/payment-api
   └─ service  payment.service

Potential issues
/var/log/payment/app.log
├─ rapid growth: 65.5 GB/hour
└─ filesystem full in ~35 minutes

System diagnostics
[info] nvme0n1: 18.4% busy, 2.1 MB/s read, 38.7 MB/s written, 40 read IOPS, 310 write IOPS
```

## What `diskc` checks automatically

- **Inode exhaustion:** reports inode utilization alongside byte utilization, so a filesystem full of tiny files is visible even when capacity remains.
- **Small, fast-growing files:** samples every scanned regular file, then reports fastest growth separately from largest files.
- **Disk I/O pressure:** samples the backing block device during the growth window and reports busy time, throughput, and IOPS when activity is observed. It also warns when Linux I/O pressure stall information is high.
- **Mount and storage health:** flags read-only mounts, degraded Linux software RAID, container overlay filesystems, and Btrfs/ZFS copy-on-write filesystems where snapshots can consume capacity beyond ordinary file totals.
- **Container context:** when Linux cgroups expose a Docker, containerd, or CRI-O container ID for a writer, includes it with the process attribution.

`diskc` identifies copy-on-write and overlay risks but does not yet measure individual snapshot, image, or volume usage. It does not replace device-vendor SMART/NVMe tooling.

## Common Commands

Inspect database configuration and database-related disk usage:

```bash
./diskc db
./diskc db --sample 5s
./diskc db --json
```

`diskc db` discovers PostgreSQL, MySQL/MariaDB, Redis, ClickHouse, and MongoDB from Linux process names, common configuration files, and standard data directories. It reports configuration paths, relevant storage settings, data paths, database processes, filesystem pressure, large database files, and growth rates. It does not connect to the database or modify any data.

Inspect a directory or filesystem:

```bash
./diskc /var
./diskc /var/log --top 50 --depth 6
```

Measure growth over a custom window:

```bash
./diskc /var --sample 5s
```

Inspect multiple filesystems explicitly:

```bash
./diskc / /data /backup --sample 5s
```

Discover physical filesystems automatically:

```bash
./diskc --all --sample 5s
```

Virtual filesystems such as `/proc`, `/sys`, `tmpfs`, and cgroups are excluded. Separate filesystem results are never mixed.

Inspect Docker storage on the host:

```bash
./diskc docker
./diskc docker --sample 5s --top 50
./diskc docker --json > docker-disk-report.json
```

`diskc docker` checks common rootful and rootless Docker locations, including Docker's `data-root`, containerd storage, Docker logs, and BuildKit cache. It reports configuration and socket paths, Docker processes, filesystem pressure, largest Docker files/directories, and growth rate. It reads local files and `/proc`; it does not call the Docker API, connect to containers, delete data, or require root.

Example Docker output:

```text
Docker storage
Runtime: docker
Config: /etc/docker/daemon.json
Data root: /var/lib/docker
Socket: /var/run/docker.sock
Processes
  PID 1842  dockerd  /usr/bin/dockerd
  PID 1764  containerd  /usr/bin/containerd

docker data root: /var/lib/docker
  Used: 86.7%  Inodes: 31.2%  Free: 42.8 GB
  Trend: +6.4 GB/hour
  Largest files
       31.4 GB  /var/lib/docker/overlay2/.../json.log  ↑ 1.8 MB/s
       18.7 GB  /var/lib/docker/containers/.../container-json.log
  Largest directories
       96.2 GB  /var/lib/docker/overlay2
       22.5 GB  /var/lib/docker/containers

BuildKit cache: /var/lib/buildkit
  Used: 43.1%  Inodes: 12.4%  Free: 210.0 GB
```

If Docker is not installed or its storage is elsewhere, the command prints `No Docker storage paths found.`. Configure a custom Docker `data-root` in `/etc/docker/daemon.json`; `diskc docker` will report that path automatically.

Inspect macOS system storage:

```bash
diskc mac system
diskc mac system --sample 5s --top 50
diskc mac system --json > mac-system-report.json
```

This macOS-only command checks `/private/var`, `/Library`, `/Users`, `/Applications`, and `/System`. It identifies large files and directories, measures optional growth, and gives a conservative cleanup recommendation for each file:

```text
macOS system disk report
Platform: darwin

/private/var (system state, logs, caches, and temporary data)
  Largest files
        9.8 GB  /private/var/folders/.../cache.db
      recommendation: Usually removable after closing the owning app; re-created when needed
      risk: review before cleaning
       6.2 GB  /private/var/log/system.log
      recommendation: Review age and ownership; rotate or remove only known-old logs
      risk: review before cleaning

/System (macOS system files)
       4.1 GB  /System/Library/...
      recommendation: Do not delete manually; managed by macOS
      risk: do not delete

No files are deleted automatically.
```

The recommendations are guidance, not permission to remove files. Never manually delete `/System`, `/usr`, `/bin`, `/sbin`, `/private/var/db`, `/private/var/vm`, application support data, containers, or files owned by an active process. Prefer the owning application's cleanup controls, macOS Storage settings, or a documented administrator procedure.

Watch continuously until `Ctrl-C`:

```bash
./diskc -watch
./diskc --watch --interval 5s /data
```

The default refresh interval is three seconds between refresh starts. If a scan takes longer, the next refresh starts immediately.

Find deleted-but-open files:

```bash
./diskc --deleted
```

Example:

```text
Deleted but still open
   38.2 GB  /var/log/payment/app.log  ← payment-api (PID 21819)
```

## JSON Output

```bash
./diskc /data --top 50 --sample 3s --deleted --json > disk-report.json
```

For multiple filesystems, JSON output is an array:

```json
[
  {
    "filesystem": {
      "path": "/data",
      "used_percent": 92.4,
      "inode_percent": 41
    },
    "files": [
      {
        "path": "/data/events.log",
        "size_bytes": 13314398617,
        "kind": "log",
        "growth_bytes_per_second": 4299161,
        "writers": [{"pid": 21819, "process": "payment-api", "exe": "/opt/payment/bin/payment-api", "uid": "1001"}]
      }
    ]
  }
]
```

With `--watch --json`, each refresh is emitted as a separate JSON document without terminal-control sequences.

## Installation

Requires Go 1.22 or newer:

```bash
go run ./cmd/diskc
go build -o diskc ./cmd/diskc
```

The module is organized for Homebrew. After publishing a tap:

```bash
brew tap your-org/diskc
brew install diskc
```

The formula builds `./cmd/diskc` and installs the resulting `diskc` binary.

## Flags

```text
--top N              number of large files (default: 20)
--depth N            maximum directory depth (default: 4)
--sample DURATION    measure growth (default: 3s; use 0 to disable)
--deleted            include deleted files held open by processes
--json               emit machine-readable JSON
--all                inspect all mounted physical filesystems
--watch              refresh continuously until interrupted
--interval DURATION  watch refresh interval (default: 3s)
```

## Disclaimer

Parts of this project, including source code, documentation, examples, and configuration, may be generated or assisted by artificial intelligence. The project is provided on an “AS IS” and “AS AVAILABLE” basis, without warranties of any kind.

Use `diskc` at your own risk. The authors, maintainers, contributors, and distributors are not liable for any damage, data loss, corruption, downtime, outage, security incident, or other loss arising from the use of all or any part of this project. Review, test, and validate the code and its output before using it in production.

## Contributing

The project uses only the Go standard library. Linux builds use filesystem statistics and `/proc` metadata.

Run formatting, tests, static analysis, and builds with:

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
go build ./cmd/diskc
GOOS=linux GOARCH=amd64 go build -o diskc-linux ./cmd/diskc
```

Pull requests and issues are welcome at [github.com/diskc-cli/diskc](https://github.com/diskc-cli/diskc).
