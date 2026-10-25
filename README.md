# diskc

`diskc` is a read-only Linux and macOS CLI for answering the first questions in a disk incident:

- Which filesystem is under pressure?
- Which files and directories consume real disk blocks?
- Which files are growing, and how quickly?
- Which Linux processes are writing them?
- Are deleted-but-open files retaining space?
- Is the pressure related to a database, Docker, mounts, inodes, I/O, or RAID?

It runs without `sudo` or root access. Permissions still determine which files and Linux processes it can see.

## Quick start

```bash
go build -o diskc ./cmd/diskc
./diskc
```

Most incidents only need one command:

| Command | Purpose |
| --- | --- |
| `diskc` | Investigate `/` with useful SRE defaults |
| `diskc light` | Take a fast, low-impact snapshot |
| `diskc list` | List mounted filesystems in a risk-first table |
| `diskc /var /data` | Inspect one or several explicit paths |
| `diskc -watch` | Refresh continuously |
| `diskc db` | Inspect database configuration and disk paths |
| `diskc docker` | Inspect Docker/containerd host storage |
| `diskc doc` | Diagnose diskc defaults, mount access, capacity, and process visibility |
| `diskc mac system` | Inspect macOS system storage safely |
| `diskc version` | Show build and platform information |

## What `diskc` does

Running `diskc` with no arguments:

1. Reads byte, reserved-space, and inode pressure for `/`.
2. Scans regular files on that filesystem to the default depth of four.
3. Ranks files and directories by allocated disk blocks, not apparent length.
4. Samples all candidate files and device activity during one shared three-second window.
5. Separately reports the largest and fastest-growing files.
6. Maps writable Linux file descriptors to their processes.
7. Detects deleted-but-open Linux files that still retain allocated blocks.
8. Estimates time-to-full from net filesystem available-byte loss during the sampling window; per-file growth identifies contributors. JSON labels the basis as `filesystem_available_bytes`.
9. Adds Linux mount, I/O pressure, RAID, overlay, and copy-on-write findings when available.

Hard links are counted once per scan. Sparse files retain their apparent length in JSON and text annotations but are ranked by the blocks they actually consume.

## Default example

```text
$ diskc
Disk pressure: WARNING  92.4% unavailable (38.0 GB available, 42.0 GB free)
Filesystem usage: 91.6% used  |  4.0 GB reserved
Inodes: 41.0% used  |  /
Filesystem net growth: +65.5 GB/hour
Estimated full at net growth: ~35 minutes

Largest files
   74.8 GB  /var/log/payment/app.log  (log)  ↑ 18.2 MB/s  [1 writer(s)]
   42.1 GB  /tmp/upload-883120.bin  (temporary/cache)

Largest scanned directories
  128.4 GB  /var/log
   67.2 GB  /tmp

Fastest growth
   18.2 MB/s  /var/log/payment/app.log  (log)

Active writers
/var/log/payment/app.log
└─ PID 21819  payment-api
   ├─ exe      /opt/payment/bin/payment-api
   └─ service  payment.service

Deleted but still open (38.2 GB retained)
   38.2 GB  /var/log/payment/app-old.log  ← payment-api (PID 21819)
```

Directory totals cover only files observed within the selected depth and current user's permissions. If a very large scan exceeds the bounded directory tracker, estimated totals are labeled and a warning is emitted.

## List filesystems

Use `diskc list` for an instant host view without traversing directory contents:

```text
$ diskc list
STATUS    MOUNT    SOURCE          TYPE/KIND     SIZE      USED      AVAIL    PRESS%  INODE%  MODE  CAPACITY
CRITICAL  /var     /dev/nvme0n1p3  ext4/local   180.0 GB  168.0 GB  7.8 GB   95.7%   42.1%   rw    dedicated
WARNING   /data    /dev/nvme1n1    xfs/local    1.8 TB    1.6 TB    136.0 GB  92.6%   18.4%   rw    dedicated
REMOTE    /backup  host:/backup    nfs4/remote  -         -         -         -       -       ro    -
```

