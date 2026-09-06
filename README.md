# diskc

Find what is filling a Linux or macOS filesystem, how fast it is growing, and which process is writing it.

`diskc` is a read-only SRE troubleshooting CLI. The default command is intentionally useful without a long list of options.

## Quick start

```bash
go build -o diskc ./cmd/diskc
./diskc
```

Most incidents only need one of these commands:

| Command | Purpose |
| --- | --- |
| `diskc` | Investigate the root filesystem |
| `diskc /var` | Investigate one filesystem or directory |
| `diskc -watch` | Refresh the investigation continuously |
| `diskc db` | Inspect local database storage and configuration |
| `diskc docker` | Inspect host Docker and containerd storage |
| `diskc mac system` | Inspect macOS system storage with cleanup guidance |

## What the default command does

Running `diskc`:

1. Checks byte and inode pressure on `/`.
2. Finds the largest scanned files and directories.
3. Samples all candidate files and device activity for three seconds.
4. Separately surfaces small files that are growing quickly.
5. Maps writable file descriptors to processes on Linux.
6. Detects deleted files that are still consuming disk space on Linux.
7. Estimates time-to-full from observed positive growth.
8. Reports Linux mount, I/O pressure, RAID, overlay, and copy-on-write findings when available.

Multiple explicit paths share one sampling window, so three filesystems do not add three separate three-second waits.

## Example

```text
$ diskc
Disk pressure: WARNING  92.4% used (38.0 GB free)
Inodes: 41.0% used  |  /
Observed growth: +65.5 GB/hour
Estimated full at observed growth: ~35 minutes

Largest files
   74.8 GB  /var/log/payment/app.log  (log)  ↑ 18.2 MB/s  [1 writer(s)]
   42.1 GB  /tmp/upload-883120.bin   (temporary/cache)

Largest scanned directories
  128.4 GB  /var/log
   67.2 GB  /tmp

Active writers
/var/log/payment/app.log
└─ PID 21819  payment-api
   ├─ exe      /opt/payment/bin/payment-api
   └─ service  payment.service

Potential issues
/var/log/payment/app.log
└─ rapid growth: 65.5 GB/hour
Filesystem could fill in ~35 minutes at the observed aggregate growth.

Deleted but still open (38.2 GB retained)
   38.2 GB  /var/log/payment/app-old.log  ← payment-api (PID 21819)
```

Directory totals represent files observed within the configured scan depth and permissions. They are not complete recursive totals when parts of the tree were not scanned.

## Common workflows

Inspect several mounted filesystems:

```bash
diskc / /data /backup
diskc --all
```

Watch a busy filesystem:

```bash
diskc -watch /var
```

Inspect database storage:

```bash
diskc db
```

`diskc db` discovers PostgreSQL, MySQL/MariaDB, Redis, ClickHouse, and MongoDB using common Linux processes, configuration files, and data directories. It does not connect to a database or modify data.

Inspect Docker storage:

```bash
diskc docker
```

This checks common rootful and rootless Docker locations, Docker's configured `data-root`, containerd storage, Docker logs, BuildKit cache, sockets, and runtime processes. It does not call the Docker API or delete container data.

Inspect macOS system storage:

```bash
diskc mac system
```

This checks `/private/var`, `/Library`, `/Users`, `/Applications`, and `/System`. Every large file receives conservative guidance such as `review before cleaning` or `do not delete`. No file is deleted automatically.

## JSON

Use JSON for automation or attaching evidence to an incident:

```bash
diskc /data --json > disk-report.json
diskc docker --json > docker-report.json
diskc db --json > database-report.json
diskc mac system --json > mac-report.json
```

When several filesystems are inspected, the JSON result is an array. In watch mode, each refresh is emitted as a separate JSON document without terminal control sequences.

## Advanced options

The defaults are designed for incident use. Tune them only when the initial result needs a narrower or deeper scan.

```text
--top N              files to report (default: 20)
--depth N            scan depth (default: 4)
--sample DURATION    growth window (default: 3s; 0 disables sampling)
--all                inspect all mounted physical filesystems
--watch              refresh continuously
--interval DURATION  watch refresh interval (default: 3s)
--json               emit machine-readable JSON
```

The former `--deleted` option is accepted for compatibility but is no longer required; deleted-open detection now runs automatically where `/proc` is available.

## Safety and limitations

- `diskc` is diagnostic and read-only. It does not delete, truncate, restart, kill, or modify anything.
- Run it as the same user as the suspected process when possible. Linux `/proc` and filesystem permissions may hide data owned by other users.
- Time-to-full is an estimate based only on growth observed during the sample window; bursty workloads can change quickly.
- Database files, Docker data, macOS system files, and application support data must not be deleted solely because they appear in a report.
- `diskc` identifies copy-on-write and overlay risks but does not replace filesystem-specific snapshot tools or vendor SMART/NVMe diagnostics.

## Installation

Requires Go 1.22 or newer:

```bash
go install github.com/debugc-clis/diskc/cmd/diskc@latest
```

Or build from a clone:

```bash
go build -o diskc ./cmd/diskc
```

Homebrew packaging is planned; do not rely on `brew install diskc` until a formula is published.

## Contributing

The project uses only the Go standard library.

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build -o /tmp/diskc-linux ./cmd/diskc
GOOS=darwin GOARCH=arm64 go build -o /tmp/diskc-macos ./cmd/diskc
```

Issues and pull requests are welcome at [github.com/debugc-clis/diskc](https://github.com/debugc-clis/diskc).

## Disclaimer

Parts of this project, including source code, documentation, examples, and configuration, may be generated or assisted by artificial intelligence. The project is provided on an “AS IS” and “AS AVAILABLE” basis, without warranties of any kind.

Use `diskc` at your own risk. The authors, maintainers, contributors, and distributors are not liable for damage, data loss, corruption, downtime, outage, security incidents, or other losses arising from the use of all or any part of this project. Review, test, and validate the code and its output before using it in production.