Remote and unknown filesystem types are listed but not probed, which avoids blocking on a stale NFS, CIFS, or similar mount. `diskc --all` also excludes remote mounts. Inspecting an explicit path such as `diskc /backup` remains an intentional opt-in and can block if that mount is stale.

APFS rows that expose the same shared container capacity are marked `shared`; do not add those capacities together.

## Light mode

Use light mode for a quick first look on a busy production host:

```bash
diskc light
diskc light /var
diskc light --all
```

Light mode uses `--top 10 --depth 2 --sample 0`. It keeps capacity, inode, file, directory, and basic health checks while skipping growth waiting, Linux process descriptor inspection, and deleted-open-file discovery.

## Multiple filesystems

Explicit paths share one growth-sampling window, so three filesystems do not add three separate three-second waits:

```bash
diskc / /var /data
diskc --all
```

Host-wide PSI and RAID findings are printed once. Deleted-open files are printed once per device even when several requested paths reside on the same filesystem.

## Watch mode

```bash
diskc -watch
diskc -watch /var --interval 2s
diskc light -watch /var
```

Each refresh starts after the configured delay from the previous completed scan, preventing an expensive scan from turning into a continuous loop. Interactive terminals are redrawn; redirected text receives timestamped refresh separators and no terminal escape sequences.

JSON watch output is newline-delimited JSON (NDJSON), one complete compact JSON value per refresh:

```bash
diskc -watch --json --sample 0 >> diskc.ndjson
```

## Database storage

```bash
diskc db
diskc db --top 10 --sample 5s
diskc db --json
```

`diskc db` discovers common PostgreSQL, MySQL/MariaDB, Redis, ClickHouse, and MongoDB processes, configuration files, data directories, log paths, and disk settings. It does not connect to a database or modify data. All discovered paths share one sampling window, and unavailable paths and partial-scan warnings remain visible.

## Docker storage

```bash
diskc docker
diskc docker --top 10 --sample 5s
diskc docker --json
```

`diskc docker` checks configured and conventional rootful/rootless Docker data roots, containerd storage, Docker logs, BuildKit cache, sockets, and runtime processes. It does not call the Docker API and does not delete container data. All storage paths share one sampling window.

## Doctor

```bash
diskc doc
diskc doc /data
diskc doc --json
```

`diskc doc` is a quick health check similar to `brew doctor`. It shows the build and all built-in scan defaults, then checks mount discovery, access to the selected filesystem, capacity and inode pressure, and Linux `/proc` availability. The default target is `/`; pass a path to diagnose another filesystem. It does not walk files or wait for growth samples. Issues are findings in the report, while usage and runtime failures keep the normal exit codes. `diskc` has no configuration file; per-run flags override the defaults shown here.

Example:

```text
$ diskc doc /data
diskc doctor
Build: dev (none)  Go: go1.22.2  Platform: linux/amd64
Built: unknown
Target: /data

Configuration (built-in defaults; no configuration file)
  full: top=20 depth=4 sample=3s
  light: top=10 depth=2 sample=0s
  db: top=20 depth=4 sample=3s
  docker: top=20 depth=4 sample=3s
  mac system: top=20 depth=5 sample=0s
  all mounts: false  watch: false  json: false
  watch interval: 3s
  Linux process checks: true  deleted-open checks: true

Mount: /data  source=/dev/nvme1n1  type=xfs  kind=local

Checks
  [OK] mount inventory: 6 mounted filesystems discovered
  [WARNING] capacity: 92.6% unavailable; 136.0 GB available
       next: investigate large and growing files with diskc on this path
  [OK] inodes: 18.4% used
  [OK] process inspection: /proc is readable; other users' processes may still be hidden by permissions

1 issue(s) found.
```

## macOS system storage

```bash
diskc mac system
diskc mac system --top 10
diskc mac system --json
```

The command checks `/private/var`, `/Library`, `/Users`, `/Applications`, and `/System`. Large files receive conservative guidance such as `review before cleaning` or `do not delete manually`. Nothing is deleted automatically.

## JSON output

Use JSON for automation or incident evidence:

```bash
diskc /data --json > disk-report.json
diskc list --json > filesystem-report.json
diskc db --json > database-report.json
diskc docker --json > docker-report.json
diskc doc --json > doctor-report.json
diskc mac system --json > mac-report.json
```

Multiple root filesystems produce an array. File and directory objects include `allocated_bytes` and `apparent_bytes`. Filesystem objects distinguish:

- `used_bytes`: blocks consumed on the filesystem.
- `free_bytes`: all free blocks, including blocks unavailable to unprivileged users.
- `available_bytes`: blocks currently available to an unprivileged process.
- `reserved_bytes`: free blocks not available to an unprivileged process.
- `used_percent`: actual consumed capacity.
- `pressure_percent`: capacity unavailable to an unprivileged process; alert status uses this value.

## Options

The defaults are designed for incident response; tune them only when needed:

```text
--top N              largest and fastest-growing files to report (default: 20; max: 1000)
--depth N            maximum directory depth (default: 4)
--sample DURATION    growth window (default: 3s; 0 disables sampling)
--all                inspect automatically discovered local filesystems
--watch              refresh continuously
--interval DURATION  delay after each completed watch scan (default: 3s)
--json               emit JSON; watch mode emits NDJSON
```

Paths and flags can be interspersed, for example:

```bash
diskc --top 5 /var --json --sample 0
```

Use `--` before a path beginning with `-`. The former `--deleted` flag is accepted for compatibility but is no longer required.

## Exit status

| Code | Meaning |
| --- | --- |
| `0` | Success, help, or a downstream pipe that closed normally |
| `1` | Runtime inspection or output failure |
| `2` | Invalid flags, values, or command usage |

Help is written to stdout. Diagnostics are written to stderr. Text output escapes terminal control characters found in paths, process names, configuration values, or kernel-provided labels.

## Installation

Requires Go 1.22 or newer:

```bash
go install github.com/debugc-clis/diskc/cmd/diskc@latest
```

Or build from a clone:

```bash
go build -o diskc ./cmd/diskc
./diskc version
```

Tagged releases publish Linux and macOS archives for `amd64` and `arm64`, SHA-256 checksums, and archive SBOMs. Homebrew packaging is planned; do not rely on `brew install diskc` until a formula is published.

Install the included manual page locally if desired:

```bash
man ./docs/diskc.1
```

## Safety and limitations

- `diskc` is diagnostic and read-only. It does not delete, truncate, restart, kill, or modify anything.
- Run it as the affected service user when practical. Filesystem and Linux `/proc` permissions may hide files or processes owned by other users.
- Time-to-full uses net filesystem available-byte loss during the short sample, including activity outside scanned candidates. Recovery or deletion can offset growth; zero or negative net growth has no time-to-full estimate. Bursty workloads and block allocation granularity can change the result.
- Database files, Docker data, macOS system files, and application support data must not be deleted solely because they appear in a report.
- Copy-on-write, snapshots, compression, reflinks, quotas, and thin provisioning can make filesystem totals differ from ordinary file sums.
- `diskc` does not replace filesystem-specific snapshot, quota, SMART, NVMe, or vendor database tools.

## Disclaimer

Parts of this project may be generated or assisted by artificial intelligence. The project is provided “AS IS” without warranties. Use it at your own risk. The authors, maintainers, contributors, and distributors are not liable for damage, data loss, corruption, downtime, outages, security incidents, or other losses arising from use of all or any part of the project.

Review, test, and validate the code and its output before production use. See [DISCLAIMER.md](DISCLAIMER.md) for the full notice.

## Contributing

The runtime uses only the Go standard library.

```bash
gofmt -w cmd internal
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/diskc-linux ./cmd/diskc
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/diskc-macos ./cmd/diskc
```

Issues and pull requests are welcome at [github.com/debugc-clis/diskc](https://github.com/debugc-clis/diskc).

## License

MIT. See [LICENSE](LICENSE).
